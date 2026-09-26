package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalModuleProxyInstallsAnUnpushedRevision is the whole point of packing
// an unrecorded commit, end to end: a module zip written from this checkout's
// git tree, served over file://, installs BY VERSION — and the binary reports
// that version, exactly as it would coming from proxy.golang.org.
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

// TestLocalModuleProxyServesTheRevisionNotTheTree: the three files agree with
// one another and with what the real proxy serves, however the working tree has
// moved on (SBX-080). Go records a hash of the .mod in go.sum, so a manifest
// from the tree would write a line the real module never matches. And it works
// from a worktree, whose .git is a file rather than a directory (SBX-050).
func TestLocalModuleProxyServesTheRevisionNotTheTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"GIT_AUTHOR_DATE": "2026-09-25T10:11:12-07:00", "GIT_COMMITTER_DATE": "2026-09-25T10:11:12-07:00",
	} {
		t.Setenv(k, v)
	}
	repo := filepath.Join(t.TempDir(), "m")
	committed := "module example.com/m\n\ngo 1.21\n"
	for name, body := range map[string]string{"go.mod": committed, "m.go": "package m\n"} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := gitAt(dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "add", "-A")
	git(repo, "commit", "-q", "-m", "one")
	revision := git(repo, "rev-parse", "HEAD")
	// The tree moves on after the commit, as it does while somebody works.
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte(committed+"\nrequire example.com/other v1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(filepath.Dir(repo), "wt")
	git(repo, "worktree", "add", "-q", "--detach", wt, revision)
	const version = "v0.0.0-20260925171112-000000000000"

	for _, from := range []string{repo, wt} {
		dir, cleanup, err := localModuleProxy(from, version, revision)
		if err != nil {
			t.Fatalf("localModuleProxy from %s: %v", from, err)
		}
		at := filepath.Join(dir, "example.com", "m", "@v", version)
		mod, err := os.ReadFile(at + ".mod")
		if err != nil {
			t.Fatal(err)
		}
		if string(mod) != committed {
			t.Errorf("from %s, .mod = %q, want the revision's %q", from, mod, committed)
		}
		raw, err := os.ReadFile(at + ".info")
		if err != nil {
			t.Fatal(err)
		}
		var info struct {
			Version string
			Time    string
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatal(err)
		}
		if info.Version != version || info.Time != "2026-09-25T17:11:12Z" {
			t.Errorf("from %s, .info = %s, want the version and the commit time in UTC", from, raw)
		}
		cleanup()
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
