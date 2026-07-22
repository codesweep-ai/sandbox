package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
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
