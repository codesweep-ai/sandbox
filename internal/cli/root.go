// Package cli wires the cobra command tree over the internal packages
// (cli depends on engine/state/run; main only maps exit codes).
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"text/tabwriter"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// Version is the tool version (set via -ldflags at release).
var Version = "0.1.0-dev"

// App holds process-wide dependencies resolved once at startup.
type App struct {
	Host     hostenv.Host
	Runner   run.Runner
	InstDir  string // XDG data: per-sandbox records
	TierDir  string // XDG data: generated tier keys
	FCCache  string // XDG cache: firecracker artifacts
	AssetDir string // checkout root holding the build assets (or "" -> embedded)
	Image    string
	Network  string
	SSHBind  string
	TZ       string
	Timeout  int
	Quiet    bool      // --quiet: silence everything, including build-phase lines
	Verbose  bool      // --verbose: also show per-command progress + full podman output
	Exec     *run.Exec // concrete, for toggling dry-run
	errW     io.Writer // sink for phase/progress lines (nil -> os.Stderr); tests inject a buffer
}

// stderr is the writer for phase/progress lines: the injected errW (tests) or
// os.Stderr.
func (a *App) stderr() io.Writer {
	if a.errW != nil {
		return a.errW
	}
	return os.Stderr
}

// engineDeps builds the shared engine dependencies from the resolved App.
func (a *App) engineDeps() engine.Deps {
	return engine.Deps{
		Runner:       a.Runner,
		Host:         a.Host,
		InstDir:      a.InstDir,
		TierDir:      a.TierDir,
		Image:        a.Image,
		Network:      a.Network,
		SSHBind:      a.SSHBind,
		TZ:           a.TZ,
		FCCache:      a.FCCache,
		AssetDir:     a.AssetDir,
		StartTimeout: a.Timeout,
		Progress:     a.progress,
		Note:         a.note,
	}
}

// note prints an always-shown advisory to stderr (not verbosity-gated) — e.g. the
// agent-login notices from create.
func (a *App) note(msg string) {
	fmt.Fprintln(a.stderr(), "cs-sandbox: "+msg)
}

// Three verbosity levels, driven by the mutually-exclusive --quiet/--verbose
// flags (default is neither): quiet silences all cs-sandbox output; the default
// shows only build-phase lines (phase); verbose additionally shows per-command
// progress (progress). Both print to stderr — stdout stays reserved for
// machine-facing result output like the final "created …" line, which is never
// gated. Errors are never gated either.

// phase prints a top-level build-phase line (e.g. "building the guest kernel").
// Shown by default; silenced only by --quiet.
func (a *App) phase(msg string) {
	if a.Quiet {
		return
	}
	fmt.Fprintln(a.stderr(), "cs-sandbox: "+msg)
}

// progress prints a per-command status line (e.g. create's "booting the
// microVM"). Shown only under --verbose.
func (a *App) progress(msg string) {
	if !a.Verbose {
		return
	}
	fmt.Fprintln(a.stderr(), "cs-sandbox: "+msg)
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command { return newRootCmd(&App{}) }

// newRootCmd builds the command tree over the given App. Tests pass a pre-wired
// App (e.g. a fake Runner, a buffer errW) to drive commands without touching real
// podman/firecracker; production passes a zero App.
func newRootCmd(app *App) *cobra.Command {
	var dryRun, verbose, quiet bool

	root := &cobra.Command{
		Use:           "cs-sandbox",
		Short:         "Manage rootless Firecracker/Podman dev sandboxes for AI coding agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if quiet && verbose {
				return fmt.Errorf("--quiet and --verbose are mutually exclusive")
			}
			h, err := hostenv.Detect()
			if err != nil {
				return err
			}
			app.Host = h
			// A pre-wired Runner (tests) is left as-is; production builds the real Exec.
			if app.Runner == nil {
				ex := &run.Exec{DryRun: dryRun}
				if dryRun || verbose {
					ex.Printer = func(argv []string) { fmt.Fprintln(os.Stderr, "+ "+join(argv)) }
				}
				app.Exec = ex
				app.Runner = ex
			}
			app.Quiet = quiet
			app.Verbose = verbose
			// State lives in the user's XDG dirs (never the source checkout); build
			// assets live in the checkout (or embedded). All are env-overridable —
			// see internal/paths.
			app.InstDir = paths.Instances()
			app.TierDir = paths.TierKeys()
			app.FCCache = paths.FCCache()
			app.AssetDir = paths.AssetDir()
			app.Image = envOr("CS_SANDBOX_IMAGE", "localhost/cs-sandbox:44")
			app.Network = "cs-sandbox-net"
			app.SSHBind = envOr("CS_SANDBOX_SSH_BIND", "127.0.0.1")
			app.TZ = envOr("CS_SANDBOX_TZ", "America/Los_Angeles")
			app.Timeout = 120
			return nil
		},
	}
	root.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "print external commands instead of running them")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output: per-command progress, full podman build output, and external commands")
	root.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "silence all output, including build-phase progress")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newLsCmd(app))
	root.AddCommand(newCreateCmd(app))
	root.AddCommand(newFetchCmd(app))
	root.AddCommand(newPushCmd(app))
	root.AddCommand(newSyncSSHConfigCmd(app))
	root.AddCommand(newForwardCmd(app))
	root.AddCommand(newForwardsCmd(app))
	root.AddCommand(newUnforwardCmd(app))
	root.AddCommand(newBuildCmd(app))
	root.AddCommand(newDoctorCmd(app))
	root.AddCommand(newAgentLoginCmd(app))
	root.AddCommand(newInstallAgentToolsCmd(app))
	root.AddCommand(newHostRouteCmd(app))
	for _, c := range newInstanceCmds(app) {
		root.AddCommand(c)
	}
	for _, c := range newStoreCmds(app) {
		root.AddCommand(c)
	}
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cs-sandbox version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "cs-sandbox %s (%s/%s, %s)\n",
				Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			return nil
		},
	}
}

func newLsCmd(app *App) *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLs(cmd.Context(), app, cmd.OutOrStdout(), quiet)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only sandbox names, one per line (for scripting)")
	return cmd
}

func runLs(ctx context.Context, app *App, out interface{ Write([]byte) (int, error) }, quiet bool) error {
	insts, err := state.List(app.InstDir)
	if err != nil {
		return err
	}
	// Data `rm` kept, whose sandbox is gone: listed too, so it can't sit on disk
	// unnoticed (docker's dangling volumes are the counter-example), and so
	// `destroy` has something to complete.
	orphans := app.engineDeps().Orphans(ctx)
	// Names only: pipeable, so `cs-sandbox ls -q | xargs -n1 cs-sandbox destroy -f`
	// works — which is also why leftovers belong here, or that idiom would leave
	// them behind. Skips the status lookup, which needs a subprocess nothing here reads.
	if quiet {
		for _, in := range insts {
			fmt.Fprintln(out, in.Name)
		}
		for _, o := range orphans {
			fmt.Fprintln(out, o.Name)
		}
		return nil
	}
	// STATUS sits next to NAME, as it does in `kubectl get` — it is the column you
	// scan for. Costs one `podman ps` for the whole listing.
	status := app.engineDeps().Statuses(ctx, insts)
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	// No PORT column: you reach a sandbox by name (`ssh <name>`, from the managed
	// ssh config), so a port here would suggest a way of working the tool doesn't
	// want. `cs-sandbox port <name>` prints it for the cases that need it.
	fmt.Fprintln(tw, "NAME\tSTATUS\tAGE\tTYPE\tENGINE\tYOLO\tSOLO")
	for _, in := range insts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			in.Name, status[in.Name], age(in.Created, time.Now()), in.Type, in.Engine,
			yn(in.Yolo), yn(in.Solo))
	}
	// Leftovers last, under the sandboxes that still exist. Only the columns the
	// data itself answers for are filled in; the rest went with the state record.
	for _, o := range orphans {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			o.Name, engine.StatusRemoved, age(o.SinceRFC3339(), time.Now()), "-", o.Engine, "-", "-")
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(orphans) > 0 {
		fmt.Fprintf(out, "\n%s = removed by `rm`, data kept: `create` with the same name reuses it, `destroy -f` deletes it.\n",
			engine.StatusRemoved)
	}
	return nil
}

// age renders how long ago created (RFC3339) was, in kubectl's compact style:
// the largest single unit, e.g. 45s, 12m, 3h, 6d. Empty or unparseable input
// gives "-" rather than a misleading duration.
func age(created string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "-" // clock skew: better than reporting a negative age
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

func join(argv []string) string {
	s := ""
	for i, a := range argv {
		if i > 0 {
			s += " "
		}
		s += a
	}
	return s
}
