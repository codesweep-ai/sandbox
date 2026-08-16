package spec

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

func mkdir(t *testing.T, base, rel string) string {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func mkGitRepo(t *testing.T, base, rel string) string {
	t.Helper()
	p := mkdir(t, base, rel)
	if err := os.Mkdir(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// resolved is what the resolvers return for p: the symlink-free real path. On
// macOS t.TempDir() lands under /var, which is itself a symlink to /private/var,
// so an expectation built from the raw temp path never matches.
func resolved(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestResolveSnapshots(t *testing.T) {
	base := t.TempDir()
	a := mkdir(t, base, "projects/api")
	mkdir(t, base, "projects/web")

	got, err := ResolveSnapshots([]string{a, filepath.Join(base, "projects/web") + ":frontend"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(got))
	}
	if got[0].Name != "api" { // default name = basename
		t.Errorf("snapshot[0].Name = %q, want api", got[0].Name)
	}
	if got[1].Name != "frontend" { // :NAME override
		t.Errorf("snapshot[1].Name = %q, want frontend", got[1].Name)
	}
}

func TestResolveSnapshotsDuplicateName(t *testing.T) {
	base := t.TempDir()
	mkdir(t, base, "a/api")
	mkdir(t, base, "b/api")
	_, err := ResolveSnapshots([]string{filepath.Join(base, "a/api"), filepath.Join(base, "b/api")}, Options{})
	if err == nil {
		t.Fatal("expected a duplicate-name error")
	}
}

func TestResolveSnapshotsErrors(t *testing.T) {
	base := t.TempDir()
	// a plain file, not a directory
	f := filepath.Join(base, "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSnapshots([]string{f}, Options{}); err == nil {
		t.Error("expected 'not a directory' error")
	}
	if _, err := ResolveSnapshots([]string{filepath.Join(base, "missing")}, Options{}); err == nil {
		t.Error("expected 'path not found' error")
	}
	dir := mkdir(t, base, "valid")
	for _, invalid := range []string{"..", "has space", "line\nbreak"} {
		if _, err := ResolveSnapshots([]string{dir + ":" + invalid}, Options{}); err == nil {
			t.Errorf("expected invalid destination name %q to fail", invalid)
		}
	}
}

func TestResolveRepoClones(t *testing.T) {
	base := t.TempDir()
	repo := mkGitRepo(t, base, "code/api")

	// PATH@REF:NAME — all three parts.
	got, err := ResolveRepoClones([]string{repo + "@v1.0:svc"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	rc := got[0]
	if rc.Name != "svc" || rc.BaseRef != "v1.0" || rc.HostPath != resolved(t, repo) {
		t.Errorf("got %+v, want HostPath %s", rc, resolved(t, repo))
	}
}

// Shared paths are resolved to their real location: the podman machine (and the
// microVM's virtiofs share) mounts what the path points AT, so a symlinked spec
// must land on the target, not the link.
func TestResolveRepoClonesResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	repo := mkGitRepo(t, base, "code/api")
	link := filepath.Join(base, "link-to-api")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveRepoClones([]string{link + ":svc"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].HostPath != resolved(t, repo) {
		t.Errorf("HostPath = %s, want the symlink target %s", got[0].HostPath, resolved(t, repo))
	}
}

// The same for --snapshot, which shares the resolver.
func TestResolveSnapshotsResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	dir := mkdir(t, base, "projects/api")
	link := filepath.Join(base, "link-to-api")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSnapshots([]string{link}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].HostPath != resolved(t, dir) {
		t.Errorf("HostPath = %s, want the symlink target %s", got[0].HostPath, resolved(t, dir))
	}
	// The default name comes from the resolved path, not the link name.
	if got[0].Name != "api" {
		t.Errorf("Name = %q, want api (basename of the target)", got[0].Name)
	}
}

func TestResolveRepoClonesDefaults(t *testing.T) {
	base := t.TempDir()
	repo := mkGitRepo(t, base, "code/web")
	got, err := ResolveRepoClones([]string{repo}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "web" || got[0].BaseRef != "" {
		t.Errorf("defaults wrong: %+v", got[0])
	}
}

func TestResolveRepoClonesRejectsNonRepo(t *testing.T) {
	base := t.TempDir()
	plain := mkdir(t, base, "notarepo")
	if _, err := ResolveRepoClones([]string{plain}, Options{}); err == nil {
		t.Error("expected 'not a git repository' error")
	}
}

func TestResolveRepoClonesRejectsInvalidManifestValues(t *testing.T) {
	base := t.TempDir()
	repo := mkGitRepo(t, base, "code/api")
	for _, invalid := range []string{
		repo + ":..",
		repo + ":has space",
		repo + "@",
		repo + "@main\nother",
	} {
		if _, err := ResolveRepoClones([]string{invalid}, Options{}); err == nil {
			t.Errorf("ResolveRepoClones(%q) should fail", invalid)
		}
	}
}

func TestGitIdentity(t *testing.T) {
	// GitIdentity uses a Runner; verify the US separator joins the two values.
	r := run.NewFake()
	r.OnStdout("config user.name", "Ada Lovelace\n")
	r.OnStdout("config user.email", "ada@example.com\n")
	got := GitIdentity(context.Background(), r, "/repo")
	want := "Ada Lovelace" + US + "ada@example.com"
	if got != want {
		t.Errorf("GitIdentity = %q, want %q", got, want)
	}
}

func TestGitIdentitySanitizesManifestSeparators(t *testing.T) {
	r := run.NewFake()
	r.OnStdout("config user.name", "Ada\nLovelace\n")
	r.OnStdout("config user.email", "ada"+US+"x@example.test\n")
	got := GitIdentity(context.Background(), r, "/repo")
	want := "Ada Lovelace" + US + "ada x@example.test"
	if got != want {
		t.Errorf("GitIdentity = %q, want %q", got, want)
	}
}
