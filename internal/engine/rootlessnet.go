package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

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
// within is how long the caller is prepared to wait for an answer, and it is
// the caller's statement about timing rather than a retry for its own sake.
//
// Create passes nothing. It asks once the fabric is up, where the keepalive
// holds the namespace and pasta is already running in it, so a probe that had
// to wait would be waiting on something that cannot arrive later.
//
// The doctor passes a budget, because it asks with nothing up. Its probe is
// what CREATES the namespace, and podman starts pasta in it asynchronously; a
// single attempt then reports a broken host on a machine that boots microVMs
// perfectly well a moment later. Measured on a four-core hosted runner, where
// every leg of a six-way tier failed this check and none of them had anything
// wrong with it.
//
// It runs for as long as one request takes on a host that answers, because it
// returns on the first success. The budget is only ever spent by a host that
// is failing.
func PastaIsSetUp(ctx context.Context, r run.Runner, within time.Duration) bool {
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
	// ONE attempt, and the caller owns the timing. Create asks after the fabric
	// is up, where the keepalive holds the namespace and pasta is already
	// running, so there is nothing to race and nothing to retry. A probe that
	// creates the namespace itself is asking about one it just made, and on a
	// slower host can outrun podman starting pasta — which reads as a broken
	// host and is not one.
	//
	ask := func() bool {
		res, err := r.Run(ctx, run.Opts{ReadOnly: true},
			"podman", "unshare", "--rootless-netns", "curl", "-s", "-o", "/dev/null",
			"-w", "%{http_code}", "--max-time", "3", "http://"+HostReachableIP+":"+port+"/")
		return err == nil && strings.TrimSpace(res.Stdout) == "200"
	}
	deadline := time.Now().Add(within)
	for {
		if ask() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// ErrNoPasta is what a caller gets when the hop does not work, and it is one
// sentence on purpose. The cause is always the same requirement, and a report
// that enumerated the ways of failing it took longer to read than to act on.
var ErrNoPasta = fmt.Errorf(
	"fc: nothing answers at %s from podman's rootless network, where a microVM looks for the host. "+
		"The firecracker engine needs podman 5.0 or later with pasta networking: check `podman "+
		"--version` and that passt is installed", HostReachableIP)

func (fe *Firecracker) ensurePasta(ctx context.Context) error {
	// Nothing: the fabric is up by now, so the answer cannot change by waiting.
	if PastaIsSetUp(ctx, fe.d.Runner, 0) {
		return nil
	}
	return ErrNoPasta
}
