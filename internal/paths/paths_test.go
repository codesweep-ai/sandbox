package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv unsets every var that influences resolution so each case starts clean.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CS_SANDBOX_INSTANCES_DIR", "CS_SANDBOX_TIER_DIR", "CS_SANDBOX_FC_CACHE",
		"CS_SANDBOX_HOME", "CS_SANDBOX_ASSETS_DIR",
		"XDG_DATA_HOME", "XDG_CACHE_HOME",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", "/home/tester")
}

func TestXDGDefaults(t *testing.T) {
	clearEnv(t)
	// On Linux CI these resolve under ~/.local/share and ~/.cache; the darwin
	// branch is covered by the OS check in the source. Assert the tail segments.
	if got := Instances(); filepath.Base(got) != "instances" || filepath.Base(filepath.Dir(got)) != "cs-sandbox" {
		t.Errorf("Instances default = %q, want …/cs-sandbox/instances", got)
	}
	if got := TierKeys(); filepath.Base(got) != "keys" || filepath.Base(filepath.Dir(got)) != "cs-sandbox" {
		t.Errorf("TierKeys default = %q, want …/cs-sandbox/keys", got)
	}
	if got := FCCache(); filepath.Base(got) != "cs-sandbox" {
		t.Errorf("FCCache default = %q, want …/cs-sandbox", got)
	}
}

func TestXDGHomeRespected(t *testing.T) {
	clearEnv(t)
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("XDG_CACHE_HOME", "/cache")
	if got, want := Instances(), "/data/cs-sandbox/instances"; got != want {
		t.Errorf("Instances = %q, want %q", got, want)
	}
	if got, want := TierKeys(), "/data/cs-sandbox/keys"; got != want {
		t.Errorf("TierKeys = %q, want %q", got, want)
	}
	if got, want := FCCache(), "/cache/cs-sandbox"; got != want {
		t.Errorf("FCCache = %q, want %q", got, want)
	}
}

func TestCSHomeRelocatesAll(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_SANDBOX_HOME", "/opt/cssb")
	cases := map[string]string{
		Instances(): "/opt/cssb/instances",
		TierKeys():  "/opt/cssb/keys",
		FCCache():   "/opt/cssb/cache",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("with CS_SANDBOX_HOME: got %q, want %q", got, want)
		}
	}
}

func TestExplicitOverridesWin(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_SANDBOX_HOME", "/opt/cssb") // must NOT override the explicit dirs below
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", "/x/inst")
	t.Setenv("CS_SANDBOX_TIER_DIR", "/x/keys")
	t.Setenv("CS_SANDBOX_FC_CACHE", "/x/cache")
	if Instances() != "/x/inst" || TierKeys() != "/x/keys" || FCCache() != "/x/cache" {
		t.Errorf("explicit dir overrides should win over CS_SANDBOX_HOME: %s %s %s", Instances(), TierKeys(), FCCache())
	}
}

func TestAssetDirOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_SANDBOX_ASSETS_DIR", "/src/checkout")
	if got := AssetDir(); got != "/src/checkout" {
		t.Errorf("AssetDir = %q, want /src/checkout", got)
	}
}

// TestSSHConfigFragment: the default root keeps the plain, predictable name;
// every other root gets its own, so roots sharing one ~/.ssh don't overwrite
// each other's Host blocks.
func TestSSHConfigFragment(t *testing.T) {
	clearEnv(t)
	if got := SSHConfigFragment(Instances()); got != "cs-sandbox" {
		t.Errorf("default root fragment = %q, want cs-sandbox", got)
	}

	a := SSHConfigFragment("/tmp/rootA/instances")
	b := SSHConfigFragment("/tmp/rootB/instances")
	for _, got := range []string{a, b} {
		if got == "cs-sandbox" || !strings.HasPrefix(got, "cs-sandbox-") {
			t.Errorf("non-default root fragment = %q, want a distinct cs-sandbox-* name", got)
		}
	}
	if a == b {
		t.Errorf("two roots share fragment %q", a)
	}

	// Two spellings of one root must not yield two fragments.
	if got := SSHConfigFragment("/tmp/rootA/./instances"); got != a {
		t.Errorf("equivalent path spelling gave %q, want %q", got, a)
	}
}

// TestFCNetIsHostGlobal: the fabric working dir must not follow a per-root
// relocation. One host has one fabric; a second root that kept its own copy
// would see an empty hostsdir, decide DNS was down, and start a second dnsmasq
// on an address the first already holds.
func TestFCNetIsHostGlobal(t *testing.T) {
	clearEnv(t)
	want := FCNet()
	if !strings.HasSuffix(want, filepath.Join("cs-sandbox", "net")) {
		t.Errorf("FCNet = %q, want it under the cs-sandbox cache", want)
	}
	for _, env := range []string{"CS_SANDBOX_HOME", "CS_SANDBOX_FC_CACHE"} {
		t.Setenv(env, filepath.Join(t.TempDir(), "elsewhere"))
		if got := FCNet(); got != want {
			t.Errorf("%s moved the fabric dir to %q, want it pinned at %q", env, got, want)
		}
		t.Setenv(env, "")
	}
	// An explicit override is still honoured, for isolated runs.
	t.Setenv("CS_SANDBOX_FC_NET", "/tmp/isolated-net")
	if got := FCNet(); got != "/tmp/isolated-net" {
		t.Errorf("CS_SANDBOX_FC_NET ignored: %q", got)
	}
}
