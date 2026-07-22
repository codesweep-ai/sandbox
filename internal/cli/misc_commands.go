package cli

import (
	"fmt"
	"path/filepath"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
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
	if !app.Verbose {
		// Only --verbose shows podman's step output; otherwise run -q (it still
		// prints the image id).
		args = append(args, "-q")
	}
	args = append(args,
		"-t", app.Image,
		"-f", filepath.Join(imgDir, "Containerfile"),
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

func newLoginCmd(app *App, agent string) *cobra.Command {
	use := agent + "-login"
	launch := "cs-" + agent
	if agent == "codex" {
		launch = "cs-codex login"
	}
	return &cobra.Command{
		Use:               use + " <name>",
		Short:             fmt.Sprintf("Authenticate the %s agent inside a sandbox", agent),
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := state.Load(app.InstDir, args[0])
			if err != nil {
				return fmt.Errorf("no such sandbox %q", args[0])
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "cs-sandbox: launching %s inside %s — follow the prompts, then exit.\n", launch, args[0])
			if in.Engine == state.Firecracker {
				// Reach the microVM over ssh as the dev user and launch the agent.
				e, _, err := app.engineFor(args[0])
				if err != nil {
					return err
				}
				return e.Exec(cmd.Context(), args[0], engine.ExecIO{
					Interactive: true, Argv: []string{"bash", "-lc", launch},
				})
			}
			// podman exec -it as the dev user in their home.
			argv := []string{"podman", "exec", "-it",
				"--user", app.Host.User,
				"--workdir", fmt.Sprintf("/home/%s", app.Host.User),
				args[0], "bash", "-lc", launch}
			_, err = app.Runner.Run(cmd.Context(), run.Opts{Interactive: true}, argv...)
			return err
		},
	}
}
