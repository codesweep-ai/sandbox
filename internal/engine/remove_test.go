package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func statOK(p string) bool { _, err := os.Stat(p); return err == nil }

// TestPodmanRmKeepsVolumesRemovesState: `rm` (purge=false) removes the container
// and the instance state, but NOT the data volumes — so `create` reuses them.
func TestPodmanRmKeepsVolumesRemovesState(t *testing.T) {
	dir := t.TempDir()
	idir := filepath.Join(dir, "box")
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idir, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	p := NewPodman(Deps{Runner: f, InstDir: dir})

	if err := p.Remove(context.Background(), "box", false); err != nil {
		t.Fatal(err)
	}
	if f.Contains("volume rm") {
		t.Errorf("rm must NOT remove data volumes; calls: %s", f)
	}
	if !f.Contains("podman rm -f box") {
		t.Errorf("rm should remove the container; calls: %s", f)
	}
	if statOK(idir) {
		t.Error("rm should remove the instance state dir")
	}
}

// TestPodmanDestroyRemovesVolumes: `destroy` (purge=true) removes the data volumes.
func TestPodmanDestroyRemovesVolumes(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "box"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	p := NewPodman(Deps{Runner: f, InstDir: dir})

	if err := p.Remove(context.Background(), "box", true); err != nil {
		t.Fatal(err)
	}
	if !f.Contains("volume rm -f cs-sandbox-home-box") {
		t.Errorf("destroy should remove the home volume; calls: %s", f)
	}
}

// fcTestDepsLocal builds a Firecracker with a fake runner + temp dirs (no KVM).
func fcRemoveDeps(t *testing.T, instDir string) *Firecracker {
	t.Helper()
	return NewFirecracker(Deps{Runner: run.NewFake(), InstDir: instDir, FCCache: t.TempDir(), Network: "cs-sandbox-net"})
}

// TestFirecrackerRmKeepsRootfs: `rm` (purge=false) keeps the home disk
// (rootfs.ext4) and drops the ephemeral disks + state, so `create` reuses the home.
func TestFirecrackerRmKeepsRootfs(t *testing.T) {
	dir := t.TempDir()
	idir := filepath.Join(dir, "box")
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"rootfs.ext4", "seed.ext4", "run.json", "repo1.ext4", "snap1.ext4"} {
		if err := os.WriteFile(filepath.Join(idir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Save(dir, &state.Instance{Name: "box", Type: "agent", Engine: state.Firecracker, FCIP: "10.89.0.200"}); err != nil {
		t.Fatal(err)
	}

	if err := fcRemoveDeps(t, dir).Remove(context.Background(), "box", false); err != nil {
		t.Fatal(err)
	}
	if !statOK(filepath.Join(idir, "rootfs.ext4")) {
		t.Error("rm must KEEP rootfs.ext4 (the home data)")
	}
	for _, gone := range []string{"seed.ext4", "run.json", "repo1.ext4", "snap1.ext4", "state.json"} {
		if statOK(filepath.Join(idir, gone)) {
			t.Errorf("rm should have removed %s", gone)
		}
	}
}

// TestFirecrackerDestroyRemovesRootfs: `destroy` (purge=true) removes everything,
// home disk included.
func TestFirecrackerDestroyRemovesRootfs(t *testing.T) {
	dir := t.TempDir()
	idir := filepath.Join(dir, "box")
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idir, "rootfs.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(dir, &state.Instance{Name: "box", Type: "agent", Engine: state.Firecracker}); err != nil {
		t.Fatal(err)
	}

	if err := fcRemoveDeps(t, dir).Remove(context.Background(), "box", true); err != nil {
		t.Fatal(err)
	}
	if statOK(idir) {
		t.Error("destroy should remove the whole instance dir, rootfs included")
	}
}
