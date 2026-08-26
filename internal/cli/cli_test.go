package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	assets "github.com/codesweep-ai/sandbox"
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
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = testVersion
	return runRootWith(t, app, unpublished(), args...)
}

// testVersion stands in for the module version a test binary does not carry. Go
// records one only when it builds from a checkout and `go test` does not, so
// without this every command that names an image would take the refusal path —
// which is a case of its own, held by TestImageTag.
const testVersion = "v0.0.0-20260101000000-0123456789ab"

// runRootAsBuilt runs the tree without standing in a version, for the tests that
// have chosen one — or chosen to have none.
func runRootAsBuilt(t *testing.T, app *App, args ...string) (*run.Fake, error) {
	t.Helper()
	return runRootWith(t, app, unpublished(), args...)
}

// unpublished is a Fake whose registry has no image for this version, which is
// the ordinary case for a working checkout and the one that makes `build`
// build. A Fake that answered yes to everything would let the pull succeed and
// skip the build, leaving every assertion about podman build nothing to read.
func unpublished() *run.Fake {
	return run.NewFake().On("podman pull", run.Result{}, errors.New("manifest unknown"))
}

// runRootWith runs the tree against a Fake the caller prepared.
func runRootWith(t *testing.T, app *App, f *run.Fake, args ...string) (*run.Fake, error) {
	t.Helper()
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

// TestBuildPassesToolPinsFromGoMod: the sibling cs- tools the image ships are
// installed at the versions go.mod pins, not at their branch tips, and the
// versions travel as build args because the build context is rootfs/ and
// `COPY . /sandbox` would carry a manifest into the guest.
//
// Two halves, because either alone would pass while the image drifted: the
// build args must carry the manifest's versions, and the Containerfile must
// actually consume them rather than reaching for @latest.
func TestBuildPassesToolPinsFromGoMod(t *testing.T) {
	pins, err := assets.ToolPins("")
	if err != nil {
		t.Fatalf("ToolPins: %v", err)
	}
	f, err := runRoot(t, &App{}, "build", "--engine", "podman")
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
	for arg, module := range map[string]string{
		"CS_LINT_VERSION":   "github.com/codesweep-ai/lint",
		"CS_LEDGER_VERSION": "github.com/codesweep-ai/ledger",
		"CS_TRACER_VERSION": "github.com/codesweep-ai/tracer",
	} {
		want := pins[module]
		if want == "" {
			t.Errorf("go.mod pins no version for %s", module)
			continue
		}
		if !slices.Contains(argv, arg+"="+want) {
			t.Errorf("podman build missing %s=%s (%v)", arg, want, argv)
		}
	}

	// The other half: the Containerfile has to install each sibling at its
	// build arg. cs-sandbox is here too, though a module cannot pin itself — the
	// cs-sandbox running the build passes its own version instead.
	cf, err := os.ReadFile(filepath.Join("..", "..", "image", "Containerfile"))
	if err != nil {
		t.Fatalf("read Containerfile: %v", err)
	}
	for bin, arg := range map[string]string{
		"cs-sandbox": "CS_SANDBOX_VERSION",
		"cs-lint":    "CS_LINT_VERSION",
		"cs-ledger":  "CS_LEDGER_VERSION",
		"cs-tracer":  "CS_TRACER_VERSION",
	} {
		want := "/cmd/" + bin + "@${" + arg + "}"
		if !strings.Contains(string(cf), want) {
			t.Errorf("Containerfile does not install %s at its pin (want %q)", bin, want)
		}
		if strings.Contains(string(cf), "/cmd/"+bin+"@latest") {
			t.Errorf("Containerfile still installs %s at @latest", bin)
		}
	}
}

// TestSandboxPinIsInstallable: the version cs-sandbox passes for itself has to
// be one `go install` accepts. Go's build info appends +dirty to a binary built
// from a modified tree and a module version cannot carry that, so it is trimmed
// — the image gets the committed revision, which is the only one installable.
func TestSandboxPinIsInstallable(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	cases := []struct {
		name, stamp, want string
		wantNote          bool
	}{
		{"clean pseudo-version", "v0.0.0-20260826151729-1c4a9cc0fe4c", "v0.0.0-20260826151729-1c4a9cc0fe4c", false},
		{"dirty tree", "v0.0.0-20260826151729-1c4a9cc0fe4c+dirty", "v0.0.0-20260826151729-1c4a9cc0fe4c", true},
		{"tagged", "v1.2.3", "v1.2.3", false},
		{"dirty at a tag", "v1.2.3+dirty", "v1.2.3", true},
		{"no build info", devVersion, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			Version = c.stamp
			got, note := sandboxPin()
			if got != c.want {
				t.Errorf("sandboxPin() = %q, want %q", got, c.want)
			}
			if (note != "") != c.wantNote {
				t.Errorf("note = %q, want note=%v", note, c.wantNote)
			}
			if strings.Contains(got, "+") {
				t.Errorf("%q still carries a build-info suffix; go install would reject it", got)
			}
		})
	}
}

// ociTag is the tag grammar a registry accepts. A reference podman rejects is
// found here rather than several minutes into a build.
var ociTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// TestImageTag: the image is named after the version that built it, so the tag
// has to BE that version — and has to be a legal tag. The two do not always
// coincide: Go marks a binary from a modified tree with +dirty, and + is not a
// tag character. -dirty is, nothing ever publishes one, and so a dirty binary
// asks the registry for something that cannot exist, which is the intent.
func TestImageTag(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	cases := []struct {
		name, stamp, want, wantErr string
	}{
		{"pseudo-version", "v0.0.0-20260826151729-1c4a9cc0fe4c", "v0.0.0-20260826151729-1c4a9cc0fe4c", ""},
		{"release", "v1.2.3", "v1.2.3", ""},
		{"dirty tree", "v0.0.0-20260826151729-1c4a9cc0fe4c+dirty", "v0.0.0-20260826151729-1c4a9cc0fe4c-dirty", ""},
		{"dirty at a tag", "v1.2.3+dirty", "v1.2.3-dirty", ""},
		{"no version at all", devVersion, "", "reports no version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			Version = c.stamp
			got, err := imageTag()
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("imageTag() error = %v, want one saying %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("imageTag(): %v", err)
			}
			if got != c.want {
				t.Errorf("imageTag() = %q, want %q", got, c.want)
			}
			if !ociTag.MatchString(got) {
				t.Errorf("%q is not a legal OCI tag; podman would refuse the reference", got)
			}
		})
	}
}

// TestImageRefUsesTheVersionedPackages: each of the three images is named for
// the same version, in a package of its own. A sandbox package that could hand
// out a toolchain-less image is the thing this prevents.
func TestImageRefUsesTheVersionedPackages(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = testVersion

	for repo, want := range map[string]string{
		imageRepo:           "ghcr.io/codesweep-ai/sandbox:" + testVersion,
		slimImageRepo:       "ghcr.io/codesweep-ai/sandbox-slim:" + testVersion,
		slimAgentsImageRepo: "ghcr.io/codesweep-ai/sandbox-slim-agents:" + testVersion,
	} {
		got, err := imageRef(repo)
		if err != nil {
			t.Fatalf("imageRef(%q): %v", repo, err)
		}
		if got != want {
			t.Errorf("imageRef(%q) = %q, want %q", repo, got, want)
		}
	}
}

// TestPullShowsProgressUnlessQuiet: a pull moves gigabytes, and podman prints
// the only sign that anything is happening. Quietened, it reads as a hang, and
// the person watching kills it. Only --quiet, which asks for silence, gets it.
func TestPullShowsProgressUnlessQuiet(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		wantQ bool
	}{
		{"default", nil, false},
		{"verbose", []string{"--verbose"}, false},
		{"quiet", []string{"--quiet"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{}, c.args...), "build", "--engine", "podman")
			f, err := runRoot(t, &App{}, args...)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var pull []string
			for _, call := range f.Calls {
				if len(call) >= 2 && call[0] == "podman" && call[1] == "pull" {
					pull = call
				}
			}
			if pull == nil {
				t.Fatalf("no podman pull call; calls=%s", f)
			}
			if got := slices.Contains(pull, "-q"); got != c.wantQ {
				t.Errorf("podman pull -q = %v, want %v (%v)", got, c.wantQ, pull)
			}
		})
	}
}

// TestBuildSkipsThePullForALocalImage: a localhost/ reference names an image
// that only ever exists in the local store, so there is no registry to ask.
// podman does not know that: it resolves the name to a registry called
// localhost and spends three retries on https://localhost/v2/ before failing.
// CI sets CS_SANDBOX_IMAGE to a local tag for every job that builds one, so
// without this it pays that on every run.
func TestBuildSkipsThePullForALocalImage(t *testing.T) {
	t.Setenv("CS_SANDBOX_IMAGE", "localhost/sandbox:ci")
	f, err := runRoot(t, &App{}, "build", "--engine", "podman")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var built bool
	for _, call := range f.Calls {
		if len(call) < 2 || call[0] != "podman" {
			continue
		}
		if call[1] == "pull" {
			t.Errorf("tried to pull a local-only image: %v", call)
		}
		built = built || call[1] == "build"
	}
	if !built {
		t.Errorf("no podman build call; calls=%s", f)
	}
}

// TestBuildDryRunReachesTheBuild: a dry run prints the pull AND the build.
// Pulling is a mutation, so --dry-run skips the command and reports success;
// reading that as a hit would leave a dry run of `build` having printed nothing
// that makes an image, which is the one thing it exists to show.
func TestBuildDryRunReachesTheBuild(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = testVersion

	// A Fake with no canned failures answers yes to everything, which is what a
	// skipped-because-dry-run command looks like from here.
	app := &App{Exec: &run.Exec{DryRun: true}}
	f, err := runRootWith(t, app, run.NewFake(), "build", "--engine", "podman")
	if err != nil {
		t.Fatalf("build --dry-run: %v", err)
	}
	var pulled, built bool
	for _, call := range f.Calls {
		if len(call) < 2 || call[0] != "podman" {
			continue
		}
		switch call[1] {
		case "pull":
			pulled = true
		case "build":
			built = true
		}
	}
	if !pulled {
		t.Errorf("a dry run of build never reached the pull; calls=%s", f)
	}
	if !built {
		t.Error("the skipped pull was taken as a hit, so the dry run printed no build")
	}
}

// TestVersionImages: `version --images` is what the publish workflow reads to
// learn what to push, so it has to name all three packages and fail rather than
// print a partial list. A workflow that pushed two of three would leave a
// release half published, with nothing saying so.
func TestVersionImages(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	t.Run("names every package", func(t *testing.T) {
		Version = testVersion
		app := &App{errW: io.Discard}
		var out bytes.Buffer
		root := newRootCmd(app)
		root.SetArgs([]string{"version", "--images"})
		root.SetOut(&out)
		root.SetErr(io.Discard)
		if err := root.Execute(); err != nil {
			t.Fatalf("version --images: %v", err)
		}
		for _, want := range []string{
			"image              " + imageRepo + ":" + testVersion,
			"image-slim         " + slimImageRepo + ":" + testVersion,
			"image-slim-agents  " + slimAgentsImageRepo + ":" + testVersion,
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q in:\n%s", want, out.String())
			}
		}
	})

	t.Run("refuses an unversioned binary", func(t *testing.T) {
		Version = devVersion
		app := &App{errW: io.Discard}
		root := newRootCmd(app)
		root.SetArgs([]string{"version", "--images"})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		if err := root.Execute(); err == nil {
			t.Fatal("version --images succeeded with no version to name images after")
		}
	})
}

// TestBuildSlim: --slim derives a Containerfile with ci-slim.sh and builds from
// that one, under a tag of its own; the default build is untouched. The tag
// matters as much as the file — three images that are not interchangeable, and
// only the tag tells a later `create` which one it got.
func TestBuildSlim(t *testing.T) {
	cases := []struct {
		name          string
		flags         []string
		env           string
		wantImage     string
		wantSlimFile  bool
		wantKeepAgent bool
	}{
		{"default", nil, "", imageRepo + ":" + testVersion, false, false},
		{"slim", []string{"--slim"}, "", slimImageRepo + ":" + testVersion, true, false},
		{"slim with agents", []string{"--slim", "--with-agents"}, "", slimAgentsImageRepo + ":" + testVersion, true, true},
		// An explicit reference wins: it is how CI pins the build and the test run to one.
		{"slim honours CS_SANDBOX_IMAGE", []string{"--slim"}, "localhost/pinned:7", "localhost/pinned:7", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv("CS_SANDBOX_IMAGE", c.env)
			} else {
				t.Setenv("CS_SANDBOX_IMAGE", "")
			}
			args := append([]string{"build", "--engine", "podman"}, c.flags...)
			f, err := runRoot(t, &App{}, args...)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			var build, slim []string
			for _, call := range f.Calls {
				switch {
				case len(call) >= 2 && call[0] == "podman" && call[1] == "build":
					build = call
				case len(call) >= 2 && call[0] == "sh" && strings.HasSuffix(call[1], "ci-slim.sh"):
					slim = call
				}
			}
			if build == nil {
				t.Fatalf("no podman build call; calls=%s", f)
			}
			if !slices.Contains(build, c.wantImage) {
				t.Errorf("built tag: want %s in %v", c.wantImage, build)
			}
			if (slim != nil) != c.wantSlimFile {
				t.Errorf("ci-slim.sh invoked=%v, want %v", slim != nil, c.wantSlimFile)
			}
			// The -f argument names the derived file only for a slim build.
			i := slices.Index(build, "-f")
			if i < 0 || i+1 >= len(build) {
				t.Fatalf("no -f in %v", build)
			}
			gotSlimFile := strings.HasSuffix(build[i+1], "Containerfile.ci")
			if gotSlimFile != c.wantSlimFile {
				t.Errorf("-f %s: slim=%v, want %v", build[i+1], gotSlimFile, c.wantSlimFile)
			}
		})
	}
}

// TestBuildWithAgentsNeedsSlim: --with-agents alone is refused rather than
// quietly ignored — the full image has the agents already, so a run that meant
// --slim and lost it would otherwise build 9.3 GB and say nothing.
func TestBuildWithAgentsNeedsSlim(t *testing.T) {
	_, err := runRoot(t, &App{}, "build", "--engine", "podman", "--with-agents")
	if err == nil || !strings.Contains(err.Error(), "--slim") {
		t.Fatalf("err = %v, want a --slim-only error", err)
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
	if err := os.MkdirAll(state.Dir(dir, state.DefaultGroup, "leftover"), 0o700); err != nil {
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
	idir := state.Dir(dir, state.DefaultGroup, "leftover")
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
