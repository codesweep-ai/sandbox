package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcdisk"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func statOK(p string) bool { _, err := os.Stat(p); return err == nil }

// TestPodmanRmKeepsVolumesRemovesState: `rm` (purge=false) removes the container
// and the instance state, but NOT the data volumes — so `create` reuses them.
func TestPodmanRmKeepsVolumesRemovesState(t *testing.T) {
	dir := t.TempDir()
	idir := state.Dir(dir, state.DefaultGroup, "box")
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
	idir := state.Dir(dir, state.DefaultGroup, "box")
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"rootfs.ext4", fcdisk.RootfsImage, "seed.ext4", "run.json", "repo1.ext4", "snap1.ext4"} {
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
	// The record of the image the disk came from goes with the disk: without it
	// a later create cannot tell a kept disk from another image.
	if !statOK(filepath.Join(idir, fcdisk.RootfsImage)) {
		t.Errorf("rm must KEEP %s beside the disk", fcdisk.RootfsImage)
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
	idir := state.Dir(dir, state.DefaultGroup, "box")
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

// keptDisk lays down what `rm` leaves of a microVM: its rootfs.ext4 and, when
// recorded is not empty, the id of the image that disk was copied from.
func keptDisk(t *testing.T, instDir, recorded string) string {
	t.Helper()
	idir := state.Dir(instDir, state.DefaultGroup, "box")
	if err := os.MkdirAll(idir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idir, "rootfs.ext4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recorded != "" {
		if err := os.WriteFile(filepath.Join(idir, fcdisk.RootfsImage), []byte(recorded+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return idir
}

// TestKeptDiskNamesItsImage: create reusing a kept disk says so, and names the
// image the disk came from. The disk is the whole root filesystem, so a disk
// from another image would run that image's tools; that is refused unless the
// caller asks for it. A disk kept before the image was recorded is booted.
func TestKeptDiskNamesItsImage(t *testing.T) {
	const old, cur = "sha256:0123456789abcdef0000", "sha256:fedcba9876543210ffff"
	for _, tc := range []struct {
		name     string
		recorded string
		reuse    bool
		wantErr  bool
		want     []string // in the error when refused, else in the one note
	}{
		{name: "same image", recorded: cur, want: []string{"reusing the kept disk of box from image fedcba987654"}},
		{name: "another image", recorded: old, wantErr: true,
			want: []string{"0123456789ab", "fedcba987654", "cs-sandbox destroy box.default -f", "--reuse-kept-disk"}},
		{name: "another image, reuse asked for", recorded: old, reuse: true,
			want: []string{"reusing the kept disk of box from image 0123456789ab", "fedcba987654"}},
		{name: "no record", want: []string{"reusing the kept disk of box from image unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			idir := keptDisk(t, dir, tc.recorded)
			var notes []string
			fe := NewFirecracker(Deps{
				Runner: run.NewFake().OnStdout("podman image inspect", cur+" 123"), InstDir: dir, Image: "sandbox:new",
				Note: func(s string) { notes = append(notes, s) },
			})
			err := fe.checkKeptDisk(context.Background(), CreateSpec{Name: "box", ReuseKeptDisk: tc.reuse}, idir)
			got := strings.Join(notes, "\n")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("a kept disk from another image was accepted; notes %q", notes)
				}
				got = err.Error()
			} else if err != nil {
				t.Fatalf("checkKeptDisk = %v, want nil", err)
			}
			if !tc.wantErr && len(notes) != 1 {
				t.Errorf("notes = %q, want exactly one line", notes)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not name %q", got, w)
				}
			}
		})
	}
}

// TestCreateRefusesKeptDiskFromAnotherImage: the refusal comes before anything
// is provisioned, so a refused create claims no address and leaves the kept disk
// and its record as they were. It is checked ahead of the KVM preflight, which
// is what lets it run here.
func TestCreateRefusesKeptDiskFromAnotherImage(t *testing.T) {
	dir := t.TempDir()
	idir := keptDisk(t, dir, "sha256:0123456789abcdef")
	fe := NewFirecracker(Deps{
		Runner:  run.NewFake().OnStdout("podman image inspect", "sha256:fedcba9876543210 123"),
		InstDir: dir, FCCache: t.TempDir(), Image: "sandbox:new",
	})
	_, err := fe.Create(context.Background(), CreateSpec{Name: "box", Type: "agent"})
	if err == nil || !strings.Contains(err.Error(), "--reuse-kept-disk") {
		t.Fatalf("Create over a kept disk from another image = %v, want the refusal", err)
	}
	if _, lerr := state.Load(dir, state.DefaultGroup, "box"); lerr == nil {
		t.Error("a refused create saved an instance record")
	}
	for _, f := range []string{"rootfs.ext4", fcdisk.RootfsImage} {
		if !statOK(filepath.Join(idir, f)) {
			t.Errorf("a refused create removed %s", f)
		}
	}
}
