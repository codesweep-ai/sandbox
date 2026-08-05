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
	"github.com/codesweep-ai/sandbox/internal/state"
)

// KeepaliveName is the container that pins podman's rootless netns + bridge on
// the default network. Every network has one, named after it.
const KeepaliveName = "cs-sandbox-net-keepalive"

// KeepaliveFor is the keepalive/gateway container for a network. One container
// per network serves both roles: it pins the bridge (netavark tears the bridge
// down around running containers, so a lone VM would otherwise lose it) and,
// with a published port, it is the group's ssh jump host. Two roles, but one
// lifetime — the gateway exists exactly as long as the fabric it fronts.
func KeepaliveFor(network string) string { return network + "-keepalive" }

// InternalSSHPort is the port the guest sshd listens on.
const InternalSSHPort = 22

// Fabric bundles the dependencies for managing the shared network.
type Fabric struct {
	Runner  run.Runner
	Network string // podman network name (cs-sandbox-net)
	Image   string // keepalive container image
	NetDir  string // the fabric working dir (paths.FCNet): dnsmasq hostsdir + log
	// Suffix is the DNS suffix this fabric is authoritative for. Empty reads
	// CS_SANDBOX_DNS_SUFFIX, then falls back to cs.sandbox.
	Suffix string
	// GWPort publishes this network's keepalive as the group's ssh gateway.
	// 0 leaves it unpublished (bridge-pinning only).
	GWPort int
	GWBind string // host bind address for GWPort (127.0.0.1 default)
	GWSeed string // seed dir holding the gateway's authorized_keys
	GWUser string // host user the gateway's sshd should accept
	GWUID  int
	GWGID  int
	GWHome string
	// TapPrefix names this fabric's VM taps. Interface names are host-global even
	// though the taps attach to different bridges, so each group is allocated its
	// own prefix; empty means the historical default fabric.
	TapPrefix string
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

// EnsureGateway brings up this network's keepalive/gateway container. It is the
// same container in both roles, so a group's ingress lives exactly as long as
// the fabric it fronts.
func (f Fabric) EnsureGateway(ctx context.Context) error { return f.keepaliveUp(ctx) }

// Keepalive is this fabric's keepalive/gateway container name.
func (f Fabric) Keepalive() string { return KeepaliveFor(f.Network) }

// gatewayResolvesVMs reports whether the running gateway was given the fabric
// resolver. Read back from the container rather than assumed from how we would
// create one today: the point is to catch a gateway created by an older build,
// which is exactly the case where our assumptions do not apply.
//
// A keepalive with no published port is not a gateway — nobody jumps through it
// — so it is left alone rather than churned.
func (f Fabric) gatewayResolvesVMs(ctx context.Context) bool {
	if f.GWPort == 0 {
		return true
	}
	want := f.DNSIP(ctx)
	if !strings.HasSuffix(want, ".53") {
		return true // cannot determine the fabric address; do not churn on a guess
	}
	got := run.Output(ctx, f.Runner, "podman", "inspect", f.Keepalive(),
		"--format", "{{range .HostConfig.Dns}}{{.}} {{end}}")
	return strings.Contains(got, want)
}

func (f Fabric) keepaliveRunning(ctx context.Context) bool {
	return run.Output(ctx, f.Runner, "podman", "inspect", f.Keepalive(),
		"--format", "{{.State.Running}}") == "true"
}

// --- fabric bring-up / teardown ---

func (f Fabric) hostsDir() string { return filepath.Join(f.NetDir, "hosts.d") }

// suffix is the DNS suffix this fabric owns. It is read here rather than
// threaded through every construction site so a fabric started by any command
// is authoritative for the same domain host-route publishes into.
func (f Fabric) suffix() string {
	if f.Suffix != "" {
		return f.Suffix
	}
	if s := os.Getenv("CS_SANDBOX_DNS_SUFFIX"); s != "" {
		return s
	}
	return "cs.sandbox"
}

// keepaliveUp ensures the keepalive container is running.
func (f Fabric) keepaliveUp(ctx context.Context) error {
	if f.keepaliveRunning(ctx) {
		if f.gatewayResolvesVMs(ctx) {
			return nil
		}
		// A gateway from before the fabric resolver was wired in. Left alone it
		// keeps running and keeps failing to resolve members, which is the
		// quiet version of this bug: reachable by address, nameless. Replace it.
		_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", f.Keepalive())
	} else if _, err := f.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "container", "exists", f.Keepalive()); err == nil {
		// Try to start an existing (stopped) one.
		_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "start", f.Keepalive())
		if f.keepaliveRunning(ctx) {
			if f.gatewayResolvesVMs(ctx) {
				return nil
			}
			_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", f.Keepalive())
		}
	}
	argv := []string{"podman", "run", "-d", "--name", f.Keepalive(),
		"--hostname", "gateway", "--network", f.Network, "--restart=always",
		"--label", "cs-sandbox.managed=1", "--label", "cs-sandbox.keepalive=1"}
	if f.GWPort != 0 {
		// The gateway leg. One published port per group: the host jumps through
		// it and then reaches every member by its bare name over the group's own
		// DNS, which is the same path members use to reach each other. The image
		// entrypoint already starts sshd, so the gateway needs only an identity
		// and the group's authorized_keys.
		//
		// --dns is what makes that true. A group has two resolvers: aardvark,
		// which containers get by default and which knows container names, and
		// the fabric's dnsmasq, which serves microVM names from the hostsdir and
		// forwards everything else to aardvark. Without this the gateway
		// inherited aardvark and could reach a microVM member by address but
		// never by name — the promise above, unkept for the firecracker engine,
		// while a podman-only group worked and hid it.
		argv = append(argv,
			"--dns", f.DNSIP(ctx),
			"-p", fmt.Sprintf("%s:%d:22", f.GWBind, f.GWPort),
			"--label", "cs-sandbox.gateway=1",
			"--userns=keep-id", "--user", "0:0",
			// The seed is a host dir owned by the invoking user; without this
			// SELinux denies the container's read and sshd comes up with no
			// authorized_keys — the same option the sandbox run path uses.
			"--security-opt", "label=disable",
			"-e", "CS_SANDBOX_TYPE=agent",
			"-e", "CS_SANDBOX_SSH_PORT=22",
			"-e", "CS_SANDBOX_USER="+f.GWUser,
			"-e", fmt.Sprintf("CS_SANDBOX_UID=%d", f.GWUID),
			"-e", fmt.Sprintf("CS_SANDBOX_GID=%d", f.GWGID),
			"-e", "CS_SANDBOX_HOME="+f.GWHome,
			"-v", f.GWSeed+":/run/cs-sandbox-seed:ro")
	}
	argv = append(argv, f.Image, "sleep", "infinity")
	_, _ = f.Runner.Run(ctx, run.Opts{}, argv...)
	if f.keepaliveRunning(ctx) {
		return nil
	}
	_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "start", f.Keepalive())
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

// dnsmasqScript starts the fabric resolver inside the rootless netns.
//
// Stay as (userns-)root so dnsmasq can traverse $HOME (mode 750) to re-read
// --hostsdir; the default drop to "nobody" cannot. An empty --pid-file asks
// dnsmasq not to write one: the running process is the record (see dnsState).
// Values arrive as positional args and are never interpolated into the script,
// so a path with a space or a quote (e.g. under $HOME) cannot split or inject.
//
// --local=/$SUFFIX/ makes dnsmasq authoritative for our own suffix, answering
// from the hostsdir alone. Without it every name it holds no record for — which
// includes EVERY AAAA lookup, since the hostsdir carries only A records — is
// forwarded to the bridge's aardvark, which never answers for these names. Each
// such query then costs a 5s timeout and systemd-resolved retries it across
// scopes: measured at 3s to resolve a default-group name and over 30s for a
// group-qualified one, against 10ms once dnsmasq answers authoritatively.
const dnsmasqScript = `set -eu
DNS="$1"; BR="$2"; GW="$3"; HOSTSDIR="$4"; SUFFIX="$5"
ip addr add "$DNS/24" dev "$BR" 2>/dev/null || true
exec dnsmasq --keep-in-foreground --user=root --group=root --bind-interfaces \
  --listen-address="$DNS" --no-hosts --no-resolv --server="$GW" \
  --local="/$SUFFIX/" \
  --hostsdir="$HOSTSDIR" --pid-file= --conf-file=/dev/null`

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
	logf, err := os.Create(filepath.Join(f.NetDir, "dnsmasq.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command("podman", "unshare", "--rootless-netns", "bash", "-c", dnsmasqScript,
		"_", dns, br, gw, f.hostsDir(), f.suffix())
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
//
// Dropping the address is not optional cleanup. It sits on netavark's OWN
// bridge, and while any address remains the bridge survives the network's
// removal — carrying that network's gateway with it. Podman then hands the
// freed subnet to the next network, on a different bridge, and two bridges
// answer for one subnet: every member of the new network loses outbound
// traffic, with nothing in podman's view to explain it.
//
// So the delete is attempted whenever the bridge is still resolvable, rather
// than only while the keepalive happens to be running — by teardown time it
// often is not, and the earlier guard turned that into a silent leak. It runs
// before the keepalive is removed, since the bridge name comes from inspecting
// the network the keepalive holds up.
func (f Fabric) Down(ctx context.Context) {
	if pid := f.dnsPid(); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if br, dns := f.Bridge(ctx), f.DNSIP(ctx); br != "" && dns != "" && dns != ".53" {
		_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "unshare", "--rootless-netns",
			"ip", "addr", "del", dns+"/24", "dev", br)
	}
	_, _ = f.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", f.Keepalive())
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
		if n != "" && n != f.Keepalive() {
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

// prefix is this fabric's tap prefix, defaulting to the historical one.
func (f Fabric) prefix() string {
	if f.TapPrefix == "" {
		return tapPrefix
	}
	return f.TapPrefix
}

// TapName derives a VM tap name within this fabric. Two groups whose VMs land
// on the same last octet must NOT produce the same interface name.
func (f Fabric) TapName(ip string) string { return f.prefix() + lastOctet(ip) }

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
		if oct, ok := strings.CutPrefix(name, f.prefix()); ok && oct != "" {
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
	sock := filepath.Join(idir, state.SockFwd)
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
	_, _ = f.Runner.Run(context.Background(), run.Opts{}, "pkill", "-f", filepath.Join(idir, state.SockFwd))
	_ = os.Remove(filepath.Join(idir, state.SockFwd))
}

// --- helpers ---

func lastOctet(ip string) string {
	if i := strings.LastIndexByte(ip, '.'); i >= 0 {
		return ip[i+1:]
	}
	return ip
}
