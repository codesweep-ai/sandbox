package hostroute

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// newTestHostRoute wires a HostRoute over temp dirs with a scripted runner, one
// leg per named group (the default group always first).
func newTestHostRoute(t *testing.T, fake *run.Fake, groups ...string) HostRoute {
	t.Helper()
	root := t.TempDir()
	h := HostRoute{
		Runner:  fake,
		InstDir: t.TempDir(),
		NetDir:  root,
		Suffix:  "cs.sandbox",
	}
	for i, g := range append([]string{state.DefaultGroup}, groups...) {
		netDir := root
		tap := ""
		if g != state.DefaultGroup {
			netDir = filepath.Join(root, g)
			tap = "fd000" + string(rune('0'+i))
		}
		if err := os.MkdirAll(filepath.Join(netDir, "hosts.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		h.Legs = append(h.Legs, Leg{
			Group: g, TapPrefix: tap, NetDir: netDir,
			Fab: fcnet.Fabric{Runner: fake, Network: state.NetworkName(g), NetDir: netDir, TapPrefix: tap},
		})
	}
	return h
}

func (h HostRoute) legFor(t *testing.T, group string) Leg {
	t.Helper()
	for _, l := range h.Legs {
		if l.Group == group {
			return l
		}
	}
	t.Fatalf("no leg for group %q", group)
	return Leg{}
}

func published(t *testing.T, l Leg) string {
	t.Helper()
	data, err := os.ReadFile(l.dnsHostsFile())
	if err != nil {
		t.Fatalf("read published names for %q: %v", l.Group, err)
	}
	return string(data)
}

// TestWriteDNSAsksPodmanForTheObjectName: podman objects carry the group
// (<name>.<group>), so inspecting the bare sandbox name returns "no such
// object". instanceIP read that as "no address" and skipped the sandbox, which
// silently published nothing at all while status still reported UP.
func TestWriteDNSAsksPodmanForTheObjectName(t *testing.T) {
	fake := run.NewFake()
	fake.OnStdout("podman inspect web.default", "10.89.4.7")
	h := newTestHostRoute(t, fake)
	if err := state.Save(h.InstDir, &state.Instance{
		Name: "web", Group: state.DefaultGroup, Engine: state.Podman, Port: 2200,
	}); err != nil {
		t.Fatal(err)
	}

	l := h.legFor(t, state.DefaultGroup)
	if err := h.writeDNS(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if got, want := published(t, l), "10.89.4.7 web.cs.sandbox\n"; got != want {
		t.Errorf("published names = %q, want %q", got, want)
	}
	for _, c := range fake.Rendered() {
		if strings.HasSuffix(c, "podman inspect web") || strings.Contains(c, "podman inspect web ") {
			t.Errorf("podman was asked for the bare name: %q", c)
		}
	}
}

// TestPublishedNamesAreGroupScoped: a bare <name>.<suffix> means the default
// group, exactly as a bare CLI reference does; every other group publishes
// <name>.<group>.<suffix>. Two groups may hold the same sandbox name, so a flat
// namespace here would put two answers behind one DNS name.
func TestPublishedNamesAreGroupScoped(t *testing.T) {
	fake := run.NewFake()
	fake.OnStdout("podman inspect web.default", "10.89.4.7")
	fake.OnStdout("podman inspect web.cache-redis", "10.89.2.7")
	h := newTestHostRoute(t, fake, "cache-redis")
	for _, in := range []*state.Instance{
		{Name: "web", Group: state.DefaultGroup, Engine: state.Podman, Port: 2200},
		{Name: "web", Group: "cache-redis", Engine: state.Podman, Port: 2201},
		{Name: "vm", Group: "cache-redis", Engine: state.Firecracker, FCIP: "10.89.2.200"},
	} {
		if err := state.Save(h.InstDir, in); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range h.Legs {
		if err := h.writeDNS(context.Background(), l); err != nil {
			t.Fatal(err)
		}
	}

	// Each group's names live in its OWN fabric's hostsdir, served by its own
	// dnsmasq on its own subnet: one shared DNS would hand every group the
	// names of all the others.
	def := published(t, h.legFor(t, state.DefaultGroup))
	if def != "10.89.4.7 web.cs.sandbox\n" {
		t.Errorf("default group published %q", def)
	}
	camp := published(t, h.legFor(t, "cache-redis"))
	for _, want := range []string{"10.89.2.7 web.cache-redis.cs.sandbox", "10.89.2.200 vm.cache-redis.cs.sandbox"} {
		if !strings.Contains(camp, want) {
			t.Errorf("cache-redis should publish %q:\n%s", want, camp)
		}
	}
	if strings.Contains(camp, "web.cs.sandbox\n") {
		t.Errorf("a group member must not claim the bare default-group name:\n%s", camp)
	}
}

// TestLegNamesAvoidTheTapPrefix: the fabric finds running microVMs by scanning
// the netns for interfaces whose name starts with the group's tap prefix. A veth
// sharing that prefix would be counted as a VM — pinning the fabric against GC
// forever and feeding a non-numeric octet into VM address allocation.
func TestLegNamesAvoidTheTapPrefix(t *testing.T) {
	l := Leg{Group: "cache-redis", TapPrefix: "fd0001"}
	for _, name := range []string{l.hostVeth(), l.nsVeth()} {
		if strings.HasPrefix(name, l.TapPrefix) {
			t.Errorf("veth %q starts with the tap prefix %q: the fabric would read it as a VM", name, l.TapPrefix)
		}
		// IFNAMSIZ leaves 15 usable characters.
		if len(name) > 15 {
			t.Errorf("veth name %q is %d chars, over the 15-char kernel limit", name, len(name))
		}
	}
	if l.hostVeth() == l.nsVeth() {
		t.Error("the two ends of a veth pair need different names")
	}
	// Distinct groups get distinct names, because tap prefixes are allocated.
	other := Leg{Group: "cache-memory", TapPrefix: "fd0002"}
	if other.hostVeth() == l.hostVeth() {
		t.Errorf("two groups derived the same veth name %q", l.hostVeth())
	}
	// The default group keeps the historical pair so an existing host-route
	// survives the upgrade and `resolvectl revert cs-sandbox` still applies.
	def := Leg{Group: state.DefaultGroup}
	if def.hostVeth() != vethHost || def.nsVeth() != vethNS {
		t.Errorf("default group veth names changed: %q/%q", def.hostVeth(), def.nsVeth())
	}
}

// TestResolverDomainsAreMostSpecific: resolved routes a query to the longest
// matching routing domain, so the group scopes must be strictly more specific
// than the default group's — otherwise ~cs.sandbox would swallow every
// qualified name and answer it from the wrong fabric.
func TestResolverDomainsAreMostSpecific(t *testing.T) {
	def := Leg{Group: state.DefaultGroup}.domain("cs.sandbox")
	camp := Leg{Group: "cache-redis"}.domain("cs.sandbox")
	if def != "~cs.sandbox" {
		t.Errorf("default domain = %q", def)
	}
	if camp != "~cache-redis.cs.sandbox" {
		t.Errorf("group domain = %q", camp)
	}
	if !strings.HasSuffix(camp, strings.TrimPrefix(def, "~")) || len(camp) <= len(def) {
		t.Errorf("group domain %q must be a strictly more specific suffix than %q", camp, def)
	}
}

// TestWriteDNSKeepsNamesOnListFailure: republishing an empty file would drop
// every name at once. A transient failure to read the instances dir must not
// look like "no sandboxes exist".
func TestWriteDNSKeepsNamesOnListFailure(t *testing.T) {
	h := newTestHostRoute(t, run.NewFake())
	l := h.legFor(t, state.DefaultGroup)
	if err := os.WriteFile(l.dnsHostsFile(), []byte("10.89.4.7 web.cs.sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A regular file where the instances dir belongs: unreadable, as distinct
	// from absent (which legitimately means "no sandboxes").
	notADir := filepath.Join(t.TempDir(), "instances")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.InstDir = notADir

	if err := h.writeDNS(context.Background(), l); err == nil {
		t.Fatal("a list failure must be surfaced, not swallowed")
	}
	if got := published(t, l); !strings.Contains(got, "web.cs.sandbox") {
		t.Errorf("existing names must survive a list failure, got %q", got)
	}
}

// TestLegNameRegexpMatchesOnlyOurOwnLegs: reap deletes host interfaces, so the
// pattern must match exactly what legPrefix produces and nothing a user might
// have named themselves.
func TestLegNameRegexpMatchesOnlyOurOwnLegs(t *testing.T) {
	for _, tap := range []string{"fd0001", "fd00ff", "fdffff"} {
		name := Leg{Group: "g", TapPrefix: tap}.hostVeth()
		if !legNameRe.MatchString(name) {
			t.Errorf("generated leg %q is not matched by its own pattern", name)
		}
	}
	for _, no := range []string{
		"cs-sandbox", // the default group's leg is never reaped by pattern
		"hr0001n",    // the netns peer goes with its host end, not by name
		"hr0001",     // no suffix
		"hrXXXXh",    // not hex
		"eth0", "docker0", "hr", "myhr0001host",
	} {
		if legNameRe.MatchString(no) {
			t.Errorf("pattern must not match %q — reap deletes host interfaces", no)
		}
	}
}

// TestForwardingIsOffBeforeTheLinkComesUp: the host holds a leg in every
// group's subnet, so a leg that is up while forwarding is still enabled is
// briefly a router between groups. The knob must be written before `ip link set
// up`, in the same transaction that creates the interface.
func TestForwardingIsOffBeforeTheLinkComesUp(t *testing.T) {
	deny := strings.Index(wireScript, "ipv4/conf/\"$HVE\"/forwarding")
	up := strings.Index(wireScript, `ip link set "$HVE" up`)
	add := strings.Index(wireScript, "ip link add")
	if deny < 0 || up < 0 || add < 0 {
		t.Fatalf("wire script lost a required step:\n%s", wireScript)
	}
	if !(add < deny && deny < up) {
		t.Errorf("forwarding must be disabled after link add and BEFORE link up:\n%s", wireScript)
	}
	// IPv6 too: a leg that forwards v6 is just as much a router.
	if !strings.Contains(wireScript, "ipv6/conf/\"$HVE\"/forwarding") {
		t.Errorf("IPv6 forwarding must be disabled as well:\n%s", wireScript)
	}
}

// TestDenyForwardingCoversEveryLeg: writing the global net.ipv4.ip_forward
// propagates to every interface, so legs wired earlier get silently re-enabled.
// Re-assertion has to cover all of them, not just the ones being created.
func TestDenyForwardingCoversEveryLeg(t *testing.T) {
	fake := run.NewFake()
	h := newTestHostRoute(t, fake, "cache-redis", "cache-memory")
	if err := h.denyForwarding(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("expected one batched call, got %d", len(fake.Calls))
	}
	got := strings.Join(fake.Calls[0], " ")
	for _, l := range h.Legs {
		if !strings.Contains(got, l.hostVeth()) {
			t.Errorf("leg %q (group %q) was not covered: %s", l.hostVeth(), l.Group, got)
		}
	}
	// The default group's leg is not exempt: a default-group member routing into
	// another group's subnet is the same bypass.
	if !strings.Contains(got, vethHost) {
		t.Errorf("the default group's leg must be covered too: %s", got)
	}
}

// TestStatusReportsForwardingAsDegraded: a leg that forwards is the dangerous
// state, and it must never read as plain UP.
func TestStatusReportsForwardingAsDegraded(t *testing.T) {
	h := newTestHostRoute(t, run.NewFake(), "cache-redis")
	root := t.TempDir()
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })
	for i, l := range h.Legs {
		dir := filepath.Dir(forwardingPath(l.hostVeth()))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Only the second leg forwards.
		if err := os.WriteFile(forwardingPath(l.hostVeth()), []byte(map[bool]string{true: "1", false: "0"}[i == 1]+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if on := h.forwardingLegs(); len(on) != 1 || on[0] != h.legFor(t, "cache-redis").hostVeth() {
		t.Fatalf("forwardingLegs() = %v, want just cache-redis's leg", on)
	}
	if err := os.WriteFile(h.markFile(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.Status(context.Background())
	if !strings.Contains(out, "DEGRADED") {
		t.Errorf("status must not read as healthy while a leg forwards:\n%s", out)
	}
	if !strings.Contains(out, "host-route refresh") {
		t.Errorf("status should name the fix:\n%s", out)
	}
}

// TestDropLegRetiresOnlyItsOwn: a rootless `group rm` retires its group's leg by
// deleting the netns end, which takes the host end with it — wiring one needed
// root, unwiring one does not. It must stay narrow: the default group's leg is
// host-wide and outlives every group, and a group with no recorded prefix has no
// derived name, so guessing one could delete an interface belonging to someone
// else.
func TestDropLegRetiresOnlyItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name, group, prefix, want string
	}{
		{"a group's own leg", "cache-redis", "fd0007", "ip link del hr0007n"},
		{"the default group's leg is never dropped", state.DefaultGroup, "fd0000", ""},
		{"a group with no recorded prefix", "cache-redis", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := run.NewFake()
			h := HostRoute{Runner: f}
			h.DropLeg(context.Background(), Leg{Group: tc.group, TapPrefix: tc.prefix})
			if tc.want == "" {
				if len(f.Calls) != 0 {
					t.Fatalf("nothing should be deleted, got: %v", f.Rendered())
				}
				return
			}
			if !f.Contains(tc.want) {
				t.Fatalf("want %q, got: %v", tc.want, f.Rendered())
			}
			// Rootless: the host end is never touched with sudo here.
			if f.Contains("sudo") {
				t.Errorf("DropLeg must stay rootless: %v", f.Rendered())
			}
		})
	}
}
