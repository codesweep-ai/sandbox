//go:build integration

// Live firecracker download test: fetches the pinned release from upstream into
// a temp cache and proves the digest committed in fcDigests still matches the
// artifact being served — the check the unit tests cannot make, since they never
// touch the network. Run it when bumping DefaultFCVersion.
//
//	go test -tags integration -run TestFirecrackerDownload ./internal/fcdisk/ -v
//
// Requires network access, curl and tar. Skips when curl is unavailable.
package fcdisk

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

func TestFirecrackerDownloadPinnedDigest(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	if _, err := fcArch(); err != nil {
		t.Skipf("unsupported architecture: %v", err)
	}
	bc := BuildConfig{}.Defaulted()
	c := Cache{Dir: t.TempDir(), Progress: func(s string) { t.Log(s) }}

	if err := c.ensureFirecrackerBin(context.Background(), &run.Exec{}, bc); err != nil {
		t.Fatalf("ensureFirecrackerBin = %v — upstream %s no longer matches the digest pinned in fcDigests?", err, bc.FCVersion)
	}
	fi, err := os.Stat(c.FirecrackerBin())
	if err != nil {
		t.Fatalf("firecracker binary not installed: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("firecracker binary mode = %v, want executable", fi.Mode())
	}
	if got := c.FirecrackerVersion(); got != bc.FCVersion {
		t.Errorf("fc-version stamp = %q, want %q", got, bc.FCVersion)
	}

	// The binary upstream ships must actually report the release we pinned.
	out, err := exec.Command(c.FirecrackerBin(), "--version").Output()
	if err != nil {
		t.Fatalf("firecracker --version: %v", err)
	}
	if want := "Firecracker " + bc.FCVersion; !strings.HasPrefix(string(out), want) {
		t.Errorf("firecracker --version = %q, want it to start with %q", firstLineOf(string(out)), want)
	}

	// A populated cache is left alone — no second download.
	before := len(mustReadDir(t, c.Dir))
	if err := c.ensureFirecrackerBin(context.Background(), &run.Exec{}, bc); err != nil {
		t.Fatalf("second ensureFirecrackerBin = %v, want nil", err)
	}
	if after := len(mustReadDir(t, c.Dir)); after != before {
		t.Errorf("cache entries %d -> %d, want the populated cache untouched", before, after)
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	e, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
