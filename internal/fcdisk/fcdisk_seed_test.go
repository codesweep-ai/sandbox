package fcdisk

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// stagingSpy snapshots the fc-seed staging dir on every runner call, because
// BuildSeedExt4 packs it into the image and then deletes it — by the time the
// call returns there is nothing left on disk to assert against.
type stagingSpy struct {
	*run.Fake
	dir  string
	seen map[string]string
}

func (s *stagingSpy) Run(ctx context.Context, opts run.Opts, argv ...string) (run.Result, error) {
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(s.dir, e.Name())); err == nil {
			s.seen[e.Name()] = string(b)
		}
	}
	return s.Fake.Run(ctx, opts, argv...)
}

// TestBuildSeedExt4CarriesHostConfig: the non-secret host config the podman
// entrypoint reads straight out of <idir>/seed reaches a microVM only if it is
// on this copy list. Nothing fails loudly when one is missing — the VM just
// comes up with the wrong git identity, or on the theme picker.
func TestBuildSeedExt4CarriesHostConfig(t *testing.T) {
	dir := t.TempDir()
	seedDir := filepath.Join(dir, "seed")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"git_identity": "name\tAda\nemail\tada@example.com\n",
		"claude_theme": "light\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(seedDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fcSeed := filepath.Join(dir, "fc-seed")
	spy := &stagingSpy{Fake: run.NewFake(), dir: fcSeed, seen: map[string]string{}}
	in := SeedInput{SeedDir: seedDir, FCSeed: fcSeed, User: "dev", UID: 1000, GID: 1000}

	c := Cache{Dir: dir}
	if err := c.BuildSeedExt4(context.Background(), spy, in, filepath.Join(dir, "seed.ext4")); err != nil {
		t.Fatal(err)
	}
	if len(spy.seen) == 0 {
		t.Fatal("nothing was ever staged, so this test proves nothing")
	}
	for name, body := range files {
		got, ok := spy.seen[name]
		if !ok {
			t.Errorf("%s never reached the seed disk; staged: %v", name, slices.Sorted(maps.Keys(spy.seen)))
			continue
		}
		if got != body {
			t.Errorf("%s = %q, want %q", name, got, body)
		}
	}
}
