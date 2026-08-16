package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/ports"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/spec"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Podman is the rootless-container engine adapter. It drives the podman CLI
// through the Runner, so every command it issues is assertable in tests.
type Podman struct{ d Deps }

// NewPodman constructs the podman adapter.
func NewPodman(d Deps) *Podman { return &Podman{d: d} }

func (p *Podman) Name() state.Engine { return state.Podman }

// runParams are the fully-resolved inputs to the podman run argv. Kept separate
// from Create so buildRunArgs is a pure, golden-testable function.
type runParams struct {
	Name       string // guest hostname and in-network DNS alias (bare)
	Obj        string // host-global podman object name (<name>.<group>)
	Network    string // the group's isolated network
	Type       string
	Port       int
	SSHBind    string
	IntPort    int
	DNSPrimary string // forwarding dnsmasq (<prefix>.53)
	DNSGateway string
	Privileged bool
	Yolo       bool
	Solo       bool
	User       string
	UID, GID   int
	// Group is the host's primary group NAME, passed only on macOS: the gid there
	// (20 = staff) collides with an unrelated system group in the image, so the
	// entrypoint renames it to match the host. Empty on Linux, where podman's
	// keep-id entry already carries the right name and renaming a system group
	// would be a gratuitous change.
	Group     string
	Home      string
	TZ        string
	HomeVol   string
	ContVol   string // nested podman's store, mounted at the user's rootless graphroot
	SeedDir   string
	Image     string
	EnvFile   string   // path to the seed's inject-env (--env KEY=VALUE lines), if any
	Stores    []string // container paths (/var/lib/shared/<name>) for CS_SANDBOX_IMAGE_STORES
	StoreVols []string // "vol:/var/lib/shared/<name>:ro"
	Mounts    []string // extra -v specs (snapshots, repos)
}

// obj is the host-global name of a sandbox's podman objects (container and
// volumes). See state.ObjectName: every layer that addresses a sandbox from
// outside spells it the same way.
func obj(group, name string) string { return state.ObjectName(group, name) }

// splitObj is the inverse of obj: a qualified object name back to (group, name).
// An unqualified string is treated as a default-group member.
func splitObj(ref string) (group, name string) {
	if i := strings.LastIndex(ref, "."); i > 0 && i < len(ref)-1 {
		return ref[i+1:], ref[:i]
	}
	return state.DefaultGroup, ref
}

// macOSGroup returns the host's primary group name on macOS only, so the guest
// can name the gid the way the host does. Linux keeps whatever name the image
// (or podman's keep-id entry) already has for that gid.
func macOSGroup(h hostenv.Host) string {
	if !h.IsMacOS {
		return ""
	}
	return h.Group
}

// buildRunArgs assembles the full `podman run` argv, in a fixed order (so a
// golden test can pin it). Pure.
func buildRunArgs(p runParams) []string {
	a := []string{
		"podman", "run", "-d",
		"--name", p.Obj, "--hostname", p.Name,
		"--network", p.Network, "--network-alias", p.Name,
		"-p", fmt.Sprintf("%s:%d:%d", p.SSHBind, p.Port, p.IntPort),
		"--dns", p.DNSPrimary, "--dns", p.DNSGateway,
		"--init",
	}
	if p.Privileged {
		a = append(a, "--privileged")
	} else {
		// What nested ROOTLESS podman needs, and nothing more (SPEC.md §11.2):
		//   SYS_ADMIN  — the inner engine's namespaces and mounts.
		//   SETFCAP    — so /sandbox/nested-rootless can grant newuidmap/newgidmap
		//                the file caps rootless podman writes uid_maps with.
		//   /dev/net/tun — pasta/slirp4netns, how a rootless inner container reaches
		//                the network.
		//   unmask=ALL — the inner container mounts a fresh procfs in a NEW user
		//                namespace, which the kernel refuses while any of this
		//                container's /proc is masked ("mount `proc` to `proc`:
		//                Operation not permitted"). Cheaper than it reads: the
		//                container is rootless, so its root is an unprivileged
		//                subuid and the kernel still denies /proc/kcore,
		//                sysrq-trigger and the non-namespaced sysctls on its own.
		// Seccomp stays on, and the container still sees only the devices above.
		a = append(a,
			"--cap-add=SYS_ADMIN", "--cap-add=SETFCAP",
			"--device", "/dev/net/tun",
			"--security-opt", "unmask=ALL",
		)
	}
	a = append(a,
		"--security-opt", "label=disable",
		"--cap-add=NET_RAW", "--cap-add=NET_BIND_SERVICE",
		"--sysctl", fmt.Sprintf("net.ipv4.ping_group_range=%d %d", p.GID, p.GID),
		"--userns=keep-id",
		"--user", "0:0",
		"-e", "TZ="+p.TZ,
		"-e", "CS_SANDBOX_TYPE="+p.Type,
		"-e", "CS_SANDBOX_YOLO="+boolFlag(p.Yolo),
		"-e", fmt.Sprintf("CS_SANDBOX_SSH_PORT=%d", p.IntPort),
		"-e", "CS_SANDBOX_USER="+p.User,
		"-e", fmt.Sprintf("CS_SANDBOX_UID=%d", p.UID),
		"-e", fmt.Sprintf("CS_SANDBOX_GID=%d", p.GID),
		"-e", "CS_SANDBOX_HOME="+p.Home,
	)
	if p.Group != "" {
		a = append(a, "-e", "CS_SANDBOX_GROUP="+p.Group)
	}
	// Derived from Obj rather than carried separately, so the label can never
	// disagree with the object name it describes. (podmanSpec.Group is the host's
	// unix group on macOS — a different thing entirely.)
	sandboxGroup, _ := splitObj(p.Obj)
	a = append(a,
		"--label", "cs-sandbox.managed=1",
		"--label", "cs-sandbox.type="+p.Type,
		"--label", fmt.Sprintf("cs-sandbox.ssh_port=%d", p.Port),
		"--label", "cs-sandbox.name="+p.Name,
		"--label", "cs-sandbox.group="+sandboxGroup,
		"-v", p.HomeVol+":"+p.Home,
		// The nested engine is rootless, so its store is the rootless graphroot under
		// the user's home — the same path the microVM uses. Its own volume all the
		// same (SPEC.md §11.3): nested images then survive recreation, stay out of the
		// home volume, and land on a non-overlay filesystem where the kernel's native
		// overlay driver works. Mounted deeper than the home volume, which podman
		// orders correctly for us.
		"-v", p.ContVol+":"+p.Home+"/.local/share/containers",
		"-v", p.SeedDir+":/run/cs-sandbox-seed:ro",
	)
	if p.Yolo {
		a = append(a, "--label", "cs-sandbox.yolo=1")
	}
	if p.Solo {
		a = append(a, "--label", "cs-sandbox.solo=1")
	}
	// Injected --env/--env-file values are handed over as a file, never as `-e
	// KEY=VALUE` argv: argv is world-readable via /proc/<pid>/cmdline while the
	// process runs, and these values are secrets (the seed file is mode 0600).
	if p.EnvFile != "" {
		a = append(a, "--env-file", p.EnvFile)
	}
	for _, v := range p.StoreVols {
		a = append(a, "-v", v)
	}
	if len(p.Stores) > 0 {
		a = append(a, "-e", "CS_SANDBOX_IMAGE_STORES="+strings.Join(p.Stores, ":"))
	}
	for _, m := range p.Mounts {
		a = append(a, "-v", m)
	}
	a = append(a, p.Image, "sleep", "infinity")
	return a
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Create provisions and starts a podman-container sandbox.
func (p *Podman) Create(ctx context.Context, s CreateSpec) (inst *state.Instance, err error) {
	d := p.d
	idir := d.InstanceDir(s.Name)
	seedDir := filepath.Join(idir, "seed")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return nil, err
	}

	gw := d.networkGateway(ctx)
	dnsPrimary := dnsmasqIP(gw)

	// Build the trust seed (authorized_keys, tier key, ssh_config, host keys, …).
	agentLogins, err := d.writeSeed(ctx, seedDir, s, gw)
	if err != nil {
		return nil, err
	}

	// Materialize --snapshot copies and --repo mounts, and the repos manifest.
	mounts, err := d.materializeShares(ctx, idir, seedDir, s)
	if err != nil {
		return nil, err
	}

	// Resolve image-store volumes.
	var storeVols, storePaths []string
	for _, name := range s.ImageStores {
		vol := "cs-sandbox-shared-" + name
		if !d.volumeExists(ctx, vol) {
			return nil, fmt.Errorf("shared store %q does not exist — create it with: cs-sandbox seed-store %s <image>", name, name)
		}
		storeVols = append(storeVols, fmt.Sprintf("%s:/var/lib/shared/%s:ro", vol, name))
		storePaths = append(storePaths, "/var/lib/shared/"+name)
	}

	params := runParams{
		Name: s.Name, Obj: obj(p.d.group(), s.Name), Network: d.Network,
		Type: s.Type, Port: 0, SSHBind: d.SSHBind, IntPort: 22,
		DNSPrimary: dnsPrimary, DNSGateway: gw, Privileged: s.Privileged,
		Yolo: s.Yolo, Solo: s.Solo, User: d.Host.User, UID: d.Host.UID, GID: d.Host.GID,
		Group: macOSGroup(d.Host), Home: "/home/" + d.Host.User, TZ: d.TZ,
		HomeVol: "cs-sandbox-home-" + obj(p.d.group(), s.Name), ContVol: "cs-sandbox-containers-" + obj(p.d.group(), s.Name),
		SeedDir: seedDir, Image: d.Image,
		EnvFile: envFilePath(seedDir, s.InjectedEnv), Stores: storePaths, StoreVols: storeVols, Mounts: mounts,
	}
	inst = &state.Instance{
		Name: s.Name, Group: p.d.group(), Type: s.Type, Engine: state.Podman, Yolo: s.Yolo, Solo: s.Solo,
		Shared: s.ImageStores, AgentLogins: agentLogins, Created: nowUTC(),
	}
	// Serialize the race-sensitive prefix — port allocation through the claim —
	// as the firecracker engine does for its ip+port. Without this, two parallel
	// creates can probe the same free port and the loser fails at `podman run`.
	l := lock.New(d.InstDir)
	if err := l.Acquire(); err != nil {
		return nil, err
	}
	locked := true
	unlock := func() {
		if locked {
			l.Release()
			locked = false
		}
	}
	defer unlock()

	// The host SSH port must be resolved before rendering argv (it is published).
	port, err := d.allocPodmanPort(ctx)
	if err != nil {
		return nil, err
	}
	inst.Port = port
	params.Port = inst.Port
	for _, sn := range s.Snapshots {
		inst.Snapshots = append(inst.Snapshots, sn.HostPath+":"+sn.Name)
	}
	for _, rc := range s.RepoClones {
		inst.RepoClones = append(inst.RepoClones, state.RepoClone{
			Source: rc.HostPath, Dir: rc.Name, Branch: state.BranchName(s.Group, s.Name),
		})
	}

	argv := buildRunArgs(params)
	d.say("starting the container…")
	reusingData := d.volumeExists(ctx, params.HomeVol)
	if _, err = d.Runner.Run(ctx, run.Opts{}, argv...); err != nil {
		return nil, fmt.Errorf("podman run: %w", err)
	}
	started := true
	defer func() {
		if err != nil && started {
			// Preserve a home volume kept by an earlier `rm`; otherwise remove
			// the partial container and the data volumes it just created.
			_ = p.Remove(context.Background(), s.Name, !reusingData)
		}
	}()

	if err = state.Save(d.InstDir, inst); err != nil {
		return nil, err
	}
	unlock() // the port is bound and claimed; let other creates proceed
	if err = d.waitReady(ctx, s.Name); err != nil {
		return nil, err
	}
	started = false
	return inst, nil
}

// Prepare is a no-op: the podman engine's only reusable artifact is the shared
// image, which `cs-sandbox build` builds directly.
func (p *Podman) Prepare(ctx context.Context) error { return nil }

// Verify confirms the shared image exists, so create fails cleanly (pointing at
// build) instead of with a raw "image not known" from podman run.
func (p *Podman) Verify(ctx context.Context) error {
	if _, err := p.d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", p.d.Image); err != nil {
		return fmt.Errorf("sandbox image %q not built — run: cs-sandbox build", p.d.Image)
	}
	return nil
}

func (p *Podman) Start(ctx context.Context, name string) error {
	// Re-establish the shared network before starting the container, in case it
	// went away (a host reboot, or a `podman network` reset) — mirrors create, so
	// `start` after a reboot brings the sandbox back fully. (Podman uses podman's
	// own network + published port; the fcnet keepalive/dnsmasq fabric is
	// Firecracker-only, so there's nothing else to bring up here.)
	if err := p.d.EnsureNetwork(ctx); err != nil {
		return err
	}
	if _, err := p.d.Runner.Run(ctx, run.Opts{}, "podman", "start", obj(p.d.group(), name)); err != nil {
		return err
	}
	return p.d.waitReady(ctx, name)
}

func (p *Podman) Stop(ctx context.Context, name string) error {
	_, err := p.d.Runner.Run(ctx, run.Opts{}, "podman", "stop", obj(p.d.group(), name))
	return err
}

func (p *Podman) Remove(ctx context.Context, name string, purge bool) error {
	_, _ = p.d.Runner.Run(ctx, run.Opts{}, "podman", "rm", "-f", obj(p.d.group(), name))
	if purge {
		// destroy: also drop the data volumes (home + nested-containers store).
		for _, v := range []string{"cs-sandbox-home-" + obj(p.d.group(), name), "cs-sandbox-containers-" + obj(p.d.group(), name)} {
			_, _ = p.d.Runner.Run(ctx, run.Opts{}, "podman", "volume", "rm", "-f", v)
		}
	}
	// Always drop the instance state so the sandbox leaves `ls` and the name is
	// createable again. On `rm` the home volume is kept, so `create <name>` reuses
	// it (podman remounts the existing named volume); only `destroy` deletes it.
	_ = os.RemoveAll(p.d.InstanceDir(name))
	return nil
}

func (p *Podman) Exec(ctx context.Context, name string, io ExecIO) error {
	// -i and -t are independent podman flags: -i forwards stdin into the
	// container, -t allocates a TTY. They used to be passed together as one -it
	// token, added only for an interactive shell — so a one-shot command
	// declined the TTY and lost stdin with it, dropping anything piped in and
	// still reporting exit 0. ssh forwards stdin whether or not it allocates a
	// TTY, so the firecracker engine never had the defect; -i unconditionally
	// makes `exec` mean the same on both engines.
	argv := []string{"podman", "exec", "-i"}
	if io.Interactive {
		argv = append(argv, "-t")
	}
	// Run as the dev user in their home, matching what `ssh <name>` gives and what the
	// firecracker engine does over ssh. The container's main process runs as uid 0, so
	// without this every command would run as root with HOME=/root — the wrong agent
	// profile, and any file it creates owned by root.
	// obj(), not the bare name: the container is created as <name>.<group>, so
	// asking podman for the bare name finds nothing in any group, default
	// included. Every other method here already addresses it this way.
	argv = append(argv, "--user", p.d.Host.User, "--workdir", "/home/"+p.d.Host.User,
		obj(p.d.group(), name))
	if len(io.Argv) > 0 {
		argv = append(argv, io.Argv...)
	} else {
		argv = append(argv, "bash", "-l")
	}
	// Attach the host process's stdio to the podman CLI, so a one-shot command's
	// output streams to the user and its stdin is the caller's; -i above carries
	// that stdin the rest of the way, into the container.
	_, err := p.d.Runner.Run(ctx, run.Opts{Interactive: true}, argv...)
	return err
}

func (p *Podman) Port(ctx context.Context, name string) (int, error) {
	in, err := state.Load(p.d.InstDir, p.d.group(), name)
	if err != nil {
		return 0, err
	}
	return in.Port, nil
}

// --- shared Deps helpers used by adapters ---

func (d Deps) networkGateway(ctx context.Context) string {
	res, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "network", "inspect", d.Network,
		"--format", "{{(index .Subnets 0).Gateway}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

func (d Deps) volumeExists(ctx context.Context, vol string) bool {
	_, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "volume", "exists", vol)
	return err == nil
}

func (d Deps) allocPodmanPort(ctx context.Context) (int, error) {
	reserved := d.reservedPorts(ctx)
	return allocPort(ports.Min, ports.Split-1, reserved)
}

func (d Deps) waitReady(ctx context.Context, name string) error {
	d.say("waiting for the sandbox to be ready…")
	budget := time.Duration(d.StartTimeout) * time.Second
	deadline := time.Now().Add(budget)
	for {
		if _, err := d.Runner.Run(ctx, run.Opts{}, "podman", "exec", obj(d.group(), name), "test", "-f", "/run/cs-sandbox-ready"); err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for sandbox %q readiness: %w", name, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("sandbox %q did not become ready within %s", name, budget)
		}
		delay := min(time.Second, remaining)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for sandbox %q readiness: %w", name, ctx.Err())
		case <-timer.C:
		}
	}
}

func dnsmasqIP(gw string) string {
	if i := strings.LastIndexByte(gw, '.'); i > 0 {
		return gw[:i] + ".53"
	}
	return gw
}

// hostReachableIP is the address at which the host itself is reachable from
// inside a sandbox on the rootless network: pasta's host-loopback mapping, the
// same address Podman exposes as host.containers.internal. It is reachable from
// both Podman containers and Firecracker taps, which share the one rootless
// network namespace — so a single mapping serves both engines.
const hostReachableIP = "169.254.1.2"

// hostHostsLine builds the "<ip> <name…>" line that pins the host's own
// name(s) to hostReachableIP in the guest's /etc/hosts, so `ssh <hostname>` /
// `curl <hostname>:PORT` from a sandbox reach the host. Returns "" when the host
// exposes no usable name.
func hostHostsLine(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return hostReachableIP + " " + strings.Join(names, " ")
}

// envFilePath returns the seed's inject-env path when there is injected env to
// pass (the seed writer wrote that file, mode 0600), or "" for none. Handing
// podman a file rather than `-e KEY=VALUE` keeps the values out of argv.
func envFilePath(seedDir, block string) string {
	if strings.TrimSpace(block) == "" {
		return ""
	}
	return filepath.Join(seedDir, "inject-env")
}

// writeSeed resolves seed inputs from the host + spec and materializes the seed.
func (d Deps) writeSeed(ctx context.Context, seedDir string, s CreateSpec, gw string) ([]string, error) {
	hostPubs, _ := d.Host.PubKeys()
	userPub, _ := os.ReadFile(filepath.Join(d.TierDir, seed.TierUserKey+".pub"))
	agentPub, _ := os.ReadFile(filepath.Join(d.TierDir, seed.TierAgentKey+".pub"))
	typ := seed.Type(s.Type)
	tierName := seed.TierKey(typ, s.Solo)
	in := seed.Input{
		Type: typ, Solo: s.Solo,
		HostPubs: hostPubs, UserTierPub: string(userPub), AgentTierPub: string(agentPub),
		TierName: tierName, FabricGW: gw, InjectedEnv: s.InjectedEnv,
		HostHosts: hostHostsLine(d.Host.Names),
	}
	if tierName != "" {
		in.TierPrivPath = filepath.Join(d.TierDir, tierName)
		in.TierPubPath = filepath.Join(d.TierDir, tierName+".pub")
	}
	in.GitIdent = d.globalGitIdentity(ctx)
	if err := seed.Write(ctx, d.Runner, seedDir, in); err != nil {
		return nil, err
	}
	// Carry the host login of each agent the user asked to inherit (opt-in).
	return seed.WriteAgentLogins(seedDir, d.Host.Home, s.InheritAgentLogin, d.note)
}

func (d Deps) globalGitIdentity(ctx context.Context) seed.GitIdentity {
	return seed.GitIdentity{
		Name:  run.Output(ctx, d.Runner, "git", "config", "--global", "user.name"),
		Email: run.Output(ctx, d.Runner, "git", "config", "--global", "user.email"),
	}
}

// materializeShares copies --snapshot dirs and writes the seed repos manifest,
// returning the extra -v mount specs.
func (d Deps) materializeShares(ctx context.Context, idir, seedDir string, s CreateSpec) ([]string, error) {
	var mounts []string
	home := "/home/" + d.Host.User
	if len(s.Snapshots) > 0 {
		snapRoot := filepath.Join(idir, "snap")
		_ = os.RemoveAll(snapRoot)
		if err := os.MkdirAll(snapRoot, 0o755); err != nil {
			return nil, err
		}
		for _, sn := range s.Snapshots {
			dst := filepath.Join(snapRoot, sn.Name)
			if err := d.copyTree(ctx, sn.HostPath, dst); err != nil {
				return nil, fmt.Errorf("--snapshot: failed to copy %q: %w", sn.HostPath, err)
			}
			mounts = append(mounts, fmt.Sprintf("%s:%s/%s:ro", dst, home, sn.Name))
		}
	}
	if len(s.RepoClones) > 0 {
		var b strings.Builder
		for _, rc := range s.RepoClones {
			mountPath := "/run/cs-sandbox-repos/" + rc.Name
			mounts = append(mounts, fmt.Sprintf("%s:%s:ro", rc.HostPath, mountPath))
			id := spec.GitIdentity(ctx, d.Runner, rc.HostPath)
			// 6 US-separated fields: dir, mountpath, branch, ref, name, email (id is name<US>email).
			fmt.Fprintf(&b, "%s%s%s%s%s%s%s%s%s\n",
				rc.Name, spec.US, mountPath, spec.US, state.BranchName(s.Group, s.Name),
				spec.US, rc.BaseRef, spec.US, id)
		}
		if err := os.WriteFile(filepath.Join(seedDir, "repos"), []byte(b.String()), 0o600); err != nil {
			return nil, err
		}
	}
	return mounts, nil
}
