package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	assets "github.com/codesweep-ai/sandbox"
)

// TestMain keeps every test off this machine's own build store, whose contents
// change with each `make ci` run anywhere on it. A test that wants a store makes
// one (withStore).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cs-sandbox-test-builds-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CS_BUILDS_DIR", dir)
	os.Unsetenv("CS_BUILD_STORE")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// withStore makes a build store for this binary's owner holding each
// module@version given, with the go.mod text given for it, and points
// CS_BUILDS_DIR at it. It returns the store's goproxy/ directory.
func withStore(t *testing.T, mods map[string]string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CS_BUILDS_DIR", root)
	proxy := filepath.Join(root, imageOwner, "goproxy")
	if err := os.MkdirAll(proxy, 0o755); err != nil {
		t.Fatal(err)
	}
	for mv, gomod := range mods {
		m, v, _ := strings.Cut(mv, "@")
		at := filepath.Join(proxy, filepath.FromSlash(m), "@v")
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		for ext, body := range map[string]string{".info": `{"Version":"` + v + `"}`, ".mod": gomod} {
			if err := os.WriteFile(filepath.Join(at, v+ext), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return proxy
}

// lintPin is cs-lint at the version go.mod pins it, as the build reads it.
func lintPin(t *testing.T) string {
	t.Helper()
	const module = "github.com/codesweep-ai/lint"
	pins, err := assets.ToolPins("")
	if err != nil || pins[module] == "" {
		t.Fatalf("no pin for %s: %v", module, err)
	}
	return module + "@" + pins[module]
}

// leafBuild runs `cs-sandbox build` with the given flags and returns the argv of
// the podman build that makes the image the tools are installed in.
func leafBuild(t *testing.T, flags ...string) ([]string, error) {
	t.Helper()
	f, err := runRootAsBuilt(t, &App{}, append([]string{"build", "--engine", "podman"}, flags...)...)
	var argv []string
	for _, call := range f.Calls {
		if len(call) >= 2 && call[0] == "podman" && call[1] == "build" {
			argv = call
		}
	}
	if err == nil && argv == nil {
		t.Fatalf("no podman build call; calls=%s", f)
	}
	return argv, err
}

// buildArg is the value of one --build-arg, and whether it was passed.
func buildArg(argv []string, name string) (string, bool) {
	for _, a := range argv {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// mounts lists the -v arguments.
func mounts(argv []string) []string {
	var out []string
	for i, a := range argv {
		if a == "-v" && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// asBuilt stands in for a binary built from this checkout at HEAD, or at no
// revision at all when revision is empty.
func asBuilt(t *testing.T, revision string) {
	t.Helper()
	savedV, savedR := Version, sandboxRevision
	t.Cleanup(func() { Version, sandboxRevision = savedV, savedR })
	Version = testVersion
	sandboxRevision = func() string { return revision }
}

// headOf is this checkout's HEAD, which a test binary does not record.
func headOf(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoRoot(t), "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skipf("no git revision to zip: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestBuildOfPublishedModulesSetsNoProxy: with nothing in the store and no
// commit to pack, the build is the one CI runs, with no override, no mount and
// SELinux confinement left on.
func TestBuildOfPublishedModulesSetsNoProxy(t *testing.T) {
	asBuilt(t, "")
	argv, err := leafBuild(t)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if v, ok := buildArg(argv, "CS_GOPROXY"); ok {
		t.Errorf("plain build passed CS_GOPROXY=%s", v)
	}
	if m := mounts(argv); len(m) > 0 {
		t.Errorf("plain build bind-mounts %v", m)
	}
	if slices.Contains(argv, "--security-opt") {
		t.Errorf("plain build drops SELinux confinement: %v", argv)
	}
}

// TestBuildTakesAPinTheStoreHolds: a tool pinned at a version a clean `make ci`
// recorded installs from the store, with no flag. Only the modules the store
// serves skip the checksum database, down to what they require from it: a
// local lint that pins a local ledger, which pins a local vcr, needs all three
// named. A version the store holds that nothing pins is not.
func TestBuildTakesAPinTheStoreHolds(t *testing.T) {
	asBuilt(t, "")
	lint := lintPin(t)
	ledger := "github.com/codesweep-ai/ledger@v0.0.0-20260925000000-aaaaaaaaaaaa"
	proxy := withStore(t, map[string]string{
		lint:   "module github.com/codesweep-ai/lint\n\nrequire github.com/codesweep-ai/ledger " + strings.TrimPrefix(ledger, "github.com/codesweep-ai/ledger@") + "\n",
		ledger: "module github.com/codesweep-ai/ledger\n\nrequire github.com/codesweep-ai/vcr v0.0.0-20260925000000-cccccccccccc\n",
		"github.com/codesweep-ai/vcr@v0.0.0-20260925000000-cccccccccccc":    "module github.com/codesweep-ai/vcr\n",
		"github.com/codesweep-ai/tracer@v0.0.0-20260925000000-bbbbbbbbbbbb": "module github.com/codesweep-ai/tracer\n",
	})

	argv, err := leafBuild(t)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, _ := buildArg(argv, "CS_GOPROXY"); got != "file://"+guestProxyDir+",https://proxy.golang.org,direct" {
		t.Errorf("CS_GOPROXY = %q, want the store ahead of the real proxy", got)
	}
	if got, _ := buildArg(argv, "CS_GONOSUMDB"); got != "github.com/codesweep-ai/ledger,github.com/codesweep-ai/lint,github.com/codesweep-ai/vcr" {
		t.Errorf("CS_GONOSUMDB = %q, want lint and what it requires from the store, and nothing else", got)
	}
	if m := mounts(argv); !slices.Equal(m, []string{proxy + ":" + guestProxyDir + ":ro"}) {
		t.Errorf("mounts = %v, want the store's proxy, read-only", m)
	}
	// Without this an SELinux host denies the build every read of the mount and
	// `go install` stops on the .info with "permission denied".
	if i := slices.Index(argv, "--security-opt"); i < 0 || i+1 >= len(argv) || argv[i+1] != "label=disable" {
		t.Errorf("the mounted proxy is not readable under SELinux confinement (%v)", argv)
	}
}

// TestBuildPacksItsOwnUnrecordedCommit: cs-sandbox's own commit, which no clean
// `make ci` recorded, installs from a proxy packed out of the checkout. It is
// the version the image is labelled with, so that is the one packed.
func TestBuildPacksItsOwnUnrecordedCommit(t *testing.T) {
	asBuilt(t, headOf(t))
	// root resolves AssetDir itself, so point it at the checkout the way an
	// operator would rather than setting the field and watching it be replaced.
	t.Setenv("CS_SANDBOX_ASSETS_DIR", repoRoot(t))

	argv, err := leafBuild(t)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, _ := buildArg(argv, "CS_GOPROXY"); got != "file://"+guestSelfProxyDir+",https://proxy.golang.org,direct" {
		t.Errorf("CS_GOPROXY = %q, want the packed commit ahead of the real proxy", got)
	}
	if got, _ := buildArg(argv, "CS_GONOSUMDB"); got != sandboxModule {
		t.Errorf("CS_GONOSUMDB = %q, want %s alone", got, sandboxModule)
	}
	m := mounts(argv)
	if len(m) != 1 || !strings.HasSuffix(m[0], ":"+guestSelfProxyDir+":ro") {
		t.Errorf("mounts = %v, want the packed proxy, read-only", m)
	}
	if got, _ := buildArg(argv, "CS_SANDBOX_VERSION"); got != testVersion {
		t.Errorf("the image would install %q, not the version packed", got)
	}
}

// TestBuildTakesItsOwnCommitFromTheStoreFirst: a commit `make ci` recorded is
// not packed again, since the store already holds what Go would make of it.
func TestBuildTakesItsOwnCommitFromTheStoreFirst(t *testing.T) {
	asBuilt(t, headOf(t))
	t.Setenv("CS_SANDBOX_ASSETS_DIR", repoRoot(t))
	withStore(t, map[string]string{sandboxModule + "@" + testVersion: "module " + sandboxModule + "\n"})

	argv, err := leafBuild(t)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, _ := buildArg(argv, "CS_GOPROXY"); got != "file://"+guestProxyDir+",https://proxy.golang.org,direct" {
		t.Errorf("CS_GOPROXY = %q, want the store alone ahead of the real proxy", got)
	}
}

// TestBuildWithoutARevisionPacksNothing: a version that came from -ldflags with
// no VCS stamp behind it has nothing to zip. That is a published binary, so it
// installs from the proxy. One with no version at all cannot name an image, so
// it never reaches the question.
func TestBuildWithoutARevisionPacksNothing(t *testing.T) {
	asBuilt(t, "")
	t.Setenv("CS_SANDBOX_ASSETS_DIR", repoRoot(t))
	argv, err := leafBuild(t)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if v, ok := buildArg(argv, "CS_GOPROXY"); ok {
		t.Errorf("a build with no revision to pack passed CS_GOPROXY=%s", v)
	}

	Version = devVersion
	if _, err := leafBuild(t); err == nil || !strings.Contains(err.Error(), "reports no version") {
		t.Errorf("build with no version = %v, want the refusal", err)
	}
}

// TestLocalModulesNoneReadsNothing: the build CI's publish workflow runs, from
// published modules only, whatever this machine holds.
func TestLocalModulesNoneReadsNothing(t *testing.T) {
	asBuilt(t, headOf(t))
	t.Setenv("CS_SANDBOX_ASSETS_DIR", repoRoot(t))
	withStore(t, map[string]string{
		lintPin(t): "module github.com/codesweep-ai/lint\n",
	})
	argv, err := leafBuild(t, "--local-modules", "none")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if v, ok := buildArg(argv, "CS_GOPROXY"); ok {
		t.Errorf("--local-modules none passed CS_GOPROXY=%s", v)
	}
	if m := mounts(argv); len(m) > 0 {
		t.Errorf("--local-modules none bind-mounts %v", m)
	}
}

// TestLocalModulesNamesAnotherStore: a campaign's store, say, read in place of
// the owner's. A directory that is no store fails before anything is built.
func TestLocalModulesNamesAnotherStore(t *testing.T) {
	asBuilt(t, "")
	other := t.TempDir()
	lint := lintPin(t)
	at := filepath.Join(other, "goproxy", "github.com", "codesweep-ai", "lint", "@v")
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(at, strings.SplitN(lint, "@", 2)[1]+".info"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	argv, err := leafBuild(t, "--local-modules", other)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if m := mounts(argv); !slices.Equal(m, []string{filepath.Join(other, "goproxy") + ":" + guestProxyDir + ":ro"}) {
		t.Errorf("mounts = %v, want the named store's proxy", m)
	}

	argv, err = leafBuild(t, "--local-modules", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no build store") {
		t.Errorf("--local-modules on an empty directory = %v, want it refused", err)
	}
	if argv != nil {
		t.Errorf("podman build ran anyway: %v", argv)
	}
}

// TestSlimBuildReadsNoStore: the slim image installs no cs- tools, so it needs
// no proxy for them, and the `make ci` smoke build it serves stays CI's.
func TestSlimBuildReadsNoStore(t *testing.T) {
	asBuilt(t, headOf(t))
	t.Setenv("CS_SANDBOX_ASSETS_DIR", repoRoot(t))
	withStore(t, map[string]string{
		lintPin(t): "module github.com/codesweep-ai/lint\n",
	})
	argv, err := leafBuild(t, "--slim")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if v, ok := buildArg(argv, "CS_GOPROXY"); ok {
		t.Errorf("a slim build passed CS_GOPROXY=%s", v)
	}
}

// recorded makes the entry a clean `make ci` of rev leaves in a fresh store,
// and returns the store.
func recorded(t *testing.T, rev string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CS_BUILDS_DIR", root)
	store := filepath.Join(root, imageOwner)
	entry := filepath.Join(store, "status", "sandbox", rev+".json")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte(`{"commit": "`+rev+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestBuildRecordsItsImageForASiblingsRepin: an image made here of a recorded
// commit gets its file in the store, which is what campaign's `make repin`
// waits for before it pins that build. The full image and the slim one each
// write their own, and a second build leaves the first file as it was.
func TestBuildRecordsItsImageForASiblingsRepin(t *testing.T) {
	rev := strings.Repeat("ab", 20)
	asBuilt(t, rev)
	t.Setenv("CS_SANDBOX_IMAGE", "")
	store := recorded(t, rev)

	for _, c := range []struct {
		flags       []string
		image, repo string
	}{{nil, "sandbox", localImageRepo}, {[]string{"--slim"}, "sandbox-slim", localSlimImageRepo}} {
		if _, err := leafBuild(t, c.flags...); err != nil {
			t.Fatalf("build %v: %v", c.flags, err)
		}
		file := filepath.Join(store, "images", "sandbox", rev, c.image+".json")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("no image file for %s: %v", c.image, err)
		}
		var got struct{ Name, Commit, Image, Ref string }
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != "sandbox" || got.Commit != rev || got.Image != c.image || got.Ref != c.repo+":"+testVersion {
			t.Errorf("image file = %s", data)
		}
		if _, err := leafBuild(t, c.flags...); err != nil {
			t.Fatal(err)
		}
		if again, _ := os.ReadFile(file); string(again) != string(data) {
			t.Errorf("a second build rewrote %s", file)
		}
	}
}

// TestBuildCommitsItsImageRecordInARepositoryStore: a campaign's store is a
// git repository, and a record there is a commit the orchestrator carries.
func TestBuildCommitsItsImageRecordInARepositoryStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	rev := strings.Repeat("ef", 20)
	asBuilt(t, rev)
	t.Setenv("CS_SANDBOX_IMAGE", "")
	store := recorded(t, rev)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "seed"}} {
		if _, err := gitAt(store, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := leafBuild(t, "--slim"); err != nil {
		t.Fatal(err)
	}
	if got, _ := gitAt(store, "log", "-1", "--format=%s"); got != "Record image sandbox-slim of sandbox "+rev[:7] {
		t.Errorf("last commit = %q, want the image record", got)
	}
	if got, _ := gitAt(store, "status", "--porcelain"); got != "" {
		t.Errorf("store left with changes: %q", got)
	}
}

// TestBuildRecordsNoImageSiblingsMayNotPin: a commit no clean gate recorded, a
// dirty binary, and an image under a name somebody chose are none of them a
// local build of the commit, so none leaves a file.
func TestBuildRecordsNoImageSiblingsMayNotPin(t *testing.T) {
	rev := strings.Repeat("cd", 20)
	images := func(store string) []string {
		entries, _ := os.ReadDir(filepath.Join(store, "images", "sandbox", rev))
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}

	t.Run("unrecorded", func(t *testing.T) {
		asBuilt(t, rev)
		t.Setenv("CS_SANDBOX_IMAGE", "")
		store := filepath.Join(t.TempDir(), imageOwner)
		t.Setenv("CS_BUILDS_DIR", filepath.Dir(store))
		if _, err := leafBuild(t); err != nil {
			t.Fatal(err)
		}
		if got := images(store); len(got) > 0 {
			t.Errorf("recorded %v for a commit no gate recorded", got)
		}
	})
	t.Run("dirty", func(t *testing.T) {
		asBuilt(t, rev)
		Version = testVersion + "+dirty"
		t.Setenv("CS_SANDBOX_IMAGE", "")
		store := recorded(t, rev)
		if _, err := leafBuild(t); err != nil {
			t.Fatal(err)
		}
		if got := images(store); len(got) > 0 {
			t.Errorf("recorded %v for a dirty binary", got)
		}
	})
	t.Run("a chosen name", func(t *testing.T) {
		asBuilt(t, rev)
		t.Setenv("CS_SANDBOX_IMAGE", "localhost/pinned:7")
		store := recorded(t, rev)
		if _, err := leafBuild(t); err != nil {
			t.Fatal(err)
		}
		if got := images(store); len(got) > 0 {
			t.Errorf("recorded %v for an image under CS_SANDBOX_IMAGE", got)
		}
	})
}
