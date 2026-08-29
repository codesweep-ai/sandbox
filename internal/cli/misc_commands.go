package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
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
	var slim, localSandbox, rebuildBase bool
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Set up the sandbox image and, on capable hosts, the Firecracker artifacts",
		Long: "Set up the reusable, host-wide sandbox artifacts so later `create`s are fast.\n\n" +
			"With no flags it sets up every engine the host supports: the podman image always,\n" +
			"plus the Firecracker binary/kernel/rootfs on a Firecracker-capable host (and it fails\n" +
			"if the Firecracker host packages are missing). Restrict with --engine, e.g.\n" +
			"`--engine podman` for the image only, or `--engine firecracker` to force the FC set.\n\n" +
			"The image is built in three tiers, and by default this builds only the last of them:\n" +
			"the OS and toolchains (" + baseImageRepo + ")\n" +
			"and the agent CLIs on top of them (" + agentsImageRepo + ")\n" +
			"are pulled at the tag image/Containerfile pins, and what is built here is this\n" +
			"repository on top — about 25 MB against 2.2 GB. --rebuild-base builds all three\n" +
			"locally instead, which is what an edit to Containerfile.base or Containerfile.agents\n" +
			"needs; without it such an edit is silently ignored, because the tier it changed came\n" +
			"from the registry.\n\n" +
			"--slim builds the CI image instead: the same three Containerfiles with the developer\n" +
			"toolchains removed, ~700 MB against 9.3 GB and minutes against tens of them. It is\n" +
			"what makes booting real sandboxes on a hosted runner affordable: building the full one\n" +
			"on every push costs more time and disk than such a job has. It carries the agent CLIs\n" +
			"— there is no agent-free variant, because the one that existed saved ~325 MB on a CI\n" +
			"artifact and cost a seventh published package.\n\n" +
			"A slim build goes to " + slimImageRepo + ",\n" +
			"tagged with this cs-sandbox's version, unless CS_SANDBOX_IMAGE says otherwise — so it\n" +
			"can never be mistaken for the shipped image. Point the same variable at that reference\n" +
			"when running the tests.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBuild(cmd, app, engines, slim, localSandbox, rebuildBase)
		},
	}
	cmd.Flags().StringArrayVar(&engines, "engine", nil,
		"engine to set up: podman | firecracker (repeatable; default: every engine the host supports)")
	cmd.Flags().BoolVar(&slim, "slim", false,
		"build the slimmed CI image (no developer toolchains) instead of the shipped one")
	cmd.Flags().BoolVar(&rebuildBase, "rebuild-base", false,
		"build the OS/toolchain and agent tiers locally too, instead of pulling the tags image/Containerfile pins")
	cmd.Flags().BoolVar(&localSandbox, "local-sandbox", false,
		"install cs-sandbox in the image from this checkout's commit instead of from the module proxy, for a revision that is not pushed yet")
	_ = cmd.RegisterFlagCompletionFunc("engine", fixedComp("podman", "firecracker"))
	return cmd
}

func runBuild(cmd *cobra.Command, app *App, engines []string, slim, localSandbox, rebuildBase bool) error {
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
	// The slim image is a package of its own because it is not a sandbox:
	// ci-slim.sh strips every toolchain, so somebody who found it under the
	// sandbox package and pulled it would get a container that boots and then
	// has no go and no node. A package whose name says slim cannot be mistaken
	// that way.
	if slim && os.Getenv("CS_SANDBOX_IMAGE") == "" {
		ref, err := imageRef(slimImageRepo)
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
	//
	// --rebuild-base skips the pull outright: it was asked for precisely because
	// a lower tier changed, and the published image was built from the old one.
	if !localSandbox && !rebuildBase && app.pullImage(cmd.Context()) {
		app.phase("pulled " + app.Image)
	} else if err := buildImage(cmd, app, slim, localSandbox, rebuildBase); err != nil {
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
	// localhost/ is podman's name for an image that exists only in the local
	// store, so there is no registry to ask. Without this podman spends three
	// retries on https://localhost/v2/ before failing, on every build whose
	// CS_SANDBOX_IMAGE names a local tag, which is every build CI runs.
	if strings.HasPrefix(a.Image, "localhost/") {
		return false
	}
	a.phase("looking for " + a.Image + " on the registry…")
	// Progress unless --quiet, which is NOT the shape the build below uses. A
	// pull moves gigabytes, and podman's progress is the only thing between the
	// phase line above and several silent minutes: quietened, a working download
	// is indistinguishable from a hang. `podman build -q` is different, because
	// it suppresses a RUN's chatter rather than the one report of progress.
	argv := []string{"podman", "pull"}
	if a.Quiet {
		argv = append(argv, "-q")
	}
	_, err := a.Runner.Run(ctx, run.Opts{Interactive: true}, append(argv, a.Image)...)
	// A dry run prints the pull and then has to reach the build as well. Pulling
	// is a mutation, so --dry-run skips the command and reports success; reading
	// that as a hit would end the run having printed nothing that makes an image.
	return err == nil && !a.dryRun()
}

// bootstrapTag is what image/tiers.env carries before any tier has been
// published — the split had to land before the workflow that fills it in could
// run once.
const bootstrapTag = ":v0.0.0-BOOTSTRAP"

// tiers names the three Containerfiles one build reads, in build order.
type tiers struct{ base, agents, leaf string }

// localTierRefs names the tier images a --rebuild-base build leaves behind. They
// are localhost/ tags so nothing ever tries to push or pull them, and they are
// per-family so a slim rebuild cannot hand its toolchain-free base to a full
// build that asked for the real thing.
//
// Derived from the published repos rather than spelled out, so a locally built
// tier reads as the same thing as the one it stands in for. Spelled out, they
// drifted immediately: the published packages are sandbox-base and friends,
// and the local ones picked up a cs- prefix that nothing else here uses.
func localTierRefs(slim bool) (base, agents string) {
	b, a := baseImageRepo, agentsImageRepo
	if slim {
		b, a = slimBaseImageRepo, slimAgentsImageRepo
	}
	return localTierRef(b), localTierRef(a)
}

// localTierRef turns a published tier repo into the tag a --rebuild-base build
// writes: the same name, no registry, ":local".
func localTierRef(repo string) string {
	return "localhost/" + path.Base(repo) + ":local"
}

// buildTier runs one `podman build`. Every tier goes through here so the quiet
// flag, the build context and the argv shape are decided once.
func (a *App) buildTier(ctx context.Context, containerfile, tag, ctxDir string, buildArgs, extra []string) error {
	argv := []string{"podman", "build"}
	// BUILD_VERBOSE mirrors -q into the build itself. `podman build -q` only silences a
	// RUN's stdout — its stderr still reaches the terminal, and the steps that drive
	// `nvim --headless` write everything there. Those steps capture their own output and
	// replay it only on failure unless BUILD_VERBOSE=1, so --verbose stays fully verbose.
	if !a.Verbose {
		argv = append(argv, "-q")
	}
	argv = append(argv, "-t", tag, "-f", containerfile)
	for _, ba := range buildArgs {
		argv = append(argv, "--build-arg", ba)
	}
	argv = append(argv, extra...)
	argv = append(argv, ctxDir)
	_, err := a.Runner.Run(ctx, run.Opts{Interactive: true}, argv...)
	return err
}

// buildImage builds the sandbox image from the three tier Containerfiles. Split
// from runBuild so the pull above can stand beside it as the other way to arrive
// at the same image, rather than being an early return inside one long function.
//
// Only the leaf is built by default. The two tiers below it are published
// images that image/Containerfile names by tag, rebuilt on a schedule rather
// than per commit — see Containerfile.base for why. --rebuild-base builds all
// three here and wires the FROMs to the local ones.
func buildImage(cmd *cobra.Command, app *App, slim, localSandbox, rebuildBase bool) error {
	// The build assets come from the checkout when present, else from the
	// binary's embedded copy (so a downloaded binary can build the image).
	imgDir, cleanup, err := assets.ImageDir(app.AssetDir)
	if err != nil {
		return err
	}
	defer cleanup()

	files := tiers{
		base:   filepath.Join(imgDir, "Containerfile.base"),
		agents: filepath.Join(imgDir, "Containerfile.agents"),
		leaf:   filepath.Join(imgDir, "Containerfile"),
	}
	if slim {
		if files, err = slimContainerfiles(cmd.Context(), app, imgDir); err != nil {
			return err
		}
	}
	// rootfs/ is the context for every tier: it is what `COPY . /sandbox` and the
	// staged nvim config copy from, and a tier that copies nothing does not mind
	// being handed one.
	ctxDir := filepath.Join(imgDir, "rootfs")

	buildVerbose := "0"
	if app.Verbose {
		buildVerbose = "1"
	}

	// The tier this leaf is built on. --rebuild-base names the one it just made;
	// otherwise it is the pin in image/tiers.env, which is what a commit bumps.
	agentsRef := ""
	if rebuildBase {
		baseRef, localAgents := localTierRefs(slim)
		app.phase("building the OS/toolchain tier (this takes several minutes)…")
		if err := app.buildTier(cmd.Context(), files.base, baseRef, ctxDir,
			[]string{"BUILD_VERBOSE=" + buildVerbose}, nil); err != nil {
			return err
		}
		app.phase("building the agent tier…")
		if err := app.buildTier(cmd.Context(), files.agents, localAgents, ctxDir,
			[]string{"BASE_REF=" + baseRef}, nil); err != nil {
			return err
		}
		agentsRef = localAgents
	}

	if agentsRef == "" {
		pins, err := assets.TierPins(app.AssetDir)
		if err != nil {
			return fmt.Errorf("reading image/tiers.env, which names the tier this image builds on: %w", err)
		}
		key := "AGENTS_REF"
		if slim {
			key = "SLIM_AGENTS_REF"
		}
		if agentsRef = pins[key]; agentsRef == "" {
			return fmt.Errorf("image/tiers.env names no %s, so there is no base to build on. "+
				"Bump it to a published tier, or pass --rebuild-base to build the tiers locally", key)
		}
		// The placeholder the split shipped with. Said rather than raised: the
		// build below is the thing that fails, and podman's own answer —
		// "manifest unknown" — names the tag without naming the remedy. This
		// puts the remedy on screen first and leaves the failure where it is.
		if strings.HasSuffix(agentsRef, bootstrapTag) {
			app.phase(fmt.Sprintf("image/tiers.env still names the placeholder %s for %s, so no tier "+
				"has been published for this build to stand on. Run the base-images workflow and paste "+
				"the two lines it prints, or pass --rebuild-base to build all three tiers locally",
				bootstrapTag, key))
		}
	}

	app.phase(fmt.Sprintf("building image %s…", app.Image))
	// Generic image — no identity baked in.
	args := []string{
		"CS_SANDBOX_PRIVATE_REGISTRY=" + envOr("CS_SANDBOX_PRIVATE_REGISTRY", ""),
		"CS_SANDBOX_PRIVATE_REGISTRY_INSECURE=" + normalizeInsecure(envOr("CS_SANDBOX_PRIVATE_REGISTRY_INSECURE", "0")),
	}
	args = append(args, "AGENTS_REF="+agentsRef)

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
	args = append(args,
		"CS_SANDBOX_VERSION="+sbVersion,
		// The commit that version resolves to, for the revision label. Not
		// validated the way the versions are: a release version names no commit
		// on its own, so the label is worth having, but no build should fail for
		// want of it.
		"CS_SANDBOX_REVISION="+sandboxRevision())

	var extra []string

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
		extra = append(extra,
			"-v", proxyDir+":"+guestProxyDir+":ro",
			// The proxy is a host temp dir the invoking user owns, and on an
			// SELinux host the build's RUN steps are denied every read of it:
			// `go install` fails on the .info with "permission denied" and the
			// mount looks empty rather than forbidden. Confinement off rather
			// than a :z relabel, the same choice the run paths make — a relabel
			// also has to be undone, and it fails outright on the virtiofs
			// mounts a macOS podman machine serves host directories from.
			"--security-opt", "label=disable")
		args = append(args,
			// The real proxy still serves everything else, the Go toolchain
			// included: a bare file:// proxy 404s for those and the build stops.
			"CS_GOPROXY=file://"+guestProxyDir+",https://proxy.golang.org,direct",
			// Scoped, not GOSUMDB=off: an unpublished module has no checksum-db
			// entry, but every other module must still be verified.
			"CS_GONOSUMDB=github.com/codesweep-ai/*")
	}

	pins, err := assets.ToolPins(app.AssetDir)
	if err != nil {
		return err
	}
	for _, tool := range []struct{ arg, module, bin string }{
		{"CS_LINT_VERSION", "github.com/codesweep-ai/lint", "cs-lint"},
		{"CS_LEDGER_VERSION", "github.com/codesweep-ai/ledger", "cs-ledger"},
		{"CS_TRACER_VERSION", "github.com/codesweep-ai/tracer", "cs-tracer"},
		{"CS_VCR_VERSION", "github.com/codesweep-ai/vcr", "cs-vcr"},
	} {
		v := pins[tool.module]
		if v == "" {
			return fmt.Errorf("go.mod pins no version for %s: run `go get -tool %s/cmd/%s@main`",
				tool.module, tool.module, tool.bin)
		}
		args = append(args, tool.arg+"="+v)
	}
	return app.buildTier(cmd.Context(), files.leaf, app.Image, ctxDir, args, extra)
}

// slimContainerfiles derives the CI image's three Containerfiles from the real
// ones and returns the paths to build from. The derivation lives in
// image/ci-slim.sh — extracted alongside the Containerfiles it reads — rather
// than being reimplemented here, so there is one description of what the slim
// image drops. A marker in that script going stale can then cost CI a slower
// image; it can never produce one that diverges from what the shipped
// Containerfiles build.
//
// Run through sh rather than executed: the extractor normalizes modes, and a
// temp dir can be mounted noexec. ReadOnly because the only thing written is
// this build's own temp tree, so a --dry-run still derives the files and prints
// a `podman build -f` naming ones that exist.
func slimContainerfiles(ctx context.Context, app *App, imgDir string) (tiers, error) {
	var out tiers
	script := filepath.Join(imgDir, "ci-slim.sh")
	if _, err := os.Stat(script); err != nil {
		return out, fmt.Errorf("--slim: the build assets carry no ci-slim.sh: %w", err)
	}
	for _, t := range []struct{ tier, out string }{
		{"base", "Containerfile.base.ci"},
		{"agents", "Containerfile.agents.ci"},
		{"leaf", "Containerfile.ci"},
	} {
		dst := filepath.Join(imgDir, t.out)
		if _, err := app.Runner.Run(ctx, run.Opts{ReadOnly: true, StdoutFile: dst},
			"sh", script, t.tier); err != nil {
			return out, fmt.Errorf("--slim: deriving the slim %s Containerfile: %w", t.tier, err)
		}
		switch t.tier {
		case "base":
			out.base = dst
		case "agents":
			out.agents = dst
		default:
			out.leaf = dst
		}
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
