package assets

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// envName matches a shell/Containerfile variable name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

// TestGuestEnvIsDeclaredInOnePlace guards the mechanism that gives a sandbox its
// environment, because an OCI ENV reaches neither place the guest runs code:
// sshd resets the environment for every session, and a microVM never had it —
// the rootfs is a `podman export`, which carries files but not image config.
//
// So rootfs/etc/cs-sandbox/env is the declaration, .bashrc replays it for
// shells, and the microVM's PID 1 replays it for the process tree. Adding a
// variable there must be the whole change. Re-declaring them by hand is what let
// CHROME_BIN and the DISABLE_* pair drift out of sync in the first place.
func TestGuestEnvIsDeclaredInOnePlace(t *testing.T) {
	dir, cleanup, err := ImageDir("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	const decl = "/etc/cs-sandbox/env"
	for _, w := range []struct{ file, why string }{
		{"Containerfile", "the build must install the declaration"},
		{"rootfs/home/.bashrc", "shells must replay it — sshd resets the environment"},
		{"guest/init", "a microVM inherits no OCI ENV, so PID 1 must replay it"},
	} {
		if !strings.Contains(read(w.file), decl) {
			t.Errorf("%s does not reference %s — %s", w.file, decl, w.why)
		}
	}

	declared := map[string]bool{}
	for _, line := range strings.Split(read("rootfs/etc/cs-sandbox/env"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, ok := strings.Cut(line, "="); ok && envName.MatchString(name) {
			declared[name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("the declaration parsed as empty — the parser has drifted, not the image")
	}

	// A Containerfile ENV is legitimate when the BUILD needs it. It is a bug when
	// it exists for the guest's benefit, because it will not reach one.
	buildOnly := map[string]string{
		"PATH":                "composed by .bashrc; the image's copy would fight that",
		"PYENV_ROOT":          "used by build RUN steps; .bashrc sets it and sources pyenv",
		"NVM_DIR":             "used by build RUN steps; .bashrc sets it and sources nvm",
		"JAVA_HOME":           "used by build RUN steps; .bashrc sets it",
		"CS_SANDBOX_SSH_PORT": "read by the container entrypoint, never needed in a shell",
	}
	lines := strings.Split(read("Containerfile"), "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "ENV ") {
			continue
		}
		stmt := strings.TrimSpace(lines[i])
		for strings.HasSuffix(stmt, "\\") && i+1 < len(lines) {
			i++
			stmt = strings.TrimSuffix(stmt, "\\") + " " + strings.TrimSpace(lines[i])
		}
		for _, tok := range strings.Fields(strings.TrimPrefix(stmt, "ENV ")) {
			name, _, ok := strings.Cut(tok, "=")
			if !ok || !envName.MatchString(name) {
				continue
			}
			if _, exempt := buildOnly[name]; exempt || declared[name] {
				continue
			}
			t.Errorf("Containerfile sets ENV %s, which reaches no SSH session and no microVM. "+
				"Declare it in rootfs/etc/cs-sandbox/env, or record why the build needs it.", name)
		}
	}
}
