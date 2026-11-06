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
