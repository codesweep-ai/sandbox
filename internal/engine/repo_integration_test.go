//go:build integration || smoke

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/repo"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/spec"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// TestPodmanRepoFetchLive proves the core agent workflow end-to-end: share a
// repo with --repo, let the sandbox commit on its cs-sandbox/<name> branch, and
// fetch those commits back to the host (the repo path).
func TestPodmanRepoFetchLive(t *testing.T) {
	ctx := context.Background()
	d := testDeps(t)
	if err := d.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatal(err)
	}

	// A host source repo with an identity (carried into the sandbox clone).
	src := filepath.Join(t.TempDir(), "proj")
	gitInit(t, d, src)

	p := NewPodman(d)
	name := uniqName("csgorepo")
	t.Cleanup(func() { _ = p.Remove(context.Background(), name, true) })

	inst, err := p.Create(ctx, CreateSpec{
		Name: name, Type: "agent",
		RepoClones: []spec.RepoClone{{HostPath: src, Name: "proj"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Wait for the first-boot alternates clone to appear.
	waitSSH(t, d, inst.Port, "test -d ~/proj/.git", 90*time.Second)

	// The sandbox is on its own branch; make a commit there.
	branch := sshOut(ctx, d, inst.Port, "git -C ~/proj rev-parse --abbrev-ref HEAD")
	if branch != "cs-sandbox/"+name {
		t.Fatalf("sandbox checkout on branch %q, want cs-sandbox/%s", branch, name)
	}
	if _, err := sshRun(ctx, d, inst.Port, `git -C ~/proj commit --allow-empty -m "from sandbox agent"`); err != nil {
		t.Fatalf("sandbox commit: %v", err)
	}

	// Fetch it back to the host.
	tr := repo.Transport{Host: d.Host, TierDir: d.TierDir, Name: name, Port: inst.Port}
	rc := state.RepoClone{Source: src, Dir: "proj", Branch: "cs-sandbox/" + name}
	tip, err := repo.Fetch(ctx, d.Runner, tr, rc)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(tip, "from sandbox agent") {
		t.Fatalf("fetched tip = %q, want the sandbox commit", tip)
	}

	// The host repo now has the branch with that commit.
	got := run.Output(ctx, d.Runner, "git", "-C", src, "log", "--oneline", "cs-sandbox/"+name)
	if !strings.Contains(got, "from sandbox agent") {
		t.Fatalf("host branch missing sandbox commit:\n%s", got)
	}
}

func gitInit(t *testing.T, d Deps, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{
		{"git", "-C", dir, "init", "-b", "main"},
		{"git", "-C", dir, "config", "user.name", "Repo Tester"},
		{"git", "-C", dir, "config", "user.email", "repo@test.local"},
		{"git", "-C", dir, "commit", "--allow-empty", "-m", "initial"},
	}
	for _, s := range steps {
		if _, err := d.Runner.Run(context.Background(), run.Opts{}, s...); err != nil {
			t.Fatalf("git init step %v: %v", s, err)
		}
	}
}

func sshBase(d Deps, port int) []string {
	key := filepath.Join(d.TierDir, "id_cs-sandbox_user")
	return []string{"ssh",
		"-i", key, "-p", fmt.Sprintf("%d", port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@127.0.0.1", d.Host.User),
	}
}

func sshRun(ctx context.Context, d Deps, port int, cmd string) (string, error) {
	res, err := d.Runner.Run(ctx, run.Opts{}, append(sshBase(d, port), cmd)...)
	return res.Stdout, err
}

func sshOut(ctx context.Context, d Deps, port int, cmd string) string {
	out, _ := sshRun(ctx, d, port, cmd)
	return strings.TrimSpace(out)
}

func waitSSH(t *testing.T, d Deps, port int, cmd string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := sshRun(context.Background(), d, port, cmd); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("condition %q not met within %s", cmd, budget)
}
