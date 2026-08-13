package fcdisk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// sparseImg makes a gb-sized sparse file — apparent size only, no blocks, so a
// test can ask for a "14 GiB disk" without writing 14 GiB.
func sparseImg(t *testing.T, gb int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rootfs.ext4")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(gb << 30); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGrowRootfsGrows: the three steps, in order, against the image — extend the
// file, check the filesystem (resize2fs refuses an unchecked one), extend it.
func TestGrowRootfsGrows(t *testing.T) {
	img := sparseImg(t, 14)
	f := run.NewFake()
	if err := GrowRootfs(context.Background(), f, img, 32); err != nil {
		t.Fatalf("GrowRootfs: %v", err)
	}
	want := []string{"truncate -s 32G " + img, "e2fsck -fp " + img, "resize2fs " + img}
	if len(f.Calls) != len(want) {
		t.Fatalf("ran %d commands, want %d:\n%s", len(f.Calls), len(want), f)
	}
	for i, w := range want {
		if got := strings.Join(f.Calls[i], " "); got != w {
			t.Errorf("call %d = %q, want %q", i, got, w)
		}
	}
}

// TestGrowRootfsNoopWhenLargeEnough: a --disk that matches (or is under) what the
// sandbox already has costs nothing — no fsck, no resize, no shrink. This is the
// common case on the reuse path, where every create would otherwise re-fsck a
// disk it is not changing.
func TestGrowRootfsNoopWhenLargeEnough(t *testing.T) {
	img := sparseImg(t, 32)
	for _, gb := range []int{32, 8} {
		f := run.NewFake()
		if err := GrowRootfs(context.Background(), f, img, gb); err != nil {
			t.Fatalf("GrowRootfs(%d): %v", gb, err)
		}
		if len(f.Calls) != 0 {
			t.Errorf("GrowRootfs(%d) on a 32 GiB disk ran %d commands, want none:\n%s", gb, len(f.Calls), f)
		}
	}
}

// TestGrowRootfsToleratesFsckExit1: e2fsck exits 1 for "errors found and
// corrected", which is a success here — treating it as failure would refuse to
// grow a disk that fsck had just made good.
func TestGrowRootfsToleratesFsckExit1(t *testing.T) {
	img := sparseImg(t, 14)
	f := run.NewFake().On("e2fsck", run.Result{}, &run.ExitError{
		Argv: []string{"e2fsck", "-fp", img}, ExitCode: 1, Stderr: "corrected",
	})
	if err := GrowRootfs(context.Background(), f, img, 32); err != nil {
		t.Fatalf("GrowRootfs with fsck exit 1: %v", err)
	}
	if !f.Contains("resize2fs") {
		t.Errorf("resize2fs did not run after a corrected fsck:\n%s", f)
	}
}

// TestGrowRootfsFailsOnFsckExit2: 2+ means uncorrected damage (or a reboot
// needed). Growing a filesystem in that state is how a disk gets destroyed, so
// it must stop — and say which image and which size it stopped on.
func TestGrowRootfsFailsOnFsckExit2(t *testing.T) {
	img := sparseImg(t, 14)
	f := run.NewFake().On("e2fsck", run.Result{}, &run.ExitError{
		Argv: []string{"e2fsck", "-fp", img}, ExitCode: 2, Stderr: "unfixed",
	})
	err := GrowRootfs(context.Background(), f, img, 32)
	if err == nil {
		t.Fatal("GrowRootfs succeeded on an uncorrected fsck failure")
	}
	if !strings.Contains(err.Error(), img) || !strings.Contains(err.Error(), "32") {
		t.Errorf("error names neither the image nor the size: %v", err)
	}
	if f.Contains("resize2fs") {
		t.Errorf("resize2fs ran after an uncorrected fsck:\n%s", f)
	}
}

// TestGrowRootfsMissingImage: a missing disk is the caller's bug (nothing was
// reflinked), and must not read as "already big enough".
func TestGrowRootfsMissingImage(t *testing.T) {
	f := run.NewFake()
	if err := GrowRootfs(context.Background(), f, filepath.Join(t.TempDir(), "absent.ext4"), 32); err == nil {
		t.Fatal("GrowRootfs on a missing image returned nil")
	}
	if len(f.Calls) != 0 {
		t.Errorf("ran commands against a missing image:\n%s", f)
	}
}

// TestBaseRootfsStampTracksSize: the cached base is judged fresh by this string,
// so a size change has to change it — otherwise raising the default (or
// CS_SANDBOX_FC_ROOTFS_GB) reuses the old, smaller disk forever.
func TestBaseRootfsStampTracksSize(t *testing.T) {
	at14 := baseRootfsStamp("sha256:img", "6.19.10", "fedora", "abc123", 14)
	at32 := baseRootfsStamp("sha256:img", "6.19.10", "fedora", "abc123", 32)
	if at14 == at32 {
		t.Errorf("stamp is identical at 14 and 32 GiB (%q) — a resize would never rebuild", at14)
	}
	if again := baseRootfsStamp("sha256:img", "6.19.10", "fedora", "abc123", 32); again != at32 {
		t.Errorf("stamp not stable for identical inputs: %q vs %q", again, at32)
	}
}
