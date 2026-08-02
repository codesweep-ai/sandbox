package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/spf13/cobra"
)

func newBuildCmd(app *App) *cobra.Command {
	var engines []string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Set up the sandbox image and, on capable hosts, the Firecracker artifacts",
		Long: "Set up the reusable, host-wide sandbox artifacts so later `create`s are fast.\n\n" +
			"With no flags it sets up every engine the host supports: the podman image always,\n" +
			"plus the Firecracker binary/kernel/rootfs on a Firecracker-capable host (and it fails\n" +
			"if the Firecracker host packages are missing). Restrict with --engine, e.g.\n" +
			"`--engine podman` for the image only, or `--engine firecracker` to force the FC set.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuild(cmd, app, engines)
		},
	}
	cmd.Flags().StringArrayVar(&engines, "engine", nil,
		"engine to set up: podman | firecracker (repeatable; default: every engine the host supports)")
	_ = cmd.RegisterFlagCompletionFunc("engine", fixedComp("podman", "firecracker"))
	return cmd
}

func runBuild(cmd *cobra.Command, app *App, engines []string) error {
	wantFC, err := buildWantsFirecracker(app, engines)
	if err != nil {
		return err
	}

	// The shared image first — both engines are built from it.
	// The build assets come from the checkout when present, else from the
	// binary's embedded copy (so a downloaded binary can build the image).
	imgDir, cleanup, err := assets.ImageDir(app.AssetDir)
	if err != nil {
		return err
	}
	defer cleanup()
	app.phase(fmt.Sprintf("building image %s (this can take several minutes)…", app.Image))
	// Generic image — no identity baked in.
	args := []string{"podman", "build"}
	// BUILD_VERBOSE mirrors -q into the build itself. `podman build -q` only silences a
	// RUN's stdout — its stderr still reaches the terminal, and the steps that drive
	// `nvim --headless` write everything there. Those steps capture their own output and
	// replay it only on failure unless BUILD_VERBOSE=1, so --verbose stays fully verbose.
	buildVerbose := "0"
	if !app.Verbose {
		// Only --verbose shows podman's step output; otherwise run -q (it still
		// prints the image id).
		args = append(args, "-q")
	} else {
		buildVerbose = "1"
	}
	args = append(args,
		"-t", app.Image,
		"-f", filepath.Join(imgDir, "Containerfile"),
		"--build-arg", "BUILD_VERBOSE="+buildVerbose,
		"--build-arg", "CS_SANDBOX_PRIVATE_REGISTRY="+envOr("CS_SANDBOX_PRIVATE_REGISTRY", ""),
		"--build-arg", "CS_SANDBOX_PRIVATE_REGISTRY_INSECURE="+normalizeInsecure(envOr("CS_SANDBOX_PRIVATE_REGISTRY_INSECURE", "0")),
		filepath.Join(imgDir, "rootfs"),
	)
	if _, err := app.Runner.Run(cmd.Context(), run.Opts{Interactive: true}, args...); err != nil {
		return err
	}

	// Firecracker artifacts (download + guest kernel + base rootfs), built from
	// the image just produced. Prepare() preflights first, so a host missing the
	// FC packages or /dev/kvm fails fast with an actionable error.
	if wantFC {
		app.phase("setting up firecracker artifacts…")
		// The FC build emits top-level phase lines (guest kernel, base rootfs,
		// firecracker download), so route this engine's callback to phase (shown
		// unless --quiet) rather than the verbose-only per-command progress sink.
		d := app.engineDeps()
		d.Progress = app.phase
		if err := engine.NewFirecracker(d).Prepare(cmd.Context()); err != nil {
			return err
		}
	}
	return nil
}

// buildWantsFirecracker decides whether `build` should also set up Firecracker.
// With no --engine flags it follows the host's capability (same signal as the
// create default); otherwise the flags select the engines explicitly.
func buildWantsFirecracker(app *App, engines []string) (bool, error) {
	if len(engines) == 0 {
		return autoEngine(app.Host.IsMacOS) == "firecracker", nil
	}
	wantFC := false
	for _, e := range engines {
		switch e {
		case "podman":
		case "firecracker":
			wantFC = true
		default:
			return false, fmt.Errorf("--engine must be podman or firecracker, got %q", e)
		}
	}
	return wantFC, nil
}

// normalizeInsecure maps 1/true/yes/on -> "1", anything else -> "0" (the
// PRIVATE_REGISTRY_INSECURE normalization).
func normalizeInsecure(v string) string {
	switch v {
	case "1", "true", "yes", "on", "TRUE", "YES", "ON", "True", "Yes", "On":
		return "1"
	default:
		return "0"
	}
}

// agentLaunch maps an agent name to the in-sandbox command that starts its login
// flow. Claude is the odd one: the others put their login behind a subcommand.
var agentLaunch = map[string]string{
	"claude":   "cs-claude",
	"codex":    "cs-codex login",
	"opencode": "cs-opencode providers login",
}

// agentNames lists the supported agents, sorted, for errors and completion.
func agentNames() []string {
	out := make([]string, 0, len(agentLaunch))
	for a := range agentLaunch {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// newAgentLoginCmd authenticates one agent inside a sandbox. Both engines go
// through Engine.Exec, which lands as the dev user in their home.
func newAgentLoginCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "agent-login <agent> <name>",
		Short: "Authenticate an agent (" + strings.Join(agentNames(), " | ") + ") inside a sandbox",
		Args:  cobra.ExactArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				return agentNames(), cobra.ShellCompDirectiveNoFileComp
			case 1:
				return app.completeSandbox(cmd, args, toComplete)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, name := args[0], args[1]
			launch, ok := agentLaunch[agent]
			if !ok {
				return fmt.Errorf("unknown agent %q: use one of %s", agent, strings.Join(agentNames(), ", "))
			}
			e, _, err := app.engineFor(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "cs-sandbox: launching %s inside %s — follow the prompts, then exit.\n", launch, name)
			return e.Exec(cmd.Context(), name, engine.ExecIO{
				Interactive: true, Argv: []string{"bash", "-lc", launch},
			})
		},
	}
}
