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
	instDir := filepath.Join(t.TempDir(), "instances")
	insts := []*state.Instance{
		{Name: "a", Port: 2200},
		{Name: "b", Port: 2301},
		{Name: "skip", Port: 0}, // no port -> skipped
	}
	if err := SyncSSHConfig(h, "/tier", instDir, insts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile(instDir))
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
	if err := SyncSSHConfig(h, "/tier", instDir, insts); err != nil {
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
	instDir := filepath.Join(t.TempDir(), "instances")
	tierDir := filepath.Join(home, "cs-sandbox", "keys")
	if err := SyncSSHConfig(h, tierDir, instDir, []*state.Instance{{Name: "a", Port: 2200}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile(instDir))
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
	assertSSHAccepts(t, h.SSHConfigFile(instDir), "a")
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
	instDir := filepath.Join(t.TempDir(), "instances")
	if err := SyncSSHConfig(h, "/tier", instDir, []*state.Instance{{Name: "a", Port: 2200}}); err != nil {
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

// TestSyncSSHConfigIsolatesInstancesRoots: ~/.ssh is shared by every instances
// root on the host, so a sync in one root must not erase the Host blocks of
// sandboxes another root owns — otherwise a second sandbox set (or a test run)
// silently breaks `ssh <name>` for the first.
func TestSyncSSHConfigIsolatesInstancesRoots(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := hostenv.Host{User: "dev", Home: home}

	rootA := filepath.Join(t.TempDir(), "rootA")
	if err := SyncSSHConfig(h, "/tier", rootA, []*state.Instance{{Name: "alpha", Port: 2200}}); err != nil {
		t.Fatal(err)
	}
	fragA := h.SSHConfigFile(rootA)

	rootB := filepath.Join(t.TempDir(), "rootB")
	if err := SyncSSHConfig(h, "/tier", rootB, []*state.Instance{{Name: "beta", Port: 2201}}); err != nil {
		t.Fatal(err)
	}
	fragB := h.SSHConfigFile(rootB)

	if fragA == fragB {
		t.Fatalf("both roots wrote the same fragment %q", fragA)
	}
	a, err := os.ReadFile(fragA)
	if err != nil {
		t.Fatalf("rootA's fragment was destroyed by rootB's sync: %v", err)
	}
	if !strings.Contains(string(a), "Host alpha") {
		t.Errorf("rootA's Host block lost:\n%s", a)
	}
	b, _ := os.ReadFile(fragB)
	if !strings.Contains(string(b), "Host beta") {
		t.Errorf("rootB's Host block missing:\n%s", b)
	}
	if strings.Contains(string(b), "Host alpha") {
		t.Errorf("rootB's fragment describes another root's sandbox:\n%s", b)
	}

	// One glob Include covers every root's fragment.
	main, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if strings.Count(string(main), "Include ~/.ssh/config.d/cs-sandbox*") != 1 {
		t.Errorf("want exactly one glob Include:\n%s", main)
	}
}

// TestSyncSSHConfigRemovesEmptyFragment: a root with no sandboxes leaves nothing
// behind, so throwaway roots don't accumulate in ~/.ssh/config.d.
func TestSyncSSHConfigRemovesEmptyFragment(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := hostenv.Host{User: "dev", Home: home}
	instDir := filepath.Join(t.TempDir(), "instances")
	if err := SyncSSHConfig(h, "/tier", instDir, []*state.Instance{{Name: "a", Port: 2200}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.SSHConfigFile(instDir)); err != nil {
		t.Fatalf("fragment should exist while a sandbox does: %v", err)
	}
	if err := SyncSSHConfig(h, "/tier", instDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.SSHConfigFile(instDir)); !os.IsNotExist(err) {
		t.Errorf("fragment should be gone once no sandbox has a port, got err=%v", err)
	}
}

// TestEnsureIncludeUpdatesInPlace: an Include that predates the glob is rewritten
// where it sits, so a sandbox set added later is picked up without the user
// editing ~/.ssh/config or ending up with two directives.
func TestEnsureIncludeUpdatesInPlace(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".ssh", "config")
	if err := os.WriteFile(cfg, []byte("Host work\n  User me\n\nInclude ~/.ssh/config.d/cs-sandbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := hostenv.Host{User: "dev", Home: home}
	if err := SyncSSHConfig(h, "/tier", filepath.Join(t.TempDir(), "instances"),
		[]*state.Instance{{Name: "a", Port: 2200}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	if n := strings.Count(string(got), "Include ~/.ssh/config.d/cs-sandbox"); n != 1 {
		t.Errorf("want exactly one managed Include, got %d:\n%s", n, got)
	}
	if !strings.Contains(string(got), "Include ~/.ssh/config.d/cs-sandbox*") {
		t.Errorf("Include not updated to the glob:\n%s", got)
	}
	if !strings.Contains(string(got), "Host work") {
		t.Errorf("user's own config lost:\n%s", got)
	}
}
