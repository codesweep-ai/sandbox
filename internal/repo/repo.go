// Package repo implements the host-initiated --repo fetch/push transport. Both
// directions are fast-forward-only over the sandbox's published SSH port, using
// a self-contained core.sshCommand (the U tier key + HostKeyAlias) so git works
// without depending on the user's ssh config. Engine-agnostic.
package repo

import (
	"context"
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/hostcfg"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Transport carries the host-side connection details shared by fetch/push.
type Transport struct {
	Host    hostenv.Host
	TierDir string
	Name    string
	Port    int
}

func (t Transport) sshCmd() string {
	return hostcfg.SSHCommandString(t.Host, t.TierDir, t.Name, t.Port)
}

// remote is <user>@127.0.0.1:<dir> — the sandbox-side checkout at ~/<dir>.
func (t Transport) remote(dir string) string {
	return fmt.Sprintf("%s@127.0.0.1:%s", t.Host.User, dir)
}

// Fetch pulls the sandbox's new commits on branch <branch> into the host source
// repo's refs/heads/<branch> (git -C src fetch name:dir branch:refs/heads/branch),
// fast-forward-only. Returns the fetched tip summary.
func Fetch(ctx context.Context, r run.Runner, t Transport, rc state.RepoClone) (string, error) {
	res, err := r.Run(ctx, run.Opts{}, "git", "-C", rc.Source,
		"-c", "core.sshCommand="+t.sshCmd(),
		"fetch", t.remote(rc.Dir),
		rc.Branch+":refs/heads/"+rc.Branch)
	if err != nil {
		return "", fmt.Errorf("fetch %s:%s (%s): %w", t.Name, rc.Dir, rc.Branch, err)
	}
	_ = res
	tip := run.Output(ctx, r, "git", "-C", rc.Source, "log", "--oneline", "-1", rc.Branch)
	return tip, nil
}

// Push sends host-side HEAD into the sandbox's <branch> checkout
// (receive.denyCurrentBranch=updateInstead updates its worktree), fast-forward
// only (git -C src push name:dir HEAD:branch).
func Push(ctx context.Context, r run.Runner, t Transport, rc state.RepoClone) error {
	_, err := r.Run(ctx, run.Opts{}, "git", "-C", rc.Source,
		"-c", "core.sshCommand="+t.sshCmd(),
		"push", t.remote(rc.Dir), "HEAD:"+rc.Branch)
	if err != nil {
		return fmt.Errorf("push %s -> %s:%s (%s): the sandbox branch must fast-forward and its tree be clean: %w",
			rc.Source, t.Name, rc.Dir, rc.Branch, err)
	}
	return nil
}

// Select returns the recorded repo clones for an instance, optionally filtered
// to one dir.
func Select(in *state.Instance, want string) []state.RepoClone {
	var out []state.RepoClone
	for _, rc := range in.RepoClones {
		if want == "" || want == rc.Dir {
			out = append(out, rc)
		}
	}
	return out
}
