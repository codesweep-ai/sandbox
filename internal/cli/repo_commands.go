package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/codesweep-ai/sandbox/internal/hostcfg"
	"github.com/codesweep-ai/sandbox/internal/repo"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// syncSSHConfig regenerates the host ssh config for all instances.
func (a *App) syncSSHConfig() error {
	insts, err := state.List(a.InstDir)
	if err != nil {
		return err
	}
	return hostcfg.SyncSSHConfig(a.Host, a.TierDir, a.InstDir, insts)
}

func (a *App) transport(name string) (repo.Transport, *state.Instance, error) {
	in, err := state.Load(a.InstDir, name)
	if err != nil {
		return repo.Transport{}, nil, fmt.Errorf("no such sandbox %q", name)
	}
	return repo.Transport{Host: a.Host, TierDir: a.TierDir, Name: name, Port: in.Port}, in, nil
}

func newFetchCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "fetch <name> [dir]",
		Short:             "Fetch a sandbox's --repo commits back to the host (fast-forward only)",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: app.completeSandboxRepoDir,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, in, err := app.transport(args[0])
			if err != nil {
				return err
			}
			want := ""
			if len(args) == 2 {
				want = args[1]
			}
			clones := repo.Select(in, want)
			if len(clones) == 0 {
				return fmt.Errorf("no --repo checkouts recorded for %s", args[0])
			}
			var failures []error
			for _, rc := range clones {
				fmt.Fprintf(os.Stderr, "cs-sandbox: fetch %s:%s (%s)\n", args[0], rc.Dir, rc.Branch)
				tip, err := repo.Fetch(cmd.Context(), app.Runner, t, rc)
				if err != nil {
					failures = append(failures, err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s -> %s\n", rc.Source, rc.Branch, tip)
			}
			return batchError("fetch", len(clones), failures)
		},
	}
}

func newPushCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "push <name> [dir]",
		Short:             "Push host commits into a sandbox's --repo checkout (fast-forward only)",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: app.completeSandboxRepoDir,
		RunE: func(cmd *cobra.Command, args []string) error {
			t, in, err := app.transport(args[0])
			if err != nil {
				return err
			}
			want := ""
			if len(args) == 2 {
				want = args[1]
			}
			clones := repo.Select(in, want)
			if len(clones) == 0 {
				return fmt.Errorf("no --repo checkouts recorded for %s", args[0])
			}
			var failures []error
			for _, rc := range clones {
				fmt.Fprintf(os.Stderr, "cs-sandbox: push %s HEAD -> %s:%s (%s)\n", rc.Source, args[0], rc.Dir, rc.Branch)
				if err := repo.Push(cmd.Context(), app.Runner, t, rc); err != nil {
					failures = append(failures, err)
				}
			}
			return batchError("push", len(clones), failures)
		},
	}
}

func batchError(action string, total int, failures []error) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s failed for %d of %d repositories: %w",
		action, len(failures), total, errors.Join(failures...))
}

func newSyncSSHConfigCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "sync-ssh-config",
		Short: "Regenerate ~/.ssh/config.d/cs-sandbox for all sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.syncSSHConfig()
		},
	}
}
