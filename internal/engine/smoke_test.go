//go:build integration || smoke

// The engine half of the smoke profile — see internal/cli/smoke_test.go for
// what the profile is and the two rules that keep it honest.
//
// Both tests here are the front of TestPodmanCreateLive, stopped before the
// part that needs a built sandbox image: the group's isolated network and the
// tier keys every sandbox is sealed with. Neither is reachable from the unit
// tier, which drives this package with a fake Runner and so never runs podman,
// ssh-keygen, or the host-wide flock at all.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
)

// runID is a per-process random suffix so a namespaced object can't collide
// with a leftover from a crashed prior run that happened to reuse this PID.
// Shared by both profiles, which is why it lives here.
var runID = func() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// uniqName builds a unique, namespaced sandbox name for a live test.
func uniqName(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, os.Getpid(), runID)
}

// requirePodman skips unless podman answers. An empty requiredImage asks only
// for the daemon — which is what makes it usable from the smoke profile, where
// no sandbox image exists.
func requirePodman(t *testing.T, requiredImage string) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := &run.Exec{}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "info"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	if requiredImage == "" {
		return
	}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", requiredImage); err != nil {
		t.Skipf("image %s not built (run: cs-sandbox build): %v", requiredImage, err)
	}
}

// TestSmokeTierKeys covers EnsureTierKeys, which testDeps calls before every
// live create and no unit test can: it shells out to ssh-keygen, takes the
// host-wide flock, and depends on modes surviving the filesystem it lands on —
// three things that differ per platform, and all three faked in the unit tier.
func TestSmokeTierKeys(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH")
	}
	dir := t.TempDir()
	d := Deps{Runner: &run.Exec{}, InstDir: dir, TierDir: filepath.Join(dir, "tier-keys")}
	ctx := context.Background()
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatalf("EnsureTierKeys: %v", err)
	}

	for _, k := range []string{seed.TierUserKey, seed.TierAgentKey} {
		priv := filepath.Join(d.TierDir, k)
		pub := priv + ".pub"
		fi, err := os.Stat(priv)
		if err != nil {
			t.Errorf("%s: private key missing: %v", k, err)
			continue
		}
		// A private key ssh considers world-readable is one ssh refuses to use,
		// so the mode is part of the artifact, not an implementation detail.
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", k, fi.Mode().Perm())
		}
		pfi, err := os.Stat(pub)
		if err != nil {
			t.Errorf("%s: public key missing: %v", k, err)
			continue
		}
		if pfi.Mode().Perm() != 0o644 {
			t.Errorf("%s.pub mode = %o, want 644", k, pfi.Mode().Perm())
		}
		// Generated, not merely present: ssh-keygen reads its own output back.
		if out, err := exec.Command("ssh-keygen", "-l", "-f", pub).CombinedOutput(); err != nil {
			t.Errorf("%s.pub is not a usable public key: %v\n%s", k, err, out)
		}
	}

	// Idempotent, because every create calls it: minting a new identity instead
	// of reusing the recorded one would lock the host out of every sandbox
	// already running.
	before, err := os.ReadFile(filepath.Join(d.TierDir, seed.TierUserKey+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatalf("second EnsureTierKeys: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(d.TierDir, seed.TierUserKey+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("EnsureTierKeys replaced an existing tier key; every live sandbox would stop authenticating")
	}
}

// TestSmokePodmanNetwork covers EnsureNetwork, the other thing testDeps does
// before a live create. It is the profile's only live engine touch, and it
// needs no sandbox image — so it can say what doctor cannot: that rootless
// podman really answers here, and that the isolate option the whole group
// boundary rests on is one THIS podman records rather than merely accepts.
//
// Skips where the engine is genuinely unreachable, which on a macOS CI runner
// it is: containers there live in a podman machine VM, and GitHub's Apple
// Silicon runners have no nested virtualization to run one.
func TestSmokePodmanNetwork(t *testing.T) {
	requirePodman(t, "")
	r := &run.Exec{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Namespaced, so this can never adopt or remove a real group's fabric.
	network := uniqName("cs-sandbox-smoke")
	d := Deps{Runner: r, InstDir: t.TempDir(), Network: network}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), run.Opts{}, "podman", "network", "rm", "-f", network)
	})

	if err := d.EnsureNetwork(ctx); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "exists", network); err != nil {
		t.Fatalf("network %s was reported ready but does not exist: %v", network, err)
	}
	got := strings.TrimSpace(run.Output(ctx, r, "podman", "network", "inspect", network,
		"--format", `{{ index .Options "isolate" }}|{{ index .Labels "cs-sandbox.managed" }}`))
	if got != "true|1" {
		t.Errorf("network isolate|managed = %q, want \"true|1\"", got)
	}
	// Idempotent: start re-runs it, so a second call must adopt the network
	// rather than fail on it.
	if err := d.EnsureNetwork(ctx); err != nil {
		t.Errorf("second EnsureNetwork: %v", err)
	}
}
