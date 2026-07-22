package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestVerbosityGating pins the three-level model: phase() shows unless --quiet;
// progress() shows only under --verbose.
func TestVerbosityGating(t *testing.T) {
	cases := []struct {
		name              string
		quiet, verbose    bool
		wantPhase, wantPr bool
	}{
		{"default", false, false, true, false},
		{"quiet", true, false, false, false},
		{"verbose", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			app := &App{Quiet: c.quiet, Verbose: c.verbose, errW: &buf}
			app.phase("PHASE_LINE")
			app.progress("PROGRESS_LINE")
			out := buf.String()
			if got := strings.Contains(out, "PHASE_LINE"); got != c.wantPhase {
				t.Errorf("phase shown=%v, want %v (out=%q)", got, c.wantPhase, out)
			}
			if got := strings.Contains(out, "PROGRESS_LINE"); got != c.wantPr {
				t.Errorf("progress shown=%v, want %v (out=%q)", got, c.wantPr, out)
			}
		})
	}
}

// runRoot drives the real command tree with a fake Runner and buffered output,
// so no external podman/firecracker is touched.
func runRoot(t *testing.T, app *App, args ...string) (*run.Fake, error) {
	t.Helper()
	f := run.NewFake()
	app.Runner = f
	if app.errW == nil {
		app.errW = io.Discard
	}
	root := newRootCmd(app)
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return f, root.Execute()
}

// TestQuietVerboseMutualExclusion: --quiet and --verbose together is rejected.
func TestQuietVerboseMutualExclusion(t *testing.T) {
	_, err := runRoot(t, &App{}, "--quiet", "--verbose", "version")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want mutual-exclusion error", err)
	}
	// Each alone is fine.
	if _, err := runRoot(t, &App{}, "--quiet", "version"); err != nil {
		t.Errorf("--quiet alone: %v", err)
	}
	if _, err := runRoot(t, &App{}, "--verbose", "version"); err != nil {
		t.Errorf("--verbose alone: %v", err)
	}
}

// TestBuildPodmanQuietFlag: podman build runs -q by default and under --quiet,
// and WITHOUT -q under --verbose.
func TestBuildPodmanQuietFlag(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		wantQ bool
	}{
		{"default", nil, true},
		{"quiet", []string{"--quiet"}, true},
		{"verbose", []string{"--verbose"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{}, c.flags...)
			args = append(args, "build", "--engine", "podman")
			f, err := runRoot(t, &App{}, args...)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var argv []string
			for _, call := range f.Calls {
				if len(call) >= 2 && call[0] == "podman" && call[1] == "build" {
					argv = call
				}
			}
			if argv == nil {
				t.Fatalf("no podman build call; calls=%s", f)
			}
			hasQ := slices.Contains(argv, "-q")
			if hasQ != c.wantQ {
				t.Errorf("podman build -q present=%v, want %v (%v)", hasQ, c.wantQ, argv)
			}
		})
	}
}

// TestInstallAgentToolsCopies: the command copies the tool set into the target
// dir with the right modes (scripts 0755, docs 0644) and skips the guest-only
// user-podman.
func TestInstallAgentToolsCopies(t *testing.T) {
	dest := t.TempDir()
	if _, err := runRoot(t, &App{}, "install-agent-tools", dest); err != nil {
		t.Fatalf("install-agent-tools: %v", err)
	}

	// A launch wrapper is present and executable.
	if m := mode(t, filepath.Join(dest, "cs-claude")); m&0o111 == 0 {
		t.Errorf("cs-claude mode = %o, want executable", m)
	}
	// A reference doc is present and NOT executable (0644).
	if m := mode(t, filepath.Join(dest, "CS_CLAUDE_REMOTE.md")); m.Perm() != 0o644 {
		t.Errorf("CS_CLAUDE_REMOTE.md mode = %o, want 644", m.Perm())
	}
	// The guest-only helper is skipped.
	if _, err := os.Stat(filepath.Join(dest, "user-podman")); !os.IsNotExist(err) {
		t.Errorf("user-podman should not be installed on the host (err=%v)", err)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}
