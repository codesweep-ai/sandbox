// Package hostroute implements the optional, Linux-only `host-route` feature:
// direct host->sandbox reachability by name (any protocol) under a DNS suffix,
// via a veth from the host root netns into podman's rootless netns plus a
// systemd-resolved per-link resolver pointed at the fabric dnsmasq. It is the
// only feature that uses sudo (and only for up/down/refresh's wiring).
//
// Groups are separate bridges on separate subnets, so the host needs one leg
// per group: a single veth reaches exactly one of them. host-route is therefore
// a list of Legs, one per group, wired together by up/refresh.
package hostroute

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

const (
	vethHost  = "cs-sandbox"    // default group's host-side veth (root netns)
	vethNS    = "cs-sandbox-ns" // its peer, enslaved to the bridge in the netns
	hostOctet = 251             // host address in every group = <prefix>.251
)

// Leg is one group's presence on the host: a veth pair into that group's
// bridge, a systemd-resolved scope for its names, and the fabric DNS that
// answers them.
type Leg struct {
	Group     string
	TapPrefix string // the group's allocated prefix; the veth names derive from it
	NetDir    string // that group's fabric working dir (paths.FCNetFor)
	Fab       fcnet.Fabric
}

// HostRoute drives the feature across every group's leg.
type HostRoute struct {
	Runner  run.Runner
	InstDir string
	NetDir  string // the default group's fabric dir, where the on-marker lives
	UID     int
	Suffix  string // DNS suffix (default cs.sandbox)
	Legs    []Leg
}

// hostVeth is the root-netns end of this group's veth. The default group keeps
// the historical name so an existing host-route survives the upgrade and
// `resolvectl revert cs-sandbox` still names the right link.
//
// Other groups derive theirs from the group's allocated tap prefix, which is
// already unique per group and short enough for IFNAMSIZ. The "fd" is swapped
// for "hr" deliberately: the fabric finds VM taps by scanning the netns for
// interfaces whose name starts with the tap prefix, and a leg sharing that
// prefix would be counted as a running microVM — pinning the fabric against
// GC forever and polluting VM address allocation with a non-numeric octet.
func (l Leg) hostVeth() string {
	if l.isDefault() {
		return vethHost
	}
	return l.legPrefix() + "h"
}

// nsVeth is the peer, enslaved to the bridge inside podman's rootless netns.
func (l Leg) nsVeth() string {
	if l.isDefault() {
		return vethNS
	}
	return l.legPrefix() + "n"
}

func (l Leg) legPrefix() string { return "hr" + strings.TrimPrefix(l.TapPrefix, "fd") }

func (l Leg) isDefault() bool { return l.Group == "" || l.Group == state.DefaultGroup }

// fqdn is how a member of this group is addressed from the host. The default
// group publishes a bare <name>.<suffix>, matching the rule that an unqualified
// reference means the default group; every other group publishes
// <name>.<group>.<suffix>, so two groups holding "worker-01" stay distinct.
func (l Leg) fqdn(name, suffix string) string {
	if l.isDefault() {
		return name + "." + suffix
	}
	return name + "." + l.Group + "." + suffix
}

// domain is the systemd-resolved routing domain for this leg. Resolved routes a
// query to the most specific match, so ~<group>.<suffix> wins over the default
// group's ~<suffix> for a qualified name without either scope knowing about the
// other.
func (l Leg) domain(suffix string) string {
	if l.isDefault() {
		return "~" + suffix
	}
	return "~" + l.Group + "." + suffix
}

// dnsHostsFile is this group's host-route record inside its OWN fabric's
// hostsdir. Each group's names are served only by its own dnsmasq, on its own
// subnet: publishing every group's inventory into one shared DNS would hand
// each group the names of all the others, which is the boundary groups exist
// to draw.
func (l Leg) dnsHostsFile() string { return filepath.Join(l.NetDir, "hosts.d", "_host-route") }

func (h HostRoute) markFile() string { return filepath.Join(h.NetDir, "host-route.on") }

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

// Up wires the host into every group's network and points the resolver at each
// group's fabric DNS.
func (h HostRoute) Up(ctx context.Context) error {
	if !h.UseResolved() {
		return fmt.Errorf("host-route requires systemd-resolved (resolvectl) to route .%s to the fabric DNS rootlessly", h.Suffix)
	}
	if err := h.fabricsUp(ctx); err != nil {
		return err
	}
	// Cache sudo creds with a single prompt.
	if _, err := h.Runner.Run(ctx, run.Opts{Interactive: true}, "sudo", "-v"); err != nil {
		return fmt.Errorf("host-route: needs sudo to wire the host into the sandbox network")
	}
	return h.wire(ctx)
}

// fabricsUp starts each group's dnsmasq (rootless). A group that has only ever
// run podman sandboxes has no fabric of its own yet: podman's own DNS serves
// its members from inside, but nothing answers the host. Bringing it up is what
// makes the group's names resolvable over its leg.
func (h HostRoute) fabricsUp(ctx context.Context) error {
	for _, l := range h.Legs {
		if err := l.Fab.Up(ctx); err != nil {
			return fmt.Errorf("host-route: could not bring up group %q's network fabric: %w", l.Group, err)
		}
	}
	return nil
}

// Down reverts everything, for every group.
func (h HostRoute) Down(ctx context.Context) {
	_, _ = h.Runner.Run(ctx, run.Opts{}, "sudo", "-v")
	for _, l := range h.Legs {
		h.resolverDown(ctx, l)
		h.teardownVeth(ctx, l)
		_ = os.Remove(l.dnsHostsFile())
		l.Fab.Reload()
	}
	_ = h.reap(ctx, h.orphanLegs())
	_ = os.Remove(h.markFile())
	for _, l := range h.Legs {
		group := l.Group
		l.Fab.GC(ctx, func() bool { return h.anyVMRunning(ctx, group, l.Fab) })
	}
}

// Refresh re-asserts every leg, resolver scope, and published name. It is also
// how a group created since the last `up` gets its leg: wiring one needs root,
// so the rootless lifecycle hook cannot do it. That group's fabric is brought
// up here too — a leg pointed at a DNS address nothing is listening on would
// wire cleanly and then resolve nothing.
func (h HostRoute) Refresh(ctx context.Context) error {
	if !h.Active() {
		return fmt.Errorf("host-route is not up (run: cs-sandbox host-route up)")
	}
	if err := h.fabricsUp(ctx); err != nil {
		return err
	}
	return h.wire(ctx)
}

// wire brings every leg to the desired state and records that host-route is on.
func (h HostRoute) wire(ctx context.Context) error {
	if err := h.wireLegs(ctx); err != nil {
		return err
	}
	if err := h.denyForwarding(ctx); err != nil {
		return err
	}
	for _, l := range h.Legs {
		if h.UseResolved() {
			if err := h.resolverUp(ctx, l); err != nil {
				return fmt.Errorf("host-route: failed to point systemd-resolved at group %q's fabric DNS: %w", l.Group, err)
			}
		}
		if err := h.writeDNS(ctx, l); err != nil {
			return err
		}
	}
	return os.WriteFile(h.markFile(), nil, 0o644)
}

// RefreshIfActive republishes instance->name DNS as instances change (rootless,
// no sudo). Lifecycle hook. Only legs that are actually wired are republished:
// advertising a name the host has no interface to reach is worse than silence,
// because it looks like it should work.
func (h HostRoute) RefreshIfActive(ctx context.Context) {
	if !h.Active() {
		return
	}
	for _, l := range h.Legs {
		if h.legWired(ctx, l) {
			_ = h.writeDNS(ctx, l)
		}
	}
}

// Status renders the current state, one block per group. The wiring is read
// back from the host rather than assumed: a leg whose fabric was recreated
// underneath it keeps its old address, and reporting the address host-route
// *meant* to have would hide exactly that failure.
func (h HostRoute) Status(ctx context.Context) string {
	if !h.Active() {
		return "host-route: down  (enable with: cs-sandbox host-route up)"
	}
	var b strings.Builder
	if on := h.forwardingLegs(); len(on) > 0 {
		// Not a warning to skim past: with forwarding on, a member of one group
		// can route to another — and to the host's LAN — through the host.
		fmt.Fprintf(&b, "host-route: DEGRADED — forwarding is enabled on %s; a sandbox could reach "+
			"another group (run: cs-sandbox host-route refresh)\n", strings.Join(on, ", "))
	}
	fmt.Fprintf(&b, "host-route: UP  (<name>.%s in the default group, <name>.<group>.%s elsewhere)\n",
		h.Suffix, h.Suffix)
	for _, l := range h.Legs {
		prefix := l.Fab.Prefix(ctx)
		switch {
		case prefix == "":
			fmt.Fprintf(&b, "  %s: fabric is down (network %s)\n", l.Group, l.Fab.Network)
			continue
		case !h.hostLinkHasAddr(l.hostVeth(), fmt.Sprintf("%s.%d", prefix, hostOctet)):
			fmt.Fprintf(&b, "  %s: NOT WIRED — no %s.%d on %s (run: cs-sandbox host-route refresh)\n",
				l.Group, prefix, hostOctet, l.hostVeth())
			continue
		}
		fmt.Fprintf(&b, "  %s: host %s.%d on %s via %s; %s -> %s\n",
			l.Group, prefix, hostOctet, l.Fab.Bridge(ctx), l.hostVeth(),
			l.domain(h.Suffix), l.Fab.DNSIP(ctx))
		if data, err := os.ReadFile(l.dnsHostsFile()); err == nil && len(data) > 0 {
			for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// wireLegs creates or repairs every group's veth. The netns handle is obtained
// once and the pairs are created in a single sudo script: parking a process
// inside the rootless netns and re-prompting for root per group would pay both
// costs N times for nothing.
func (h HostRoute) wireLegs(ctx context.Context) error {
	if _, err := os.Stat(h.nsPath()); err != nil {
		return fmt.Errorf("host-route: rootless netns not found at %s (is the fabric up?)", h.nsPath())
	}
	type todo struct {
		leg  Leg
		addr string
	}
	var need []todo
	for _, l := range h.Legs {
		if !l.isDefault() && l.TapPrefix == "" {
			return fmt.Errorf("host-route: group %q has no tap prefix recorded; recreate it so its interface names are unique", l.Group)
		}
		prefix := l.Fab.Prefix(ctx)
		if prefix == "" || l.Fab.Bridge(ctx) == "" {
			return fmt.Errorf("host-route: group %q's network fabric is not up (create or start an instance in it first)", l.Group)
		}
		addr := fmt.Sprintf("%s.%d", prefix, hostOctet)
		if h.hostLinkHasAddr(l.hostVeth(), addr) && h.nsLinkExists(ctx, l.nsVeth()) {
			continue // healthy already
		}
		// Drop a stale peer from a prior fabric (user side).
		_, _ = h.pnet(ctx, "ip", "link", "del", l.nsVeth())
		need = append(need, todo{leg: l, addr: addr})
	}
	orphans := h.orphanLegs()
	if len(need) == 0 && len(orphans) == 0 {
		return nil
	}
	if err := h.reap(ctx, orphans); err != nil {
		return err
	}
	if len(need) == 0 {
		return nil
	}

	pid, cleanup, err := h.parkInNetns()
	if err != nil {
		return fmt.Errorf("host-route: could not obtain a PID inside the rootless netns: %w", err)
	}
	defer cleanup()

	argv := []string{"sudo", "bash", "-s", "--", strconv.Itoa(pid)}
	for _, t := range need {
		argv = append(argv, t.leg.hostVeth(), t.leg.nsVeth(), t.addr)
	}
	if _, err := h.Runner.Run(ctx, run.Opts{Stdin: wireScript}, argv...); err != nil {
		return fmt.Errorf("host-route: failed to wire the host into the sandbox network: %w", err)
	}
	// Enslave each peer to its group's bridge + raise it (user owns the netns).
	for _, t := range need {
		br := t.leg.Fab.Bridge(ctx)
		if _, err := h.pnet(ctx, "ip", "link", "set", t.leg.nsVeth(), "master", br); err != nil {
			return fmt.Errorf("host-route: failed to enslave group %q's veth peer to %s: %w", t.leg.Group, br, err)
		}
		if _, err := h.pnet(ctx, "ip", "link", "set", t.leg.nsVeth(), "up"); err != nil {
			return fmt.Errorf("host-route: failed to raise group %q's veth peer: %w", t.leg.Group, err)
		}
	}
	return nil
}

// wireScript creates each veth pair, with the peer dropped straight into the
// netns by pid. Values arrive as positional args and are never interpolated, so
// a name cannot split or inject.
//
// Forwarding is turned off on the host end BEFORE the link is brought up, in
// the same transaction that creates it. Ordering matters: the host holds a leg
// in every group's subnet, so a leg that is up with forwarding still enabled is
// briefly a router between groups. Doing it here means that state never exists.
const wireScript = `set -euo pipefail
NSPID="$1"; shift
while [ "$#" -ge 3 ]; do
  HVE="$1"; NVE="$2"; HADDR="$3"; shift 3
  ip link del "$HVE" 2>/dev/null || true
  ip link add "$HVE" type veth peer name "$NVE" netns "$NSPID"
  echo 0 > /proc/sys/net/ipv4/conf/"$HVE"/forwarding
  echo 0 > /proc/sys/net/ipv6/conf/"$HVE"/forwarding 2>/dev/null || true
  ip addr add "$HADDR/24" dev "$HVE"
  ip link set "$HVE" up
done`

// denyForwardingScript re-asserts the knob on every leg, including ones that
// were already healthy. Changing the GLOBAL net.ipv4.ip_forward propagates to
// every interface, so a later `sysctl -w net.ipv4.ip_forward=1` on a host where
// it was 0 — a Docker install doing it at package time, say — silently
// re-enables forwarding on legs wired long before. (The kernel propagates on a
// change, not on every write, so re-writing the value it already holds is a
// no-op.) Re-asserting is what keeps that from going unnoticed.
//
// Written through /proc rather than `sysctl -w`, so an interface name never has
// to survive translation into a dotted sysctl key.
const denyForwardingScript = `set -eu
for HVE in "$@"; do
  echo 0 > /proc/sys/net/ipv4/conf/"$HVE"/forwarding
  echo 0 > /proc/sys/net/ipv6/conf/"$HVE"/forwarding 2>/dev/null || true
done`

// procRoot is /proc, overridden in tests.
var procRoot = "/proc"

// forwardingPath is a leg's per-interface IPv4 forwarding knob.
func forwardingPath(iface string) string {
	return filepath.Join(procRoot, "sys", "net", "ipv4", "conf", iface, "forwarding")
}

// denyForwarding disables IP forwarding on every leg.
//
// This is what keeps the host from being a router between groups, and it
// replaces any need for firewall rules: ip_forward() gates on the INCOMING
// interface, so a leg with forwarding off cannot carry transit traffic at all —
// not to another group, and not to the host's own LAN either. It is a core IPv4
// knob, present wherever host-route can run, rather than a netfilter feature
// whose availability varies by distribution and kernel build.
//
// Host access is untouched: traffic to and from the host itself is INPUT and
// OUTPUT, which never reaches the forwarding path.
func (h HostRoute) denyForwarding(ctx context.Context) error {
	argv := []string{"sudo", "bash", "-s", "--"}
	for _, l := range h.Legs {
		argv = append(argv, l.hostVeth())
	}
	if _, err := h.Runner.Run(ctx, run.Opts{Stdin: denyForwardingScript}, argv...); err != nil {
		return fmt.Errorf("host-route: failed to disable forwarding on the host's legs, "+
			"which is what keeps a member of one group from routing to another through the host: %w", err)
	}
	return nil
}

// HostLegs names every group's host-side interface, for callers that inspect
// host state without driving the feature (doctor).
func (h HostRoute) HostLegs() []string {
	out := make([]string, 0, len(h.Legs))
	for _, l := range h.Legs {
		out = append(out, l.hostVeth())
	}
	return out
}

// forwardingLegs names the legs the kernel would currently forward for. Read
// from /proc, so it needs no privilege and status can always tell the truth.
func (h HostRoute) forwardingLegs() []string {
	var on []string
	for _, l := range h.Legs {
		data, err := os.ReadFile(forwardingPath(l.hostVeth()))
		if err != nil {
			continue // no such interface: nothing to forward
		}
		if strings.TrimSpace(string(data)) == "1" {
			on = append(on, l.hostVeth())
		}
	}
	return on
}

// legNameRe matches a derived group leg exactly as legPrefix builds one. Only
// names this generator could have produced are ever removed by reap.
var legNameRe = regexp.MustCompile(`^hr[0-9a-f]{4}h$`)

// orphanLegs lists host legs whose group no longer exists. `group rm` cannot
// remove one itself — deleting a link needs root, and rm is rootless — so a
// removed group would otherwise leave an interface holding an address on a
// subnet podman is free to hand to the next group, which is exactly the failure
// its leaked dnsmasq used to cause.
func (h HostRoute) orphanLegs() []string {
	want := map[string]bool{}
	for _, l := range h.Legs {
		want[l.hostVeth()] = true
	}
	out, err := exec.Command("ip", "-br", "link", "show").Output()
	if err != nil {
		return nil
	}
	var orphans []string
	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		name, _, _ = strings.Cut(name, "@")
		if legNameRe.MatchString(name) && !want[name] {
			orphans = append(orphans, name)
		}
	}
	return orphans
}

// reap deletes orphaned legs. Removing the host end takes its netns peer with
// it, so the namespace needs no separate cleanup.
func (h HostRoute) reap(ctx context.Context, orphans []string) error {
	if len(orphans) == 0 {
		return nil
	}
	var b strings.Builder
	for _, name := range orphans {
		fmt.Fprintf(&b, "link del %s\n", name)
	}
	// -force keeps one already-gone link from aborting the rest of the batch.
	if _, err := h.Runner.Run(ctx, run.Opts{Stdin: b.String()}, "sudo", "ip", "-force", "-batch", "-"); err != nil {
		return fmt.Errorf("host-route: failed to remove legs for groups that no longer exist (%s): %w",
			strings.Join(orphans, ", "), err)
	}
	return nil
}

// legWired reports whether this group's host end carries the address its
// current fabric expects.
func (h HostRoute) legWired(ctx context.Context, l Leg) bool {
	prefix := l.Fab.Prefix(ctx)
	if prefix == "" {
		return false
	}
	return h.hostLinkHasAddr(l.hostVeth(), fmt.Sprintf("%s.%d", prefix, hostOctet))
}

func (h HostRoute) teardownVeth(ctx context.Context, l Leg) {
	_, _ = h.Runner.Run(ctx, run.Opts{}, "sudo", "ip", "link", "del", l.hostVeth())
	_, _ = h.pnet(ctx, "ip", "link", "del", l.nsVeth())
}

// DropLeg retires one group's leg from inside podman's rootless netns, so a
// rootless `group rm` can take its own leg with it. Wiring a leg needs root,
// but deleting either end of a veth destroys the pair — and the netns end is
// ours.
//
// Not tidiness: netavark removes a network's bridge only once nothing is left
// attached, so a leg that outlives its group pins the bridge, and the bridge
// keeps the address of the subnet it was built for. Podman hands that interface
// name to the next network it creates, netavark adopts the interface as it
// finds it, and that group's members come up pointing at a gateway which does
// not exist: no DNS, no outbound, and no error anywhere to say so.
func (h HostRoute) DropLeg(ctx context.Context, l Leg) {
	// The default group's fabric is host-wide and never reclaimed, and a group
	// with no recorded prefix has no derived name to delete.
	if l.isDefault() || l.TapPrefix == "" {
		return
	}
	_, _ = h.pnet(ctx, "ip", "link", "del", l.nsVeth())
}

func (h HostRoute) resolverUp(ctx context.Context, l Leg) error {
	dev := l.hostVeth()
	if _, err := h.Runner.Run(ctx, run.Opts{}, "sudo", "resolvectl", "dns", dev, l.Fab.DNSIP(ctx)); err != nil {
		return err
	}
	_, err := h.Runner.Run(ctx, run.Opts{}, "sudo", "resolvectl", "domain", dev, l.domain(h.Suffix))
	return err
}

func (h HostRoute) resolverDown(ctx context.Context, l Leg) {
	if _, err := exec.LookPath("resolvectl"); err == nil {
		_, _ = h.Runner.Run(ctx, run.Opts{}, "sudo", "resolvectl", "revert", l.hostVeth())
	}
}

// writeDNS publishes this group's members into its own fabric dnsmasq hostsdir
// (rootless), then reloads.
func (h HostRoute) writeDNS(ctx context.Context, l Leg) error {
	hostsDir := filepath.Join(l.NetDir, "hosts.d")
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
		if group(in) != l.groupName() {
			continue
		}
		ip := h.instanceIP(ctx, in, l)
		if ip == "" {
			continue
		}
		fmt.Fprintf(&b, "%s %s\n", ip, l.fqdn(in.Name, h.Suffix))
	}
	tmp := l.dnsHostsFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.dnsHostsFile()); err != nil {
		return err
	}
	l.Fab.Reload()
	return nil
}

func (l Leg) groupName() string {
	if l.Group == "" {
		return state.DefaultGroup
	}
	return l.Group
}

func group(in *state.Instance) string {
	if in.Group == "" {
		return state.DefaultGroup
	}
	return in.Group
}

// instanceIP returns an instance's address on its group's fabric
// (engine-agnostic). Podman is asked for the host-global object name, not the
// bare sandbox name: a bare name is "no such object", which this would then
// read as "no address" and silently publish nothing.
func (h HostRoute) instanceIP(ctx context.Context, in *state.Instance, l Leg) string {
	if in.Engine == state.Firecracker {
		return in.FCIP
	}
	return run.Output(ctx, h.Runner, "podman", "inspect", state.ObjectName(in.Group, in.Name),
		"--format", fmt.Sprintf("{{(index .NetworkSettings.Networks %q).IPAddress}}", l.Fab.Network))
}

// anyVMRunning reports whether any microVM still needs this group's fabric. A
// tap on the fabric counts: it belongs to a VM this instances dir cannot see,
// and tearing the fabric down would cut that VM off the network.
func (h HostRoute) anyVMRunning(ctx context.Context, grp string, fab fcnet.Fabric) bool {
	if len(fab.TapOctets(ctx)) > 0 {
		return true
	}
	insts, _ := state.List(h.InstDir)
	for _, in := range insts {
		if in.Engine != state.Firecracker || group(in) != grp {
			continue
		}
		pid := filepath.Join(state.Dir(h.InstDir, in.Group, in.Name), "fc.pid")
		if data, err := os.ReadFile(pid); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && alive(p) {
				return true
			}
		}
	}
	return false
}

// pnet runs a command inside podman's rootless netns.
func (h HostRoute) pnet(ctx context.Context, argv ...string) (run.Result, error) {
	full := append([]string{"podman", "unshare", "--rootless-netns"}, argv...)
	return h.Runner.Run(ctx, run.Opts{}, full...)
}

func (h HostRoute) hostLinkHasAddr(link, addr string) bool {
	out, err := exec.Command("ip", "-br", "addr", "show", link).Output()
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
