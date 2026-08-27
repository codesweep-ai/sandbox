package cli

import (
	"context"
	"fmt"
	"strconv"

	"path/filepath"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/forward"
	"github.com/codesweep-ai/sandbox/internal/hostcfg"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// engineFor returns the adapter matching an instance's recorded engine.
func (a *App) engineFor(name string) (engine.Engine, *state.Instance, error) {
	in, err := a.resolve(name)
	if err != nil {
		return nil, nil, err
	}
	d := a.engineDepsFor(in.Group)
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
			func(ctx context.Context, e engine.Engine, in *state.Instance) error { return e.Start(ctx, in.Name) }),
		simpleInstanceCmd(app, "stop", "Stop a running sandbox (keep its state)",
			func(ctx context.Context, e engine.Engine, in *state.Instance) error {
				forward.KillAll(app.InstDir, in.Group, in.Name)
				return e.Stop(ctx, in.Name)
			}),
		newRmCmd(app),
		newDestroyCmd(app),
		newExecCmd(app),
		newSSHCmd(app),
		newPortCmd(app),
	}
}

func simpleInstanceCmd(app *App, use, short string, fn func(context.Context, engine.Engine, *state.Instance) error) *cobra.Command {
	return &cobra.Command{
		Use:               use + " <name>",
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, in, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			// The engine takes the BARE name — its Deps carries the group and
			// qualifies podman object names itself. Handing it the user's
			// qualified ref would address <name>.<group>.<group>, which matches
			// nothing and fails silently. Callbacks that need the group as well
			// take it from the instance.
			return fn(cmd.Context(), e, in)
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
			e, in, err := app.engineFor(args[0])
			if err != nil {
				// No state record — but `rm` may have kept this name's data,
				// and destroy is what deletes data.
				return destroyOrphan(cmd, app, args[0], force)
			}
			if !force {
				fmt.Fprintf(cmd.OutOrStdout(), "destroying %q and all its data. Re-run with -f to confirm.\n", args[0])
				return nil
			}
			forward.KillAll(app.InstDir, in.Group, in.Name)
			if err := e.Remove(cmd.Context(), in.Name, true); err != nil {
				return err
			}
			// The loan record went with the instance directory, so the lender
			// stops honouring this sandbox's token. Stop the lender too once no
			// sandbox on this host holds one.
			app.stopLenderIfIdle()
			app.refreshHostRoute(cmd) // unpublish the destroyed name if host-route is on
			if err := app.syncSSHConfig(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "destroyed %s\n", args[0])
			return nil
		},
	}
	// Not "do not prompt": nothing prompts. Without -f, destroy reports what it
	// would delete and exits 0, so -f is the confirmation rather than a way past
	// one.
	cmd.Flags().BoolVarP(&force, "force", "f", false,
		"confirm the deletion; without it, destroy only reports what it would delete")
	return cmd
}

// destroyOrphan handles `destroy <name>` for a name with no sandbox: `rm` keeps
// the data and drops the state record, so this is the only way to reclaim it.
// Reports plainly when there is nothing left to delete, rather than pretending
// the name is unknown.
//
// The reference is qualified first. Orphans are keyed by the qualified name,
// because that is what a podman volume carries, while `rm` tells the user to run
// `destroy <bare name>` — so comparing the raw argument made that advice
// un-followable in the default group: `destroy worker-01 -f` answered "no data
// left over from one" with the volumes still there, and only the fully-spelled
// `worker-01.default` worked. A bare name is a default-group reference here for
// the same reason it is everywhere else (see SplitRef), never a search.
func destroyOrphan(cmd *cobra.Command, app *App, ref string, force bool) error {
	name, group := SplitRef(ref)
	if group == "" {
		group = state.DefaultGroup
	}
	o, ok := app.engineDeps().Orphan(cmd.Context(), state.ObjectName(group, name))
	if !ok {
		return app.noOrphan(cmd.Context(), ref, name, group)
	}
	// Both messages name the qualified reference rather than what was typed, so a
	// bare argument still says which group's data is going.
	if !force {
		fmt.Fprintf(cmd.OutOrStdout(),
			"destroying the data kept when %q was removed. Re-run with -f to confirm.\n", o.Name)
		return nil
	}
	if err := app.engineDeps().PurgeOrphan(cmd.Context(), o); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "destroyed %s (the data kept when it was removed)\n", o.Name)
	return nil
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
			e, in, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			forward.KillAll(app.InstDir, in.Group, in.Name)
			if err := e.Remove(cmd.Context(), in.Name, false); err != nil {
				return err
			}
			app.stopLenderIfIdle()
			app.refreshHostRoute(cmd) // unpublish the name if host-route is on
			if err := app.syncSSHConfig(); err != nil {
				return err
			}
			// Say how to get rid of the data too: `rm` drops the state record, so
			// without this the kept data is easy to forget and hard to find again.
			fmt.Fprintf(cmd.OutOrStdout(),
				"removed %s — kept its data; recreate with the same name (and the same --repo/--snapshot) to reuse it, or 'cs-sandbox destroy %s -f' to delete it\n",
				args[0], args[0])
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
			e, in, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			// A leading `--` terminates cs-sandbox's own flags; it is not part
			// of the command to run. Interspersed parsing is off, so cobra
			// leaves it in args — and only ssh happens to swallow it, while
			// `podman exec` hands it to the container and fails with
			// "executable file `--` not found".
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "--" {
				rest = rest[1:]
			}
			io := engine.ExecIO{Interactive: len(rest) == 0, Argv: rest}
			return sandboxedExit(e.Exec(cmd.Context(), in.Name, io))
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
			// the host-global object name in a dedicated file, so a recycled port
			// never trips "host key changed", the user's main known_hosts is
			// untouched, and the same fixture in two groups keys two entries
			// rather than one the second connection fails on.
			key := filepath.Join(paths.GroupKeys(in.Group), "id_cs-sandbox_user")
			knownHosts := app.Host.SSHDir() + "/known_hosts.cs-sandbox"
			sshArgs := []string{
				"-i", key, "-p", strconv.Itoa(in.Port),
				"-o", "HostKeyAlias=" + hostcfg.Ref(in),
				"-o", "UserKnownHostsFile=" + knownHosts,
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "IdentitiesOnly=yes",
				app.Host.User + "@127.0.0.1",
			}
			sshArgs = append(sshArgs, args[1:]...)
			_, err = app.Runner.Run(cmd.Context(), run.Opts{Interactive: true}, append([]string{"ssh"}, sshArgs...)...)
			return sandboxedExit(err)
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
			in, err := app.resolve(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), in.Port)
			return nil
		},
	}
}
