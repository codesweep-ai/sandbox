package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcdisk"
)

// TestOtherRootfsNote: a build names what the cache holds for other image
// versions, since one is kept per version and nothing else would say so, and
// stays quiet where its own is the only one.
func TestOtherRootfsNote(t *testing.T) {
	c := fcdisk.Cache{Dir: t.TempDir()}
	const img = "ghcr.io/codesweep-ai/sandbox:v2"
	write := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("ext4"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(c.BaseRootfs(img))
	if got := otherRootfsNote(c, img); got != "" {
		t.Errorf("note with only this image's rootfs = %q, want none", got)
	}
	write(c.BaseRootfs("ghcr.io/codesweep-ai/sandbox:v1"))
	write(filepath.Join(c.Dir, "base-rootfs.ext4"))
	got := otherRootfsNote(c, img)
	for _, want := range []string{"2 base rootfs", "cs-sandbox doctor"} {
		if !strings.Contains(got, want) {
			t.Errorf("note = %q, want it to carry %q", got, want)
		}
	}
}

// TestOtherKernelNote: a build names how many guest kernel sets the cache holds
// besides its own, since one is kept per kernel and nothing else would say so,
// and stays quiet where its own is the only one.
func TestOtherKernelNote(t *testing.T) {
	c := fcdisk.Cache{Dir: t.TempDir()}
	own := fcdisk.KernelSet{Dir: filepath.Join(c.Dir, "kernels", "7.2.5-200.fc44.x86_64-fedora-aaaaaaaa")}
	mk := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vmlinux.elf"), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk(own.Dir)
	if got := otherKernelNote(c, own); got != "" {
		t.Errorf("note with only this build's set = %q, want none", got)
	}
	mk(filepath.Join(c.Dir, "kernels", "6.19.10-300.fc44.x86_64-fedora-bbbbbbbb"))
	mk(filepath.Join(c.Dir, "kernels", "7.2.6-200.fc44.x86_64-fedora-cccccccc"))
	got := otherKernelNote(c, own)
	for _, want := range []string{"2 other guest kernel sets", "cs-sandbox doctor"} {
		if !strings.Contains(got, want) {
			t.Errorf("note = %q, want it to carry %q", got, want)
		}
	}
}
