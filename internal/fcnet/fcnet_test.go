package fcnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestLastOctet pins the IP-tail extraction that names taps and MACs.
func TestLastOctet(t *testing.T) {
	cases := map[string]string{
		"10.89.0.200": "200",
		"10.89.0.5":   "5",
		"192.168.1.1": "1",
		"nodot":       "nodot", // no '.', returned as-is
		"":            "",
	}
	for in, want := range cases {
		if got := lastOctet(in); got != want {
			t.Errorf("lastOctet(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTapName pins the "fdt"+octet tap naming.
func TestTapName(t *testing.T) {
	if got := TapName("10.89.0.200"); got != "fdt200" {
		t.Errorf("TapName = %q, want fdt200", got)
	}
	if got := TapName("10.89.0.5"); got != "fdt5" {
		t.Errorf("TapName = %q, want fdt5", got)
	}
}

// TestGuestMAC pins the stable per-IP MAC derivation (octet -> 2-hex low byte).
func TestGuestMAC(t *testing.T) {
	cases := map[string]string{
		"10.89.0.200": "02:fc:0a:59:00:c8", // 200 = 0xc8
		"10.89.0.5":   "02:fc:0a:59:00:05",
		"10.89.0.16":  "02:fc:0a:59:00:10",
		"bad":         "02:fc:0a:59:00:00", // non-numeric octet -> Atoi 0
	}
	for in, want := range cases {
		if got := GuestMAC(in); got != want {
			t.Errorf("GuestMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPickDNSAdoptsOursIgnoresOthers: the fabric's resolver is found by what is
// actually running, so a root that never wrote the bookkeeping still sees it.
func TestPickDNSAdoptsOursIgnoresOthers(t *testing.T) {
	procs := []dnsProc{
		{pid: 10, addr: "192.168.0.53", hostsDir: "/elsewhere/hosts.d"}, // unrelated dnsmasq
		{pid: 42, addr: "10.89.0.53", hostsDir: "/net/hosts.d"},
	}
	pid, err := pickDNS(procs, "10.89.0.53", "/net/hosts.d")
	if err != nil {
		t.Fatalf("healthy resolver reported as a conflict: %v", err)
	}
	if pid != 42 {
		t.Errorf("pid = %d, want 42", pid)
	}
	// Trailing-slash spelling of the same dir is still ours.
	if pid, err := pickDNS(procs, "10.89.0.53", "/net/hosts.d/"); err != nil || pid != 42 {
		t.Errorf("pickDNS(equivalent path) = %d, %v; want 42, nil", pid, err)
	}
}

// TestPickDNSNoneRunning: nothing on our address means we start one.
func TestPickDNSNoneRunning(t *testing.T) {
	pid, err := pickDNS([]dnsProc{{pid: 7, addr: "192.168.0.53", hostsDir: "/x"}}, "10.89.0.53", "/net/hosts.d")
	if err != nil || pid != 0 {
		t.Errorf("pickDNS = %d, %v; want 0, nil", pid, err)
	}
}

// TestPickDNSReportsConflict: a dnsmasq holding our address but serving a
// different hostsdir must NOT be adopted — its answers come from somewhere else,
// so names registered here would never resolve. Naming it beats letting dnsmasq
// fail later with a bare "Address already in use".
func TestPickDNSReportsConflict(t *testing.T) {
	procs := []dnsProc{{pid: 99, addr: "10.89.0.53", hostsDir: "/other/hosts.d"}}
	pid, err := pickDNS(procs, "10.89.0.53", "/net/hosts.d")
	if err == nil {
		t.Fatalf("conflicting resolver adopted (pid %d); want an error", pid)
	}
	for _, want := range []string{"99", "10.89.0.53", "/other/hosts.d", "/net/hosts.d"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestSweepNamesDropsTaplessRecords: a create killed outright leaves a name
// record no deferred cleanup could remove. A record with no tap behind it has no
// VM answering, so it goes; anything with a live tap stays, whichever sandbox
// root created it.
func TestSweepNamesDropsTaplessRecords(t *testing.T) {
	netDir := t.TempDir()
	hosts := filepath.Join(netDir, "hosts.d")
	if err := os.MkdirAll(hosts, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(hosts, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("live-vm", "10.89.0.200 live-vm\n")      // tap present below
	write("other-root-vm", "10.89.0.201 other\n")  // tap present, foreign root
	write("killed-create", "10.89.0.202 killed\n") // no tap -> stale
	write("_host", "")                             // machinery, never swept
	write("_host-route", "10.89.0.200 x.cs.sandbox\n")

	f := run.NewFake()
	f.OnStdout("ip -br link show", "podman1 UP aa\nfdt200 UP bb\nfdt201 UP cc\n")
	Fabric{Runner: f, NetDir: netDir}.sweepNames(context.Background())

	left := map[string]bool{}
	ents, err := os.ReadDir(hosts)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		left[e.Name()] = true
	}
	for _, keep := range []string{"live-vm", "other-root-vm", "_host", "_host-route"} {
		if !left[keep] {
			t.Errorf("%q was swept but should have been kept", keep)
		}
	}
	if left["killed-create"] {
		t.Error("tapless record survived the sweep")
	}
}

// TestTapNamesAreGroupScoped: interface names are host-global, so two fabrics
// whose VMs land on the same last octet must not produce the same tap name.
// This is why the prefix is allocated per group and recorded, not derived.
func TestTapNamesAreGroupScoped(t *testing.T) {
	a := Fabric{TapPrefix: "fd0001"}
	b := Fabric{TapPrefix: "fd0002"}
	const ip = "10.89.4.200" // the same last octet in both groups' subnets
	if a.TapName(ip) == b.TapName(ip) {
		t.Fatalf("two groups produced the same tap name %q for %s", a.TapName(ip), ip)
	}
	// The default fabric keeps the historical name so existing taps still match.
	if got := (Fabric{}).TapName("10.89.0.200"); got != "fdt200" {
		t.Errorf("default fabric tap = %q, want fdt200", got)
	}
	if got := a.TapName(ip); got != "fd0001200" {
		t.Errorf("group tap = %q, want fd0001200", got)
	}
	if len(a.TapName(ip)) > 15 {
		t.Errorf("tap name %q exceeds IFNAMSIZ", a.TapName(ip))
	}
}

// TestDNSMasqIsAuthoritativeForTheSuffix: the hostsdir carries only A records,
// so without --local every AAAA lookup is forwarded to the bridge's aardvark,
// which never answers for these names. Each one then costs a 5s timeout that
// systemd-resolved retries across scopes — measured at over 30s to resolve a
// group-qualified name that dnsmasq itself answers in milliseconds.
func TestDNSMasqIsAuthoritativeForTheSuffix(t *testing.T) {
	if !strings.Contains(dnsmasqScript, `--local="/$SUFFIX/"`) {
		t.Errorf("dnsmasq must be authoritative for the suffix:\n%s", dnsmasqScript)
	}
	// The suffix arrives as a positional arg, never interpolated into the script.
	if strings.Contains(dnsmasqScript, "--local=/cs.sandbox/") {
		t.Error("the suffix must be passed positionally, not baked into the script")
	}
}

// TestFabricSuffixPrecedence: a fabric started by any command must be
// authoritative for the same domain host-route publishes into, so the suffix is
// resolved here rather than threaded through every construction site.
func TestFabricSuffixPrecedence(t *testing.T) {
	t.Setenv("CS_SANDBOX_DNS_SUFFIX", "")
	if got := (Fabric{}).suffix(); got != "cs.sandbox" {
		t.Errorf("default suffix = %q", got)
	}
	t.Setenv("CS_SANDBOX_DNS_SUFFIX", "box.test")
	if got := (Fabric{}).suffix(); got != "box.test" {
		t.Errorf("env suffix = %q", got)
	}
	if got := (Fabric{Suffix: "explicit.test"}).suffix(); got != "explicit.test" {
		t.Errorf("explicit suffix should win over the environment, got %q", got)
	}
}

// TestDownReleasesTheBridgeAddress: the fabric's DNS address sits on netavark's
// OWN bridge, and while any address remains the bridge outlives the network's
// removal — still carrying that network's gateway. Podman then reuses the freed
// subnet for a different network on a different bridge, two bridges answer for
// one subnet, and every member of the new one loses outbound traffic with
// nothing in podman's view to explain it.
//
// The delete used to be conditional on the keepalive still running, which by
// teardown time it usually is not — so it silently never ran.
func TestDownReleasesTheBridgeAddress(t *testing.T) {
	fake := run.NewFake()
	fake.OnStdout("network inspect cs-sandbox-net --format {{.NetworkInterface}}", "podman1")
	fake.OnStdout("network inspect cs-sandbox-net --format {{(index .Subnets 0).Gateway}}", "10.89.4.1")
	// Deliberately NOT running: that is the state teardown actually happens in.
	fake.OnStdout("inspect cs-sandbox-net-keepalive", "false")

	Fabric{Runner: fake, Network: "cs-sandbox-net"}.Down(context.Background())

	var deleted bool
	for _, c := range fake.Rendered() {
		if strings.Contains(c, "ip addr del 10.89.4.53/24 dev podman1") {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("Down must release the DNS address even with the keepalive gone:\n%s",
			strings.Join(fake.Rendered(), "\n"))
	}
}

// TestDownSkipsTheDeleteWhenTheBridgeIsUnknown: once the network is gone there
// is nothing to inspect, and a delete built from empty strings would be a
// malformed command run for no reason.
func TestDownSkipsTheDeleteWhenTheBridgeIsUnknown(t *testing.T) {
	fake := run.NewFake() // every inspect returns empty
	Fabric{Runner: fake, Network: "cs-sandbox-gone"}.Down(context.Background())
	for _, c := range fake.Rendered() {
		if strings.Contains(c, "ip addr del") {
			t.Errorf("no delete should be attempted without a bridge: %q", c)
		}
	}
}

// A group has two resolvers: aardvark, which containers get by default and which
// knows container names, and the fabric's dnsmasq, which serves microVM names
// from the hostsdir and forwards the rest to aardvark. The gateway exists so the
// host can reach members BY NAME, so it must be given the second one — without
// it a microVM member is reachable by address and nameless, which is the promise
// docs/groups.md makes going unkept on the firecracker engine.
func TestGatewayIsGivenTheFabricResolver(t *testing.T) {
	f := run.NewFake()
	f.OnStdout("network inspect", "10.89.0.1\n")            // Gateway() -> prefix 10.89.0
	f.OnStdout("inspect cs-sandbox-g-keepalive", "false\n") // not running
	fab := Fabric{Runner: f, Network: "cs-sandbox-g", Image: "img", GWPort: 2401,
		GWBind: "127.0.0.1", GWSeed: "/seed", GWUser: "dev", GWHome: "/home/dev"}
	_ = fab.keepaliveUp(context.Background())

	var create string
	for _, line := range f.Rendered() {
		if strings.Contains(line, "podman run -d --name cs-sandbox-g-keepalive") {
			create = line
		}
	}
	if create == "" {
		t.Fatalf("no gateway was created:\n%s", strings.Join(f.Rendered(), "\n"))
	}
	if !strings.Contains(create, "--dns 10.89.0.53") {
		t.Errorf("gateway must resolve through the fabric dnsmasq (<prefix>.53):\n%s", create)
	}
}

// A keepalive with no published port is not a gateway — nobody jumps through it
// — so it must not be churned for lacking a resolver it has no use for.
func TestBridgePinningKeepaliveIsNotChurned(t *testing.T) {
	f := run.NewFake()
	f.OnStdout("network inspect", "10.89.0.1\n")
	f.OnStdout("inspect", "true\n")                                   // already running
	fab := Fabric{Runner: f, Network: "cs-sandbox-net", Image: "img"} // GWPort 0
	if err := fab.keepaliveUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, line := range f.Rendered() {
		if strings.Contains(line, "podman rm -f") {
			t.Errorf("a bridge-pinning keepalive was recreated for no reason:\n%s", line)
		}
	}
}

// A gateway created before the resolver was wired in keeps running and keeps
// failing to resolve members — the quiet version of the bug. Detect it from what
// the container actually has, not from how we would create one today, and
// replace it.
func TestStaleGatewayWithoutTheResolverIsReplaced(t *testing.T) {
	f := run.NewFake()
	f.OnStdout("network inspect", "10.89.0.1\n")
	f.OnStdout("--format {{.State.Running}}", "true\n")
	f.OnStdout("{{range .HostConfig.Dns}}", "\n") // no --dns: the old shape
	fab := Fabric{Runner: f, Network: "cs-sandbox-g", Image: "img", GWPort: 2401,
		GWBind: "127.0.0.1", GWSeed: "/seed", GWUser: "dev", GWHome: "/home/dev"}
	_ = fab.keepaliveUp(context.Background())

	rendered := strings.Join(f.Rendered(), "\n")
	if !strings.Contains(rendered, "podman rm -f cs-sandbox-g-keepalive") {
		t.Errorf("a resolver-less gateway must be replaced, not left running:\n%s", rendered)
	}
	if !strings.Contains(rendered, "--dns 10.89.0.53") {
		t.Errorf("its replacement must have the fabric resolver:\n%s", rendered)
	}
}

// And one that already has it is left alone: recreating a healthy gateway would
// drop every ssh session jumping through it.
func TestHealthyGatewayIsLeftAlone(t *testing.T) {
	f := run.NewFake()
	f.OnStdout("network inspect", "10.89.0.1\n")
	f.OnStdout("--format {{.State.Running}}", "true\n")
	f.OnStdout("{{range .HostConfig.Dns}}", "10.89.0.53 \n")
	fab := Fabric{Runner: f, Network: "cs-sandbox-g", Image: "img", GWPort: 2401,
		GWBind: "127.0.0.1", GWSeed: "/seed", GWUser: "dev", GWHome: "/home/dev"}
	if err := fab.keepaliveUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, line := range f.Rendered() {
		if strings.Contains(line, "podman rm -f") || strings.Contains(line, "podman run -d") {
			t.Errorf("a healthy gateway must not be recreated:\n%s", line)
		}
	}
}
