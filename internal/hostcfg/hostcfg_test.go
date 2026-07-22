package hostcfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func TestSSHCommandString(t *testing.T) {
	h := hostenv.Host{User: "dev", Home: "/home/dev"}
	got := SSHCommandString(h, "/tier", "feature", 2201)
	for _, want := range []string{
		"ssh", "-i /tier/id_cs-sandbox_user", "-p 2201",
		"HostKeyAlias=feature", "IdentitiesOnly=yes", "StrictHostKeyChecking=accept-new",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SSHCommandString missing %q:\n%s", want, got)
		}
	}
}

func TestSSHCommandStringQuotesShellMetacharacters(t *testing.T) {
	h := hostenv.Host{User: "dev", Home: "/home/dev"}
	got := SSHCommandString(h, "/tmp/user's keys", "feature", 2201)
	want := `'/tmp/user'"'"'s keys/id_cs-sandbox_user'`
	if !strings.Contains(got, want) {
		t.Errorf("SSHCommandString = %q, want safely quoted path %q", got, want)
	}
}

func TestSyncSSHConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := hostenv.Host{User: "dev", Home: home}
	insts := []*state.Instance{
		{Name: "a", Port: 2200},
		{Name: "b", Port: 2301},
		{Name: "skip", Port: 0}, // no port -> skipped
	}
	if err := SyncSSHConfig(h, "/tier", insts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	for _, want := range []string{
		"Host a\n", "Port 2200", "HostKeyAlias a",
		"Host b\n", "Port 2301",
		"IdentityFile /tier/id_cs-sandbox_user",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "Host skip") {
		t.Errorf("port-less instance should be skipped:\n%s", cfg)
	}

	// The Include directive is prepended to ~/.ssh/config.
	main, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if !strings.Contains(string(main), "Include ~/.ssh/config.d/cs-sandbox") {
		t.Errorf("~/.ssh/config missing Include:\n%s", main)
	}

	// Idempotent: a second sync doesn't duplicate the Include.
	if err := SyncSSHConfig(h, "/tier", insts); err != nil {
		t.Fatal(err)
	}
	main2, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if strings.Count(string(main2), "Include ~/.ssh/config.d/cs-sandbox") != 1 {
		t.Errorf("Include duplicated on re-sync:\n%s", main2)
	}
}

// On macOS the tier keys live under "~/Library/Application Support/…". An
// unquoted space there makes ssh reject the entire config file ("keyword
// identityfile extra arguments at end of line"), so every sandbox becomes
// unreachable — and so does every other host in the user's config.
func TestSyncSSHConfigQuotesSpacedPaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Library", "Application Support")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := hostenv.Host{User: "dev", Home: home}
	tierDir := filepath.Join(home, "cs-sandbox", "keys")
	if err := SyncSSHConfig(h, tierDir, []*state.Instance{{Name: "a", Port: 2200}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	for _, want := range []string{
		`IdentityFile "` + filepath.Join(tierDir, "id_cs-sandbox_user") + `"`,
		`UserKnownHostsFile "` + KnownHostsFile(h) + `"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("spaced path must be quoted, want %q:\n%s", want, cfg)
		}
	}
	assertSSHAccepts(t, h.SSHConfigFile(), "a")
}

func TestSyncSSHConfigPreservesSymlinkedMainConfig(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	dotfiles := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "ssh-config")
	if err := os.WriteFile(target, []byte("Host example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sshDir, "config")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	h := hostenv.Host{User: "dev", Home: home}
	if err := SyncSSHConfig(h, "/tier", []*state.Instance{{Name: "a", Port: 2200}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("main config symlink was replaced: mode=%v", fi.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Include ~/.ssh/config.d/cs-sandbox") ||
		!strings.Contains(string(data), "Host example") {
		t.Errorf("symlink target was not updated correctly:\n%s", data)
	}
}

// assertSSHAccepts parses the generated file with the real ssh client, which is
// the only authority on whether the quoting is right.
func assertSSHAccepts(t *testing.T, path, host string) {
	t.Helper()
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh client to validate against")
	}
	// -G resolves the config and exits without connecting.
	out, err := exec.Command(ssh, "-F", path, "-G", host).CombinedOutput()
	if err != nil {
		t.Errorf("ssh rejected the generated config: %v\n%s", err, out)
	}
}
