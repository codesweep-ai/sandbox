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
