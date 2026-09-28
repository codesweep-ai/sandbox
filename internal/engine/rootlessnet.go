package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// What a microVM needs of the host's networking, and how this engine checks it.
//
// A firecracker guest reaches the host at HostReachableIP, and that address is
// PASTA's host-loopback mapping — nothing else publishes it. Podman's other
// rootless stack, slirp4netns, puts the host at its own gateway instead, so the
// address the seed pins answers for nobody. Packets leave and are dropped, which
// is the worst shape this can take: not a refusal a caller can read, but a
// ten-second timeout surfacing somewhere inside their agent.
//
// So the engine requires podman 5.0 or later. That is the release where pasta
// became the default rootless network, and no configuration gets it out of an
// older one — 4.x reads default_rootless_network_cmd for a container's own
// network mode while leaving the shared namespace on slirp4netns. Measured, on
// podman 4.9.3 with passt installed and the setting in place: the namespace
// still came up slirp4netns. Upstream agrees with the direction; podman 6.0
// removes slirp4netns outright.

// PastaIsSetUp answers the only question that matters here: from inside the
// namespace a microVM's tap lives in, does the host answer at HostReachableIP?
//
// Asked by DOING it, because every indirect reading of this was wrong on some
// real host. Podman reports the stack it is configured for rather than the one
// it runs, and a host without slirp4netns installed reports slirp4netns while
// serving pasta. Podman below 5.0 has no such field at all. The namespace's own
// addressing is a signature, and a signature is a guess about a program's
// habits. A request that arrives is not a guess.
//
// The listener is NON-LOOPBACK deliberately, and the check is worthless
// otherwise: pasta forwards this address to the host's outward address, not to
// its loopback. Measured — the same probe bound on 127.0.0.1 is refused and on
// 0.0.0.0 answers 200. It is the same reason the lender binds non-loopback.
//
// One attempt, and the caller owns the timing. Create asks after the fabric is
// up, where the keepalive holds the namespace and pasta is already running, so
// there is nothing to race and nothing to retry.
//
// It runs for as long as one request takes, on a port the kernel picks.
func PastaIsSetUp(ctx context.Context, r run.Runner) bool {
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return false
	}
	defer func() { _ = l.Close() }()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()

	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return false
	}
	// Through podman's rootless netns, which is where a microVM's tap is.
	//
	// The ANSWER decides this, not podman's exit status, and the two disagree
	// on a real host.
	//
	// `podman unshare --rootless-netns` tears the namespace down again when
	// nothing else is holding it, and that teardown can fail on its own terms:
	// measured on a GitHub hosted runner, every invocation ended with "rootless
	// netns: cleanup: kill network process: permission denied" and a non-zero
	// exit, AFTER curl had already been answered with 200. Requiring err == nil
	// threw the answer away and reported a host with no pasta as one that has
	// it — on a runner that then booted microVMs perfectly well.
	//
	// It is also why create never saw this and the doctor always did. Create
	// asks while the keepalive holds the namespace, so podman leaves it up and
	// has nothing to fail at; the doctor asks with nothing up, so every one of
	// its probes pays the teardown.
	//
	// A command that could not run at all returns no "200" either, so dropping
	// the error costs nothing: this asks whether the host answered, and only
	// the body can say so.
	ask := func() bool {
		res, _ := r.Run(ctx, run.Opts{ReadOnly: true},
			"podman", "unshare", "--rootless-netns", "curl", "-s", "-o", "/dev/null",
			"-w", "%{http_code}", "--max-time", "3", "http://"+HostReachableIP+":"+port+"/")
		return strings.TrimSpace(res.Stdout) == "200"
	}
	return ask()
}

// ErrNoPasta is what a caller gets when the hop does not work on a namespace
// that has the host's own address, and it is one sentence on purpose. The cause
// is then the same requirement, and a report that enumerated the ways of failing
// it took longer to read than to act on.
var ErrNoPasta = fmt.Errorf(
	"fc: nothing answers at %s from podman's rootless network, where a microVM looks for the host. "+
		"The firecracker engine needs podman 5.0 or later with pasta networking: check `podman "+
		"--version` and that passt is installed", HostReachableIP)

// ensurePasta checks the hop with the fabric up. A namespace set up before the
// host changed networks is rebuilt when nothing but idle fabrics holds it, and
// the fabric brought up again in the new one.
func (fe *Firecracker) ensurePasta(ctx context.Context, fab fcnet.Fabric) error {
	r := fe.d.Runner
	if PastaIsSetUp(ctx, r) {
		return nil
	}
	stale := StaleRootlessAddr(ctx, r)
	if stale == "" {
		return ErrNoPasta
	}
	if busy := rootlessBusy(ctx, r, fe.anyVMRunning(ctx)); busy != "" {
		return staleError(stale, busy)
	}
	releaseRootless(ctx, r)
	if err := fab.Up(ctx); err != nil {
		return err
	}
	if PastaIsSetUp(ctx, r) {
		return nil
	}
	return staleError(stale, "")
}

// StaleRootlessAddr says how podman's rootless network namespace has fallen
// behind the host, or nothing where it has not. pasta copies the host's
// addresses in once, when it starts. After the host moves to another network,
// as a laptop does on a new Wi-Fi lease, the namespace keeps the old address
// while anything holds it, and nothing answers at HostReachableIP inside it
// (SBX-088).
func StaleRootlessAddr(ctx context.Context, r run.Runner) string {
	res, _ := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "unshare", "--rootless-netns",
		"ip", "-4", "-o", "addr", "show", "scope", "global")
	return staleAddr(res.Stdout, hostAddrs())
}

// staleAddr compares the namespace's `ip -4 -o addr` with the host's addresses,
// interface by interface. Only an interface the host has counts: the bridges in
// the namespace are its own.
func staleAddr(nsAddrs string, host map[string][]string) string {
	for line := range strings.SplitSeq(nsAddrs, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "inet" {
			continue
		}
		name := f[1]
		ip, _, _ := strings.Cut(f[3], "/")
		have, ok := host[name]
		if !ok || slices.Contains(have, ip) {
			continue
		}
		if len(have) == 0 {
			return fmt.Sprintf("%s on %s, which has no address on the host now", ip, name)
		}
		return fmt.Sprintf("%s on %s, where the host now has %s", ip, name, strings.Join(have, ", "))
	}
	return ""
}

// hostAddrs is the host's global IPv4 addresses by interface, with an entry for
// each interface even where it has none. A variable, so a test can stand in
// for the host.
var hostAddrs = func() map[string][]string {
	out := map[string][]string{}
	ifs, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, i := range ifs {
		out[i.Name] = nil
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsGlobalUnicast() {
				out[i.Name] = append(out[i.Name], n.IP.String())
			}
		}
	}
	return out
}

// rootlessBusy names what, besides the fabrics' keepalives and resolvers, runs
// in podman's rootless namespace, or nothing. Such a thing is somebody's work,
// and a rebuild would take its network from under it. vm is whether a microVM
// of this root runs; one of any root counts too.
func rootlessBusy(ctx context.Context, r run.Runner, vm bool) string {
	if vm || microVMRunning() {
		return "a microVM"
	}
	var others []string
	out := run.Output(ctx, r, "podman", "ps", "--format", `{{.Names}} {{index .Labels "cs-sandbox.keepalive"}}`)
	for line := range strings.SplitSeq(out, "\n") {
		name, keepalive, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name != "" && keepalive != "1" {
			others = append(others, name)
		}
	}
	if len(others) > 0 {
		return "the container " + strings.Join(others, ", ")
	}
	return ""
}

// microVMRunning reports whether a firecracker process runs on the host,
// whichever root started it. A variable, so a test can stand in for the host.
var microVMRunning = func() bool {
	comms, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, c := range comms {
		if b, err := os.ReadFile(c); err == nil && strings.TrimSpace(string(b)) == "firecracker" {
			return true
		}
	}
	return false
}

// releaseRootless lets go of podman's rootless namespace: it removes the
// fabrics' keepalives and stops their resolvers, which the next Up starts
// again, then waits for podman to tear the namespace down, so that the fabric
// that follows comes up in a new one.
func releaseRootless(ctx context.Context, r run.Runner) {
	if ids := strings.Fields(run.Output(ctx, r, "podman", "ps", "-q", "--filter", "label=cs-sandbox.keepalive=1")); len(ids) > 0 {
		_, _ = r.Run(ctx, run.Opts{}, append([]string{"podman", "rm", "-f"}, ids...)...)
	}
	fcnet.StopResolvers()
	for range 10 {
		if StaleRootlessAddr(ctx, r) == "" {
			return
		}
		time.Sleep(time.Second)
	}
}

// staleError says what is wrong with a namespace the host has moved away from,
// and what the caller can do about it.
func staleError(stale, busy string) error {
	why := fmt.Sprintf("fc: podman's rootless network still has %s. The host changed networks after "+
		"that network was set up, so nothing answers at %s from it", stale, HostReachableIP)
	if busy == "" {
		return fmt.Errorf("%s, and setting it up again did not help: stop every podman container and "+
			"microVM, then create again", why)
	}
	return fmt.Errorf("%s. It is set up again once nothing else runs in it, and %s still does: stop "+
		"that, then create again", why, busy)
}
