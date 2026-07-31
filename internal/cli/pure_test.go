package cli

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestParsePortSpec covers VMPORT and HOSTPORT:VMPORT forms and the rejected
// cases (non-numeric, non-positive, malformed).
func TestParsePortSpec(t *testing.T) {
	ok := []struct {
		spec             string
		wantHost, wantVM int
	}{
		{"8080", 8080, 8080},  // single port -> both equal
		{"9090:80", 9090, 80}, // host:vm
		{"1:65535", 1, 65535}, // extremes
	}
	for _, c := range ok {
		h, v, err := parsePortSpec(c.spec)
		if err != nil || h != c.wantHost || v != c.wantVM {
			t.Errorf("parsePortSpec(%q) = (%d,%d,%v), want (%d,%d,nil)",
				c.spec, h, v, err, c.wantHost, c.wantVM)
		}
	}
	for _, bad := range []string{
		"", "0", "-1", "65536", "abc",
		"80:xyz", "80:0", "0:80", "65536:80", "80:65536", ":80", "80:",
	} {
		if _, _, err := parsePortSpec(bad); err == nil {
			t.Errorf("parsePortSpec(%q) should fail", bad)
		}
	}
}

// TestAutoEngine: macOS always gets podman (no KVM microVMs there). The Linux
// branch depends on the host, so we only assert it returns a valid engine.
func TestAutoEngine(t *testing.T) {
	if got := autoEngine(true); got != "podman" {
		t.Errorf("autoEngine(macOS) = %q, want podman", got)
	}
	if got := autoEngine(false); got != "podman" && got != "firecracker" {
		t.Errorf("autoEngine(linux) = %q, want podman or firecracker", got)
	}
}

// TestNormalizeInsecure: truthy spellings map to "1", everything else to "0".
func TestNormalizeInsecure(t *testing.T) {
	for _, truthy := range []string{"1", "true", "yes", "on", "TRUE", "YES", "ON", "True", "Yes", "On"} {
		if got := normalizeInsecure(truthy); got != "1" {
			t.Errorf("normalizeInsecure(%q) = %q, want 1", truthy, got)
		}
	}
	for _, falsy := range []string{"", "0", "false", "no", "off", "2", "tRuE", "enabled"} {
		if got := normalizeInsecure(falsy); got != "0" {
			t.Errorf("normalizeInsecure(%q) = %q, want 0", falsy, got)
		}
	}
}

// TestSplitLines drops blank/whitespace-only lines and keeps the rest verbatim.
func TestSplitLines(t *testing.T) {
	got := splitLines("busybox:latest  4 MB\n\n   \nalpine:3  7 MB\n")
	want := []string{"busybox:latest  4 MB", "alpine:3  7 MB"}
	if len(got) != len(want) {
		t.Fatalf("splitLines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitLines[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if splitLines("") != nil && len(splitLines("")) != 0 {
		t.Errorf("splitLines(\"\") should be empty, got %v", splitLines(""))
	}
}

func TestBatchError(t *testing.T) {
	sentinel := errors.New("transport failed")
	if err := batchError("fetch", 2, nil); err != nil {
		t.Fatalf("batchError without failures = %v", err)
	}
	err := batchError("fetch", 2, []error{sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("batchError = %v, want wrapped transport error", err)
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("batchError = %q, want failure count", err)
	}
}

// TestInheritAgentLoginValidation: an unknown agent is rejected before anything
// is created, and the error names the valid ones. Nothing is inherited by
// default, so a plain create needs no flag.
func TestInheritAgentLoginValidation(t *testing.T) {
	app := &App{InstDir: t.TempDir(), TierDir: t.TempDir()}
	_, err := runRoot(t, app, "create", "box", "--inherit-agent-login", "bogus")
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v, want an unknown-agent error", err)
	}
	for _, want := range []string{"claude", "codex", "opencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q as valid: %v", want, err)
		}
	}
	// A valid agent gets past validation (it fails later, on the real engine).
	if _, err := runRoot(t, app, "create", "box2", "--inherit-agent-login", "claude"); err != nil &&
		strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("claude should be accepted: %v", err)
	}
}

// TestAgentLoginArgValidation: agent-login rejects an unknown agent before it
// touches any sandbox state.
func TestAgentLoginArgValidation(t *testing.T) {
	app := &App{InstDir: t.TempDir(), TierDir: t.TempDir()}
	_, err := runRoot(t, app, "agent-login", "bogus", "box")
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v, want an unknown-agent error", err)
	}
	// A known agent gets past the agent check and fails on the missing sandbox.
	_, err = runRoot(t, app, "agent-login", "claude", "nosuchbox")
	if err == nil || strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v, want a no-such-sandbox error", err)
	}
}

// TestAge renders the largest single unit, kubectl-style, and refuses to invent
// a duration it can't compute.
func TestAge(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	cases := map[string]string{
		at(30 * time.Second):    "30s",
		at(90 * time.Second):    "1m",
		at(45 * time.Minute):    "45m",
		at(3 * time.Hour):       "3h",
		at(23 * time.Hour):      "23h",
		at(26 * time.Hour):      "1d",
		at(15 * 24 * time.Hour): "15d",
		"":                      "-", // no timestamp
		"not-a-time":            "-", // unparseable
		at(-2 * time.Hour):      "-", // clock skew: never a negative age
	}
	for in, want := range cases {
		if got := age(in, now); got != want {
			t.Errorf("age(%q) = %q, want %q", in, got, want)
		}
	}
}
