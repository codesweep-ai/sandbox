package cli

import (
	"errors"
	"strings"
	"testing"
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
