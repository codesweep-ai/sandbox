//go:build integration || smoke

// Firecracker live integration test. Boots a real microVM on the shared podman
// fabric and proves the end-to-end trust path: create -> boot -> ssh in with the
// user-tier key -> land as the dev user -> destroy -> teardown is complete.
//
//	go test -tags integration -run TestFirecracker ./internal/engine/ -v
//
// Requires a Linux/KVM host with the firecracker artifacts already built into
// the artifact cache. Skips gracefully if KVM or the cache is unavailable.
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// fcTestDeps mirrors testDeps but points FCCache at the real shared artifact
// cache (a temp InstDir has no cached kernel/rootfs). Skips if the cache or KVM
// is missing.
func fcTestDeps(t *testing.T) Deps {
	t.Helper()
	cache := findFCCache(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	requirePodman(t, "")
	h, err := hostenv.Detect()
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir(): each VM copies the base rootfs (14 GB at the time of
	// writing) into the instances dir, and tmpfs cannot hold it — the copy fails
	// with "Disk quota exceeded" before anything under test runs.
	// Short prefix on purpose: the instance dir shares one 108-byte AF_UNIX
	// budget with the sockets inside it, and MkdirTemp's suffix width varies.
	dir, err := os.MkdirTemp(h.Home, ".cs-fct-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return Deps{
		Runner:       &run.Exec{},
		Host:         h,
		InstDir:      dir,
		TierDir:      filepath.Join(dir, "tier-keys"),
		Image:        image(),
		Network:      "cs-sandbox-net",
		SSHBind:      "127.0.0.1",
		TZ:           "America/Los_Angeles",
		FCCache:      cache,
		AssetDir:     repoRoot(t), // for fc/init if a base-rootfs rebuild is needed
		StartTimeout: 120,
	}
}

// repoRoot walks up from the package dir to the checkout root (holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return wd
		}
		dir = parent
	}
}

// findFCCache locates the firecracker artifact cache, skipping the test when it
// holds no built artifacts.
func findFCCache(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("CS_SANDBOX_FC_CACHE"); d != "" {
		// Guarded like the fallback below, and not returned on trust: a cache
		// pointed at explicitly but not yet built used to sail past here and die
		// deep in Create with `cp: cannot stat .../base-rootfs.ext4`, which reads
		// as a broken engine rather than an unbuilt one. CI sets this variable on
		// every run, so the unbuilt case is its normal cold start.
		if hasArtifacts(d) {
			return d
		}
		t.Skipf("CS_SANDBOX_FC_CACHE=%s holds no built artifacts (run: cs-sandbox build --engine firecracker)", d)
	}
	if cand := paths.FCCache(); hasArtifacts(cand) {
		return cand
	}
	t.Skip("no cached firecracker artifacts found (run: cs-sandbox build --engine firecracker)")
	return ""
}

func hasArtifacts(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "base-rootfs.ext4"))
	return err == nil
}

func TestFirecrackerCreateLive(t *testing.T) {
	ctx := context.Background()
	d := fcTestDeps(t)
	if err := d.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatal(err)
	}

	fe := NewFirecracker(d)
	name := uniqName("fcgotest")

	// Belt-and-suspenders teardown even if Create fails midway.
	t.Cleanup(func() { _ = fe.Remove(context.Background(), name, true) })

	// Progress markers: booting a microVM takes tens of seconds, and this test is
	// the longest in the suite. Under -v (what make test-integration uses) these
	// stream live instead of leaving the run silent.
	t.Logf("[%s] booting microVM %s (reflink rootfs + boot, ~30s)…", time.Now().Format("15:04:05"), name)
	start := time.Now()
	inst, err := fe.Create(ctx, CreateSpec{
		Name: name, Type: "agent", CPUs: 2, MemMiB: 1024,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("[%s] microVM booted in %s (port %d, ip %s)", time.Now().Format("15:04:05"),
		time.Since(start).Round(time.Second), inst.Port, inst.FCIP)
	if inst.Port < 2300 || inst.Port > 2399 {
		t.Errorf("fc port %d outside VM pool [2300,2399]", inst.Port)
	}
	if inst.FCIP == "" {
		t.Errorf("no VM IP allocated")
	}

	// The microVM process is up and the serial log reached readiness.
	idir := d.InstanceDir(name)
	if !fcRunning(idir) {
		t.Fatalf("firecracker process not running after Create")
	}

	// End-to-end trust proof: ssh in with the user-tier key and land as the dev user.
	waitSSH(t, d, inst.Port, "true", 60*time.Second)
	who := sshOut(ctx, d, inst.Port, "whoami")
	if who != d.Host.User {
		t.Errorf("ssh whoami = %q, want %q (dev user via U tier key over the fabric forwarder)", who, d.Host.User)
	}
	host := sshOut(ctx, d, inst.Port, "hostname")
	if host != name {
		t.Errorf("guest hostname = %q, want %q", host, name)
	}

	// The dnsmasq hostsdir carries the VM name -> IP mapping. Asked of paths,
	// not derived from FCCache: the fabric dir is deliberately host-global (one
	// rootless fabric per host) and so does NOT follow CS_SANDBOX_FC_CACHE. The
	// two coincide only while the cache sits at its default, which is why
	// pointing the cache elsewhere — as CI does — used to fail here.
	reg := filepath.Join(paths.FCNet(), "hosts.d", name)
	if _, err := os.Stat(reg); err != nil {
		t.Errorf("dnsmasq registration %s missing: %v", reg, err)
	}

	// The tap exists on the bridge (inside podman's netns).
	tap := fcnet.TapName(inst.FCIP)
	if _, err := d.Runner.Run(ctx, run.Opts{}, "podman", "unshare", "--rootless-netns",
		"ip", "link", "show", tap); err != nil {
		t.Errorf("tap %s not present during run: %v", tap, err)
	}

	// --- destroy + assert teardown ---
	if err := fe.Remove(ctx, name, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if fcRunning(idir) {
		t.Errorf("firecracker process still running after destroy")
	}
	if _, err := os.Stat(idir); !os.IsNotExist(err) {
		t.Errorf("instance dir %s not removed after destroy", idir)
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Errorf("dnsmasq registration %s not removed after destroy", reg)
	}
	if _, err := d.Runner.Run(ctx, run.Opts{}, "podman", "unshare", "--rootless-netns",
		"ip", "link", "show", tap); err == nil {
		t.Errorf("tap %s still present after destroy", tap)
	}
	// State is gone.
	if _, err := state.Load(d.InstDir, d.group(), name); err == nil {
		t.Errorf("state still present after destroy")
	}
}
