package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/spec"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/codesweep-ai/sandbox/internal/store"
	"github.com/spf13/cobra"
)

// autoEngine picks the default engine when --engine is unset: firecracker on an
// x86_64 Linux host with a usable /dev/kvm, else podman (macOS / non-KVM / non-
// amd64). The Firecracker engine ships an x86_64 guest kernel, so it is gated on
// amd64 even when /dev/kvm is present on another arch.
func autoEngine(isMacOS bool) string {
	if isMacOS || runtime.GOARCH != "amd64" {
		return "podman"
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		_ = f.Close()
		return "firecracker"
	}
	return "podman"
}

type createFlags struct {
	group             string
	typ               string
	engine            string
	yolo              bool
	solo              bool
	privileged        bool
	inheritAgentLogin []string
	cpus, mem         int
	repos             []string
	snapshots         []string
	envs              []string
	envFiles          []string
	imageStores       []string
}

func newCreateCmd(app *App) *cobra.Command {
	f := &createFlags{}
	cmd := &cobra.Command{
		Use:   "create <name> [flags]",
		Short: "Create and start a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCreate(cmd.Context(), app, args[0], f, cmd)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.group, "group", envOr("CS_SANDBOX_GROUP", state.DefaultGroup),
		"group whose isolated network, SSH keys and gateway this sandbox joins")
	fl.StringVar(&f.typ, "type", "agent", "sandbox type: agent | user")
	fl.StringVar(&f.engine, "engine", envOr("CS_SANDBOX_ENGINE", ""), "engine: podman | firecracker (default: firecracker on Linux/KVM, else podman)")
	fl.BoolVar(&f.yolo, "yolo", false, "skip all agent permission prompts")
	fl.BoolVar(&f.solo, "solo", false, "agent with no outbound SSH into the fabric (agent type only)")
	fl.BoolVar(&f.privileged, "privileged", false, "podman: use --privileged instead of the scaled-down cap set")
	fl.StringSliceVar(&f.inheritAgentLogin, "inherit-agent-login", nil,
		"inherit this agent's host login into the sandbox: "+strings.Join(seed.AgentNames(), " | ")+" (repeatable, comma-separated; default: inherit nothing)")
	fl.IntVar(&f.cpus, "cpus", 4, "firecracker: vCPUs")
	fl.IntVar(&f.mem, "mem", 4096, "firecracker: memory (MiB)")
	fl.StringArrayVar(&f.repos, "repo", nil, "share a git repo: PATH[@REF][:NAME] (repeatable)")
	fl.StringArrayVar(&f.snapshots, "snapshot", nil, "share a frozen dir copy: PATH[:NAME] (repeatable)")
	fl.StringArrayVarP(&f.envs, "env", "e", nil, "inject env var: KEY=VALUE or KEY (repeatable)")
	fl.StringArrayVar(&f.envFiles, "env-file", nil, "inject env vars from a file (repeatable)")
	fl.StringArrayVar(&f.imageStores, "image-store", nil, "mount a shared image store read-only (repeatable)")

	// flag-value completion
	_ = cmd.RegisterFlagCompletionFunc("engine", fixedComp("podman", "firecracker"))
	_ = cmd.RegisterFlagCompletionFunc("type", fixedComp("agent", "user"))
	_ = cmd.RegisterFlagCompletionFunc("image-store", func(c *cobra.Command, _ []string, tc string) ([]string, cobra.ShellCompDirective) {
		return app.storeMatches(c, tc), cobra.ShellCompDirectiveNoFileComp
	})
	// the positional arg is a NEW name; don't offer file/name completions for it.
	cmd.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runCreate(ctx context.Context, app *App, name string, f *createFlags, cmd *cobra.Command) error {
	// Creation is the single gate for sandbox names: everything downstream (the
	// instance dir, the managed ssh config, the fabric hosts entry) derives from
	// an instance that got past here.
	if err := state.ValidName(name); err != nil {
		return err
	}
	if f.typ != "agent" && f.typ != "user" {
		return fmt.Errorf("--type must be agent or user, got %q", f.typ)
	}
	if f.solo && f.typ != "agent" {
		return fmt.Errorf("--solo is only valid for agent sandboxes")
	}
	if err := state.ValidGroup(f.group); err != nil {
		return err
	}
	if f.engine == "" {
		f.engine = autoEngine(app.Host.IsMacOS) // firecracker on Linux/KVM, else podman
	}
	// Firecracker keeps its sockets in the instance directory, and both names
	// are legal on their own by here — only together can they overrun the
	// 108-byte path budget. Checked before anything is provisioned; podman
	// sandboxes have no such path and are unconstrained.
	if f.engine == "firecracker" {
		if err := state.ValidInstancePath(app.InstDir, f.group, name); err != nil {
			return err
		}
	}
	if app.exists(f.group, name) {
		return fmt.Errorf("sandbox %q already exists in group %q", name, f.group)
	}

	// Resolve directory-sharing specs against the host.
	opt := spec.Options{IsMacOS: app.Host.IsMacOS, Home: app.Host.Home}
	snaps, err := spec.ResolveSnapshots(f.snapshots, opt)
	if err != nil {
		return err
	}
	repos, err := spec.ResolveRepoClones(f.repos, opt)
	if err != nil {
		return err
	}
	for _, a := range f.inheritAgentLogin {
		if !seed.ValidAgent(a) {
			return fmt.Errorf("--inherit-agent-login: unknown agent %q: use one of %s",
				a, strings.Join(seed.AgentNames(), ", "))
		}
	}
	seenStores := make(map[string]struct{}, len(f.imageStores))
	for _, name := range f.imageStores {
		if err := store.ValidName(name); err != nil {
			return err
		}
		if _, ok := seenStores[name]; ok {
			return fmt.Errorf("duplicate image store %q", name)
		}
		seenStores[name] = struct{}{}
	}

	// Resolve --env / --env-file.
	injected, warns := resolveEnv(f)
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "cs-sandbox: "+w)
	}

	// The group's artifacts (network, keys, gateway) and its record must exist
	// before Deps is built: the engines take a COPY of Deps, so a field set
	// afterwards — the allocated tap prefix — would never reach them.
	app.progress("preparing the group's isolated network and trust keys…")
	if _, err := app.ensureGroup(ctx, f.group); err != nil {
		return err
	}
	d := app.engineDepsFor(f.group)
	var eng engine.Engine
	switch f.engine {
	case "podman":
		eng = engine.NewPodman(d)
	case "firecracker":
		if f.cpus <= 0 {
			return fmt.Errorf("--cpus must be greater than zero")
		}
		if f.mem <= 0 {
			return fmt.Errorf("--mem must be greater than zero")
		}
		eng = engine.NewFirecracker(d)
	default:
		return fmt.Errorf("--engine must be podman or firecracker, got %q", f.engine)
	}
	// The build artifacts (image / firecracker cache) are `cs-sandbox build`'s
	// job. Fail with an actionable message rather than building them under the
	// covers, so create stays fast and predictable.
	if err := eng.Verify(ctx); err != nil {
		return err
	}

	if err := d.EnsureNetwork(ctx); err != nil {
		return err
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		return err
	}

	cs := engine.CreateSpec{
		Name: name, Group: f.group, Type: f.typ, Yolo: f.yolo, Solo: f.solo, Privileged: f.privileged,
		CPUs: f.cpus, MemMiB: f.mem, Snapshots: snaps, RepoClones: repos,
		ImageStores: f.imageStores, InjectedEnv: injected, InheritAgentLogin: f.inheritAgentLogin,
	}
	inst, err := eng.Create(ctx, cs)
	if err != nil {
		return err
	}
	// A recreated name gets fresh per-instance host keys; drop any stale entry
	// (keyed by HostKeyAlias=<name>) so accept-new relearns it instead of failing
	// with "host key changed" when a name or freed port is reused.
	kh := filepath.Join(app.Host.SSHDir(), "known_hosts.cs-sandbox")
	_, _ = app.Runner.Run(ctx, run.Opts{}, "ssh-keygen", "-R", name, "-f", kh)
	if err := app.syncSSHConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "cs-sandbox: warning: could not update ssh config: %v\n", err)
	}
	app.refreshHostRoute(cmd) // republish names if host-route is on (rootless, best-effort)

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "created %s (type=%s, engine=%s, ssh port=%d)\n", name, f.typ, f.engine, inst.Port)
	fmt.Fprintf(out, "  shell: ssh %s\n", name+"."+f.group)
	if len(inst.AgentLogins) > 0 {
		fmt.Fprintf(out, "  agent login: %s (inherited from your host)\n", strings.Join(inst.AgentLogins, " + "))
	} else {
		fmt.Fprintf(out, "  agent login: none — add --inherit-agent-login %s, or run 'cs-sandbox agent-login %s %s'\n",
			seed.AgentNames()[0], seed.AgentNames()[0], name)
	}
	for _, sn := range snaps {
		fmt.Fprintf(out, "  snapshot: %s -> ~/%s (read-only, frozen at create)\n", sn.HostPath, sn.Name)
	}
	for _, rc := range repos {
		fmt.Fprintf(out, "  repo:     ~/%s on branch cs-sandbox/%s\n", rc.Name, name)
	}
	return nil
}

// resolveEnv builds the injected env block from --env tokens and --env-file
// contents, using the process environment for bare-key passthrough.
func resolveEnv(f *createFlags) (string, []string) {
	var fileSets [][]string
	var warns []string
	for _, path := range f.envFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			warns = append(warns, fmt.Sprintf("--env-file %s: %v", path, err))
			continue
		}
		fileSets = append(fileSets, strings.Split(string(data), "\n"))
	}
	block, w := seed.ResolveInjectedEnv(f.envs, fileSets, os.LookupEnv)
	return block, append(warns, w...)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
