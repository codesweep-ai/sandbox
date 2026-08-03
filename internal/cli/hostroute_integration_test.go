//go:build integration

// host-route's cross-group forwarding test.
//
// This one is opt-in beyond the build tag, because unlike the other integration
// tests it CANNOT be isolated into temp dirs: the network fabric, the veths and
// the on-marker are host-global by design (one rootless netns per host, shared
// by every sandbox root). Running it wires real interfaces on the developer's
// machine and needs root to do it.
//
// Under NOPASSWD sudo (CI) the usual invocation works:
//
//	CS_SANDBOX_IT_HOSTROUTE=1 go test -tags integration ./internal/cli/ -run HostRoute -v
//
// With a password-protected sudo it does not: `go test` gives the test binary
// stdin on /dev/null and no controlling terminal, so sudo has nowhere to prompt.
// Build the binary and run it directly, which keeps the terminal:
//
//	go test -c -tags integration -o /tmp/cli.test ./internal/cli
//	CS_SANDBOX_IT_HOSTROUTE=1 /tmp/cli.test -test.run HostRouteBlocks -test.v
//
// It restores whatever host-route state it found: if the feature was off before,
// it is taken back down afterwards.
package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func forwardingOf(t *testing.T, leg string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv4/conf", leg, "forwarding"))
	if err != nil {
		t.Fatalf("read forwarding for %s: %v", leg, err)
	}
	return strings.TrimSpace(string(data))
}

func setForwarding(t *testing.T, r *run.Exec, leg, v string) {
	t.Helper()
	if _, err := r.Run(context.Background(), run.Opts{},
		"sudo", "bash", "-c", "echo "+v+" > /proc/sys/net/ipv4/conf/"+leg+"/forwarding"); err != nil {
		t.Fatalf("set forwarding=%s on %s: %v", v, leg, err)
	}
}

// TestHostRouteBlocksCrossGroupForwardingLive is the live proof behind the claim
// in docs/groups.md that the host is not a router between groups.
//
// The assertion that matters is an A/B, not a bare "is it blocked": with
// forwarding ON the bypass must be REACHABLE, and only then does BLOCKED with it
// off mean the knob did the work. Without that control the test would pass just
// as happily on a host whose firewall blocks forwarding for its own reasons —
// which is exactly how this was nearly misdiagnosed the first time.
func TestHostRouteBlocksCrossGroupForwardingLive(t *testing.T) {
	if os.Getenv("CS_SANDBOX_IT_HOSTROUTE") == "" {
		t.Skip("set CS_SANDBOX_IT_HOSTROUTE=1 — this test wires real host interfaces")
	}
	r, host := liveSetup(t)
	if host.IsMacOS {
		t.Skip("host-route is Linux-only")
	}
	ctx := context.Background()
	// Acquire sudo. A cached credential or NOPASSWD (CI) needs no prompt at all,
	// so try that first: `go test` runs the test binary with stdin on /dev/null
	// and no controlling terminal, and a password-protected sudo there stalls on
	// its PAM timeout before failing — half a minute spent reaching a skip.
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "sudo", "-n", "true"); err != nil {
		if !haveTTY() {
			t.Skip("needs sudo, and `go test` gives the test binary no terminal to prompt on. Run it as:\n" +
				"  go test -c -tags integration -o /tmp/cli.test ./internal/cli && \\\n" +
				"    CS_SANDBOX_IT_HOSTROUTE=1 /tmp/cli.test -test.run HostRouteBlocks -test.v")
		}
		if _, err := r.Run(ctx, run.Opts{Interactive: true}, "sudo", "-v"); err != nil {
			t.Skip("needs sudo: the test wires legs and flips the control knob")
		}
	}
	if exec.Command("systemctl", "is-active", "--quiet", "systemd-resolved").Run() != nil {
		t.Skip("host-route requires systemd-resolved")
	}

	ga, gb := boxName(t, "hra"), boxName(t, "hrb")
	// Leave host-route exactly as it was found.
	wasUp := strings.Contains(mustRun(t, "host-route", "status"), "UP")
	t.Cleanup(func() {
		_, _ = execRoot(t, "group", "rm", ga, "-f")
		_, _ = execRoot(t, "group", "rm", gb, "-f")
		if !wasUp {
			_, _ = execRoot(t, "host-route", "down")
		}
	})

	step(t, "creating one sandbox in each of two groups…")
	createBox(t, r, "a."+ga, "--group", ga)
	createBox(t, r, "b."+gb, "--group", gb)

	step(t, "bringing host-route up (wires one leg per group)…")
	if out, err := execRoot(t, "host-route", "up"); err != nil {
		t.Fatalf("host-route up: %v (out=%q)", err, out)
	}

	// What host-route itself set, before any test manipulation: this is the
	// shipped behaviour, not a value the test wrote.
	legs := map[string]string{}
	for _, g := range []string{ga, gb, state.DefaultGroup} {
		grp, err := state.LoadGroup(instancesDir(t), g)
		if err != nil {
			continue
		}
		leg := "cs-sandbox"
		if g != state.DefaultGroup {
			leg = "hr" + strings.TrimPrefix(grp.TapPrefix, "fd") + "h"
		}
		legs[g] = leg
		if got := forwardingOf(t, leg); got != "0" {
			t.Errorf("host-route left forwarding=%s on %s (group %s); the host is a router between groups", got, leg, g)
		}
	}
	legA, legB := legs[ga], legs[gb]
	if legA == "" || legB == "" {
		t.Fatalf("could not resolve both legs: %v", legs)
	}

	// Route each group's subnet via the host leg on its own subnet, both
	// directions, so the result cannot rest on asymmetric return. Sandboxes hold
	// NET_ADMIN, so this is available to whatever runs inside one.
	subnet := func(g string) string {
		return run.Output(ctx, r, "podman", "network", "inspect", state.NetworkName(g),
			"--format", "{{range .Subnets}}{{.Subnet}}{{end}}")
	}
	hostAddr := func(g string) string {
		s := subnet(g)
		return s[:strings.LastIndex(s[:strings.LastIndex(s, ".")], ".")+1] + "251"
	}
	bIP := run.Output(ctx, r, "podman", "inspect", "b."+gb,
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")
	if bIP == "" {
		t.Fatal("could not read b's address")
	}
	_, _ = r.Run(ctx, run.Opts{}, "podman", "exec", "a."+ga, "ip", "route", "add", subnet(gb), "via", hostAddr(ga))
	_, _ = r.Run(ctx, run.Opts{}, "podman", "exec", "b."+gb, "ip", "route", "add", subnet(ga), "via", hostAddr(gb))

	probe := func() string {
		return inBox(ctx, r, host, "a."+ga,
			"timeout 5 bash -c '</dev/tcp/"+bIP+"/22' 2>/dev/null && echo REACHABLE || echo BLOCKED")
	}

	step(t, "positive control: forwarding ON, the bypass must be reachable…")
	setForwarding(t, r, legA, "1")
	setForwarding(t, r, legB, "1")
	t.Cleanup(func() { setForwarding(t, r, legA, "0"); setForwarding(t, r, legB, "0") })
	if got := probe(); got != "REACHABLE" {
		// Not a failure: something else on this host (typically firewalld's
		// forward policy) already blocks it, so the knob's own contribution
		// cannot be observed here. Reporting PASS would be a lie.
		t.Skipf("control did not reach (%s) — this host blocks forwarding for its own reasons, "+
			"so this test cannot show what per-leg forwarding contributes", got)
	}

	step(t, "the test: forwarding OFF, as host-route sets it…")
	setForwarding(t, r, legA, "0")
	setForwarding(t, r, legB, "0")
	if got := probe(); got != "BLOCKED" {
		t.Errorf("a member of %s reached %s through the host with forwarding off: %s", ga, gb, got)
	}
}

// mustRun executes a command tree and returns stdout, ignoring the error: used
// for probes whose failure is itself information.
func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := execRoot(t, args...)
	return out
}

// instancesDir is the redirected instances root liveSetup installed.
func instancesDir(t *testing.T) string {
	t.Helper()
	return os.Getenv("CS_SANDBOX_INSTANCES_DIR")
}

// haveTTY reports whether a terminal is available to prompt on. Checking
// /dev/tty rather than stdin is deliberate: sudo reads the password from the
// controlling terminal, and stdin under `go test` is /dev/null — itself a
// character device, so the usual "is stdin a tty" test would answer yes and be
// wrong here.
func haveTTY() bool {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
