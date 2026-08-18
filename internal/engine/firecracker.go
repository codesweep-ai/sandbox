package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	assets "github.com/codesweep-ai/sandbox"
	"github.com/codesweep-ai/sandbox/internal/fcconfig"
	"github.com/codesweep-ai/sandbox/internal/fcdisk"
	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/ports"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/spec"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Firecracker is the microVM engine adapter. It boots a real
// Firecracker microVM on the shared podman fabric that the caller can ssh into.
type Firecracker struct{ d Deps }

// NewFirecracker constructs the firecracker adapter.
func NewFirecracker(d Deps) *Firecracker { return &Firecracker{d: d} }

func (fe *Firecracker) Name() state.Engine { return state.Firecracker }

// Prepare downloads/builds the shared firecracker artifacts (binary + guest
// kernel + base rootfs), building from the image. preflight() first, so a host
// missing /dev/kvm or the FC packages fails fast before the long build.
func (fe *Firecracker) Prepare(ctx context.Context) error {
	if err := fe.preflight(); err != nil {
		return err
	}
	return fe.cache().EnsureArtifacts(ctx, fe.d.Runner, fe.buildConfig())
}

// Verify confirms the cached artifacts a microVM boots from are present, so
// create fails cleanly (pointing at build) rather than mid-boot.
func (fe *Firecracker) Verify(ctx context.Context) error {
	return fe.cache().VerifyArtifacts()
}

// fabric builds the fcnet.Fabric from Deps.
func (fe *Firecracker) fabric() fcnet.Fabric {
	return fcnet.Fabric{
		Runner:    fe.d.Runner,
		Network:   fe.d.Network,
		Image:     fe.d.Image,
		NetDir:    paths.FCNetFor(fe.d.group()),
		TapPrefix: fe.d.TapPrefix,
	}
}

func (fe *Firecracker) cache() fcdisk.Cache {
	return fcdisk.Cache{Dir: fe.d.FCCache, Progress: fe.d.Progress}
}

// buildConfig gathers the inputs the artifact BUILD path needs. An empty
// InitPath just means the base rootfs cannot be rebuilt from scratch (a cached
// one is still reused).
func (fe *Firecracker) buildConfig() fcdisk.BuildConfig {
	kernel := os.Getenv("CS_SANDBOX_FC_KERNEL")
	if kernel == "" {
		kernel = "fedora" // FC_KERNEL default; part of the base-rootfs stamp
	}
	bc := fcdisk.BuildConfig{
		Image: fe.d.Image,
		// The guest init comes from the checkout when present, else the binary's
		// embedded copy (materialized into the cache). Its content hash keys the
		// base-rootfs stamp and is identical either way.
		InitPath: assets.GuestInitPath(fe.d.AssetDir, fe.d.FCCache),
		// Source for the static initramfs that mounts root and hands over to
		// InitPath. Same checkout-else-embedded resolution as InitPath.
		InitramfsSrc: assets.GuestInitramfsSrcPath(fe.d.AssetDir, fe.d.FCCache),
		Kernel:       kernel,
		FCVersion:    os.Getenv("CS_SANDBOX_FC_VERSION"),
	}
	if v := os.Getenv("CS_SANDBOX_FC_KVER"); v != "" {
		bc.KVerPin = v
	} else {
		bc.KVerPin = fcdisk.DefaultKVerPin
	}
	if v := os.Getenv("CS_SANDBOX_FC_ROOTFS_GB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			bc.RootfsGB = n
		}
	}
	return bc
}

// Create provisions and boots a microVM.
func (fe *Firecracker) Create(ctx context.Context, s CreateSpec) (inst *state.Instance, err error) {
	d := fe.d
	idir := d.InstanceDir(s.Name)
	if err := os.MkdirAll(idir, 0o700); err != nil {
		return nil, err
	}
	// A prior `rm` keeps the home disk; if it's still here, reuse it (don't reflink
	// a fresh one) so the sandbox comes back with its data. Only `destroy` deletes it.
	rootfs := filepath.Join(idir, "rootfs.ext4")
	_, statErr := os.Stat(rootfs)
	reuseRootfs := statErr == nil

	if err := fe.preflight(); err != nil {
		return nil, err
	}

	fab := fe.fabric()

	// --- serialized allocation + claim + fabric bring-up (parallel-create safe) ---
	l := lock.New(d.InstDir)
	var ip string
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

	gw := fab.Gateway(ctx)
	if gw == "" {
		return nil, fmt.Errorf("fc: cannot read podman network %q gateway", d.Network)
	}
	ip, err = fe.allocIP(ctx, fab, gw)
	if err != nil {
		return nil, err
	}
	port, err := allocPort(ports.Split, ports.Max, d.reservedPorts(ctx))
	if err != nil {
		return nil, err
	}

	inst = &state.Instance{
		Name: s.Name, Group: fe.d.group(), Type: s.Type, Engine: state.Firecracker,
		FCIP: ip, Port: port, CPUs: s.CPUs, MemMiB: s.MemMiB,
		Yolo: s.Yolo, Solo: s.Solo, Shared: s.ImageStores, Created: nowUTC(),
	}
	for _, sn := range s.Snapshots {
		inst.Snapshots = append(inst.Snapshots, sn.HostPath+":"+sn.Name)
	}
	for _, rc := range s.RepoClones {
		inst.RepoClones = append(inst.RepoClones, state.RepoClone{
			Source: rc.HostPath, Dir: rc.Name, Branch: state.BranchName(s.Group, s.Name),
		})
	}
	// Claim ip+port by persisting state now, still under the lock.
	if err := state.Save(d.InstDir, inst); err != nil {
		return nil, err
	}

	// Arm failure cleanup: reap the half-built VM (address+port+dir) on any error.
	// A reused home disk (recreate) is kept — cleanup uses rm, not destroy.
	defer func() {
		if err != nil {
			_ = fe.Remove(context.Background(), s.Name, !reuseRootfs)
		}
	}()

	if err = fab.Up(ctx); err != nil {
		return nil, err
	}
	unlock()

	// --- per-instance seed (built unlocked) ---
	seedDir := filepath.Join(idir, "seed")
	if err = os.MkdirAll(seedDir, 0o700); err != nil {
		return nil, err
	}
	agentLogins, err := d.writeSeed(ctx, seedDir, s, gw)
	if err != nil {
		return nil, err
	}
	// The ip+port claim was persisted above; record the inherited logins too.
	if len(agentLogins) > 0 {
		inst.AgentLogins = agentLogins
		if err = state.Save(d.InstDir, inst); err != nil {
			return nil, err
		}
	}

	// --- auxiliary disks (repos/snapshots/image-stores) + manifests ---
	d.say("assembling the sandbox filesystem…")
	var repoDisks, snapDisks, storeDisks []string
	var reposManifest, snapsManifest, storesManifest string
	repoDisks, reposManifest, err = fe.buildRepoDisks(ctx, idir, s)
	if err != nil {
		return nil, err
	}
	snapDisks, snapsManifest, err = fe.buildSnapshotDisks(ctx, idir, s)
	if err != nil {
		return nil, err
	}
	storeDisks, storesManifest, err = fe.buildStoreDisks(ctx, s)
	if err != nil {
		return nil, err
	}

	// --- seed.ext4 ---
	seedImg := filepath.Join(idir, "seed.ext4")
	si := fcdisk.SeedInput{
		SeedDir: seedDir, FCSeed: filepath.Join(idir, "fc-seed"),
		User: d.Host.User, UID: d.Host.UID, GID: d.Host.GID,
		Hostname: s.Name, Type: s.Type, Yolo: s.Yolo,
		IP: ip, GW: gw, DNS: fab.DNSIP(ctx),
		Repos: reposManifest, Snapshots: snapsManifest, ImageStores: storesManifest,
	}
	if err = fe.cache().BuildSeedExt4(ctx, d.Runner, si, seedImg); err != nil {
		return nil, err
	}

	// --- per-instance writable rootfs (CoW reflink; skipped when reusing a kept home) ---
	if !reuseRootfs {
		if err = fe.cache().ReflinkRootfs(ctx, d.Runner, rootfs); err != nil {
			return nil, err
		}
	}
	// --disk: grow this instance's disk past the base rootfs size. Applied to the
	// kept disk too, so `rm` + `create --disk N` grows a sandbox in place without
	// losing its data — the only way to widen one that already exists, since a
	// running VM's virtio-blk capacity is fixed at boot.
	if s.DiskGB > 0 {
		d.say("sizing the disk to %d GiB…", s.DiskGB)
		if err = fcdisk.GrowRootfs(ctx, d.Runner, rootfs, s.DiskGB); err != nil {
			return nil, err
		}
	}

	// --- run.json ---
	vsock := filepath.Join(idir, state.SockVsock)
	removeVsock(idir)
	cfg := fcconfig.Build(fcconfig.Spec{
		KernelPath: fe.cache().Kernel(), InitrdPath: fe.cache().Initrd(),
		RootfsPath: rootfs, SeedPath: seedImg,
		TapName: fe.fabric().TapName(ip), MAC: fcnet.GuestMAC(ip), VsockPath: vsock,
		VCPUs: s.CPUs, MemMiB: s.MemMiB,
		RepoDisks: repoDisks, SnapshotDisks: snapDisks, StoreDisks: storeDisks,
	})
	if err = cfg.WriteFile(filepath.Join(idir, "run.json")); err != nil {
		return nil, err
	}

	// --- launch (tap + forwarder + firecracker) ---
	d.say("booting the microVM…")
	if err = fe.launch(ctx, s.Name, inst); err != nil {
		return nil, err
	}

	// --- wait for FC-VM-READY in serial.log ---
	if err = fe.waitReady(ctx, s.Name); err != nil {
		return nil, err
	}
	return inst, nil
}

// launch brings up this VM's tap + host→VM forwarder, then boots firecracker.
func (fe *Firecracker) launch(ctx context.Context, name string, inst *state.Instance) error {
	d := fe.d
	idir := d.InstanceDir(name)
	fab := fe.fabric()
	tap := fe.fabric().TapName(inst.FCIP)
	removeVsock(idir)
	if err := fab.Up(ctx); err != nil {
		return err
	}
	if err := fab.TapUp(ctx, tap); err != nil {
		return err
	}
	// Publish the name only now that the tap exists. The two share a lifetime, so
	// a name record always has a link behind it — which is what lets the fabric
	// recognise a record left by a killed create as stale (fcnet.sweepNames).
	if err := fab.Register(name, inst.FCIP); err != nil {
		return err
	}
	if err := fab.FwdUp(idir, inst.Port, inst.FCIP, d.SSHBind); err != nil {
		return err
	}
	// Firecracker runs INSIDE podman's rootless netns (it has /dev/kvm + the tap).
	return launchFirecracker(idir, fe.cache().FirecrackerBin())
}

// waitReady polls serial.log for the guest's FC-VM-READY marker, giving up if
// firecracker dies.
func (fe *Firecracker) waitReady(ctx context.Context, name string) error {
	fe.d.say("waiting for the sandbox to be ready…")
	idir := fe.d.InstanceDir(name)
	serial := filepath.Join(idir, "serial.log")
	budget := time.Duration(fe.d.StartTimeout) * time.Second
	deadline := time.Now().Add(budget)
	for {
		if data, err := os.ReadFile(serial); err == nil && strings.Contains(string(data), "FC-VM-READY") {
			return nil
		}
		if !fcRunning(idir) {
			return fmt.Errorf("fc: microVM %q exited before becoming ready (see %s)", name, serial)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for microVM %q readiness: %w", name, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		delay := min(time.Second, remaining)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for microVM %q readiness: %w", name, ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("fc: microVM %q failed to become ready within %s (see %s)", name, budget, serial)
}

// Start re-launches a stopped microVM (launch re-asserts its dnsmasq name).
func (fe *Firecracker) Start(ctx context.Context, name string) error {
	in, err := state.Load(fe.d.InstDir, fe.d.group(), name)
	if err != nil {
		return err
	}
	if err := fe.launch(ctx, name, in); err != nil {
		return err
	}
	return fe.waitReady(ctx, name)
}

// Stop shuts the microVM down (graceful reboot -f, then kill) and tears down its
// forwarder, but keeps the disks/state.
func (fe *Firecracker) Stop(ctx context.Context, name string) error {
	idir := fe.d.InstanceDir(name)
	fe.shutdown(ctx, name)
	fe.fabric().FwdDown(idir)
	fe.fabric().GC(ctx, func() bool { return fe.anyVMRunning(ctx) })
	return nil
}

// Remove destroys the microVM and (with purge) its disks + instance dir.
func (fe *Firecracker) Remove(ctx context.Context, name string, purge bool) error {
	idir := fe.d.InstanceDir(name)
	fab := fe.fabric()
	fe.shutdown(ctx, name)
	fab.FwdDown(idir)
	if in, err := state.Load(fe.d.InstDir, fe.d.group(), name); err == nil && in.FCIP != "" {
		fab.TapDel(ctx, fab.TapName(in.FCIP))
	}
	fab.Unregister(name)
	if purge {
		// destroy: remove everything, including the home disk (rootfs.ext4).
		_ = os.RemoveAll(idir)
	} else {
		// rm: keep the home disk (rootfs.ext4) so `create <name>` can reuse it;
		// drop the ephemeral disks, boot files, seed, and the instance state.
		if entries, err := os.ReadDir(idir); err == nil {
			for _, e := range entries {
				if e.Name() == "rootfs.ext4" {
					continue
				}
				_ = os.RemoveAll(filepath.Join(idir, e.Name()))
			}
		}
	}
	fab.GC(ctx, func() bool { return fe.anyVMRunning(ctx) })
	return nil
}

// shutdown gracefully stops the VM (reboot -f over ssh), then kills firecracker.
func (fe *Firecracker) shutdown(ctx context.Context, name string) {
	idir := fe.d.InstanceDir(name)
	// Only the graceful half is worth skipping when the pid is gone: it costs an
	// ssh round trip to a VM whose wrapper already died. killFirecracker runs
	// either way, because a dead wrapper says nothing about the VMM below it.
	if fcRunning(idir) {
		// Best-effort graceful reboot over the published port.
		if in, err := state.Load(fe.d.InstDir, fe.d.group(), name); err == nil && in.Port > 0 {
			reboot := append(fe.sshArgs(name, in.Port), "sync; sudo sh -c \"sync; reboot -f\"")
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			_, _ = fe.d.Runner.Run(cctx, run.Opts{}, reboot...)
			cancel()
		}
	}
	killFirecracker(idir)
}

// anyVMRunning reports whether any firecracker instance is still up (for GC).
// A tap on the fabric counts: it belongs to a VM this instances dir cannot see,
// and tearing the fabric down would cut that VM off the network.
func (fe *Firecracker) anyVMRunning(ctx context.Context) bool {
	if len(fe.fabric().TapOctets(ctx)) > 0 {
		return true
	}
	insts, _ := state.List(fe.d.InstDir)
	for _, in := range insts {
		if in.Engine == state.Firecracker && fcRunning(fe.d.InstanceDir(in.Name)) {
			return true
		}
	}
	return false
}

// Exec runs a command (or login shell) inside the microVM over ssh.
func (fe *Firecracker) Exec(ctx context.Context, name string, io ExecIO) error {
	in, err := state.Load(fe.d.InstDir, fe.d.group(), name)
	if err != nil {
		return err
	}
	argv := fe.sshArgs(name, in.Port)
	if io.Interactive {
		argv = append(argv, "-t")
	}
	if len(io.Argv) > 0 {
		// Quote each word. ssh joins its command words with spaces and the
		// remote shell re-parses the result, so an unquoted argv arrives
		// word-split: `exec box printf '[%s]' "one two"` would print [one][two],
		// and any argument carrying a space, $, ; or glob would be reinterpreted
		// by a shell the caller never asked for. Quoting makes `exec` mean the
		// same on both engines — run this argv, do not interpret it — and
		// `cs-sandbox ssh` remains the door to a remote shell.
		for _, a := range io.Argv {
			argv = append(argv, shellQuote(a))
		}
	} else {
		argv = append(argv, "bash", "-l")
	}
	_, err = fe.d.Runner.Run(ctx, run.Opts{Interactive: true}, argv...)
	return err
}

// shellQuote renders s so that a POSIX shell parses it back to exactly s.
// Single quotes suppress every expansion; the only character that cannot appear
// inside them is a single quote itself, which is closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Port returns the instance's published SSH port.
func (fe *Firecracker) Port(ctx context.Context, name string) (int, error) {
	in, err := state.Load(fe.d.InstDir, fe.d.group(), name)
	if err != nil {
		return 0, err
	}
	return in.Port, nil
}

// sshArgs builds the ssh argv reaching the VM by its published port with the
// user-tier key, keyed by HostKeyAlias (as cmd ssh does).
func (fe *Firecracker) sshArgs(name string, port int) []string {
	key := filepath.Join(fe.d.TierDir, "id_cs-sandbox_user")
	knownHosts := fe.d.Host.SSHDir() + "/known_hosts.cs-sandbox"
	// The host-global object name, not the bare one: two groups holding the same
	// fixture present different host keys, and one alias for both fails the
	// second with "host key changed" under BatchMode, with nobody to accept it.
	return []string{"ssh",
		"-i", key, "-p", strconv.Itoa(port),
		"-o", "HostKeyAlias=" + state.ObjectName(fe.d.group(), name),
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		fe.d.Host.User + "@127.0.0.1",
	}
}

// allocIP picks the next free VM address from the high end of the /24 (.200-.250),
// avoiding addresses already claimed by other instances.
func (fe *Firecracker) allocIP(ctx context.Context, fab fcnet.Fabric, gw string) (string, error) {
	prefix := gw
	if i := strings.LastIndexByte(gw, '.'); i > 0 {
		prefix = gw[:i]
	}
	used := map[string]bool{}
	insts, _ := state.List(fe.d.InstDir)
	for _, in := range insts {
		if in.FCIP != "" {
			used[in.FCIP] = true
		}
	}
	// Instances in other roots (another CS_SANDBOX_HOME, a test run) are invisible
	// to the list above, but their taps are not — so ask the fabric too.
	taps := fab.TapOctets(ctx)
	const lo, hi = 200, 250
	for n := lo; n <= hi; n++ {
		ip := fmt.Sprintf("%s.%d", prefix, n)
		if !used[ip] && !taps[strconv.Itoa(n)] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("fc: no free VM address in %s.%d-%d", prefix, lo, hi)
}

// --- auxiliary disk builders ---

// buildRepoDisks builds one RO objects disk per --repo and the 5-field repos
// manifest (dir, branch, base, name, email). The manifest is 5 US-separated
// fields (no mount path — the repo is a positional
// disk). Each disk is content-addressed: the bare clone is built once under
// the artifact cache's repo-disks/<srcid>-<key>.ext4 (key = sha256 of ref tips + HEAD) and
// reflink-copied per instance, so multiple VMs from the same commit share one
// build. On a caching failure it falls back to building the disk in place.
func (fe *Firecracker) buildRepoDisks(ctx context.Context, idir string, s CreateSpec) ([]string, string, error) {
	if len(s.RepoClones) == 0 {
		return nil, "", nil
	}
	c := fe.cache()
	c.RepoCacheGC(cacheTTLDays(), fe.cachedDisksInUse())
	var disks []string
	var man strings.Builder
	for i, rc := range s.RepoClones {
		branch := state.BranchName(s.Group, s.Name)
		id := spec.GitIdentity(ctx, fe.d.Runner, rc.HostPath)
		// 5 US-separated fields: dir, branch, base, name, email (id is name<US>email).
		fmt.Fprintf(&man, "%s%s%s%s%s%s%s\n", rc.Name, spec.US, branch, spec.US, rc.BaseRef, spec.US, id)

		disk := filepath.Join(idir, fmt.Sprintf("repo%d.ext4", i+1))
		if cached, cerr := c.RepoDisk(ctx, fe.d.Runner, rc.HostPath, rc.Name); cerr == nil {
			// Attach the cached disk itself. It is content-addressed and mounted
			// read-only by the guest, so sharing it is safe — and it is the
			// whole point: a reflink copy shares disk extents but gets a new
			// inode, and the page cache is per-inode, so N sandboxes reading the
			// same repo cached those same bytes N times in host RAM. The cache
			// GC skips paths any instance still references (cachedDisksInUse).
			disks = append(disks, cached)
			continue
		}
		// Caching failed (not a normal path) — build the disk in place, as before.
		bare := filepath.Join(idir, fmt.Sprintf("repo%d.git", i+1))
		_ = os.RemoveAll(bare)
		if _, err := fe.d.Runner.Run(ctx, run.Opts{}, "git", "clone", "-q", "--bare", rc.HostPath, bare); err != nil {
			return nil, "", fmt.Errorf("fc: repo disk %s: clone: %w", rc.Name, err)
		}
		if err := fcdisk.BuildExt4Dir(ctx, fe.d.Runner, bare, disk, 96); err != nil {
			return nil, "", fmt.Errorf("fc: repo disk %s: %w", rc.Name, err)
		}
		_ = os.RemoveAll(bare)
		disks = append(disks, disk)
	}
	return disks, man.String(), nil
}

// cachedDisksInUse is the set of host paths that some instance's run.json still
// names as a drive.
//
// Repo and image-store disks are attached straight from the artifact cache
// rather than copied per instance, so the same inode backs every sandbox using
// that content — which is the point: the host page cache then holds one copy
// instead of N. The cost is that the cache GC can no longer treat those files as
// unreferenced. Unlinking one would not disturb a *running* VM (its open fd
// keeps the inode alive), but the next `start` would find no disk.
//
// run.json is the authority rather than the instance list, because it is what a
// restart actually feeds to firecracker. The walk is layout-agnostic so it keeps
// working if the group/instance directory nesting changes.
func (fe *Firecracker) cachedDisksInUse() map[string]bool {
	inUse := map[string]bool{}
	_ = filepath.WalkDir(fe.d.InstDir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || e.Name() != "run.json" {
			return nil //nolint:nilerr // an unreadable instance dir must not block a build
		}
		cfg, rerr := fcconfig.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, d := range cfg.Drives {
			if d.PathOnHost != "" {
				inUse[d.PathOnHost] = true
			}
		}
		return nil
	})
	return inUse
}

// cacheTTLDays is the prune window (in days) for the content-addressed repo and
// store disk caches (CS_SANDBOX_FC_REPO_CACHE_TTL_DAYS, default 14, 0 disables).
func cacheTTLDays() int {
	if v := os.Getenv("CS_SANDBOX_FC_REPO_CACHE_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 14
}

// buildSnapshotDisks builds one RO ext4 disk per --snapshot (frozen at create)
// and the snapshots manifest (one name per line).
func (fe *Firecracker) buildSnapshotDisks(ctx context.Context, idir string, s CreateSpec) ([]string, string, error) {
	if len(s.Snapshots) == 0 {
		return nil, "", nil
	}
	var disks []string
	var man strings.Builder
	for i, sn := range s.Snapshots {
		fmt.Fprintf(&man, "%s\n", sn.Name)
		disk := filepath.Join(idir, fmt.Sprintf("snap%d.ext4", i+1))
		if err := fcdisk.BuildExt4Dir(ctx, fe.d.Runner, sn.HostPath, disk, 64); err != nil {
			return nil, "", fmt.Errorf("fc: snapshot disk %s: %w", sn.Name, err)
		}
		disks = append(disks, disk)
	}
	return disks, man.String(), nil
}

// buildStoreDisks builds one RO ext4 disk per --image-store and the imagestores
// manifest (one store name per line). Each disk is content-addressed: it is
// built once from the shared podman volume cs-sandbox-shared-<name> under
// the artifact cache's store-disks/<name>-<key>.ext4 (key = sha256 of the store's
// images.json+layers.json) and reflink-copied per instance. The guest init wires
// the disk into nested podman's additionalimagestores.
func (fe *Firecracker) buildStoreDisks(ctx context.Context, s CreateSpec) ([]string, string, error) {
	if len(s.ImageStores) == 0 {
		return nil, "", nil
	}
	c := fe.cache()
	c.StoreCacheGC(cacheTTLDays(), fe.cachedDisksInUse())
	var disks []string
	var man strings.Builder
	for _, name := range s.ImageStores {
		fmt.Fprintf(&man, "%s\n", name)
		cached, err := c.StoreDisk(ctx, fe.d.Runner, fe.d.Image, name)
		if err != nil {
			return nil, "", fmt.Errorf("fc: image-store disk %s: %w (is the store seeded?)", name, err)
		}
		// Attached directly rather than copied, for the same reason as the repo
		// disks: the guest mounts it read-only, and one inode means the host
		// page cache holds a single copy across every sandbox using this store.
		disks = append(disks, cached)
	}
	return disks, man.String(), nil
}
