package assets

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// With no assetDir (a downloaded binary), everything resolves from the embed.
func TestImageDirFromEmbed(t *testing.T) {
	dir, cleanup, err := ImageDir("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Containerfile + the build context + the guest tree are all present. Each agent's
	// dot-prefixed profile skeleton is listed too: `//go:embed all:image` is what pulls
	// those in, and dropping the `all:` would silently ship an image without them.
	for _, f := range []string{
		"Containerfile", "rootfs/entrypoint", "guest/init", "rootfs/home/.bashrc",
		"rootfs/home/.cs-claude/CLAUDE.md",
		"rootfs/home/.cs-codex/config.toml",
		"rootfs/home/.cs-opencode/opencode.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected extracted %s: %v", f, err)
		}
	}
	// Executables get 0755; data files 0644 (embed.FS drops all to 0444).
	if m := mode(t, dir, "rootfs/entrypoint"); m != 0o755 {
		t.Errorf("entrypoint mode = %o, want 755", m)
	}
	if m := mode(t, dir, "guest/init"); m != 0o755 {
		t.Errorf("guest/init mode = %o, want 755", m)
	}
	for _, w := range []string{"cs-claude", "cs-codex", "cs-opencode"} {
		if m := mode(t, dir, "rootfs/home/.local/bin/"+w); m != 0o755 {
			t.Errorf("home/.local/bin/%s mode = %o, want 755", w, m)
		}
	}
	if m := mode(t, dir, "rootfs/home/.bashrc"); m != 0o644 {
		t.Errorf(".bashrc mode = %o, want 644", m)
	}
	// Fixed mtime for reproducibility.
	fi, _ := os.Stat(filepath.Join(dir, "Containerfile"))
	if !fi.ModTime().Equal(fixedMtime) {
		t.Errorf("Containerfile mtime = %v, want fixed %v", fi.ModTime(), fixedMtime)
	}
}

// Two extractions are byte-identical (same content, modes, mtimes) -> a
// reproducible build context -> a reproducible image id.
func TestImageDirDeterministic(t *testing.T) {
	a, ca, _ := ImageDir("")
	defer ca()
	b, cb, _ := ImageDir("")
	defer cb()
	walkEq(t, a, b)
}

func TestGuestInitPathEmbed(t *testing.T) {
	cache := t.TempDir()
	p := GuestInitPath("", cache)
	if p == "" {
		t.Fatal("GuestInitPath returned empty")
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("guest-init not materialized: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("guest-init mode = %o, want 755", fi.Mode().Perm())
	}
}

func TestHostHelpersEmbed(t *testing.T) {
	f, err := HostHelpers("")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"cs-claude", "cs-codex", "cs-opencode"} {
		if _, err := fs.Stat(f, w); err != nil {
			t.Errorf("expected %s in host helpers: %v", w, err)
		}
	}
}

func mode(t *testing.T, dir, rel string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func walkEq(t *testing.T, a, b string) {
	t.Helper()
	_ = filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(a, p)
		da, _ := os.ReadFile(p)
		db, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil {
			t.Errorf("%s missing in second extract", rel)
			return nil
		}
		if string(da) != string(db) {
			t.Errorf("%s content differs between extracts", rel)
		}
		fa, _ := os.Stat(p)
		fb, _ := os.Stat(filepath.Join(b, rel))
		if fa.Mode() != fb.Mode() || !fa.ModTime().Equal(fb.ModTime()) {
			t.Errorf("%s mode/mtime differs between extracts", rel)
		}
		return nil
	})
}
