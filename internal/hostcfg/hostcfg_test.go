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
	got := SSHCommandString(h, "/tier", "feature", Route{Port: 2201})
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
	got := SSHCommandString(h, "/tmp/user's keys", "feature", Route{Port: 2201})
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
		{Name: "a", Port: 2200},                                // published: dialled by port
		{Name: "b", Engine: state.Podman},                      // the default: reached through the engine
		{Name: "c", Engine: state.Firecracker, Group: "cache"}, // and a microVM, over its socket
	}
	if err := SyncSSHConfig(h, "/tier", instDir, insts, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile(instDir))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	for _, want := range []string{
		// Every sandbox gets its qualified alias; the bare one rides along
		// because these are default-group members.
		"Host a.default a\n", "HostName 127.0.0.1", "Port 2200", "HostKeyAlias a.default",
		// No port, so no host to dial: ssh gets its stream from the engine.
		"Host b.default b\n", "ProxyCommand podman exec -i b.default socat - TCP:127.0.0.1:22",
		// A microVM has no container to enter. It answers on the socket its
		// forwarder already listens on, in its own instance directory.
		"Host c.cache\n", "ProxyCommand socat - UNIX-CONNECT:" + filepath.Join(instDir, "cache", "c", "fwd.sock"),
		// Trust material is per group, so the identity path names the group.
		"IdentityFile /tier/groups/default/id_cs-sandbox_user",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
	// One or the other, never both: a block that carried a Port and a
	// ProxyCommand would dial the port and ignore the command.
	for _, block := range strings.Split(cfg, "\nHost ")[1:] {
		if strings.Contains(block, "ProxyCommand") && strings.Contains(block, "\n    Port ") {
			t.Errorf("a block carries both a port and a proxy:\n%s", block)
		}
	}

	// The Include directive is prepended to ~/.ssh/config.
	main, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if !strings.Contains(string(main), "Include ~/.ssh/config.d/cs-sandbox") {
		t.Errorf("~/.ssh/config missing Include:\n%s", main)
	}

	// Idempotent: a second sync doesn't duplicate the Include.
	if err := SyncSSHConfig(h, "/tier", instDir, insts, nil); err != nil {
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
	if err := SyncSSHConfig(h, tierDir, instDir, []*state.Instance{{Name: "a", Port: 2200}}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile(instDir))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	for _, want := range []string{
		`IdentityFile "` + filepath.Join(GroupKeysDir(tierDir, state.DefaultGroup), "id_cs-sandbox_user") + `"`,
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
	if err := SyncSSHConfig(h, "/tier", instDir, []*state.Instance{{Name: "a", Port: 2200}}, nil); err != nil {
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
	if err := SyncSSHConfig(h, "/tier", rootA, []*state.Instance{{Name: "alpha", Port: 2200}}, nil); err != nil {
		t.Fatal(err)
	}
	fragA := h.SSHConfigFile(rootA)

	rootB := filepath.Join(t.TempDir(), "rootB")
	if err := SyncSSHConfig(h, "/tier", rootB, []*state.Instance{{Name: "beta", Port: 2201}}, nil); err != nil {
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
	if err := SyncSSHConfig(h, "/tier", instDir, []*state.Instance{{Name: "a", Port: 2200}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.SSHConfigFile(instDir)); err != nil {
		t.Fatalf("fragment should exist while a sandbox does: %v", err)
	}
	if err := SyncSSHConfig(h, "/tier", instDir, nil, nil); err != nil {
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
		[]*state.Instance{{Name: "a", Port: 2200}}, nil); err != nil {
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

// TestSyncSSHConfigGroups covers what the group model changed in the generated
// config: identity is (group, name), so the qualified alias is always present,
// the bare one belongs to the default group alone, each sandbox points at its
// OWN group's key, and each group gets a gateway block.
func TestSyncSSHConfigGroups(t *testing.T) {
	home := t.TempDir()
	h := hostenv.Host{Home: home, User: "dev"}
	instDir := filepath.Join(t.TempDir(), "instances")
	insts := []*state.Instance{
		{Name: "worker", Group: "cache-redis", Port: 2200},
		{Name: "worker", Group: "cache-memory", Port: 2201}, // same name, other group
		{Name: "only", Group: "cache-redis", Port: 2202},    // unique, but still not default
		{Name: "plain", Group: "default", Port: 2203},       // default group: bare alias
	}
	groups := []*state.Group{{Name: "cache-redis", GWPort: 2400}, {Name: "cache-memory"}}
	if err := SyncSSHConfig(h, "/tier", instDir, insts, groups); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.SSHConfigFile(instDir))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)

	for _, want := range []string{
		"Host worker.cache-redis\n", // qualified only: not the default group
		"Host worker.cache-memory\n",
		"Host only.cache-redis\n",    // unique host-wide, but still qualified only
		"Host plain.default plain\n", // default group: the bare alias rides along
		// Each sandbox authenticates with its own group's key.
		"IdentityFile /tier/groups/cache-redis/id_cs-sandbox_user",
		"IdentityFile /tier/groups/cache-memory/id_cs-sandbox_user",
		// Every group gets a gateway alias. A published port is dialled where
		// one was asked for; otherwise the engine's own channel carries it, so
		// the alias works with nothing bound on the host.
		"Host cache-redis-gw\n",
		"Port 2400",
		"Host cache-memory-gw\n",
		"ProxyCommand podman exec -i cs-sandbox-cache-memory-keepalive socat - TCP:127.0.0.1:22",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
	// No bare alias for a non-default group, whether or not the name collides:
	// ssh takes the first match for a keyword, and a bare name that depended on
	// what else existed on the host is exactly the brittleness this removes.
	for _, bad := range []string{"Host worker\n", "Host worker ", "Host only\n", " only\n"} {
		if strings.Contains(cfg, bad) {
			t.Errorf("bare alias %q must not be emitted for a non-default group:\n%s", bad, cfg)
		}
	}
	// The unpublished gateway dials nothing: a Port line there would send ssh
	// to a host port that was never bound.
	at := strings.Index(cfg, "Host cache-memory-gw")
	if at < 0 {
		t.Fatalf("no gateway block for the unpublished group:\n%s", cfg)
	}
	unpub := cfg[at:]
	if i := strings.Index(unpub, "\nHost "); i > 0 {
		unpub = unpub[:i]
	}
	if strings.Contains(unpub, "Port ") || strings.Contains(unpub, "HostName ") {
		t.Errorf("an unpublished gateway should carry no host or port:\n%s", unpub)
	}
	// The gateway authorizes only its group's key; offering the host's other
	// identities first would exhaust sshd's MaxAuthTries before it was tried.
	// Locate the block before slicing: a missing alias is a real failure, and
	// slicing on Index's -1 would report it as an opaque bounds panic.
	start := strings.Index(cfg, "Host cache-redis-gw")
	if start < 0 {
		t.Fatalf("no gateway alias for cache-redis:\n%s", cfg)
	}
	gw := cfg[start:]
	if end := strings.Index(gw[1:], "\nHost "); end >= 0 {
		gw = gw[:end]
	}
	if strings.Count(gw, "IdentityFile") != 1 {
		t.Errorf("gateway block should offer exactly one identity:\n%s", gw)
	}
}
