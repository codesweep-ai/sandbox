package fcdisk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
)

// TestBuildSeedExt4CarriesEveryAgentCredential: the microVM seed disk is the
// only route an inherited login takes into a VM, and it copies one tree per
// agent. A hardcoded {"claude", "codex"} here silently dropped opencode's, so
// `--inherit-agent-login opencode --engine firecracker` reported success and
// produced a sandbox with no login — invisible because the podman path built
// its seed elsewhere and was fine.
//
// Asserting against seed.AgentNames() rather than naming the agents is the
// point: a fourth agent must not need anyone to remember this file.
func TestBuildSeedExt4CarriesEveryAgentCredential(t *testing.T) {
	dir := t.TempDir()
	seedDir := filepath.Join(dir, "seed")
	agents := seed.AgentNames()
	if len(agents) < 2 {
		t.Fatalf("expected several agents, got %v", agents)
	}
	for _, a := range agents {
		if err := os.MkdirAll(filepath.Join(seedDir, a), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := run.NewFake()
	in := SeedInput{SeedDir: seedDir, FCSeed: filepath.Join(dir, "fc-seed"), User: "dev", UID: 1000, GID: 1000}

	c := Cache{Dir: dir}
	if err := c.BuildSeedExt4(context.Background(), fake, in, filepath.Join(dir, "seed.ext4")); err != nil {
		t.Fatal(err)
	}

	rendered := strings.Join(fake.Rendered(), "\n")
	for _, a := range agents {
		want := "cp -a " + filepath.Join(seedDir, a)
		if !strings.Contains(rendered, want) {
			t.Errorf("agent %q's credential tree never reached the seed disk (no %q)\n%s", a, want, rendered)
		}
	}
}
