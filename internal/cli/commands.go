package cli

import (
	"context"
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/forward"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// engineFor returns the adapter matching an instance's recorded engine.
func (a *App) engineFor(name string) (engine.Engine, *state.Instance, error) {
	in, err := state.Load(a.InstDir, name)
	if err != nil {
		return nil, nil, fmt.Errorf("no such sandbox %q", name)
	}
	d := a.engineDeps()
	switch in.Engine {
	case state.Firecracker:
		return engine.NewFirecracker(d), in, nil
	default:
		return engine.NewPodman(d), in, nil
	}
}

func newInstanceCmds(app *App) []*cobra.Command {
	return []*cobra.Command{
		simpleInstanceCmd(app, "start", "Start a stopped sandbox",
			func(ctx context.Context, e engine.Engine, name string) error { return e.Start(ctx, name) }),
		simpleInstanceCmd(app, "stop", "Stop a running sandbox (keep its state)",
			func(ctx context.Context, e engine.Engine, name string) error {
				forward.KillAll(app.InstDir, name)
				return e.Stop(ctx, name)
			}),
		newRmCmd(app),
		newDestroyCmd(app),
		newExecCmd(app),
		newSSHCmd(app),
		newPortCmd(app),
	}
}

func simpleInstanceCmd(app *App, use, short string, fn func(context.Context, engine.Engine, string) error) *cobra.Command {
	return &cobra.Command{
		Use:               use + " <name>",
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, _, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			return fn(cmd.Context(), e, args[0])
		},
	}
}

func newDestroyCmd(app *App) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:               "destroy <name>",
		Short:             "Remove a sandbox AND its volumes/data",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, _, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			if !force {
				fmt.Fprintf(cmd.OutOrStdout(), "destroying %q and all its data. Re-run with -f to confirm.\n", args[0])
				return nil
			}
			forward.KillAll(app.InstDir, args[0])
			if err := e.Remove(cmd.Context(), args[0], true); err != nil {
				return err
			}
			app.refreshHostRoute(cmd) // unpublish the destroyed name if host-route is on
			if err := app.syncSSHConfig(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "destroyed %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "do not prompt")
	return cmd
}

// newRmCmd removes a sandbox but keeps its data (home volume / rootfs disk), so
// `create` with the same name reuses it. `destroy` is the one that deletes data.
func newRmCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "rm <name>",
		Short:             "Remove a sandbox but keep its data (recreate with the same name to reuse it)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, _, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			forward.KillAll(app.InstDir, args[0])
			if err := e.Remove(cmd.Context(), args[0], false); err != nil {
				return err
			}
			app.refreshHostRoute(cmd) // unpublish the name if host-route is on
			if err := app.syncSSHConfig(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"removed %s — kept its data; recreate with the same name (and the same --repo/--snapshot) to reuse it\n", args[0])
			return nil
		},
	}
}

func newExecCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "exec <name> [cmd...]",
		Short:             "Run a command (or a login shell) inside a sandbox",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: app.completeSandbox, // only the first arg (the sandbox); the rest is the command
		RunE: func(cmd *cobra.Command, args []string) error {
			e, _, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			io := engine.ExecIO{Interactive: len(args) == 1, Argv: args[1:]}
			return e.Exec(cmd.Context(), args[0], io)
		},
	}
	// Stop flag parsing after the sandbox name so `exec x id -un` passes -un through.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func newSSHCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "ssh <name> [args...]",
		Short:             "SSH into a sandbox by name",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, in, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			// Reach the published port with the user-tier key. Key known-hosts by
			// HostKeyAlias=<name> in a dedicated file so a recycled port never trips
			// "host key changed" and the user's main known_hosts is untouched.
			key := app.TierDir + "/id_cs-sandbox_user"
			knownHosts := app.Host.SSHDir() + "/known_hosts.cs-sandbox"
			sshArgs := []string{
				"-i", key, "-p", fmt.Sprintf("%d", in.Port),
				"-o", "HostKeyAlias=" + args[0],
				"-o", "UserKnownHostsFile=" + knownHosts,
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "IdentitiesOnly=yes",
				fmt.Sprintf("%s@127.0.0.1", app.Host.User),
			}
			sshArgs = append(sshArgs, args[1:]...)
			_, err = app.Runner.Run(cmd.Context(), run.Opts{Interactive: true}, append([]string{"ssh"}, sshArgs...)...)
			return err
		},
	}
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func newPortCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:               "port <name>",
		Short:             "Print a sandbox's host SSH port",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := state.Load(app.InstDir, args[0])
			if err != nil {
				return fmt.Errorf("no such sandbox %q", args[0])
			}
			fmt.Fprintln(cmd.OutOrStdout(), in.Port)
			return nil
		},
	}
}
