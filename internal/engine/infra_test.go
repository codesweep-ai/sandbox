package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func TestEnsureTierKeysReusesCompleteKeys(t *testing.T) {
	dir := t.TempDir()
	tierDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(tierDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{seed.TierUserKey, seed.TierAgentKey} {
		for _, suffix := range []string{"", ".pub"} {
			if err := os.WriteFile(filepath.Join(tierDir, name+suffix), []byte("key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := run.NewFake()
	d := Deps{Runner: r, InstDir: filepath.Join(dir, "instances"), TierDir: tierDir}
	if err := d.EnsureTierKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Calls) != 0 {
		t.Errorf("complete key pairs should be reused; calls: %v", r.Rendered())
	}
}

func TestEnsureTierKeysRecoversMissingPublicKey(t *testing.T) {
	dir := t.TempDir()
	tierDir := filepath.Join(dir, "keys")
	if err := os.MkdirAll(tierDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{seed.TierUserKey, seed.TierAgentKey} {
		if err := os.WriteFile(filepath.Join(tierDir, name), []byte("private\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := run.NewFake().OnStdout("ssh-keygen -y", "ssh-ed25519 AAAATEST\n")
	d := Deps{Runner: r, InstDir: filepath.Join(dir, "instances"), TierDir: tierDir}
	if err := d.EnsureTierKeys(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{seed.TierUserKey, seed.TierAgentKey} {
		data, err := os.ReadFile(filepath.Join(tierDir, name+".pub"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "ssh-ed25519 AAAATEST\n" {
			t.Errorf("%s public key = %q", name, data)
		}
	}
}

// TestNetworkCreateArgvIsolates: separate bridges and subnets are NOT a
// boundary — netavark forwards between bridges in the same rootless namespace.
// isolate=true is what blocks that, and the managed label is what lets us tell
// our networks from a user's later on. Losing either silently voids isolation.
func TestNetworkCreateArgvIsolates(t *testing.T) {
	argv := networkCreateArgv("cs-sandbox-cache-redis")
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"podman network create", "--opt isolate=true",
		"--label cs-sandbox.managed=1", "cs-sandbox-cache-redis",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("network create argv missing %q: %v", want, argv)
		}
	}
}

// TestEnsureNetworkRefusesUnmanagedNetwork: a pre-existing network of the right
// NAME is not good enough. Adopting a bridge of unknown provenance would leave
// members believing they are isolated when they are not, so verification fails
// closed rather than trusting the name.
func TestEnsureNetworkRefusesUnmanagedNetwork(t *testing.T) {
	for _, tc := range []struct {
		name, inspect string
		wantErr       bool
	}{
		{"ours and isolated", "true|1\n", false},
		{"not isolated", "|1\n", true},
		{"isolated but not ours", "true|\n", true},
		{"inspect says nothing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := run.NewFake() // network exists
			f.On("network inspect", run.Result{Stdout: tc.inspect}, nil)
			err := Deps{Runner: f, Network: "cs-sandbox-cache-redis"}.EnsureNetwork(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("unverified network must be refused (inspect=%q)", tc.inspect)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("managed isolated network should be accepted: %v", err)
			}
		})
	}
}

// TestReclaimNetworkSpareTheDefault: a group owns its network, so removing the
// group removes it. The default fabric is shared host-wide by every root on the
// machine and must never be reclaimed out from under them.
func TestReclaimNetworkSparesTheDefault(t *testing.T) {
	f := run.NewFake()
	Deps{Runner: f, Network: state.NetworkName(state.DefaultGroup)}.ReclaimNetwork(context.Background())
	if f.Contains("network rm") {
		t.Errorf("the default fabric must never be reclaimed: %v", f.Rendered())
	}
	f2 := run.NewFake()
	Deps{Runner: f2, Network: state.NetworkName("cache-redis")}.ReclaimNetwork(context.Background())
	if !f2.Contains("network rm") {
		t.Errorf("a group's own network should be reclaimed: %v", f2.Rendered())
	}
}

// TestHealDefaultFabricOnlyWhenSafe: a default fabric created before group
// isolation has neither the option nor the label, and isolate is a
// creation-time option — so the only way to fix it is to recreate it. That is
// destructive, so it happens only for the default network and only when nothing
// but our own keepalive is attached. Named groups are never healed: an
// unlabelled network of that name may be the user's own.
func TestHealDefaultFabricOnlyWhenSafe(t *testing.T) {
	def := state.NetworkName(state.DefaultGroup)

	t.Run("recreates a bare default fabric", func(t *testing.T) {
		// The fake answers one way per command, so drive the heal directly: it
		// stands in for "verification just failed" and the inspect it runs
		// afterwards sees the recreated, isolated network.
		f := run.NewFake()
		f.On("podman ps", run.Result{Stdout: fcnet.KeepaliveFor(def) + "\n"}, nil)
		f.On("network inspect", run.Result{Stdout: "true|1\n"}, nil)
		err := (Deps{Runner: f, Network: def}).healDefaultFabric(context.Background(), errors.New("not isolated"))
		if err != nil {
			t.Fatalf("a bare default fabric should be healed: %v", err)
		}
		if !f.Contains("network rm") || !f.Contains("isolate=true") {
			t.Errorf("healing must recreate the network isolated: %v", f.Rendered())
		}
		// The fabric must be stopped BEFORE the network goes. Its dnsmasq holds an
		// address on netavark's bridge, and an address left behind keeps the bridge
		// alive past the removal, carrying the old gateway — which then collides
		// with whichever network podman next assigns that subnet, silently killing
		// outbound traffic for its members.
		var teardown, netrm int
		for i, c := range f.Rendered() {
			if strings.Contains(c, "ip addr del") {
				teardown = i + 1
			}
			if strings.Contains(c, "network rm") {
				netrm = i + 1
			}
		}
		if teardown == 0 {
			t.Errorf("healing must release the fabric address, else the bridge outlives the network: %v", f.Rendered())
		} else if netrm != 0 && teardown > netrm {
			t.Errorf("the fabric must be torn down before the network is removed: %v", f.Rendered())
		}
	})

	t.Run("refuses while sandboxes are attached", func(t *testing.T) {
		f := run.NewFake()
		f.On("network inspect", run.Result{Stdout: "|\n"}, nil)
		f.On("podman ps", run.Result{Stdout: fcnet.KeepaliveFor(def) + "\nbox.default\n"}, nil)
		err := (Deps{Runner: f, Network: def}).EnsureNetwork(context.Background())
		if err == nil || !strings.Contains(err.Error(), "box.default") {
			t.Fatalf("must refuse and name the attached sandbox, got %v", err)
		}
		if f.Contains("network rm") {
			t.Errorf("must not remove a network with sandboxes on it: %v", f.Rendered())
		}
	})

	t.Run("never heals a named group's network", func(t *testing.T) {
		f := run.NewFake()
		f.On("network inspect", run.Result{Stdout: "|\n"}, nil)
		err := (Deps{Runner: f, Network: state.NetworkName("cache-redis")}).EnsureNetwork(context.Background())
		if err == nil {
			t.Fatal("an unverified named network must be refused, not healed")
		}
		if f.Contains("network rm") {
			t.Errorf("a named group's network must never be deleted: %v", f.Rendered())
		}
	})
}

// TestEvictSquattingBridge: podman picks a network's interface name by scanning
// its OWN networks, so a bridge that outlived the network it was built for is
// invisible to that choice — and netavark adopts an interface it finds rather
// than re-addressing it. The result is a group whose members hold a gateway that
// does not exist: no DNS, no outbound, and nothing in any log to say why. So the
// squatter is evicted, and netavark builds the bridge properly on first attach.
func TestEvictSquattingBridge(t *testing.T) {
	const iface, gw = "podman3", "10.89.0.1"
	for _, tc := range []struct {
		name      string
		linkErr   error
		addrs     string
		wantEvict bool
	}{
		{
			name:    "no bridge yet — the usual case, netavark will create it",
			linkErr: errors.New(`Device "podman3" does not exist`),
		},
		{
			name:  "already carrying our gateway (a concurrent create won)",
			addrs: "5: podman3    inet 10.89.0.1/24 brd 10.89.0.255 scope global podman3\n",
		},
		{
			name:      "squatter from a network that is gone, still on its old subnet",
			addrs:     "5: podman3    inet 10.89.2.1/24 brd 10.89.2.255 scope global podman3\n",
			wantEvict: true,
		},
		{
			// Just as broken and harder to spot: the members' gateway is absent
			// either way.
			name:      "squatter carrying no address at all",
			wantEvict: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := run.NewFake()
			f.OnStdout("{{.NetworkInterface}}", iface+"\n")
			f.OnStdout("Gateway", gw+"\n")
			f.On("ip link show dev", run.Result{}, tc.linkErr)
			f.OnStdout("-4 addr show", tc.addrs)
			Deps{Runner: f, Network: state.NetworkName("cache-redis")}.evictSquattingBridge(context.Background())
			if got := f.Contains("ip link del " + iface); got != tc.wantEvict {
				t.Errorf("evicted = %v, want %v; calls:\n%s", got, tc.wantEvict, strings.Join(f.Rendered(), "\n"))
			}
		})
	}
}

// TestEnsureNetworkNeverEvictsAnExistingNetwork: eviction is only ever safe on a
// network we just created, where nothing of ours can be attached yet. A network
// that was already there may have running sandboxes on its bridge, and deleting
// it under them would turn a suspicion into an outage.
func TestEnsureNetworkNeverEvictsAnExistingNetwork(t *testing.T) {
	f := run.NewFake() // `network exists` succeeds -> the adopt path
	f.On("network inspect", run.Result{Stdout: "true|1\n"}, nil)
	if err := (Deps{Runner: f, Network: state.NetworkName("cache-redis")}).EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Contains("unshare") {
		t.Errorf("an existing network's bridge must be left alone:\n%s", strings.Join(f.Rendered(), "\n"))
	}
}
