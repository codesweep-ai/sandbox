package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
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
// and WITHOUT -q under --verbose. BUILD_VERBOSE tracks it, so the RUN steps that
// drive `nvim --headless` (whose stderr -q does not suppress) stay quiet unless
// the user asked for detail.
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
			// BUILD_VERBOSE is the inverse of -q: quiet build -> quiet RUN steps.
			wantBV := "BUILD_VERBOSE=1"
			if c.wantQ {
				wantBV = "BUILD_VERBOSE=0"
			}
			if !slices.Contains(argv, wantBV) {
				t.Errorf("podman build missing %s (%v)", wantBV, argv)
			}
		})
	}
}

// TestInstallAgentToolsCopies: the command copies the tool set into the target
// dir with the right modes (scripts 0755, docs 0644).
func TestInstallAgentToolsCopies(t *testing.T) {
	dest := t.TempDir()
	if _, err := runRoot(t, &App{}, "install-agent-tools", dest); err != nil {
		t.Fatalf("install-agent-tools: %v", err)
	}

	// Every agent's launch wrapper and remote family is present and executable.
	for _, tool := range []string{
		"cs-claude", "cs-claude-remote", "cs-claude-turn",
		"cs-codex", "cs-codex-remote", "cs-codex-turn",
		"cs-opencode", "cs-opencode-remote", "cs-opencode-turn",
	} {
		if m := mode(t, filepath.Join(dest, tool)); m&0o111 == 0 {
			t.Errorf("%s mode = %o, want executable", tool, m)
		}
	}
	// Each family's reference docs are present and NOT executable (0644).
	for _, doc := range []string{"CS_CLAUDE_REMOTE.md", "CS_CODEX_REMOTE.md", "CS_OPENCODE_REMOTE.md"} {
		if m := mode(t, filepath.Join(dest, doc)); m.Perm() != 0o644 {
			t.Errorf("%s mode = %o, want 644", doc, m.Perm())
		}
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

// TestLsQuietIsPipeable: -q prints bare names, one per line, with no header or
// columns — so `cs-sandbox ls -q | xargs -n1 cs-sandbox destroy -f` works.
func TestLsQuietIsPipeable(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"beta", "alpha"} {
		if err := state.Save(dir, &state.Instance{
			Name: n, Type: "agent", Engine: state.Podman, Port: 2200, Created: "2026-07-27T10:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	app := &App{InstDir: dir, TierDir: t.TempDir(), Runner: run.NewFake()}
	if err := runLs(context.Background(), app, &buf, true); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "alpha.default\nbeta.default\n" { // List sorts by name
		t.Errorf("ls -q = %q, want sorted qualified refs", got)
	}

	// The table form keeps the header and columns.
	buf.Reset()
	if err := runLs(context.Background(), app, &buf, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "STATUS", "AGE", "alpha", "beta"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("ls table missing %q:\n%s", want, buf.String())
		}
	}
}

// TestLsShowsRemovedSandboxData: data `rm` kept shows up as `removed`, with the
// hint that says how to reuse or delete it — the alternative is data sitting on
// disk that nothing lists (docker's dangling volumes).
func TestLsShowsRemovedSandboxData(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, &state.Instance{
		Name: "alive", Type: "agent", Engine: state.Podman, Port: 2200, Created: "2026-07-27T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	// A microVM home disk with no state record: what `rm` leaves behind.
	if err := os.MkdirAll(filepath.Join(state.Dir(dir, state.DefaultGroup, "leftover")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.Dir(dir, state.DefaultGroup, "leftover"), "rootfs.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	app := &App{InstDir: dir, TierDir: t.TempDir(), Runner: run.NewFake()}
	if err := runLs(context.Background(), app, &buf, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"alive", "leftover", "removed", "destroy -f"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls missing %q:\n%s", want, out)
		}
	}

	// -q lists it too, so `ls -q | xargs -n1 cs-sandbox destroy -f` cleans it up.
	buf.Reset()
	if err := runLs(context.Background(), app, &buf, true); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "alive.default\nleftover.default\n" {
		t.Errorf("ls -q = %q, want the live sandbox and the leftover", got)
	}
}

// TestDestroyReclaimsRemovedSandboxData: `rm` keeps the data and drops the state
// record, so `destroy` has to work on the name afterwards — otherwise the data
// it kept can never be deleted.
func TestDestroyReclaimsRemovedSandboxData(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", dir) // the root command resolves state dirs from the env
	idir := filepath.Join(state.Dir(dir, state.DefaultGroup, "leftover"))
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idir, "rootfs.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{InstDir: dir, TierDir: t.TempDir()}

	// Without -f it only says what it would do; the data stays.
	if _, err := runRoot(t, app, "destroy", "leftover.default"); err != nil {
		t.Fatalf("destroy without -f: %v", err)
	}
	if _, err := os.Stat(idir); err != nil {
		t.Fatal("destroy without -f must not delete anything")
	}

	if _, err := runRoot(t, app, "destroy", "leftover.default", "-f"); err != nil {
		t.Fatalf("destroy -f: %v", err)
	}
	if _, err := os.Stat(idir); !os.IsNotExist(err) {
		t.Error("destroy -f should have deleted the kept data")
	}
}

// TestDestroyUnknownNameSaysThereIsNoData: nothing to destroy is reported as
// such, rather than as a bare "no such sandbox" that hides whether data remains.
func TestDestroyUnknownNameSaysThereIsNoData(t *testing.T) {
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", t.TempDir())
	app := &App{TierDir: t.TempDir()}
	_, err := runRoot(t, app, "destroy", "never-existed", "-f")
	if err == nil || !strings.Contains(err.Error(), "no data") {
		t.Fatalf("err = %v, want it to say there is no leftover data either", err)
	}
}
