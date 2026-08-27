// Package cli wires the cobra command tree over the internal packages
// (cli depends on engine/state/run; main only maps exit codes).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// The packages the images are published to. The LOCAL image carries the same
// fully-qualified name as the published one, so an image that was built and one
// that was pulled are the same reference: nothing retags, and `podman push`
// takes what `cs-sandbox build` made.
//
// The two CI images are separate packages rather than tags of the first,
// because neither is a sandbox: ci-slim.sh strips every toolchain, so somebody
// who found one under the sandbox package and pulled it would get a container
// that boots and then has no go, no node and no agents. A package whose name
// says slim cannot be mistaken that way.
const (
	imageRepo           = "ghcr.io/codesweep-ai/sandbox"
	slimImageRepo       = imageRepo + "-slim"
	slimAgentsImageRepo = imageRepo + "-slim-agents"
)

// devVersion marks a binary that carried no release stamp.
const devVersion = "dev"

// Version is the tool version (set via -ldflags at release).
var Version = devVersion

// buildVersion reports the release stamp when there is one, and otherwise the
// module version the toolchain recorded. A binary installed straight from the
// module path carries no stamp, so without this it would answer the dev
// sentinel and leave you guessing which revision built a sandbox.
func buildVersion() string {
	if Version != devVersion {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return Version
	}
	return info.Main.Version
}

// sandboxPin is this binary's own version in the form `go install` accepts, so
// the image can ship the cs-sandbox that built it rather than whatever the
// branch tip holds. Go's build info appends +dirty to a version built from a
// modified tree; a module version cannot carry that, so it is trimmed and the
// caller says so — the image gets the committed revision, which is the only one
// installable.
//
// Empty means the binary carries no module version at all (`go run` without
// -buildvcs), the one case with no revision to name.
func sandboxPin() (version, dirtyNote string) {
	v := buildVersion()
	if v == devVersion {
		return "", ""
	}
	if trimmed, ok := strings.CutSuffix(v, "+dirty"); ok {
		return trimmed, "working tree is dirty; the image gets cs-sandbox " + trimmed
	}
	return v, ""
}

// imageRef names an image for this binary: the package, tagged with the version
// this binary reports. The image ships the cs-sandbox that built it (see
// sandboxPin), so the version is the only thing that names what is inside —
// which is why the tag is the version string rather than the revision. The same
// commit yields a different image once it is tagged for release, because the
// cs-sandbox in it then reports the release version.
func imageRef(repo string) (string, error) {
	tag, err := imageTag()
	if err != nil {
		return "", err
	}
	return repo + ":" + tag, nil
}

// imageTag is this binary's version as an OCI tag.
//
// Go marks a binary built from a modified tree with +dirty, and + is not a legal
// tag character. -dirty is, so that is what a dirty tree gets. Nothing publishes
// a -dirty tag, which makes the reference unpullable by construction rather than
// by a special case, and says plainly that the image is not the published one
// for that revision — its rootfs came from a tree nobody else has.
func imageTag() (string, error) {
	v := buildVersion()
	if v == devVersion {
		return "", errors.New("this cs-sandbox reports no version, so it cannot name its image — " +
			"build it with `make build` from a git clone rather than running `go run`, " +
			"or name an image with CS_SANDBOX_IMAGE")
	}
	if trimmed, ok := strings.CutSuffix(v, "+dirty"); ok {
		return trimmed + "-dirty", nil
	}
	return v, nil
}

// App holds process-wide dependencies resolved once at startup.
type App struct {
	Host     hostenv.Host
	Runner   run.Runner
	InstDir  string // XDG data: per-sandbox records
	TierDir  string // XDG data: generated tier keys
	FCCache  string // XDG cache: firecracker artifacts
	AssetDir string // checkout root holding the build assets (or "" -> embedded)
	Image    string
	ImageErr error // why Image is empty; raised by the commands that need one
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
// requireImage fails when this binary could not name its image, with the reason
// it could not. Commands that reach for an image call it first, so the failure
// names its cause instead of surfacing as a podman error about an empty
// reference several layers down.
func (a *App) requireImage() error {
	if a.Image != "" {
		return nil
	}
	if a.ImageErr != nil {
		return a.ImageErr
	}
	return errors.New("no sandbox image is configured")
}

// dryRun reports whether this invocation only prints what it would do.
func (a *App) dryRun() bool { return a.Exec != nil && a.Exec.DryRun }

func (a *App) stderr() io.Writer {
	if a.errW != nil {
		return a.errW
	}
	return os.Stderr
}

// engineDeps builds the shared engine dependencies for the default group.
// Operations that act on a specific sandbox or group use engineDepsFor, so the
// network, keys and instance directory all follow that group.
func (a *App) engineDeps() engine.Deps { return a.engineDepsFor(state.DefaultGroup) }

// engineDepsFor builds engine dependencies scoped to one group. A group's
// network is derived from its name rather than configured, so the two can never
// drift apart.
func (a *App) engineDepsFor(group string) engine.Deps {
	if group == "" {
		group = state.DefaultGroup
	}
	d := a.engineDepsBase()
	d.Group = group
	d.Network = state.NetworkName(group)
	// The group's allocated tap prefix, so two groups' VMs can never collide on
	// a host-global interface name. Absent record (a group being created right
	// now) falls back to the historical default.
	if g, err := state.LoadGroup(a.InstDir, group); err == nil {
		d.TapPrefix = g.TapPrefix
	}
	// Trust material is per group, so every consumer that reads TierDir (the
	// seed writer, the engines, the generated ssh config) follows the group
	// without knowing groups exist.
	d.TierDir = paths.GroupKeys(group)
	return d
}

func (a *App) engineDepsBase() engine.Deps {
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
		DryRun:       a.Exec != nil && a.Exec.DryRun,
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
				return errors.New("--quiet and --verbose are mutually exclusive")
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
					ex.Printer = func(argv []string) { fmt.Fprintln(os.Stderr, "+ "+strings.Join(argv, " ")) }
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
			// The image is named after the version that built it, so a sandbox
			// runs the image its own binary came from. A binary that reports no
			// version cannot form that name, which is not fatal here — version,
			// ls and --help all work without an image — so the reason is held
			// and raised by requireImage.
			if img := os.Getenv("CS_SANDBOX_IMAGE"); img != "" {
				app.Image = img
			} else {
				app.Image, app.ImageErr = imageRef(imageRepo)
			}
			app.Network = state.NetworkName(state.DefaultGroup)
			app.SSHBind = envOr("CS_SANDBOX_SSH_BIND", "127.0.0.1")
			app.TZ = envOr("CS_SANDBOX_TZ", "America/Los_Angeles")
			app.Timeout = 120
			return nil
		},
	}
	root.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "print external commands instead of running them")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output: per-command progress, full podman build output, and external commands")
	root.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "silence all output, including build-phase progress")

	root.AddCommand(newVersionCmd(app))
	root.AddCommand(newLsCmd(app))
	root.AddCommand(newCreateCmd(app))
	root.AddCommand(newFetchCmd(app))
	root.AddCommand(newPushCmd(app))
	root.AddCommand(newGroupCmd(app))
	root.AddCommand(newInspectCmd(app))
	root.AddCommand(newSyncSSHConfigCmd(app))
	root.AddCommand(newForwardCmd(app))
	root.AddCommand(newForwardsCmd(app))
	root.AddCommand(newUnforwardCmd(app))
	root.AddCommand(newBuildCmd(app))
	root.AddCommand(newDoctorCmd(app))
	root.AddCommand(newAgentLoginCmd(app))
	root.AddCommand(newInstallAgentToolsCmd(app))
	root.AddCommand(newAgentToolsCmd(app))
	root.AddCommand(newHostRouteCmd(app))
	root.AddCommand(newLenderCmd(app))
	for _, c := range newInstanceCmds(app) {
		root.AddCommand(c)
	}
	for _, c := range newStoreCmds(app) {
		root.AddCommand(c)
	}
	return root
}

func newVersionCmd(app *App) *cobra.Command {
	var images bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the cs-sandbox version, and the image it names",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// --images: every reference this binary names, for a caller that has
			// to act on one. The release workflow builds and pushes all three,
			// and asking here is what keeps the naming rule in one place instead
			// of half here and half in a shell substitution.
			if images {
				for _, r := range []struct{ label, repo string }{
					{"image", imageRepo},
					{"image-slim", slimImageRepo},
					{"image-slim-agents", slimAgentsImageRepo},
				} {
					ref, err := imageRef(r.repo)
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "%-18s %s\n", r.label, ref)
				}
				return nil
			}
			fmt.Fprintf(out, "cs-sandbox %s (%s/%s, %s)\n",
				buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
			// The image this binary creates from. Printed here because the name
			// is derived rather than configured, so without it the only way to
			// learn which image a host wants is to run something that needs it.
			// Second field of a labelled line, which is how a Makefile or a
			// workflow reads it back.
			if err := app.requireImage(); err != nil {
				fmt.Fprintf(out, "image      none: %s\n", err)
				return nil
			}
			fmt.Fprintf(out, "image      %s\n", app.Image)
			return nil
		},
	}
	cmd.Flags().BoolVar(&images, "images", false,
		"print every image reference this binary names, one per line, instead of the version")
	return cmd
}

func newLsCmd(app *App) *cobra.Command {
	var quiet, asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if quiet && asJSON {
				return errors.New("--quiet and --json are mutually exclusive")
			}
			if asJSON {
				return runLsJSON(cmd.Context(), app, cmd.OutOrStdout())
			}
			return runLs(cmd.Context(), app, cmd.OutOrStdout(), quiet)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only sandbox refs (<name>.<group>), one per line (for scripting)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print stable machine-readable JSON")
	return cmd
}

// lsItem is the stable shape of `ls --json`. Ref is the field a caller should
// feed back to any command that takes a sandbox: a bare name is not unique
// across groups, so scripting on Name alone is a bug waiting to happen.
type lsItem struct {
	Ref     string       `json:"ref"`
	Name    string       `json:"name"`
	Group   string       `json:"group"`
	Status  string       `json:"status"`
	Created string       `json:"created,omitempty"`
	Type    string       `json:"type,omitempty"`
	Engine  state.Engine `json:"engine"`
	Network string       `json:"network"`
	Yolo    bool         `json:"yolo"`
	Solo    bool         `json:"solo"`
	// AgentLogins are credentials this sandbox HOLDS, copied in at create.
	// Loans are credentials it BORROWS, which stay on the host. The difference
	// is the one worth scanning a listing for.
	AgentLogins []string `json:"agentlogins,omitempty"`
	Loans       []string `json:"loans,omitempty"`
}

func runLsJSON(ctx context.Context, app *App, out io.Writer) error {
	insts, err := state.List(app.InstDir)
	if err != nil {
		return err
	}
	status := app.engineDeps().Statuses(ctx, insts)
	items := make([]lsItem, 0, len(insts))
	for _, in := range insts {
		items = append(items, lsItem{
			Ref: engine.Qualify(in), Name: in.Name, Group: in.Group,
			Status: status[engine.Qualify(in)], Created: in.Created, Type: in.Type,
			Engine: in.Engine, Network: state.NetworkName(in.Group),
			Yolo: in.Yolo, Solo: in.Solo,
			AgentLogins: in.AgentLogins, Loans: loanSlots(app.InstDir, in.Group, in.Name),
		})
	}
	for _, o := range app.engineDeps().Orphans(ctx) {
		// An orphan's internal name is already the qualified ref, so split it
		// back out: `name` must mean the same thing in every row, and the group
		// is known — leaving it blank would make the row unusable for anything
		// that filters by group.
		name, group := SplitRef(o.Name)
		items = append(items, lsItem{
			Ref: o.Name, Name: name, Group: group, Status: engine.StatusRemoved,
			Created: o.SinceRFC3339(), Engine: o.Engine, Network: state.NetworkName(group),
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
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
	// Refs only: pipeable, so `cs-sandbox ls -q | xargs -n1 cs-sandbox destroy -f`
	// works — which is also why leftovers belong here, or that idiom would leave
	// them behind. Qualified, because a bare name is not unique across groups and
	// feeding one back in would hit the ambiguity error (or worse, the wrong
	// sandbox). Skips the status lookup, which needs a subprocess nothing here reads.
	if quiet {
		for _, in := range insts {
			fmt.Fprintln(out, engine.Qualify(in))
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
	// GROUP leads: the listing is primarily a view of isolation boundaries, and
	// List already returns members grouped and sorted.
	// CREDS earns its column: it is the difference between a sandbox holding a
	// copy of your credentials and one that only borrows them, which is the
	// question a listing of agent sandboxes is most often scanned for.
	fmt.Fprintln(tw, "GROUP\tNAME\tSTATUS\tAGE\tTYPE\tENGINE\tYOLO\tSOLO\tCREDS")
	for _, in := range insts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			in.Group, in.Name, status[engine.Qualify(in)], age(in.Created, time.Now()), in.Type, in.Engine,
			yn(in.Yolo), yn(in.Solo), creds(in.AgentLogins, loanSlots(app.InstDir, in.Group, in.Name)))
	}
	// Leftovers last, under the sandboxes that still exist. Only the columns the
	// data itself answers for are filled in; the rest went with the state record.
	for _, o := range orphans {
		name, group := SplitRef(o.Name)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			group, name, engine.StatusRemoved, age(o.SinceRFC3339(), time.Now()), "-", o.Engine, "-", "-", "-")
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

// creds is the CREDS column: whether this sandbox HOLDS credentials of yours,
// BORROWS them through the lender, or has none.
func creds(held, borrowed []string) string {
	switch {
	case len(held) > 0 && len(borrowed) > 0:
		return "held+lent"
	case len(held) > 0:
		return "held"
	case len(borrowed) > 0:
		return "lent"
	}
	return "-"
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
