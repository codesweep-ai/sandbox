package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// orphanDeps builds Deps over a temp instances dir with a scripted `podman
// volume ls`, so no real podman is touched.
func orphanDeps(t *testing.T, volumes string) (Deps, *run.Fake) {
	t.Helper()
	f := run.NewFake().OnStdout("volume ls", volumes)
	return Deps{Runner: f, InstDir: t.TempDir()}, f
}

// writeRootfs fakes a microVM home disk left behind by `rm`.
func writeRootfs(t *testing.T, instDir, name string) {
	t.Helper()
	idir := filepath.Join(instDir, name)
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idir, "rootfs.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestOrphansFindsLeftoverDataOfBothEngines: `rm` keeps the home (a rootfs disk
// or a home volume) and drops the state record, so the data would otherwise be
// invisible. Orphans is what makes `ls` show it and `destroy` reach it.
func TestOrphansFindsLeftoverDataOfBothEngines(t *testing.T) {
	d, _ := orphanDeps(t, "cs-sandbox-home-boxp|2026-07-27 10:00:00.0 -0700 PDT\nunrelated|2026-07-27 10:00:00.0 -0700 PDT\n")
	writeRootfs(t, d.InstDir, "boxfc")

	got := d.Orphans(context.Background())
	if len(got) != 2 {
		t.Fatalf("Orphans = %+v, want the two leftovers", got)
	}
	if got[0].Name != "boxfc" || got[0].Engine != state.Firecracker {
		t.Errorf("first orphan = %+v, want boxfc on firecracker", got[0])
	}
	if got[1].Name != "boxp" || got[1].Engine != state.Podman {
		t.Errorf("second orphan = %+v, want boxp on podman", got[1])
	}
	if got[1].Since.IsZero() {
		t.Error("podman orphan should carry the volume's creation time for AGE")
	}
}

// TestOrphansSkipsLiveSandboxes: a sandbox that still exists owns its data — it
// belongs in the normal listing, never as a leftover.
func TestOrphansSkipsLiveSandboxes(t *testing.T) {
	d, _ := orphanDeps(t, "cs-sandbox-home-live\n")
	writeRootfs(t, d.InstDir, "live")
	if err := state.Save(d.InstDir, &state.Instance{Name: "live", Type: "agent", Engine: state.Firecracker}); err != nil {
		t.Fatal(err)
	}
	if got := d.Orphans(context.Background()); len(got) != 0 {
		t.Errorf("Orphans = %+v, want none while the sandbox exists", got)
	}
}

// TestPurgeOrphanDeletesTheData: both engines, the counterpart of destroy.
func TestPurgeOrphanDeletesTheData(t *testing.T) {
	d, f := orphanDeps(t, "cs-sandbox-home-boxp\n")
	writeRootfs(t, d.InstDir, "boxfc")
	ctx := context.Background()

	if err := d.PurgeOrphan(ctx, Orphan{Name: "boxfc", Engine: state.Firecracker}); err != nil {
		t.Fatal(err)
	}
	if statOK(filepath.Join(d.InstDir, "boxfc")) {
		t.Error("purging a microVM leftover should remove its instance dir")
	}

	if err := d.PurgeOrphan(ctx, Orphan{Name: "boxp", Engine: state.Podman}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"cs-sandbox-home-boxp", "cs-sandbox-containers-boxp"} {
		if !f.Contains("volume rm -f " + v) {
			t.Errorf("purging a container leftover should remove %s; calls: %s", v, f)
		}
	}
}

// TestOrphanLooksUpByName backs `destroy <name>` for a removed sandbox.
func TestOrphanLooksUpByName(t *testing.T) {
	d, _ := orphanDeps(t, "")
	writeRootfs(t, d.InstDir, "gone")

	if _, ok := d.Orphan(context.Background(), "gone"); !ok {
		t.Error("Orphan should find leftover data by name")
	}
	if _, ok := d.Orphan(context.Background(), "never-existed"); ok {
		t.Error("Orphan should report nothing for an unknown name")
	}
}
