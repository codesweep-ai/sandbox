package cli

import (
	"strings"

	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/codesweep-ai/sandbox/internal/store"
	"github.com/spf13/cobra"
)

// Dynamic shell completion (cobra). These run in cobra's hidden `__complete`
// command, which does NOT execute PersistentPreRunE — so the helpers resolve
// their own state rather than relying on the populated App fields.

// instDirForComplete resolves the instances dir the same way startup does,
// without needing PersistentPreRunE to have run.
func (a *App) instDirForComplete() string {
	if a.InstDir != "" {
		return a.InstDir
	}
	return paths.Instances()
}

func (a *App) runnerForComplete() run.Runner {
	if a.Runner != nil {
		return a.Runner
	}
	return &run.Exec{}
}

// completeSandbox completes a single sandbox name (only the first positional).
// Reading typed state files is fast and needs no subprocess.
func (a *App) completeSandbox(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return a.sandboxMatches(toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeSandboxAlways completes a sandbox name regardless of arg position
// (for optional-name commands like `forwards [name]`).
func (a *App) completeSandboxAlways(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return a.sandboxMatches(toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (a *App) sandboxMatches(prefix string) []string {
	insts, err := state.List(a.instDirForComplete())
	if err != nil {
		return nil
	}
	var out []string
	for _, in := range insts {
		if strings.HasPrefix(in.Name, prefix) {
			out = append(out, in.Name)
		}
	}
	return out
}

// completeSandboxRepoDir completes a sandbox name (first arg) then that
// sandbox's --repo dirs (second arg) — for fetch/push.
func (a *App) completeSandboxRepoDir(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return a.sandboxMatches(toComplete), cobra.ShellCompDirectiveNoFileComp
	case 1:
		in, err := (&App{InstDir: a.instDirForComplete()}).resolve(args[0])
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, rc := range in.RepoClones {
			if strings.HasPrefix(rc.Dir, toComplete) {
				out = append(out, rc.Dir)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeStore completes an existing shared image-store name (first positional).
func (a *App) completeStore(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return a.storeMatches(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func (a *App) storeMatches(cmd *cobra.Command, prefix string) []string {
	m := store.Manager{Runner: a.runnerForComplete(), Image: a.Image}
	var out []string
	for _, n := range m.List(cmd.Context()) {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

// fixedComp returns a completion func offering a fixed value set (for flags).
func fixedComp(values ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var out []string
		for _, v := range values {
			if strings.HasPrefix(v, toComplete) {
				out = append(out, v)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}
