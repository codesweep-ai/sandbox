package spec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestParseIdentity: the flag is read before anything is provisioned, so a value it cannot
// read has to be refused there. A named identity ends up in a tab and unit-separator
// delimited seed file, so the separators are cleaned out of it as they are for the host's.
func TestParseIdentity(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Identity
	}{
		{"", Identity{}},
		{"host", Identity{}},
		{"none", Identity{Mode: IdentityNone}},
		{"Campaign Bot <bot@example.test>", Identity{Mode: IdentityNamed, Name: "Campaign Bot", Email: "bot@example.test"}},
	} {
		got, err := ParseIdentity(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseIdentity(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"nobody", "Bot", "<bot@example.test>", "Bot <not-an-address>", "Bot <a@b> extra"} {
		if got, err := ParseIdentity(bad); err == nil {
			t.Errorf("ParseIdentity(%q) = %+v; want a refusal", bad, got)
		}
	}
}

// TestANamedOrAbsentIdentityNeverAsksTheHost: the point of the flag is that the operator's
// name and address do not reach the sandbox, so with it set the host's git configuration is
// not even read. A clone is then set to the named identity, or to none.
func TestANamedOrAbsentIdentityNeverAsksTheHost(t *testing.T) {
	r := run.NewFake()
	r.OnStdout("config user.name", "The Operator\n")
	r.OnStdout("config user.email", "operator@example.com\n")
	ctx := context.Background()
	if got := (Identity{Mode: IdentityNone}).ForRepo(ctx, r, "/repo"); got != US {
		t.Errorf("none: %q; want an empty name and address", got)
	}
	named := Identity{Mode: IdentityNamed, Name: "Bot", Email: "bot@example.test"}
	if got := named.ForRepo(ctx, r, "/repo"); got != "Bot"+US+"bot@example.test" {
		t.Errorf("named: %q", got)
	}
	if calls := r.Rendered(); len(calls) != 0 {
		t.Errorf("the host's git configuration was read:\n%s", strings.Join(calls, "\n"))
	}
	if got := (Identity{}).ForRepo(ctx, r, "/repo"); got != "The Operator"+US+"operator@example.com" {
		t.Errorf("host: %q", got)
	}
}
