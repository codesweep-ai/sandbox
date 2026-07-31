package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func TestNetworkCreateArgv(t *testing.T) {
	tests := []struct {
		name    string
		network string
		want    []string
	}{
		{"default remains compatible", state.DefaultNetwork, []string{"podman", "network", "create", state.DefaultNetwork}},
		{"custom network is isolated", "campaign-a", []string{"podman", "network", "create", "--opt", "isolate=true", "--label", "cs-sandbox.managed=1", "campaign-a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := networkCreateArgv(tt.network); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("networkCreateArgv(%q) = %v, want %v", tt.network, got, tt.want)
			}
		})
	}
}

func TestReclaimNetworkRemovesUnusedManagedCustomNetwork(t *testing.T) {
	t.Setenv("CS_SANDBOX_FC_NET", filepath.Join(t.TempDir(), "net"))
	r := run.NewFake().OnStdout("podman network inspect campaign-a", "1\n")
	d := Deps{Runner: r, InstDir: t.TempDir(), Network: "campaign-a"}
	d.ReclaimNetwork(context.Background())
	if !r.Contains("podman network rm campaign-a") {
		t.Fatalf("unused managed network was not reclaimed: %v", r.Rendered())
	}
}

func TestReclaimNetworkPreservesUserManagedNetwork(t *testing.T) {
	t.Setenv("CS_SANDBOX_FC_NET", filepath.Join(t.TempDir(), "net"))
	r := run.NewFake()
	d := Deps{Runner: r, InstDir: t.TempDir(), Network: "campaign-a"}
	d.ReclaimNetwork(context.Background())
	if r.Contains("podman network rm campaign-a") {
		t.Fatalf("user-managed network must not be reclaimed: %v", r.Rendered())
	}
}

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

func TestEnsureNetworkRefusesUnmanagedExistingCustomNetwork(t *testing.T) {
	// "podman network exists" succeeds (fake default), and inspect shows a
	// network without isolate=true or the managed label.
	r := run.NewFake().OnStdout("podman network inspect campaign-a", "<no value>|\n")
	d := Deps{Runner: r, Network: "campaign-a"}
	err := d.EnsureNetwork(context.Background())
	if err == nil || !r.Contains("podman network inspect campaign-a") {
		t.Fatalf("unverified pre-existing custom network must be refused; err=%v calls=%v", err, r.Rendered())
	}
	if r.Contains("podman network create") {
		t.Fatalf("must not attempt to create over an existing network: %v", r.Rendered())
	}
}

func TestEnsureNetworkAcceptsManagedExistingCustomNetwork(t *testing.T) {
	r := run.NewFake().OnStdout("podman network inspect campaign-a", "true|1\n")
	d := Deps{Runner: r, Network: "campaign-a"}
	if err := d.EnsureNetwork(context.Background()); err != nil {
		t.Fatalf("managed isolated network must be accepted: %v", err)
	}
}

func TestEnsureNetworkDefaultSkipsIsolationVerification(t *testing.T) {
	r := run.NewFake()
	d := Deps{Runner: r, Network: state.DefaultNetwork}
	if err := d.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Contains("podman network inspect") {
		t.Fatalf("default network must keep historical behavior without inspection: %v", r.Rendered())
	}
}
