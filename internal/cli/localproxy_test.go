package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLocalModuleProxyInstallsAnUnpushedRevision is the whole point of
// --local-sandbox, end to end: a module zip written from this checkout's git
// tree, served over file://, installs BY VERSION — and the binary reports that
// version, exactly as it would coming from proxy.golang.org.
//
// It builds the real thing, so it needs git and a network-free `go install`
// against the local proxy. GOFLAGS carries the toolchain fallback the image
// build passes for the same reason: a one-module proxy 404s for everything else.
func TestLocalModuleProxyInstallsAnUnpushedRevision(t *testing.T) {
	repo := repoRoot(t)
	rev, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skipf("no git revision to zip: %v", err)
	}
	revision := strings.TrimSpace(string(rev))
	const version = "v0.0.0-20200101000000-000000000000" // never published, by construction

	dir, cleanup, err := localModuleProxy(repo, version, revision)
	if err != nil {
		t.Fatalf("localModuleProxy: %v", err)
	}
	defer cleanup()

	for _, ext := range []string{".zip", ".mod", ".info"} {
		p := filepath.Join(dir, "github.com", "codesweep-ai", "sandbox", "@v", version+ext)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("proxy is missing %s: %v", ext, err)
		}
		if fi.Size() == 0 {
			t.Errorf("%s is empty", ext)
		}
	}

	gobin := t.TempDir()
	cmd := exec.Command("go", "install", "github.com/codesweep-ai/sandbox/cmd/cs-sandbox@"+version)
	cmd.Env = append(os.Environ(),
		"GOBIN="+gobin,
		"GOPROXY=file://"+dir+",https://proxy.golang.org,direct",
		"GONOSUMDB=github.com/codesweep-ai/*",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go install from the local proxy: %v\n%s", err, out)
	}
	out, err := exec.Command(filepath.Join(gobin, "cs-sandbox"), "version").Output()
	if err != nil {
		t.Fatalf("run the installed binary: %v", err)
	}
	if !strings.Contains(string(out), version) {
		t.Errorf("installed binary reports %q, want it to carry %s", strings.TrimSpace(string(out)), version)
	}
}

// TestLocalSandboxRefusesWithoutARevision: the flag names a commit to zip, so
// the cases with no commit to name have to fail before podman is started
// rather than half way through an image build.
func TestLocalSandboxRefusesWithoutARevision(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = devVersion // no module version, so sandboxPin is empty

	f, err := runRoot(t, &App{}, "build", "--engine", "podman", "--local-sandbox")
	if err == nil {
		t.Fatalf("build --local-sandbox succeeded with no module version; calls=%s", f)
	}
	if !strings.Contains(err.Error(), "--local-sandbox") {
		t.Errorf("error does not name the flag: %v", err)
	}
	for _, call := range f.Calls {
		if len(call) >= 2 && call[0] == "podman" && call[1] == "build" {
			t.Errorf("podman build ran anyway: %v", call)
		}
	}
}

// TestLocalSandboxMountsTheProxyAndOverridesGOPROXY: the happy path. The build
// has to bind-mount the generated proxy and point the install at it — and keep
// the upstream proxy in the list, because a one-module proxy 404s for the Go
// toolchain and every other module the image installs.
func TestLocalSandboxMountsTheProxyAndOverridesGOPROXY(t *testing.T) {
	repo := repoRoot(t)
	rev, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skipf("no git revision to zip: %v", err)
	}
	savedV, savedR := Version, sandboxRevision
	t.Cleanup(func() { Version, sandboxRevision = savedV, savedR })
	Version = "v0.0.0-20200101000000-000000000000"
	sandboxRevision = func() string { return strings.TrimSpace(string(rev)) }
	// root resolves AssetDir itself, so point it at the checkout the way an
	// operator would rather than setting the field and watching it be replaced.
	t.Setenv("CS_SANDBOX_ASSETS_DIR", repo)

	f, err := runRoot(t, &App{}, "build", "--engine", "podman", "--local-sandbox")
	if err != nil {
		t.Fatalf("build --local-sandbox: %v", err)
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
	var proxyArg, mount string
	for i, a := range argv {
		if strings.HasPrefix(a, "CS_GOPROXY=") {
			proxyArg = a
		}
		if a == "-v" && i+1 < len(argv) {
			mount = argv[i+1]
		}
	}
	if !strings.HasPrefix(proxyArg, "CS_GOPROXY=file://") {
		t.Errorf("CS_GOPROXY is not a file:// proxy: %q", proxyArg)
	}
	if !strings.Contains(proxyArg, ",https://proxy.golang.org") {
		t.Errorf("CS_GOPROXY drops the upstream fallback, so the toolchain would 404: %q", proxyArg)
	}
	if !slices.Contains(argv, "CS_GONOSUMDB=github.com/codesweep-ai/*") {
		t.Errorf("no scoped checksum-db bypass; an unpublished module cannot verify (%v)", argv)
	}
	if !strings.HasSuffix(mount, guestProxyDir+":ro") {
		t.Errorf("proxy is not mounted read-only at %s: %q", guestProxyDir, mount)
	}
	if !slices.Contains(argv, "CS_SANDBOX_VERSION="+Version) {
		t.Errorf("the image would install a different version than the proxy serves (%v)", argv)
	}
}

// TestBuildWithoutLocalSandboxSetsNoProxy: the ordinary build must not carry
// the override, or every image would install from a proxy that is not there.
func TestBuildWithoutLocalSandboxSetsNoProxy(t *testing.T) {
	f, err := runRoot(t, &App{}, "build", "--engine", "podman")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, call := range f.Calls {
		if len(call) < 2 || call[0] != "podman" || call[1] != "build" {
			continue
		}
		for _, a := range call {
			if strings.HasPrefix(a, "CS_GOPROXY=") && a != "CS_GOPROXY=" {
				t.Errorf("plain build passed %s", a)
			}
		}
		if slices.Contains(call, "-v") {
			t.Errorf("plain build bind-mounts something: %v", call)
		}
	}
}

// repoRoot walks up to the directory holding go.mod, so the test works from
// wherever `go test` puts the package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
