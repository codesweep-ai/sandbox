// Package fcnet is the firecracker network fabric: the shared L2 network that
// microVMs and podman containers both live on (see docs/firecracker.md).
//
// Firecracker runs INSIDE podman's rootless network namespace so it can reach
// /dev/kvm and the tap on podman's bridge. This package manages, all through
// `podman unshare --rootless-netns`:
//
//   - a "keepalive" container that pins podman's netns + bridge + aardvark-dns;
//   - a forwarding dnsmasq (on a dedicated <prefix>.53 address) that resolves VM
//     names from a hostsdir and forwards everything else to aardvark;
//   - per-VM taps on the bridge;
//   - VM-name registration in the dnsmasq hostsdir;
//   - the host→VM socat forwarder (a unix-socket bridge across the netns boundary).
//
// Detached long-lived children (dnsmasq, socat, firecracker) are supervised
// directly via os/exec + Setpgid + pid files, not the capturing Runner, which is
// for commands that run to completion (same approach as internal/forward).
package fcnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// KeepaliveName is the container that pins podman's rootless netns + bridge.
const KeepaliveName = "cs-sandbox-net-keepalive"

// InternalSSHPort is the port the guest sshd listens on.
const InternalSSHPort = 22

// Fabric bundles the dependencies for managing the shared network.
type Fabric struct {
	Runner  run.Runner
	Network string // podman network name (cs-sandbox-net)
	Image   string // keepalive container image
	NetDir  string // the fabric working dir (paths.FCNet): dnsmasq hostsdir + log
}

// --- read-only queries (through the Runner) ---

// Gateway returns the podman network gateway, e.g. 10.89.0.1.
func (f Fabric) Gateway(ctx context.Context) string {
	return run.Output(ctx, f.Runner, "podman", "network", "inspect", f.Network,
		"--format", "{{(index .Subnets 0).Gateway}}")
}

// Prefix returns the /24 prefix, e.g. 10.89.0.
func (f Fabric) Prefix(ctx context.Context) string {
	gw := f.Gateway(ctx)
	if i := strings.LastIndexByte(gw, '.'); i > 0 {
		return gw[:i]
	}
	return gw
}

// DNSIP returns the forwarding dnsmasq address, <prefix>.53.
func (f Fabric) DNSIP(ctx context.Context) string { return f.Prefix(ctx) + ".53" }

// Bridge returns the bridge interface netavark assigned to the network.
func (f Fabric) Bridge(ctx context.Context) string {
	return run.Output(ctx, f.Runner, "podman", "network", "inspect", f.Network,
		"--format", "{{.NetworkInterface}}")
}

func (f Fabric) keepaliveRunning(ctx context.Context) bool {
	return run.Output(ctx, f.Runner, "podman", "inspect", KeepaliveName,
		"--format", "{{.State.Running}}") == "true"
}

// --- fabric bring-up / teardown ---

func (f Fabric) hostsDir() string { return filepath.Join(f.NetDir, "hosts.d") }

// keepaliveUp ensures the keepalive container is running.
func (f Fabric) keepaliveUp(ctx context.Context) error {
	if f.keepaliveRunning(ctx) {
		return nil
	}
	// Try to start an existing (stopped) one.
	if _, err := f.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "container", "exists", KeepaliveName); err == nil {
		_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "start", KeepaliveName)
		if f.keepaliveRunning(ctx) {
			return nil
		}
	}
	_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "run", "-d", "--name", KeepaliveName,
		"--network", f.Network, "--restart=always",
		"--label", "cs-sandbox.managed=1", "--label", "cs-sandbox.keepalive=1",
		f.Image, "sleep", "infinity")
	if f.keepaliveRunning(ctx) {
		return nil
	}
	_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "start", KeepaliveName)
	if f.keepaliveRunning(ctx) {
		return nil
	}
	return fmt.Errorf("fc: could not start the network keepalive container")
}

// dnsProc is a live dnsmasq: the address it answers on and the hostsdir it
// serves, both read straight from its command line.
type dnsProc struct {
	pid      int
	addr     string
	hostsDir string
}

// scanDNSMasq lists every running dnsmasq on the host, with the address and
// hostsdir each was started with. The fabric's DNS is a property of the host, so
// this asks the host: a pidfile goes stale whenever the process outlives the
// bookkeeping, and a root that never wrote one would otherwise miss a resolver
// that is already running.
func scanDNSMasq() []dnsProc {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []dnsProc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || len(raw) == 0 {
			continue
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		// argv[0] identifies the program: the launcher that execs dnsmasq carries
		// the same flags in its own command line, but not as argv[0].
		if filepath.Base(argv[0]) != "dnsmasq" {
			continue
		}
		p := dnsProc{pid: pid}
		for _, a := range argv[1:] {
			if v, ok := strings.CutPrefix(a, "--listen-address="); ok {
				p.addr = v
			}
			if v, ok := strings.CutPrefix(a, "--hostsdir="); ok {
				p.hostsDir = filepath.Clean(v)
			}
		}
		if p.addr != "" {
			out = append(out, p)
		}
	}
	return out
}

// dnsState reports the fabric's resolver: its pid if it is up and ours, 0 if
// nothing holds the address. One on our address serving a DIFFERENT hostsdir is
// a conflict, not something to adopt — its answers come from another directory,
// so names registered here would never resolve.
func (f Fabric) dnsState(ctx context.Context) (int, error) {
	return pickDNS(scanDNSMasq(), f.DNSIP(ctx), f.hostsDir())
}

// pickDNS applies dnsState's rules to an already-gathered process list.
func pickDNS(procs []dnsProc, addr, hostsDir string) (int, error) {
	want := filepath.Clean(hostsDir)
	for _, p := range procs {
		if p.addr != addr {
			continue
		}
		if p.hostsDir != want {
			return 0, fmt.Errorf("fc: dnsmasq (pid %d) already owns the fabric address %s "+
				"but serves %s instead of %s — the fabric's working dir is host-global; "+
				"unset CS_SANDBOX_FC_NET so every sandbox root shares one",
				p.pid, addr, p.hostsDir, want)
		}
		return p.pid, nil
	}
	return 0, nil
}

// dnsPid finds our dnsmasq by the hostsdir it serves, which identifies it
// without needing the network read that resolving the address would cost.
func (f Fabric) dnsPid() int {
	want := filepath.Clean(f.hostsDir())
	for _, p := range scanDNSMasq() {
		if p.hostsDir == want {
			return p.pid
		}
	}
	return 0
}

// dnsUp starts the forwarding dnsmasq (in the netns) if not already running.
func (f Fabric) dnsUp(ctx context.Context) error {
	if err := os.MkdirAll(f.hostsDir(), 0o755); err != nil {
		return err
	}
	if _, err := exec.LookPath("dnsmasq"); err != nil {
		return fmt.Errorf("fc: dnsmasq not found")
	}
	if pid, err := f.dnsState(ctx); err != nil {
		return err
	} else if pid > 0 {
		return nil
	}
	gw := f.Gateway(ctx)
	dns := f.DNSIP(ctx)
	br := f.Bridge(ctx)
	if gw == "" {
		return fmt.Errorf("fc: cannot read podman network %q gateway", f.Network)
	}
	if br == "" {
		return fmt.Errorf("fc: cannot read podman network %q bridge interface", f.Network)
	}
	// Stay as (userns-)root so dnsmasq can traverse $HOME (mode 750) to re-read
	// --hostsdir; the default drop to "nobody" cannot. An empty --pid-file asks
	// dnsmasq not to write one: the running process is the record (see dnsState).
	// Values are passed as positional args, never interpolated into the script, so
	// a path with a space or quote (e.g. under $HOME) can't split or inject.
	const script = `set -eu
DNS="$1"; BR="$2"; GW="$3"; HOSTSDIR="$4"
ip addr add "$DNS/24" dev "$BR" 2>/dev/null || true
exec dnsmasq --keep-in-foreground --user=root --group=root --bind-interfaces \
  --listen-address="$DNS" --no-hosts --no-resolv --server="$GW" \
  --hostsdir="$HOSTSDIR" --pid-file= --conf-file=/dev/null`
	logf, err := os.Create(filepath.Join(f.NetDir, "dnsmasq.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command("podman", "unshare", "--rootless-netns", "bash", "-c", script,
		"_", dns, br, gw, f.hostsDir())
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 40; i++ {
		if pid, err := f.dnsState(ctx); err != nil {
			return err
		} else if pid > 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("fc: dnsmasq failed to start (%s)", filepath.Join(f.NetDir, "dnsmasq.log"))
}

// Up brings the whole fabric up: keepalive + dnsmasq (network creation is done
// by the caller). Serialize under the create lock.
func (f Fabric) Up(ctx context.Context) error {
	if err := f.keepaliveUp(ctx); err != nil {
		return err
	}
	if err := f.dnsUp(ctx); err != nil {
		return err
	}
	f.sweepNames(ctx)
	return nil
}

// sweepNames drops name records a create left behind when it died outright, with
// no chance to clean up.
//
// A name is registered only once its tap exists, so a record with no tap has no
// VM answering. Testing taps rather than an instance list is what makes this safe
// with several sandbox roots on one fabric: a root must not delete the live names
// of sandboxes it cannot see. A stopped sandbox re-registers on start. Records
// starting with "_" belong to other machinery and are left alone.
func (f Fabric) sweepNames(ctx context.Context) {
	ents, err := os.ReadDir(f.hostsDir())
	if err != nil {
		return
	}
	taps := f.TapOctets(ctx)
	dropped := false
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		rec := filepath.Join(f.hostsDir(), e.Name())
		data, err := os.ReadFile(rec)
		if err != nil {
			continue
		}
		ip, _, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
		if taps[lastOctet(ip)] {
			continue
		}
		if os.Remove(rec) == nil {
			dropped = true
		}
	}
	if dropped {
		f.Reload()
	}
}

// Down tears down the fabric (kills dnsmasq, drops the DNS address, removes the
// keepalive).
func (f Fabric) Down(ctx context.Context) {
	if pid := f.dnsPid(); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if f.keepaliveRunning(ctx) {
		_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "unshare", "--rootless-netns",
			"ip", "addr", "del", f.DNSIP(ctx)+"/24", "dev", f.Bridge(ctx))
	}
	_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", KeepaliveName)
}

// GC tears down the fabric only if nothing uses it: no VM is running AND no
// sandbox container (other than the keepalive) remains.
// vmRunning reports whether any firecracker VM is still up.
func (f Fabric) GC(ctx context.Context, vmRunning func() bool) {
	if vmRunning != nil && vmRunning() {
		return
	}
	names := run.Output(ctx, f.Runner, "podman", "ps", "-a",
		"--filter", "label=cs-sandbox.managed=1", "--format", "{{.Names}}")
	for _, n := range strings.Split(names, "\n") {
		n = strings.TrimSpace(n)
		if n != "" && n != KeepaliveName {
			return // a sandbox container remains — keep the fabric
		}
	}
	f.Down(ctx)
}

// --- dnsmasq hostsdir registration ---

// Reload SIGHUPs dnsmasq to promptly re-read the hostsdir.
func (f Fabric) Reload() {
	if pid := f.dnsPid(); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGHUP)
	}
}

// Register maps a VM name to its IP in the dnsmasq hostsdir.
func (f Fabric) Register(name, ip string) error {
	if err := os.MkdirAll(f.hostsDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(f.hostsDir(), name), []byte(ip+" "+name+"\n"), 0o644); err != nil {
		return err
	}
	f.Reload()
	return nil
}

// Unregister removes a VM name from the hostsdir.
func (f Fabric) Unregister(name string) {
	_ = os.Remove(filepath.Join(f.hostsDir(), name))
	f.Reload()
}

// --- per-VM tap ---

// tapPrefix names a VM tap: tapPrefix + the address's last octet.
const tapPrefix = "fdt"

// TapName derives the tap name from the IP's last octet, e.g. 10.89.0.200 -> fdt200.
func TapName(ip string) string { return tapPrefix + lastOctet(ip) }

// TapOctets returns the last octet of every VM tap currently on the fabric (e.g.
// "200"). Taps are host-global — they outlive whichever instances dir created
// them — so this, not any single instance list, is the authoritative record of
// which VM addresses are taken. Handing out an address that already has a tap
// would put two VMs on one address and let either one's teardown delete the
// other's tap.
func (f Fabric) TapOctets(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(
		run.Output(ctx, f.Runner, "podman", "unshare", "--rootless-netns", "ip", "-br", "link", "show"), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		name, _, _ = strings.Cut(name, "@")
		if oct, ok := strings.CutPrefix(name, tapPrefix); ok && oct != "" {
			out[oct] = true
		}
	}
	return out
}

// GuestMAC derives a stable MAC from the IP's last octet.
func GuestMAC(ip string) string {
	n, _ := strconv.Atoi(lastOctet(ip))
	return fmt.Sprintf("02:fc:0a:59:00:%02x", n)
}

// TapUp idempotently creates the VM's tap on the bridge, inside podman's netns.
func (f Fabric) TapUp(ctx context.Context, tap string) error {
	br := f.Bridge(ctx)
	if br == "" {
		return fmt.Errorf("fc: cannot read podman network %q bridge interface", f.Network)
	}
	script := fmt.Sprintf("ip link show %s >/dev/null 2>&1 || ip tuntap add %s mode tap; "+
		"ip link set %s master %s; ip link set %s up", tap, tap, tap, br, tap)
	_, err := f.Runner.Run(ctx, run.Opts{}, "podman", "unshare", "--rootless-netns", "bash", "-c", script)
	return err
}

// TapDel removes a VM's tap.
func (f Fabric) TapDel(ctx context.Context, tap string) {
	_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "unshare", "--rootless-netns",
		"ip", "link", "del", tap)
}

// --- host → VM forwarder (unix-socket bridge across the netns boundary) ---

// FwdUp brings up the host→VM ssh forwarder for an instance: a socat in the netns
// relaying a unix socket to the VM's sshd, and a host-side socat binding the
// published port to that socket.
//
// The inner (in-netns) socat reports its OWN pid — `podman unshare` re-execs, so
// $! would be the wrapper, not the process holding the socket.
func (f Fabric) FwdUp(idir string, hostPort int, vmIP, bind string) error {
	f.FwdDown(idir)
	sock := filepath.Join(idir, "fwd.sock")
	_ = os.Remove(sock)
	_ = os.Remove(filepath.Join(idir, "fwd-ns.pid"))

	// In-netns socat: report its pid, then exec socat onto it. Paths/addresses go
	// in as positional args so the instance dir can't split or inject.
	const nsScript = `set -eu
echo $$ > "$1"
exec socat "UNIX-LISTEN:$2,fork,reuseaddr" "TCP:$3:$4"`
	nsLog, err := os.Create(filepath.Join(idir, "fwd-ns.log"))
	if err != nil {
		return err
	}
	nsCmd := exec.Command("podman", "unshare", "--rootless-netns", "sh", "-c", nsScript,
		"_", filepath.Join(idir, "fwd-ns.pid"), sock, vmIP, strconv.Itoa(InternalSSHPort))
	nsCmd.Stdout, nsCmd.Stderr = nsLog, nsLog
	nsCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := nsCmd.Start(); err != nil {
		nsLog.Close()
		return err
	}
	nsLog.Close()
	_ = nsCmd.Process.Release()

	for i := 0; i < 40; i++ {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Host-side socat: bind the published port to the socket.
	hostLog, err := os.Create(filepath.Join(idir, "fwd-host.log"))
	if err != nil {
		return err
	}
	hostCmd := exec.Command("socat",
		fmt.Sprintf("TCP-LISTEN:%d,bind=%s,fork,reuseaddr", hostPort, bind),
		"UNIX-CONNECT:"+sock)
	hostCmd.Stdout, hostCmd.Stderr = hostLog, hostLog
	hostCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := hostCmd.Start(); err != nil {
		hostLog.Close()
		return err
	}
	hostLog.Close()
	pid := hostCmd.Process.Pid
	_ = hostCmd.Process.Release()
	return os.WriteFile(filepath.Join(idir, "fwd-host.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// FwdDown tears down the host→VM forwarder.
func (f Fabric) FwdDown(idir string) {
	for _, name := range []string{"fwd-host", "fwd-ns"} {
		p := filepath.Join(idir, name+".pid")
		if data, err := os.ReadFile(p); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		_ = os.Remove(p)
	}
	// Belt-and-suspenders: reap any socat still bound to this instance's socket.
	_, _ = f.Runner.Run(context.Background(), run.Opts{}, "pkill", "-f", filepath.Join(idir, "fwd.sock"))
	_ = os.Remove(filepath.Join(idir, "fwd.sock"))
}

// --- helpers ---

func lastOctet(ip string) string {
	if i := strings.LastIndexByte(ip, '.'); i >= 0 {
		return ip[i+1:]
	}
	return ip
}
