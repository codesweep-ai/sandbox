package fcdisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// slotBuild is a BuildConfig for image over cache dir, with a guest init to
// hash, and the stamp content ensureBaseRootfs computes for it when podman
// reports imgid.
func slotBuild(t *testing.T, image, imgid string) (BuildConfig, string) {
	t.Helper()
	bc := BuildConfig{Image: image, InitPath: filepath.Join(t.TempDir(), "init"), Kernel: "fedora"}
	writeFile(t, bc.InitPath)
	data, err := os.ReadFile(bc.InitPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return bc, baseRootfsStamp(imgid, "", "fedora", hex.EncodeToString(sum[:])[:12], 0)
}

// TestTwoTagsOfOneRepositoryKeepTheirOwnRootfs is SBX-119. Two cs-sandbox
// builds pinned to two tags of one image shared one base rootfs, and the second
// one's create rebuilt it in place under the first: every sandbox the first
// made after that booted the other image. Each now has a file and a stamp of its
// own, and the second's create leaves the first's exactly as it found them.
func TestTwoTagsOfOneRepositoryKeepTheirOwnRootfs(t *testing.T) {
	c := Cache{Dir: t.TempDir()}
	const older, newer = "ghcr.io/codesweep-ai/sandbox:v0.0.0-20261025044344-a0df4c558d44",
		"ghcr.io/codesweep-ai/sandbox:v0.0.0-20260915174144-601626e43f70"

	first, _ := slotBuild(t, older, "sha256:aaaa")
	if err := c.ensureBaseRootfs(context.Background(), run.NewFake().OnStdout("image inspect", "sha256:aaaa"), first); err != nil {
		t.Fatalf("first create = %v, want nil", err)
	}
	// The fake runner writes nothing, so put down the disk the build would have.
	writeExt4(t, c.BaseRootfs(older))
	firstStamp := c.readStamp(baseRootfsStampName(older))
	if !strings.HasPrefix(firstStamp, "sha256:aaaa|") {
		t.Fatalf("first stamp = %q, want it to name its own image", firstStamp)
	}

	second, _ := slotBuild(t, newer, "sha256:bbbb")
	r := run.NewFake().OnStdout("image inspect", "sha256:bbbb")
	if err := c.ensureBaseRootfs(context.Background(), r, second); err != nil {
		t.Fatalf("second create = %v, want nil", err)
	}

	if c.BaseRootfs(older) == c.BaseRootfs(newer) {
		t.Fatalf("two tags of one repository share %s", c.BaseRootfs(older))
	}
	if baseRootfsStampName(older) == baseRootfsStampName(newer) {
		t.Fatalf("two tags of one repository share the stamp %s", baseRootfsStampName(older))
	}
	if got := c.readStamp(baseRootfsStampName(older)); got != firstStamp {
		t.Errorf("the first image's stamp moved from %q to %q under the second's create", firstStamp, got)
	}
	if !isExt4(c.BaseRootfs(older)) {
		t.Error("the second create removed or truncated the first image's rootfs")
	}
	if r.Contains(c.BaseRootfs(older)) {
		t.Errorf("the second create ran a command on the first image's rootfs: %s", r)
	}
	if got := c.readStamp(baseRootfsStampName(newer)); !strings.HasPrefix(got, "sha256:bbbb|") {
		t.Errorf("second stamp = %q, want it to name its own image", got)
	}
}

// TestEnsureBaseRootfsAdoptsTheRepositorySlot: a cache from before the
// reference keying holds one rootfs per repository. Where its stamp says it was
// built from the image being asked for, it moves to that image's own name rather
// than being rebuilt, which costs minutes and gigabytes for bytes already right.
// One whose stamp names another image is left for the older builds that read it.
func TestEnsureBaseRootfsAdoptsTheRepositorySlot(t *testing.T) {
	const img = "ghcr.io/codesweep-ai/sandbox:v0.1.0"
	// Spelled out rather than asked of the code, so a change to either key
	// fails here instead of moving with it.
	const shared, sharedStamp = "base-rootfs-ghcr.io-codesweep-ai-sandbox.ext4", "base-rootfs-ghcr.io-codesweep-ai-sandbox.stamp"
	const own = "base-rootfs-ghcr.io-codesweep-ai-sandbox-v0.1.0.ext4"

	t.Run("this image's", func(t *testing.T) {
		c := Cache{Dir: t.TempDir()}
		bc, cur := slotBuild(t, img, "sha256:deadbeef")
		writeExt4(t, filepath.Join(c.Dir, shared))
		if err := c.writeStamp(sharedStamp, cur); err != nil {
			t.Fatal(err)
		}
		r := run.NewFake().OnStdout("image inspect", "sha256:deadbeef")
		if err := c.ensureBaseRootfs(context.Background(), r, bc); err != nil {
			t.Fatalf("ensureBaseRootfs = %v, want nil", err)
		}
		if !isExt4(filepath.Join(c.Dir, own)) {
			t.Errorf("no rootfs at %s after adoption", own)
		}
		if exists(filepath.Join(c.Dir, shared)) {
			t.Errorf("%s is still there; it was copied or ignored rather than renamed", shared)
		}
		if got := c.readStamp(strings.TrimSuffix(own, ".ext4") + ".stamp"); !strings.HasPrefix(got, cur) {
			t.Errorf("adopted stamp = %q, want it to carry %q", got, cur)
		}
		for _, marker := range []string{"podman unshare", "podman export", "mke2fs"} {
			if r.Contains(marker) {
				t.Errorf("rebuilt the rootfs (%s) instead of adopting the one already there", marker)
			}
		}
	})

	t.Run("another image's", func(t *testing.T) {
		c := Cache{Dir: t.TempDir()}
		bc, _ := slotBuild(t, img, "sha256:deadbeef")
		_, theirs := slotBuild(t, "ghcr.io/codesweep-ai/sandbox:v0.0.9", "sha256:0ld")
		writeExt4(t, filepath.Join(c.Dir, shared))
		if err := c.writeStamp(sharedStamp, theirs); err != nil {
			t.Fatal(err)
		}
		r := run.NewFake().OnStdout("image inspect", "sha256:deadbeef")
		if err := c.ensureBaseRootfs(context.Background(), r, bc); err != nil {
			t.Fatalf("ensureBaseRootfs = %v, want nil", err)
		}
		if !isExt4(filepath.Join(c.Dir, shared)) || c.readStamp(sharedStamp) != theirs {
			t.Error("another image's rootfs was moved, rebuilt or restamped")
		}
		if !r.Contains("mke2fs") || r.Contains(shared) {
			t.Errorf("want a build of this image's own rootfs that never touches %s; got %s", shared, r)
		}
	})
}

// TestEnsureBaseRootfsRefusesAnotherImagesRootfs: a create that finds its slot
// stamped by another image stops and says so, rather than rebuilding the disk
// in place under whoever made it. The sentence names the owner, the image asked
// for, and the command that replaces it deliberately.
func TestEnsureBaseRootfsRefusesAnotherImagesRootfs(t *testing.T) {
	const img, owner = "ghcr.io/x/y:v2", "ghcr.io/x/y-v2:latest"
	for _, tc := range []struct {
		name, stamp, owner string
	}{
		// Two references that come to one filename.
		{"stamp names a reference", "|" + owner, owner},
		// A stamp written before the reference was recorded names its image id.
		{"stamp names an image id", "", "image sha256:0ther"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Cache{Dir: t.TempDir()}
			bc, _ := slotBuild(t, img, "sha256:mine")
			_, theirs := slotBuild(t, owner, "sha256:0ther")
			rootfs, stamp := c.BaseRootfs(img), baseRootfsStampName(img)
			writeExt4(t, rootfs)
			if err := c.writeStamp(stamp, theirs+tc.stamp); err != nil {
				t.Fatal(err)
			}
			r := run.NewFake().OnStdout("image inspect", "sha256:mine")
			err := c.ensureBaseRootfs(context.Background(), r, bc)
			if err == nil {
				t.Fatal("rebuilt another image's rootfs in place, which is SBX-119")
			}
			for _, want := range []string{rootfs, tc.owner, img, "CS_SANDBOX_IMAGE=" + img + " cs-sandbox build"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not name %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("refusal is more than one line: %q", err)
			}
			if !isExt4(rootfs) || c.readStamp(stamp) != theirs+tc.stamp {
				t.Error("the refusal still changed the other image's rootfs or stamp")
			}
			if r.Contains("mke2fs") || r.Contains("truncate") {
				t.Errorf("refused, but ran the build anyway: %s", r)
			}
		})
	}
}
