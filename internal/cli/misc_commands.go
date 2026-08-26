package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/spf13/cobra"
)

// guestProxyDir is where --local-sandbox mounts the throwaway module proxy
// inside the build. Under /tmp so nothing survives into the image.
const guestProxyDir = "/tmp/cs-goproxy"

func newBuildCmd(app *App) *cobra.Command {
	var engines []string
	var slim, withAgents, localSandbox bool
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Set up the sandbox image and, on capable hosts, the Firecracker artifacts",
		Long: "Set up the reusable, host-wide sandbox artifacts so later `create`s are fast.\n\n" +
			"With no flags it sets up every engine the host supports: the podman image always,\n" +
			"plus the Firecracker binary/kernel/rootfs on a Firecracker-capable host (and it fails\n" +
			"if the Firecracker host packages are missing). Restrict with --engine, e.g.\n" +
			"`--engine podman` for the image only, or `--engine firecracker` to force the FC set.\n\n" +
			"--slim builds the CI image instead: the same Containerfile with the developer\n" +
			"toolchains removed, ~700 MB against 9.3 GB and minutes against tens of them. It is\n" +
			"what makes booting real sandboxes on a hosted runner affordable — the full image\n" +
			"does not fit on one. Add --with-agents when the tests being run drive claude, codex\n" +
			"or opencode inside the sandbox; without it those three CLIs are absent.\n\n" +
			"A slim build goes to " + slimImageRepo + "\n" +
			"(or " + slimAgentsImageRepo + " with --with-agents),\n" +
			"tagged with this cs-sandbox's version, unless CS_SANDBOX_IMAGE says otherwise — so it\n" +
			"can never be mistaken for the shipped image, nor the two slim ones for each other.\n" +
			"Point the same variable at that reference when running the tests.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if withAgents && !slim {
				return errors.New("--with-agents applies to --slim only: the full image already has the agent CLIs")
			}
			return runBuild(cmd, app, engines, slim, withAgents, localSandbox)
		},
	}
	cmd.Flags().StringArrayVar(&engines, "engine", nil,
		"engine to set up: podman | firecracker (repeatable; default: every engine the host supports)")
	cmd.Flags().BoolVar(&slim, "slim", false,
		"build the slimmed CI image (no developer toolchains) instead of the shipped one")
	cmd.Flags().BoolVar(&withAgents, "with-agents", false,
		"with --slim: keep the claude/codex/opencode CLIs, for tests that drive an agent")
	cmd.Flags().BoolVar(&localSandbox, "local-sandbox", false,
		"install cs-sandbox in the image from this checkout's commit instead of from the module proxy, for a revision that is not pushed yet")
	_ = cmd.RegisterFlagCompletionFunc("engine", fixedComp("podman", "firecracker"))
	return cmd
}

func runBuild(cmd *cobra.Command, app *App, engines []string, slim, withAgents, localSandbox bool) error {
	wantFC, err := buildWantsFirecracker(app, engines)
	if err != nil {
		return err
	}

	// Retarget before anything reads app.Image: the pull below, the phase lines,
	// and the Firecracker artifacts, which are exported from whatever it names.
	//
	// An explicit CS_SANDBOX_IMAGE wins — that is how CI pins the build and the
	// test run to one reference. Empty counts as unset, the way it was read at
	// startup; testing for presence instead would let
	// `CS_SANDBOX_IMAGE= cs-sandbox build --slim` publish a slim image under the
	// sandbox package.
	//
	// Each slim variant has a package of its own, because none of the three
	// images is interchangeable with another: a sandbox made from a slim one has
	// no toolchains, and letting it answer to the sandbox package would hand that
	// to a developer who asked for the real thing and leave them working out why
	// go had vanished. --with-agents earns a package for the same reason one step
	// down — it is ~730 MB larger and it is the only one of the two that can run
	// an agent, so a run that got the wrong one fails at `command -v claude`
	// rather than anywhere near the flag that chose it.
	if slim && os.Getenv("CS_SANDBOX_IMAGE") == "" {
		repo := slimImageRepo
		if withAgents {
			repo = slimAgentsImageRepo
		}
		ref, err := imageRef(repo)
		if err != nil {
			return err
		}
		app.Image = ref
	}
	if err := app.requireImage(); err != nil {
		return err
	}

	// Prefer the published image. It is named after the version that would build
	// it, so pulling is not a shortcut to a lesser thing — it is the same image,
	// in a fraction of the time. Nothing is published for a dirty tree or an
	// unpushed commit, so the pull simply misses there and the build runs, which
	// is also what stops a Containerfile you are editing from being quietly
	// replaced by somebody else's image.
	if !localSandbox && app.pullImage(cmd.Context()) {
		app.phase("pulled " + app.Image)
	} else if err := buildImage(cmd, app, slim, withAgents, localSandbox); err != nil {
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

// pullImage fetches the image this binary is named for, reporting whether it
// arrived. A miss is the ordinary case for an unpublished revision, so it is not
// an error and the caller builds instead; podman's own message is left on
// stderr only under --verbose, where the reason for a miss is worth seeing.
func (a *App) pullImage(ctx context.Context) bool {
	a.phase("looking for " + a.Image + " on the registry…")
	// Interactive, and -q unless --verbose: the same shape the build below uses.
	// A pull moves gigabytes, so podman's own progress is the only thing between
	// the phase line above and several silent minutes.
	argv := []string{"podman", "pull"}
	if !a.Verbose {
		argv = append(argv, "-q")
	}
	_, err := a.Runner.Run(ctx, run.Opts{Interactive: true}, append(argv, a.Image)...)
	// A dry run prints the pull and then has to reach the build as well. Pulling
	// is a mutation, so --dry-run skips the command and reports success; reading
	// that as a hit would end the run having printed nothing that makes an image.
	return err == nil && !a.dryRun()
}

// buildImage builds the sandbox image from the Containerfile. Split from
// runBuild so the pull above can stand beside it as the other way to arrive at
// the same image, rather than being an early return inside one long function.
func buildImage(cmd *cobra.Command, app *App, slim, withAgents, localSandbox bool) error {
	// The build assets come from the checkout when present, else from the
	// binary's embedded copy (so a downloaded binary can build the image).
	imgDir, cleanup, err := assets.ImageDir(app.AssetDir)
	if err != nil {
		return err
	}
	defer cleanup()
	containerfile := filepath.Join(imgDir, "Containerfile")
	if slim {
		if containerfile, err = slimContainerfile(cmd.Context(), app, imgDir, withAgents); err != nil {
			return err
		}
	}
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
		"-f", containerfile,
		"--build-arg", "BUILD_VERBOSE="+buildVerbose,
		"--build-arg", "CS_SANDBOX_PRIVATE_REGISTRY="+envOr("CS_SANDBOX_PRIVATE_REGISTRY", ""),
		"--build-arg", "CS_SANDBOX_PRIVATE_REGISTRY_INSECURE="+normalizeInsecure(envOr("CS_SANDBOX_PRIVATE_REGISTRY_INSECURE", "0")),
	)
	// The sibling cs- tools the image ships, at the versions go.mod pins rather
	// than at their branch tips. Passed rather than copied: the build context is
	// rootfs/, and `COPY . /sandbox` would put a stray manifest in the guest.
	//
	// A missing pin is fatal here rather than in the middle of a long build: the
	// Containerfile refuses an empty version, and saying so now costs seconds
	// instead of minutes.
	// cs-sandbox pins itself: a module cannot name its own version in its
	// manifest, but the running binary knows which revision built it.
	sbVersion, dirtyNote := sandboxPin()
	if sbVersion == "" {
		return errors.New("this cs-sandbox reports no module version, so the image has no cs-sandbox to install. " +
			"Build with `make build` from a git clone rather than with `go run`")
	}
	if dirtyNote != "" {
		app.phase(dirtyNote)
	}
	args = append(args, "--build-arg", "CS_SANDBOX_VERSION="+sbVersion,
		// The commit that version resolves to, for the revision label. Not
		// validated the way the versions are: a release version names no commit
		// on its own, so the label is worth having, but no build should fail for
		// want of it.
		"--build-arg", "CS_SANDBOX_REVISION="+sandboxRevision())

	// --local-sandbox: serve that version from a file:// proxy built out of this
	// checkout, so a revision nobody has pushed can still be installed BY
	// VERSION. Bind-mounted rather than copied: the build context is rootfs/,
	// and `COPY . /sandbox` would put the proxy inside every sandbox.
	if localSandbox {
		// A binary with no module version never reaches here — it cannot name the
		// image either, so the build refused before this. What remains is the
		// binary whose version came from -ldflags with no VCS stamp behind it:
		// it has a version to install by, and no commit to zip.
		rev := sandboxRevision()
		if rev == "" {
			return errors.New("--local-sandbox needs the revision this binary was built from, and its build info records none")
		}
		if app.AssetDir == "" {
			return errors.New("--local-sandbox needs a checkout to read the module from; set CS_SANDBOX_ASSETS_DIR or run from one")
		}
		proxyDir, proxyCleanup, err := localModuleProxy(app.AssetDir, sbVersion, rev)
		if err != nil {
			return err
		}
		defer proxyCleanup()
		app.phase(fmt.Sprintf("installing cs-sandbox %s from this checkout rather than the module proxy", sbVersion))
		args = append(args,
			"-v", proxyDir+":"+guestProxyDir+":ro",
			// The proxy is a host temp dir the invoking user owns, and on an
			// SELinux host the build's RUN steps are denied every read of it:
			// `go install` fails on the .info with "permission denied" and the
			// mount looks empty rather than forbidden. Confinement off rather
			// than a :z relabel, the same choice the run paths make — a relabel
			// also has to be undone, and it fails outright on the virtiofs
			// mounts a macOS podman machine serves host directories from.
			"--security-opt", "label=disable",
			// The real proxy still serves everything else, the Go toolchain
			// included: a bare file:// proxy 404s for those and the build stops.
			"--build-arg", "CS_GOPROXY=file://"+guestProxyDir+",https://proxy.golang.org,direct",
			// Scoped, not GOSUMDB=off: an unpublished module has no checksum-db
			// entry, but every other module must still be verified.
			"--build-arg", "CS_GONOSUMDB=github.com/codesweep-ai/*",
		)
	}

	pins, err := assets.ToolPins(app.AssetDir)
	if err != nil {
		return err
	}
	// ref is what to ask for when the pin is missing: the cs- tools track their
	// branch tip, and deadcode takes a release.
	for _, tool := range []struct{ arg, module, bin, ref string }{
		{"CS_LINT_VERSION", "github.com/codesweep-ai/lint", "cs-lint", "main"},
		{"CS_LEDGER_VERSION", "github.com/codesweep-ai/ledger", "cs-ledger", "main"},
		{"CS_TRACER_VERSION", "github.com/codesweep-ai/tracer", "cs-tracer", "main"},
		{"CS_DEADCODE_VERSION", "golang.org/x/tools", "deadcode", "latest"},
	} {
		v := pins[tool.module]
		if v == "" {
			return fmt.Errorf("go.mod pins no version for %s: run `go get -tool %s/cmd/%s@%s`",
				tool.module, tool.module, tool.bin, tool.ref)
		}
		args = append(args, "--build-arg", tool.arg+"="+v)
	}
	args = append(args,
		filepath.Join(imgDir, "rootfs"),
	)
	if _, err := app.Runner.Run(cmd.Context(), run.Opts{Interactive: true}, args...); err != nil {
		return err
	}
	return nil
}

// slimContainerfile derives the CI image's Containerfile from the real one and
// returns the path to write into. The derivation lives in image/ci-slim.sh —
// extracted alongside the Containerfile it reads — rather than being
// reimplemented here, so there is one description of what the slim image drops.
// A marker in that script going stale can then cost CI a slower image; it can
// never produce one that diverges from what the shipped Containerfile builds.
//
// Run through sh rather than executed: the extractor normalizes modes, and a
// temp dir can be mounted noexec. ReadOnly because the only thing written is
// this build's own temp tree, so a --dry-run still derives the file and prints
// a `podman build -f` naming one that exists.
func slimContainerfile(ctx context.Context, app *App, imgDir string, withAgents bool) (string, error) {
	script := filepath.Join(imgDir, "ci-slim.sh")
	if _, err := os.Stat(script); err != nil {
		return "", fmt.Errorf("--slim: the build assets carry no ci-slim.sh: %w", err)
	}
	keep := "0"
	if withAgents {
		keep = "1"
	}
	out := filepath.Join(imgDir, "Containerfile.ci")
	if _, err := app.Runner.Run(ctx, run.Opts{ReadOnly: true, StdoutFile: out, Env: []string{"CI_SLIM_KEEP_AGENTS=" + keep}},
		"sh", script, filepath.Join(imgDir, "Containerfile")); err != nil {
		return "", fmt.Errorf("--slim: deriving the slim Containerfile: %w", err)
	}
	return out, nil
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
