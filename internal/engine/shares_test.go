package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/spec"
)

// TestSnapshotSharesLandOwnedByTheUser: a shared directory arrives owned by the
// user working in the sandbox, under either engine (SPEC R163). The host owner
// is not the answer to that question: a rootless container maps the caller's own
// ids and nothing else, so anything else on the host reads as "nobody" inside.
//
// Each engine gets there its own way — podman chowns the copy it bind-mounts,
// firecracker records the ownership as it packs the ext4 — and neither is proof
// of the other, so both are pinned here.
func TestSnapshotSharesLandOwnedByTheUser(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := hostenv.Host{User: "dev", UID: 4242, GID: 77}
	cs := CreateSpec{Name: "box", Snapshots: []spec.Snapshot{{HostPath: src, Name: "data"}}}
	ctx := context.Background()

	t.Run("podman", func(t *testing.T) {
		fake := run.NewFake()
		d := Deps{Runner: fake, Host: host, InstDir: t.TempDir()}
		idir := d.InstanceDir(cs.Name)
		if _, err := d.materializeShares(ctx, idir, filepath.Join(idir, "seed"), cs); err != nil {
			t.Fatal(err)
		}
		// The container mounts the copy, so it is the copy that has to belong to
		// the user: cp -a leaves it carrying whatever of the source's ownership an
		// unprivileged caller was allowed to keep.
		want := "chown -Rh 4242:77 " + filepath.Join(idir, "snap", "data")
		if !fake.Contains(want) {
			t.Errorf("no %q\n%s", want, strings.Join(fake.Rendered(), "\n"))
		}
	})

	t.Run("firecracker", func(t *testing.T) {
		fake := run.NewFake()
		fe := NewFirecracker(Deps{Runner: fake, Host: host, InstDir: t.TempDir()})
		if _, _, err := fe.buildSnapshotDisks(ctx, fe.d.InstanceDir(cs.Name), cs); err != nil {
			t.Fatal(err)
		}
		// What the image ends up recording is fcdisk's contract, and its own tests
		// hold it; what belongs here is that the disk build is told the host's ids
		// rather than left to fakeroot, which reports every file as root's.
		if want := "FC_UID=4242; FC_GID=77"; !fake.Contains(want) {
			t.Errorf("no %q\n%s", want, strings.Join(fake.Rendered(), "\n"))
		}
	})
}

// TestTheSeedCarriesTheIdentityTheCallerChose: the operator's git name and address are the
// one thing of theirs a sandbox is given without a flag naming it, and an agent that prints
// its git configuration puts them into whatever records the session. --git-identity has to
// reach both places the seed carries an identity: the global one, and the one each --repo
// clone is set to. With none, neither holds anything of the host's.
func TestTheSeedCarriesTheIdentityTheCallerChose(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	for _, tc := range []struct {
		name         string
		id           spec.Identity
		wantGlobal   string
		wantManifest string
	}{
		{"host", spec.Identity{}, "name\tThe Operator\nemail\toperator@example.com\n", "The Operator" + spec.US + "operator@example.com"},
		{"none", spec.Identity{Mode: spec.IdentityNone}, "", spec.US + "\n"},
		{"named", spec.Identity{Mode: spec.IdentityNamed, Name: "Bot", Email: "bot@example.test"},
			"name\tBot\nemail\tbot@example.test\n", "Bot" + spec.US + "bot@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := run.NewFake()
			fake.OnStdout("user.name", "The Operator\n")
			fake.OnStdout("user.email", "operator@example.com\n")
			d := Deps{Runner: fake, Host: hostenv.Host{User: "dev"}, InstDir: t.TempDir()}
			if got := d.globalGitIdentity(ctx, tc.id).File(); got != tc.wantGlobal {
				t.Errorf("global identity = %q; want %q", got, tc.wantGlobal)
			}
			cs := CreateSpec{Name: "box", GitIdentity: tc.id, RepoClones: []spec.RepoClone{{HostPath: repo, Name: "src"}}}
			idir := d.InstanceDir(cs.Name)
			seedDir := filepath.Join(idir, "seed")
			if err := os.MkdirAll(seedDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := d.materializeShares(ctx, idir, seedDir, cs); err != nil {
				t.Fatal(err)
			}
			manifest, err := os.ReadFile(filepath.Join(seedDir, "repos"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(manifest), tc.wantManifest) {
				t.Errorf("repos manifest = %q; want it to carry %q", manifest, tc.wantManifest)
			}
			if tc.name != "host" && strings.Contains(string(manifest), "Operator") {
				t.Errorf("the manifest carries the host's identity: %q", manifest)
			}
		})
	}
}
