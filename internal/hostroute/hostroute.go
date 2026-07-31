// Package hostroute implements the optional, Linux-only `host-route` feature:
// direct host->sandbox reachability by name (any protocol) under a DNS suffix,
// via a veth from the host root netns into podman's rootless netns plus a
// systemd-resolved per-link resolver pointed at the fabric dnsmasq. It is the
// only feature that uses sudo (and only for up/down/refresh's wiring).
package hostroute

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

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

const (
	vethHost  = "cs-sandbox"    // host-side veth end (root netns)
	vethNS    = "cs-sandbox-ns" // peer, enslaved to the bridge in the rootless netns
	hostOctet = 251             // host address = <prefix>.251
)

// HostRoute drives the feature.
type HostRoute struct {
	Fab     fcnet.Fabric
	Runner  run.Runner
	InstDir string
	NetDir  string // the fabric working dir (paths.FCNet)
	UID     int
	Network string
	Suffix  string // DNS suffix (default cs.sandbox)
}

func (h HostRoute) markFile() string     { return filepath.Join(h.NetDir, "host-route.on") }
func (h HostRoute) dnsHostsFile() string { return filepath.Join(h.NetDir, "hosts.d", "_host-route") }

// nsPath is where podman pins its rootless netns.
func (h HostRoute) nsPath() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = fmt.Sprintf("/run/user/%d", h.UID)
	}
	return filepath.Join(base, "containers", "networks", "rootless-netns", "rootless-netns")
}

// Active reports whether host-route is currently up.
func (h HostRoute) Active() bool {
	_, err := os.Stat(h.markFile())
	return err == nil
}

// UseResolved reports whether systemd-resolved is usable.
func (h HostRoute) UseResolved() bool {
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", "systemd-resolved").Run() == nil
}

// Up wires the host into the sandbox network and points the resolver at the
// fabric DNS.
func (h HostRoute) Up(ctx context.Context) error {
	if !h.UseResolved() {
		return fmt.Errorf("host-route requires systemd-resolved (resolvectl) to route .%s to the fabric DNS rootlessly", h.Suffix)
	}
	if err := h.Fab.Up(ctx); err != nil {
		return fmt.Errorf("host-route: could not bring up the network fabric: %w", err)
	}
	// Cache sudo creds with a single prompt.
	if _, err := h.Runner.Run(ctx, run.Opts{Interactive: true}, "sudo", "-v"); err != nil {
		return fmt.Errorf("host-route: needs sudo to wire the host into the sandbox network")
	}
	if err := h.ensureVeth(ctx); err != nil {
		return err
	}
	if err := h.resolverUp(ctx, h.Fab.DNSIP(ctx)); err != nil {
		return fmt.Errorf("host-route: failed to point systemd-resolved at the fabric DNS: %w", err)
	}
	if err := h.writeDNS(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(h.markFile(), nil, 0o644); err != nil {
		return err
	}
	return nil
}

// Down reverts everything.
func (h HostRoute) Down(ctx context.Context) {
	_, _ = h.Runner.Run(ctx, run.Opts{Interactive: true}, "sudo", "-v")
	h.resolverDown(ctx)
	h.teardownVeth(ctx)
	h.clearDNS()
	_ = os.Remove(h.markFile())
	h.Fab.GC(ctx, func() bool { return h.anyVMRunning(ctx) })
}

// Refresh re-asserts the veth, resolver, and names.
func (h HostRoute) Refresh(ctx context.Context) error {
	if !h.Active() {
		return fmt.Errorf("host-route is not up (run: cs-sandbox host-route up)")
	}
	if err := h.ensureVeth(ctx); err != nil {
		return err
	}
	if h.UseResolved() {
		if err := h.resolverUp(ctx, h.Fab.DNSIP(ctx)); err != nil {
			return err
		}
	}
	return h.writeDNS(ctx)
}

// RefreshIfActive republishes instance->name DNS as instances change (rootless,
// no sudo). Lifecycle hook.
func (h HostRoute) RefreshIfActive(ctx context.Context) {
	if h.Active() {
		_ = h.writeDNS(ctx)
	}
}

// Status renders the current state.
func (h HostRoute) Status(ctx context.Context) string {
	if !h.Active() {
		return "host-route: down  (enable with: cs-sandbox host-route up)"
	}
	prefix := h.Fab.Prefix(ctx)
	var b strings.Builder
	fmt.Fprintf(&b, "host-route: UP (host %s.%d on %s via %s; *.%s -> %s)\n",
		prefix, hostOctet, h.Fab.Bridge(ctx), vethHost, h.Suffix, h.Fab.DNSIP(ctx))
	if data, err := os.ReadFile(h.dnsHostsFile()); err == nil && len(data) > 0 {
		b.WriteString("  published names:\n")
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ensureVeth (re)creates the host<->netns veth, idempotently.
func (h HostRoute) ensureVeth(ctx context.Context) error {
	br := h.Fab.Bridge(ctx)
	prefix := h.Fab.Prefix(ctx)
	if br == "" || prefix == "" {
		return fmt.Errorf("host-route: network fabric is not up (create or start an instance first)")
	}
	if _, err := os.Stat(h.nsPath()); err != nil {
		return fmt.Errorf("host-route: rootless netns not found at %s (is the fabric up?)", h.nsPath())
	}
	haddr := fmt.Sprintf("%s.%d", prefix, hostOctet)

	// Healthy already? host end carries the address AND the ns peer is present.
	if h.hostLinkHasAddr(haddr) && h.nsLinkExists(ctx, vethNS) {
		return nil
	}
	// Drop a stale peer from a prior fabric (user side).
	_, _ = h.pnet(ctx, "ip", "link", "del", vethNS)

	pid, cleanup, err := h.parkInNetns()
	if err != nil {
		return fmt.Errorf("host-route: could not obtain a PID inside the rootless netns: %w", err)
	}
	defer cleanup()

	// Root creates the veth pair with the peer dropped straight into the netns by pid.
	script := `set -euo pipefail
HVE="$1"; NVE="$2"; NSPID="$3"; HADDR="$4"
ip link del "$HVE" 2>/dev/null || true
ip link add "$HVE" type veth peer name "$NVE" netns "$NSPID"
ip addr add "$HADDR/24" dev "$HVE"
ip link set "$HVE" up`
	if _, err := h.Runner.Run(ctx, run.Opts{Stdin: script}, "sudo", "bash", "-s", "--",
		vethHost, vethNS, strconv.Itoa(pid), haddr); err != nil {
		return fmt.Errorf("host-route: failed to wire the host into the sandbox network: %w", err)
	}
	// Enslave the peer to the bridge + raise it (user owns the netns).
	if _, err := h.pnet(ctx, "ip", "link", "set", vethNS, "master", br); err != nil {
		return fmt.Errorf("host-route: failed to enslave the veth peer to the bridge: %w", err)
	}
	if _, err := h.pnet(ctx, "ip", "link", "set", vethNS, "up"); err != nil {
		return fmt.Errorf("host-route: failed to raise the veth peer: %w", err)
	}
	return nil
}

func (h HostRoute) teardownVeth(ctx context.Context) {
	_, _ = h.Runner.Run(ctx, run.Opts{}, "sudo", "ip", "link", "del", vethHost)
	_, _ = h.pnet(ctx, "ip", "link", "del", vethNS)
}

func (h HostRoute) resolverUp(ctx context.Context, dnsIP string) error {
	if _, err := h.Runner.Run(ctx, run.Opts{}, "sudo", "resolvectl", "dns", vethHost, dnsIP); err != nil {
		return err
	}
	_, err := h.Runner.Run(ctx, run.Opts{}, "sudo", "resolvectl", "domain", vethHost, "~"+h.Suffix)
	return err
}

func (h HostRoute) resolverDown(ctx context.Context) {
	if _, err := exec.LookPath("resolvectl"); err == nil {
		_, _ = h.Runner.Run(ctx, run.Opts{}, "sudo", "resolvectl", "revert", vethHost)
	}
}

// writeDNS publishes every instance's <name>.<suffix> -> ip into the fabric
// dnsmasq hostsdir (rootless), then reloads.
func (h HostRoute) writeDNS(ctx context.Context) error {
	hostsDir := filepath.Join(h.NetDir, "hosts.d")
	if err := os.MkdirAll(hostsDir, 0o755); err != nil {
		return err
	}
	insts, err := state.List(h.InstDir)
	if err != nil {
		// Don't republish an empty hosts file (dropping every name) on a transient
		// list failure — leave the existing records in place and surface the error.
		return fmt.Errorf("host-route: list instances: %w", err)
	}
	var b strings.Builder
	for _, in := range insts {
		if state.NetworkName(in) != h.Network {
			continue
		}
		ip := h.instanceIP(ctx, in)
		if ip == "" {
			continue
		}
		fmt.Fprintf(&b, "%s %s.%s\n", ip, in.Name, h.Suffix)
	}
	tmp := h.dnsHostsFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.dnsHostsFile()); err != nil {
		return err
	}
	h.reloadDNS()
	return nil
}

func (h HostRoute) clearDNS() {
	_ = os.Remove(h.dnsHostsFile())
	h.reloadDNS()
}

// instanceIP returns an instance's address on the fabric (engine-agnostic).
func (h HostRoute) instanceIP(ctx context.Context, in *state.Instance) string {
	if in.Engine == state.Firecracker {
		return in.FCIP
	}
	return run.Output(ctx, h.Runner, "podman", "inspect", in.Name,
		"--format", fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", h.Network))
}

// anyVMRunning reports whether any microVM still needs the fabric. A tap on the
// fabric counts: it belongs to a VM this instances dir cannot see, and tearing
// the fabric down would cut that VM off the network.
func (h HostRoute) anyVMRunning(ctx context.Context) bool {
	if len(h.Fab.TapOctets(ctx)) > 0 {
		return true
	}
	insts, _ := state.List(h.InstDir)
	for _, in := range insts {
		if in.Engine == state.Firecracker && state.NetworkName(in) == h.Network {
			pid := filepath.Join(state.Dir(h.InstDir, in.Name), "fc.pid")
			if data, err := os.ReadFile(pid); err == nil {
				if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && alive(p) {
					return true
				}
			}
		}
	}
	return false
}

// reloadDNS SIGHUPs the fabric dnsmasq so it re-reads the hostsdir promptly.
func (h HostRoute) reloadDNS() { h.Fab.Reload() }

// pnet runs a command inside podman's rootless netns.
func (h HostRoute) pnet(ctx context.Context, argv ...string) (run.Result, error) {
	full := append([]string{"podman", "unshare", "--rootless-netns"}, argv...)
	return h.Runner.Run(ctx, run.Opts{}, full...)
}

func (h HostRoute) hostLinkHasAddr(addr string) bool {
	out, err := exec.Command("ip", "-br", "addr", "show", vethHost).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), addr+"/")
}

func (h HostRoute) nsLinkExists(ctx context.Context, link string) bool {
	_, err := h.pnet(ctx, "ip", "link", "show", link)
	return err == nil
}

// parkInNetns starts a sleep process inside the rootless netns and returns its
// (host-visible) pid, for use as a stable handle when creating the veth peer.
// The inner process reports its own pid across the podman-unshare re-exec.
func (h HostRoute) parkInNetns() (int, func(), error) {
	pf, err := os.CreateTemp("", "cs-hr-*.pid")
	if err != nil {
		return 0, func() {}, err
	}
	pfName := pf.Name()
	_ = pf.Close()
	cmd := exec.Command("podman", "unshare", "--rootless-netns", "sh", "-c",
		fmt.Sprintf("echo $$ > '%s'; exec sleep 300", pfName))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(pfName)
		return 0, func() {}, err
	}
	var pid int
	for i := 0; i < 60; i++ {
		data, _ := os.ReadFile(pfName)
		if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && p > 0 {
			pid = p
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cleanup := func() {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		_ = cmd.Process.Release()
		_ = os.Remove(pfName)
	}
	if pid == 0 || !alive(pid) {
		cleanup()
		return 0, func() {}, fmt.Errorf("could not obtain a live pid inside the rootless netns")
	}
	return pid, cleanup, nil
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
