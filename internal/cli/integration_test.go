//go:build integration || smoke || live_agents || agents_replay

// Integration tests drive the real cobra command tree end-to-end against live
// podman — proving the CLI wiring (flag parsing, the create/forward/destroy/ls
// commands, engine selection, repo/env sharing) works, not just the engine API
// the engine package tests cover. Instance/tier state is redirected to temp dirs
// so the developer's real sandboxes are untouched; sandboxes are namespaced
// (csgocli-*) and torn down.
//
//	go test -tags integration ./internal/cli/ -v
//
// Skips gracefully when podman or the sandbox image is unavailable.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/fcdisk"
	"github.com/codesweep-ai/sandbox/internal/fcnet"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/seed"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// runID is a per-process random suffix so namespaced names can't collide with a
// leftover from a crashed prior run that reused this PID.
var runID = func() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// image is the sandbox image the live tests run against. There is no default any
// more: the name carries the version of the cs-sandbox that built it, and a test
// binary carries no version to derive it from. `make test-integration` and
// `make test-smoke` both set the variable from the binary; a bare `go test`
// against these tags has to say which image it means.
func image(t *testing.T) string {
	t.Helper()
	v := os.Getenv("CS_SANDBOX_IMAGE")
	if v == "" {
		t.Skip("set CS_SANDBOX_IMAGE to the image to run against, or use `make test-integration`")
	}
	return v
}

// storeImage is the image a member seeds into a shared store, and it prefers the
// slimmed CI image (image/ci-slim.sh) to the shipped one. What a store member
// costs is dominated by turning the image into an ext4 disk, and none of them
// asserts anything a toolchain provides: the nested-VM member takes 599s against
// the 9.3 GB image and 75s against the 693 MB one, for the same assertions.
// `make test-smoke` already runs it this way.
//
// The microVM's own rootfs is the one built for CS_SANDBOX_IMAGE, which the
// cache keeps per image (SPEC R124) — so this member boots whichever variant the
// run was pointed at. Only the image the INNER sandbox is created from is
// chosen here,
// and that needs podman, sshd and the dev user, all of which ci-slim.sh keeps.
//
// An explicit CS_SANDBOX_IMAGE still wins, which is how to put the store path
// under the shipped image when that is the point. A host without the slim image
// falls back to the shipped one rather than skipping: slow beats not run.
func storeImage(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("CS_SANDBOX_IMAGE"); v != "" {
		return v
	}
	const slim = "localhost/sandbox-slim:ci"
	r := &run.Exec{}
	if _, err := r.Run(context.Background(), run.Opts{ReadOnly: true}, "podman", "image", "exists", slim); err == nil {
		return slim
	}
	return image(t)
}

// liveSetup skips unless podman + the image are present, redirects instance/tier
// state to temp dirs, and returns a real runner + host identity.
func liveSetup(t *testing.T) (*run.Exec, hostenv.Host) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	r := &run.Exec{}
	if _, err := r.Run(context.Background(), run.Opts{ReadOnly: true}, "podman", "info"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	img := image(t)
	if _, err := r.Run(context.Background(), run.Opts{ReadOnly: true}, "podman", "image", "exists", img); err != nil {
		t.Skipf("image %s not built (run: cs-sandbox build) — %v", img, err)
	}
	host, err := hostenv.Detect()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", filepath.Join(t.TempDir(), "instances"))
	t.Setenv("CS_SANDBOX_TIER_DIR", filepath.Join(t.TempDir(), "tier"))
	cleanSSHFragment(t, host)
	return r, host
}

// cleanSSHFragment removes this test root's ~/.ssh/config.d fragment at test
// end. The instances dir is a temp dir but ~/.ssh is the real one, so without
// this every run would leave another dead fragment in the developer's config.
// (The user's own fragment is a different file and is never touched.)
func cleanSSHFragment(t *testing.T, host hostenv.Host) {
	t.Helper()
	frag := host.SSHConfigFile(paths.Instances())
	t.Cleanup(func() { _ = os.Remove(frag) })
}

// shareDir returns a temp dir usable as a --repo / --snapshot source. On macOS
// `create` requires shared paths to resolve under $HOME (the podman-machine
// share cs-sandbox commits to), and t.TempDir() lands under /var/folders — so
// build the source under the home there instead. Removed at test end, like
// t.TempDir().
func shareDir(t *testing.T, host hostenv.Host) string {
	t.Helper()
	if !host.IsMacOS {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp(host.Home, ".cs-sandbox-itest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// boxName is a unique, namespaced sandbox name for a subtest.
func boxName(tag string) string {
	return fmt.Sprintf("csgocli-%s-%d-%s", tag, os.Getpid(), runID)
}

// step logs a timestamped progress line. A single test here can boot a real
// container or microVM and stay busy for tens of seconds; `make test-integration`
// runs with -v, under which these stream live so you can see where the time goes.
func step(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// createBox creates a podman sandbox via the CLI and registers teardown — the
// container plus its named volumes, so no test leaks the home/containers volumes.
func createBox(t *testing.T, r *run.Exec, name string, extra ...string) string {
	t.Helper()
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), run.Opts{}, "podman", "rm", "-f", objName(name))
		_, _ = r.Run(context.Background(), run.Opts{}, "podman", "volume", "rm", "-f",
			"cs-sandbox-home-"+objName(name), "cs-sandbox-containers-"+objName(name))
	})
	step(t, "creating podman sandbox %s…", name)
	start := time.Now()
	args := append([]string{"create", name, "--engine", "podman"}, extra...)
	out, err := execRoot(t, args...)
	if err != nil {
		t.Fatalf("create %s: %v (out=%q)", name, err, out)
	}
	step(t, "sandbox %s ready (%s)", name, time.Since(start).Round(time.Millisecond))
	return out
}

// fcInstancesDir moves the instances root off tmpfs for microVM tests and
// returns it. Each VM gets its own copy of the base rootfs — 14 GB at the time
// of writing — and t.TempDir() is a tmpfs on most hosts, so the copy fails with
// "Disk quota exceeded" long before anything under test runs. $HOME is disk.
//
// The prefix is kept short deliberately. A microVM's sockets live at
// <instances>/<group>/<name>/, inside one 108-byte AF_UNIX budget, and
// MkdirTemp's random suffix is 9 or 10 digits depending on the draw — long
// enough that the old ".cs-sandbox-fctest-" prefix put fwd.sock at 107 bytes on
// one run and 108 on the next, so the test passed or failed by luck.
func fcInstancesDir(t *testing.T, host hostenv.Host) string {
	t.Helper()
	dir, err := os.MkdirTemp(host.Home, ".cs-fct-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	inst := filepath.Join(dir, "instances")
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", inst)
	return inst
}

// objName is the podman object for a default-group sandbox these tests create.
// Podman objects carry the group (<name>.<group>), and a bare name matches
// nothing — silently, since `rm`/`exec` against a missing object just returns
// empty. That is how this whole suite came to assert on empty strings instead of
// failing, and how its cleanup quietly leaked containers and volumes.
func objName(name string) string { return state.ObjectName(state.DefaultGroup, name) }

// inBox runs a command inside the sandbox as the dev user and returns stdout.
func inBox(ctx context.Context, r *run.Exec, host hostenv.Host, name, sh string) string {
	return run.Output(ctx, r, "podman", "exec", "--user", host.User,
		"--workdir", "/home/"+host.User, objName(name), "bash", "-lc", sh)
}

func TestCLICreateExecDestroyLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	name := boxName("cxd")

	out, err := execRoot(t, "create", name, "--engine", "podman")
	t.Cleanup(func() { _, _ = r.Run(context.Background(), run.Opts{}, "podman", "rm", "-f", objName(name)) })
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out, "created "+name) {
		t.Errorf("create stdout = %q, want it to announce created %s", out, name)
	}
	if s := run.Output(ctx, r, "podman", "inspect", objName(name), "--format", "{{.State.Running}}"); s != "true" {
		t.Fatalf("container not running (State.Running=%q)", s)
	}
	// The agent tools resolve on PATH at ~/.local/bin (host↔sandbox unification).
	if where := strings.TrimSpace(inBox(ctx, r, host, name, "command -v cs-claude")); !strings.HasSuffix(where, "/.local/bin/cs-claude") {
		t.Errorf("cs-claude resolved to %q, want ~/.local/bin/cs-claude", where)
	}

	if _, err = execRoot(t, "destroy", name, "-f"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := r.Run(ctx, run.Opts{}, "podman", "inspect", objName(name)); err == nil {
		t.Errorf("container %s still exists after destroy", name)
	}
}

// TestCLIRmRecreateReusesDataLive: `rm` keeps the sandbox's data; recreating with
// the same name reuses it (the marker written before rm survives the recreate).
func TestCLIRmRecreateReusesDataLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	name := boxName("reuse")
	t.Cleanup(func() {
		_, _ = execRoot(t, "destroy", name, "-f")
		_, _ = r.Run(context.Background(), run.Opts{}, "podman", "rm", "-f", objName(name))
		_, _ = r.Run(context.Background(), run.Opts{}, "podman", "volume", "rm", "-f",
			"cs-sandbox-home-"+objName(name), "cs-sandbox-containers-"+objName(name))
	})

	if out, err := execRoot(t, "create", name, "--engine", "podman"); err != nil {
		t.Fatalf("create: %v (%s)", err, out)
	}
	if _, err := r.Run(ctx, run.Opts{}, "podman", "exec", "--user", host.User, objName(name),
		"bash", "-lc", "echo REUSE_MARKER > ~/reuse-marker.txt"); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// rm keeps the data, and keeps listing it as `removed` — kept data that
	// vanished from `ls` could sit on disk unnoticed, which is the whole reason
	// the status exists.
	if out, err := execRoot(t, "rm", name); err != nil {
		t.Fatalf("rm: %v (%s)", err, out)
	}
	ls, _ := execRoot(t, "ls")
	if !strings.Contains(ls, name) || !strings.Contains(ls, "removed") {
		t.Errorf("after rm, %s should still be listed as removed:\n%s", name, ls)
	}

	// recreate with the same name → reuse the kept home volume.
	if out, err := execRoot(t, "create", name, "--engine", "podman"); err != nil {
		t.Fatalf("recreate: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(inBox(ctx, r, host, name, "cat ~/reuse-marker.txt 2>/dev/null")); got != "REUSE_MARKER" {
		t.Errorf("recreated sandbox lost its data: marker = %q, want REUSE_MARKER", got)
	}
}

// TestCLIAgentToolSetLive: the full agent toolset (not just cs-claude) is present
// and executable at ~/.local/bin inside a sandbox.
func TestCLIAgentToolSetLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	name := boxName("tools")
	createBox(t, r, name)

	for _, tool := range []string{
		"cs-claude", "cs-codex", "cs-opencode",
		"cs-claude-remote", "cs-codex-remote", "cs-opencode-remote", "mdtohtml",
	} {
		got := strings.TrimSpace(inBox(ctx, r, host, name, "command -v "+tool))
		if !strings.HasSuffix(got, "/.local/bin/"+tool) {
			t.Errorf("%s resolved to %q, want ~/.local/bin/%s", tool, got, tool)
		}
	}
	// Plain `podman` is the real binary, not a wrapper on PATH ahead of it: the
	// nested engine is rootless on both engines now, so there is nothing to wrap.
	if got := strings.TrimSpace(inBox(ctx, r, host, name, "command -v podman")); got != "/usr/bin/podman" {
		t.Errorf("podman resolved to %q, want /usr/bin/podman (no wrapper)", got)
	}
}

// TestCLIAgentLoginInheritedLive: --inherit-agent-login claude snapshots the host
// login into the seed and first boot installs it at 0600. Existence/mode only,
// never contents.
func TestCLIAgentLoginInheritedLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	synthAgentHome(t, host)
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	name := boxName("login")
	out := createBox(t, r, name, "--inherit-agent-login", "claude")

	// create reports what the sandbox ended up with.
	if !strings.Contains(out, "agent login: claude") {
		t.Errorf("create should report the inherited login, got:\n%s", out)
	}
	if !fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", "claude", ".credentials.json")) {
		t.Error("create did not snapshot the host Claude login into the seed")
	}
	got := strings.TrimSpace(inBox(ctx, r, host, name,
		"stat -c %a ~/.cs-claude/.credentials.json 2>/dev/null"))
	if got != "600" {
		t.Errorf("sandbox ~/.cs-claude/.credentials.json missing or wrong mode: %q (want 600)", got)
	}
	// The instance record remembers the choice, so it stays inspectable.
	in, err := state.Load(instDir, state.DefaultGroup, name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(in.AgentLogins, ",") != "claude" {
		t.Errorf("state AgentLogins = %v, want [claude]", in.AgentLogins)
	}
	// With a login present the first-run wizard is pre-completed.
	if got := strings.TrimSpace(inBox(ctx, r, host, name,
		"test -f ~/.cs-claude/.claude.json && echo yes || echo no")); got != "yes" {
		t.Errorf("a signed-in sandbox should skip Claude's onboarding wizard, got %q", got)
	}
}

// TestCLIOpenCodeLoginInheritedLive is the opencode half of the test above: the
// credential lands in the seed and at 0600 in the profile the cs-opencode wrapper
// binds, and the wrapper hands it to opencode inline rather than leaving it on
// disk for opencode to find beside the session db.
func TestCLIOpenCodeLoginInheritedLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	synthAgentHome(t, host)
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	name := boxName("oclogin")
	out := createBox(t, r, name, "--inherit-agent-login", "opencode")

	if !strings.Contains(out, "agent login: opencode") {
		t.Errorf("create should report the inherited login, got:\n%s", out)
	}
	if !fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", "opencode", "auth.json")) {
		t.Error("create did not snapshot the host OpenCode login into the seed")
	}
	if got := strings.TrimSpace(inBox(ctx, r, host, name,
		"stat -c %a ~/.cs-opencode/auth.json 2>/dev/null")); got != "600" {
		t.Errorf("sandbox ~/.cs-opencode/auth.json missing or wrong mode: %q (want 600)", got)
	}
	in, err := state.Load(instDir, state.DefaultGroup, name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(in.AgentLogins, ",") != "opencode" {
		t.Errorf("state AgentLogins = %v, want [opencode]", in.AgentLogins)
	}
	// The wrapper folds the credential into OPENCODE_AUTH_CONTENT; opencode's own
	// data dir must stay free of it.
	if got := strings.TrimSpace(inBox(ctx, r, host, name,
		"OPENCODE_AUTH_CONTENT= cs-opencode --version >/dev/null 2>&1; "+
			"test -f ~/.local/share/opencode/auth.json && echo leaked || echo clean")); got != "clean" {
		t.Errorf("credential reached opencode's data dir: %q", got)
	}
}

// TestCLIAgentLoginOptInLive: inheriting is opt-in — a plain create carries no
// login even when the host has one, and asking for one agent never carries the
// other.
func TestCLIAgentLoginOptInLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	synthAgentHome(t, host)
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")

	plain := boxName("nologin")
	out := createBox(t, r, plain)
	if !strings.Contains(out, "agent login: none") || !strings.Contains(out, "--inherit-agent-login") {
		t.Errorf("create should say no login was inherited and how to get one, got:\n%s", out)
	}
	if fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, plain), "seed", "claude", ".credentials.json")) {
		t.Error("a plain create must not carry the host login")
	}
	// Without a login Claude must reach its own sign-in flow, so the onboarding
	// wizard must NOT be pre-completed for it.
	if got := strings.TrimSpace(inBox(ctx, r, host, plain,
		"test -f ~/.cs-claude/.claude.json && echo yes || echo no")); got != "no" {
		t.Errorf("a login-free sandbox must not have onboarding pre-completed (got %q) — "+
			"claude would show API billing instead of the sign-in options", got)
	}
	if got := strings.TrimSpace(inBox(ctx, r, host, plain,
		"test -f ~/.cs-claude/.credentials.json && echo present || echo absent")); got != "absent" {
		t.Errorf("sandbox should be login-free by default, got %q", got)
	}

	one := boxName("onelogin")
	createBox(t, r, one, "--inherit-agent-login", "claude")
	for _, other := range []string{"codex", "opencode"} {
		if fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, one), "seed", other, "auth.json")) {
			t.Errorf("%s login carried when only claude was requested", other)
		}
	}
}

// TestCLIProviderKeysNotCarriedLive: provider API keys are never carried, even
// when set in the create environment — --env is the explicit way to pass one.
func TestCLIProviderKeysNotCarriedLive(t *testing.T) {
	r, _ := liveSetup(t)
	t.Setenv("ANTHROPIC_API_KEY", "cs-test-fake-key")
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")

	name := boxName("nokeys")
	createBox(t, r, name, "--inherit-agent-login", "claude")

	if _, err := os.Stat(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", "claude", "env")); !os.IsNotExist(err) {
		t.Errorf("a provider key in the environment must not be carried into the seed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", "claude", "creds")); !os.IsNotExist(err) {
		t.Errorf("no creds/ dir should be carried (err=%v)", err)
	}
}

// TestCLIRepoShareLive: `create --repo <toplevel>` shares a working git checkout
// into the sandbox.
func TestCLIRepoShareLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	top := strings.TrimSpace(run.Output(ctx, r, "git", "rev-parse", "--show-toplevel"))
	if top == "" {
		t.Skip("not in a git checkout")
	}
	name := boxName("repo")
	createBox(t, r, name, "--repo", top)

	// A cloned repo dir with a .git shows up under the home, and git works in it.
	repoDir := strings.TrimSpace(inBox(ctx, r, host, name, "ls -d ~/*/.git 2>/dev/null | head -1 | xargs -r dirname"))
	if repoDir == "" {
		t.Fatalf("no shared repo checkout found under home in %s", name)
	}
	if head := strings.TrimSpace(inBox(ctx, r, host, name, "git -C "+repoDir+" rev-parse --abbrev-ref HEAD")); head == "" {
		t.Errorf("shared repo %s is not a working checkout (no HEAD)", repoDir)
	}
}

// TestCLIYoloLive: --yolo writes the .yolo marker for both agent and user
// sandboxes (it's not restricted to agents).
func TestCLIYoloLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()

	for _, typ := range []string{"agent", "user"} {
		name := boxName("yolo" + typ)
		createBox(t, r, name, "--type", typ, "--yolo")
		// Every agent's wrapper keys off its own marker.
		for _, profile := range []string{".cs-claude", ".cs-codex", ".cs-opencode"} {
			if got := strings.TrimSpace(inBox(ctx, r, host, name,
				"test -f ~/"+profile+"/.yolo && echo yes")); got != "yes" {
				t.Errorf("%s --yolo missing ~/%s/.yolo marker: %q", typ, profile, got)
			}
		}
	}
}

// TestCLIUserTypeSeedLive: a user sandbox seeds the user tier key (H+U), an agent
// seeds the agent tier key (G) — the trust matrix, on the real seed on disk.
func TestCLIUserTypeSeedLive(t *testing.T) {
	r, _ := liveSetup(t)
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")

	uname := boxName("user")
	createBox(t, r, uname, "--type", "user")
	userSeed := filepath.Join(state.Dir(instDir, state.DefaultGroup, uname), "seed")
	if !fileExists(filepath.Join(userSeed, "id_cs-sandbox_user")) {
		t.Errorf("user sandbox missing user tier key in seed")
	}
	if fileExists(filepath.Join(userSeed, "id_cs-sandbox_agent")) {
		t.Errorf("user sandbox should not hold the agent tier key")
	}

	aname := boxName("agent")
	createBox(t, r, aname, "--type", "agent")
	if !fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, aname), "seed", "id_cs-sandbox_agent")) {
		t.Errorf("agent sandbox missing agent tier key in seed")
	}
}

// TestCLIEnvInjectionLive: `create -e KEY=VALUE` resolves the var into the seed's
// inject-env file (installed into the sandbox as ~/.ssh/environment), written 0600.
func TestCLIEnvInjectionLive(t *testing.T) {
	r, _ := liveSetup(t)
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	name := boxName("env")
	createBox(t, r, name, "-e", "CS_TEST_TOKEN=sekret123")

	p := filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", "inject-env")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read inject-env: %v", err)
	}
	if !strings.Contains(string(data), "CS_TEST_TOKEN=sekret123") {
		t.Errorf("inject-env missing injected var; got:\n%s", data)
	}
	if fi, _ := os.Stat(p); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("inject-env mode = %o, want 600 (holds secrets)", fi.Mode().Perm())
	}
}

// TestCLINetworkReachabilityLive: two sandboxes on the shared fabric resolve each
// other by name (the core "reachable by name" promise).
func TestCLINetworkReachabilityLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	a, b := boxName("neta"), boxName("netb")
	createBox(t, r, a)
	createBox(t, r, b)

	// From box A, box B's name resolves to an address on the shared network.
	got := strings.TrimSpace(inBox(ctx, r, host, a, "getent hosts "+b+" | awk '{print $1}'"))
	if got == "" {
		t.Errorf("box %s could not resolve peer %s by name on the shared fabric", a, b)
	}
}

// sshCapture runs a shell snippet inside a sandbox over its published SSH port
// and returns stdout — the engine-agnostic way to read a command's output (the
// CLI's own `exec` attaches to the terminal, so it can't be captured). Works for
// both podman containers and Firecracker VMs.
func sshCapture(t *testing.T, host hostenv.Host, name, sh string) string {
	t.Helper()
	argv := append([]string{"ssh"}, sshArgv(t, host, name)...)
	return run.Output(context.Background(), &run.Exec{}, append(argv, sh)...)
}

// sshCaptureWithin is sshCapture with a deadline, and a post-mortem when it
// expires.
//
// It exists for one step: the nested create in TestCLINestedSandboxInVMLive.
// That step passes in well under a minute and has twice stalled for the whole
// 20-minute PACKAGE timeout in CI — silently, because the package timeout kills
// the test process rather than the step, so the panic carries a goroutine dump
// and nothing at all about the guest. A bounded step fails while the microVM is
// still up, which is the only moment anything inside it can be read.
//
// The deadline reaches the ssh process: run.Exec builds every command with
// exec.CommandContext, so an expired context kills it rather than leaking it
// for the rest of the run.
func sshCaptureWithin(t *testing.T, host hostenv.Host, name string, d time.Duration, sh string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	argv := append([]string{"ssh"}, sshArgv(t, host, name)...)
	out := run.Output(ctx, &run.Exec{}, append(argv, sh)...)
	if ctx.Err() != nil {
		vmPostMortem(t, host, name)
		t.Fatalf("%q did not answer within %s; see the post-mortem above", sh, d)
	}
	return out
}

// vmPostMortem reports what the guest was doing when a step gave up on it.
//
// Best-effort by construction: the VM is in a bad state by the time this runs,
// so every probe carries its own short deadline and a probe that cannot answer
// says so. That silence is itself the finding — it separates "the create is
// wedged" from "the guest or its ssh is gone", which the outer symptom cannot.
//
// The container logs are the point of the list. Everything the entrypoint does
// on first boot lands there, so they say how far the inner sandbox got before
// it stopped, which is the one thing a package-timeout panic never shows.
func vmPostMortem(t *testing.T, host hostenv.Host, name string) {
	t.Helper()
	argv := append([]string{"ssh"}, sshArgv(t, host, name)...)
	for _, probe := range []struct{ what, sh string }{
		{"containers", "podman ps -a 2>&1"},
		{"container logs", "for c in $(podman ps -aq); do echo \"== $c\"; podman logs --tail 40 \"$c\" 2>&1; done"},
		{"images", "podman images --format '{{.Repository}}:{{.Tag}}\t{{.Size}}'"},
		{"sandboxes", "~/bin/cs-sandbox ls 2>&1"},
		{"disk", "df -h / /var/lib/containers 2>&1 | tail -5"},
		{"memory", "free -m 2>&1 | head -3"},
		{"busy processes", "ps -eo pid,etime,stat,args | grep -E 'cs-sandbox|podman|conmon|fuse' | grep -v grep"},
		// sudo: the sandbox user cannot read the kernel buffer, and this probe
		// answered "Operation not permitted" when it was tried without.
		{"kernel ring", "sudo dmesg 2>&1 | tail -20"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		got := strings.TrimSpace(run.Output(ctx, &run.Exec{}, append(argv, probe.sh)...))
		cancel()
		if got == "" {
			got = "(no answer within 20s — the guest is not responding, not merely busy)"
		}
		t.Logf("post-mortem / %s:\n%s", probe.what, got)
	}
}

// sshArgv is sshCapture's argument vector without the trailing snippet — shared
// so a caller that needs to stream something into the sandbox's stdin (see
// sshPipe) reaches it over exactly the same port, key and options.
func sshArgv(t *testing.T, host hostenv.Host, name string) []string {
	t.Helper()
	portStr, err := execRoot(t, "port", name)
	if err != nil {
		t.Fatalf("port %s: %v", name, err)
	}
	// Trust material is per group; these tests create default-group sandboxes.
	key := filepath.Join(paths.GroupKeys(state.DefaultGroup), "id_cs-sandbox_user")
	return []string{
		"-i", key, "-p", strings.TrimSpace(portStr),
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "-o", "LogLevel=ERROR",
		host.User + "@127.0.0.1",
	}
}

// sshPipe runs a shell snippet inside a sandbox with r streamed to its stdin.
// Streamed rather than buffered through run.Opts.Stdin (a string) because what
// goes through here is a binary and a ~700 MB image tar.
func sshPipe(t *testing.T, host hostenv.Host, name, sh string, r io.Reader) string {
	t.Helper()
	cmd := exec.Command("ssh", append(sshArgv(t, host, name), sh)...)
	cmd.Stdin = r
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %s %q: %v\n%s", name, sh, err, out)
	}
	return string(out)
}

// sshOK is sshCapture for a command that must succeed: it fails the test with
// what the command itself printed, rather than returning "". sshCapture reads
// through run.Output, which discards a non-zero exit as an empty string — right
// for a probe, and the reason an assertion several engines deep would otherwise
// report nothing but the empty string it compared against.
func sshOK(t *testing.T, host hostenv.Host, name, sh string) string {
	t.Helper()
	return sshPipe(t, host, name, sh, nil) // nil stdin: /dev/null, as exec.Cmd does
}

// assertHostByName checks the host-by-name wiring for a live sandbox: the seed
// pins the host's name(s) to the reachable pasta address, and inside the guest a
// getaddrinfo client (gai.conf ordering) picks that IPv4 over any unreachable
// AAAA the host's resolver returns. inGuest runs a shell snippet in the sandbox.
//
// It then opens a connection, because resolving is not reaching. Every check
// here used to be a name lookup, which is why an address that resolved to the
// wrong thing survived until something dialled it.
func assertHostByName(t *testing.T, host hostenv.Host, name string, inGuest func(sh string) string) {
	t.Helper()
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	data, err := os.ReadFile(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", "host_hosts"))
	if err != nil {
		t.Fatalf("read host_hosts seed: %v", err)
	}
	if !strings.Contains(string(data), "169.254.1.2 ") {
		t.Errorf("host_hosts should pin the pasta host address; got:\n%s", data)
	}
	// The host's own name lands on whatever address the guest knows the host by,
	// which is the seeded literal only where nothing published a better one.
	hn := host.Names[0]
	want := strings.TrimSpace(inGuest("getent ahosts " + engine.HostReachableName + " | awk 'NR==1{print $1}'"))
	if want == "" {
		t.Fatalf("the sandbox has no address for %s at all", engine.HostReachableName)
	}
	got := strings.TrimSpace(inGuest("getent ahosts " + hn + " | awk 'NR==1{print $1}'"))
	if got != want {
		t.Errorf("host name %q resolves to %q inside the sandbox, want %q, which is where %s points",
			hn, got, want, engine.HostReachableName)
	}
	assertHostIsReachable(t, inGuest, engine.HostReachableName, hn)
}

// assertHostIsReachable serves one page on every address of this host and asks
// the sandbox to fetch it by the name a sandbox knows the host under.
//
// That name is what a loan's base URL is built from, so this is the check that
// the address behind it is this machine rather than a gateway, a VM, or nothing
// at all.
func assertHostIsReachable(t *testing.T, inGuest func(sh string) string, names ...string) {
	t.Helper()
	const page = "reached-the-host"
	l, err := net.Listen("tcp", "0.0.0.0:0") // not loopback: the guest arrives on the ordinary side
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	_, port, _ := net.SplitHostPort(l.Addr().String())
	for _, n := range names {
		url := "http://" + net.JoinHostPort(n, port) + "/"
		if body := inGuest("curl -s --max-time 10 " + url); !strings.Contains(body, page) {
			t.Errorf("the sandbox could not reach this host at %s, got %q\n%s", url, body,
				inGuest("getent ahosts "+n+"; grep -iE 'internal|"+n+"' /etc/hosts"))
		}
	}
}

// TestCLIHostByNameLive: a podman sandbox can reach the host by its own name.
func TestCLIHostByNameLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	if len(host.Names) == 0 {
		t.Skip("host exposes no name to map")
	}
	name := boxName("hostname")
	createBox(t, r, name)
	assertHostByName(t, host, name, func(sh string) string { return inBox(ctx, r, host, name, sh) })
}

// TestCLIHostByNameFirecrackerLive: same host-by-name promise, exercised through
// the Firecracker guest path (fc-init appends host_hosts + writes gai.conf, off
// the seed.ext4 disk). Skips without /dev/kvm or the cached FC artifacts.
func TestCLIHostByNameFirecrackerLive(t *testing.T) {
	_, host := liveSetup(t)
	if len(host.Names) == 0 {
		t.Skip("host exposes no name to map")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	fcInstancesDir(t, host)
	name := boxName("hostnamefc")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	step(t, "booting firecracker microVM %s (takes ~30s)…", name)
	start := time.Now()
	if out, err := execRoot(t, "create", name, "--engine", "firecracker"); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	step(t, "microVM %s booted (%s)", name, time.Since(start).Round(time.Second))
	assertHostByName(t, host, name, func(sh string) string { return sshCapture(t, host, name, sh) })
}

// TestCLINestedSandboxInVMLive: cs-sandbox running *inside* one of its own
// sandboxes. A Firecracker microVM is the outer sandbox; it gets the sandbox image
// from a shared store and the CLI over ssh; it creates a podman sandbox of its own;
// it reaches that inner sandbox by name over ssh; and the "workload" subtest then
// runs a container inside THAT — the full microVM -> sandbox -> container stack,
// each layer built by the code under test.
//
// What only this member covers: the guest's nested-podman setup as a whole —
// rootless inner engine, the newuidmap/newgidmap file caps and /dev/net/tun
// /sandbox/nested-rootless grants it — driven by the real create path (network
// fabric, tier keys, ssh config fragment, sshd) rather than by a bare `podman
// run`. It covers the --image-store path the same way: the inner create finds its
// image only if the store disk registered with the engine that reads it. The
// subtest goes one layer deeper still: a container run by the inner sandbox's
// own nested engine, under a hypervisor, where the podman-only tests exercise
// the same engine on the host.
//
// Cost: the sandbox image is seeded into a shared store and reaches the guest as a
// disk. Building that disk is most of the runtime, which is why the store is
// seeded with the slim image (see storeImage) — it does not change what any
// assertion below proves. The workload subtest ships a ~4 MB image over ssh on
// top of that, into the inner sandbox, which no store disk reaches.
// Skips without /dev/kvm or the cached FC artifacts, like the other VM members.
func TestCLINestedSandboxInVMLive(t *testing.T) {
	_, host := liveSetup(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	// Asked of the same function the engine asks, and asked about the image this
	// microVM will boot: the base rootfs is kept per image (SPEC R124), so a host
	// carrying one for some other variant has not built what this member needs.
	// A guard naming only the kernel passed there and left the failure to
	// `create`, which is a skip reported as a broken repository.
	if err := (fcdisk.Cache{Dir: paths.FCCache()}).VerifyArtifacts(image(t)); err != nil {
		t.Skipf("firecracker artifacts not built (run: cs-sandbox build --engine firecracker): %v", err)
	}
	img := storeImage(t)
	bin := buildCLI(t)

	fcInstancesDir(t, host)
	store := fmt.Sprintf("csgonest%d%s", os.Getpid(), runID)
	outer := boxName("nest")
	t.Cleanup(func() {
		_, _ = execRoot(t, "destroy", outer, "-f")
		_, _ = execRoot(t, "rm-store", store, "-f")
	})

	// The sandbox image reaches the guest as a read-only store disk rather than over
	// ssh. `podman save | podman load` would move gigabytes through the vsock bridge
	// for every run and bound this test to an image small enough to survive the trip;
	// the disk is built once, content-addressed and reflink-attached.
	step(t, "seeding %s into store %s…", img, store)
	start := time.Now()
	if out, err := execRoot(t, "create-store", store); err != nil {
		t.Fatalf("create-store: %v (%s)", err, out)
	}
	if out, err := execRoot(t, "seed-store", "--from-host", store, img); err != nil {
		t.Fatalf("seed-store --from-host %s: %v (%s)", img, err, out)
	}
	step(t, "store seeded (%s)", time.Since(start).Round(time.Second))

	step(t, "booting outer microVM %s…", outer)
	start = time.Now()
	if out, err := execRoot(t, "create", outer, "--engine", "firecracker", "--image-store", store); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	step(t, "microVM %s booted (%s)", outer, time.Since(start).Round(time.Second))

	// Ship the CLI. Same binary the host just built, so the inner sandbox is
	// created by the code under test rather than by whatever the image carries.
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sshPipe(t, host, outer, "mkdir -p ~/bin && cat > ~/bin/cs-sandbox && chmod +x ~/bin/cs-sandbox", f)
	if got := sshCapture(t, host, outer, "~/bin/cs-sandbox ls"); !strings.Contains(got, "NAME") {
		t.Fatalf("shipped CLI does not run in the guest: %q", got)
	}

	// Nothing was shipped: the guest's nested podman reads the image out of the store
	// disk. Asserted before the create that depends on it, so a store that failed to
	// register reads as itself rather than as a confusing create failure.
	if got := sshCapture(t, host, outer,
		fmt.Sprintf("podman images %s --format '{{.Repository}}:{{.Tag}}'", img)); !strings.Contains(got, img) {
		t.Fatalf("the guest did not get %s from the --image-store: %q", img, got)
	}

	// The nested create: a podman sandbox, by the shipped CLI, inside the VM.
	step(t, "creating the inner sandbox…")
	start = time.Now()
	const inner = "inner" // safe unqualified: its whole world is this throwaway VM
	// Bounded, unlike every other step here: this is the one that stalls. It
	// takes 14s on bare metal and under a minute on a green CI run, so five
	// minutes is generous by an order of magnitude and still leaves the package
	// timeout (20m, under the job's 25) far enough away to collect evidence.
	out := sshCaptureWithin(t, host, outer, 5*time.Minute,
		fmt.Sprintf("CS_SANDBOX_IMAGE=%s ~/bin/cs-sandbox create %s --engine podman", img, inner))
	if !strings.Contains(out, "created "+inner) {
		t.Fatalf("nested create did not report success:\n%s", out)
	}
	step(t, "inner sandbox created (%s)", time.Since(start).Round(time.Second))

	// The outer VM's own view of it: running, and on the podman engine.
	if got := sshCapture(t, host, outer, "~/bin/cs-sandbox ls"); !strings.Contains(got, inner) ||
		!strings.Contains(got, "running") || !strings.Contains(got, "podman") {
		t.Errorf("inner sandbox not listed as a running podman sandbox:\n%s", got)
	}

	// The point of the whole exercise: outer reaches inner BY NAME, landing as
	// the dev user — the inner sandbox's ssh fragment, trust keys and sshd all
	// wired up by a create that ran inside a VM.
	who := strings.TrimSpace(sshCapture(t, host, outer,
		fmt.Sprintf("ssh -o StrictHostKeyChecking=no %s.default 'hostname; id -un'", inner)))
	if want := inner + "\n" + host.User; who != want {
		t.Errorf("outer -> inner ssh = %q, want %q", who, want)
	}

	// One layer further down: a container workload inside the inner sandbox —
	// microVM, sandbox, container, three engines deep. The engine that runs it
	// is the inner sandbox's own nested rootless podman (SPEC R104), a different
	// engine and a different store from the guest's — so the image makes the
	// second hop too. Nothing else covers R102's namespaced capability set doing
	// its job under a hypervisor rather than on the host.
	//
	// A subtest, so it can skip on its own: the workload image comes from a
	// registry, and on an offline host losing this stage is right where losing
	// the boot, the ship and the nested create above it would not be.
	t.Run("workload", func(t *testing.T) {
		const workload = "docker.io/library/busybox"
		requireWorkloadImage(t, workload)

		// Piped straight through: `podman save` on the host, `podman load` in
		// the inner sandbox, with the guest only the ssh hop in between. It is
		// 4 MB, but staging it in the guest would put it in a third store that
		// nothing under test reads.
		step(t, "shipping %s into %s…", workload, inner)
		save := exec.Command("podman", "save", workload)
		pipe, err := save.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := save.Start(); err != nil {
			t.Fatalf("podman save %s: %v", workload, err)
		}
		load := fmt.Sprintf("ssh -o StrictHostKeyChecking=no %s.default 'podman load'", inner)
		if out := sshPipe(t, host, outer, load, pipe); !strings.Contains(out, "Loaded image") {
			t.Fatalf("podman load in the inner sandbox did not report a loaded image:\n%s", out)
		}
		if err := save.Wait(); err != nil {
			t.Fatalf("podman save %s: %v", workload, err)
		}

		// The workload itself. Default networking, no --network=none: publishing
		// a port from a nested container is a shipped promise (README), so a
		// netavark bridge that cannot come up in here is a failure, not a
		// reason to ask for less.
		step(t, "running the innermost container…")
		got := sshOK(t, host, outer, fmt.Sprintf(
			"ssh -o StrictHostKeyChecking=no %s.default 'podman run --rm %s echo nested-ok'", inner, workload))
		if !strings.Contains(got, "nested-ok") {
			t.Fatalf("podman run inside the inner sandbox printed %q, want it to print nested-ok", got)
		}

		// And it ran on the engine that makes this worth its minutes: the inner
		// sandbox's nested podman, rootless as the dev user. A rootful answer
		// would mean something re-introduced a privileged inner engine — the run
		// above would still pass, while proving something much weaker.
		rootless := strings.TrimSpace(sshCapture(t, host, outer, fmt.Sprintf(
			`ssh -o StrictHostKeyChecking=no %s.default "podman info --format '{{.Host.Security.Rootless}}'"`, inner)))
		if rootless != "true" {
			t.Errorf("inner sandbox's nested engine reports rootless=%q, want %q", rootless, "true")
		}
	})
}

// requireWorkloadImage makes sure the innermost workload image is in the host's
// store, pulling it once if it is not. It skips rather than fails when there is
// no registry to pull from: the smoke profile runs on developer machines too,
// and an offline host should lose this one stage, not the run.
func requireWorkloadImage(t *testing.T, img string) {
	t.Helper()
	ctx := context.Background()
	r := &run.Exec{}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", img); err == nil {
		return
	}
	step(t, "pulling %s (the innermost workload, ~4 MB)…", img)
	if _, err := r.Run(ctx, run.Opts{}, "podman", "pull", img); err != nil {
		t.Skipf("cannot pull %s: %v", img, err)
	}
}

// buildCLI builds cmd/cs-sandbox for the host and returns the binary path. The
// guest is x86_64 Linux and so is any host that got past the /dev/kvm check, so
// a plain build is already the right target.
//
// Deliberately NOT built with -cover, unlike the equivalent helpers in cs-vcr
// and cs-ledger. This binary is also piped into the guest and run there, where
// no GOCOVERDIR exists and none could be collected. An instrumented binary that
// cannot write its counters is not silent about it: it prints "warning:
// GOCOVERDIR not set" to stderr on every exit, or two "error:" lines when the
// directory is set but unreachable. The tests here read the CLI's output, so
// that noise would fail them for a reason having nothing to do with the change
// under test. The tier still gets coverage -- the test binaries themselves are
// instrumented through COVERFLAGS -- it just does not extend into the guest.
func buildCLI(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "cs-sandbox")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/cs-sandbox")
	cmd.Dir = "../.." // this package's dir -> repo root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cs-sandbox: %v\n%s", err, b)
	}
	return out
}

// TestCLIListShowsInstanceLive: `ls` reports a live instance with its engine.
func TestCLIListShowsInstanceLive(t *testing.T) {
	r, _ := liveSetup(t)
	name := boxName("ls")
	createBox(t, r, name)

	out, err := execRoot(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out, name) || !strings.Contains(out, "podman") {
		t.Errorf("ls output missing %s/podman:\n%s", name, out)
	}
	// A sandbox that create just returned is running, and its age is fresh.
	for _, want := range []string{"NAME", "STATUS", "AGE", "running"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q:\n%s", want, out)
		}
	}

	// Stopping it must be visible in the same column.
	if _, err := execRoot(t, "stop", name); err != nil {
		t.Fatalf("stop: %v", err)
	}
	out, err = execRoot(t, "ls")
	if err != nil {
		t.Fatalf("ls after stop: %v", err)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("ls should report the stopped sandbox as stopped:\n%s", out)
	}

	// -q is the scripting form: bare names, no header.
	q, err := execRoot(t, "ls", "-q")
	if err != nil {
		t.Fatalf("ls -q: %v", err)
	}
	if !strings.Contains(q, name) || strings.Contains(q, "NAME") || strings.Contains(q, "podman") {
		t.Errorf("ls -q should print bare names only:\n%s", q)
	}
}

// TestCLIPortForwardLive: forward a host port to a listener inside a sandbox,
// fetch through it, then tear the forward down.
func TestCLIPortForwardLive(t *testing.T) {
	r, _ := liveSetup(t)
	ctx := context.Background()
	name := boxName("fwd")
	createBox(t, r, name)

	// Start a trivial HTTP server inside the sandbox on :8099 (detached).
	if _, err := r.Run(ctx, run.Opts{}, "podman", "exec", "-d", objName(name),
		"bash", "-lc", "cd /tmp && nohup python3 -m http.server 8099 >/tmp/http.log 2>&1 &"); err != nil {
		t.Fatalf("start in-sandbox http server: %v", err)
	}

	if _, err := execRoot(t, "forward", name, "18099:8099"); err != nil {
		t.Fatalf("forward: %v", err)
	}
	t.Cleanup(func() { _, _ = execRoot(t, "unforward", name, "all") })

	// Poll the host-side forwarded port (ssh -L + server both need a moment).
	ok := false
	for range 20 {
		if _, err := r.Run(ctx, run.Opts{}, "curl", "-fsS", "-o", "/dev/null", "http://127.0.0.1:18099/"); err == nil {
			ok = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ok {
		t.Errorf("forwarded port 18099 never served a response")
	}

	// It shows up in `forwards`, and unforward clears it.
	if out, _ := execRoot(t, "forwards", name); !strings.Contains(out, "18099") {
		t.Errorf("forwards did not list the active forward:\n%s", out)
	}
	if _, err := execRoot(t, "unforward", name, "all"); err != nil {
		t.Fatalf("unforward: %v", err)
	}
}

// TestCLISocksForwardLive: `forward --socks` opens a working SOCKS proxy through
// the sandbox — a request via it reaches the sandbox's own HTTP server.
func TestCLISocksForwardLive(t *testing.T) {
	r, _ := liveSetup(t)
	ctx := context.Background()
	name := boxName("socks")
	createBox(t, r, name)

	if _, err := r.Run(ctx, run.Opts{}, "podman", "exec", "-d", objName(name),
		"bash", "-lc", "cd /tmp && nohup python3 -m http.server 8098 >/tmp/http.log 2>&1 &"); err != nil {
		t.Fatalf("start in-sandbox http server: %v", err)
	}

	// --socks needs =VALUE syntax (the flag has an optional default).
	if _, err := execRoot(t, "forward", name, "--socks=11080"); err != nil {
		t.Fatalf("forward --socks: %v", err)
	}
	t.Cleanup(func() { _, _ = execRoot(t, "unforward", name, "all") })

	// Through the SOCKS proxy, localhost:8098 resolves from the sandbox's side.
	ok := false
	for range 20 {
		if _, err := r.Run(ctx, run.Opts{}, "curl", "-fsS", "-o", "/dev/null",
			"--socks5-hostname", "127.0.0.1:11080", "http://localhost:8098/"); err == nil {
			ok = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ok {
		t.Errorf("SOCKS proxy on 11080 never reached the sandbox's HTTP server")
	}
	if out, _ := execRoot(t, "forwards", name); !strings.Contains(out, "11080") {
		t.Errorf("forwards did not list the socks proxy:\n%s", out)
	}
}

// TestCLIImageStoreUseLive: a sandbox created with --image-store can use an image
// TestCLINestedRootlessPodmanLive: nested podman in a container sandbox is rootless,
// and an inner container's bind-mounted files come back owned by the sandbox user (R106).
//
// The second half is what the container engine has to earn: a keep-id container is the
// one place where an inner root could be a subuid writing files nobody in the sandbox
// owns. Rootless makes the sandbox user the inner root, so plain `podman run -v` is
// enough — and no flag, wrapper or injected --user stands behind that.
func TestCLINestedRootlessPodmanLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	const workload = "docker.io/library/busybox"
	requireWorkloadImage(t, workload)
	name := boxName("rootless")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })

	if out, err := execRoot(t, "create", name, "--engine", "podman"); err != nil {
		t.Fatalf("create: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(inBox(ctx, r, host, name,
		"podman info --format '{{.Host.Security.Rootless}}'")); got != "true" {
		t.Fatalf("nested engine reports rootless=%q, want \"true\"", got)
	}
	// The store is the user's own rootless graphroot, on its own volume.
	if got := strings.TrimSpace(inBox(ctx, r, host, name,
		"podman info --format '{{.Store.GraphRoot}}'")); !strings.HasSuffix(got, "/.local/share/containers/storage") {
		t.Errorf("nested graphroot = %q, want the user's rootless store", got)
	}
	uid := strings.TrimSpace(inBox(ctx, r, host, name, "id -u"))
	owner := strings.TrimSpace(inBox(ctx, r, host, name, "mkdir -p ~/nested && "+
		"podman run --rm -v ~/nested:/w "+workload+" touch /w/f >/dev/null 2>&1; stat -c %u ~/nested/f"))
	if owner != uid {
		t.Errorf("file written by an inner container is owned by uid %q, want the sandbox user %q", owner, uid)
	}
}

// TestCLIImageStoreUseOnMicroVMLive: --image-store works on the microVM engine too —
// the store arrives as a read-only ext4 disk and the guest's nested podman runs from it
// without pulling.
//
// The podman-engine twin of this cannot cover it: the two engines register the store by
// different routes (a mounted volume against a built disk) and only share the config the
// guest init writes. Written to the wrong file that config is ignored in silence — the
// disk still mounts, and `podman images` just comes back empty.
func TestCLIImageStoreUseOnMicroVMLive(t *testing.T) {
	_, host := liveSetup(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	fcInstancesDir(t, host)
	store := fmt.Sprintf("csgofcstore%d%s", os.Getpid(), runID)
	name := boxName("fcstore")
	t.Cleanup(func() {
		_, _ = execRoot(t, "destroy", name, "-f")
		_, _ = execRoot(t, "rm-store", store, "-f")
	})

	if out, err := execRoot(t, "create-store", store); err != nil {
		t.Fatalf("create-store: %v (%s)", err, out)
	}
	if out, err := execRoot(t, "seed-store", store, "docker.io/library/busybox"); err != nil {
		t.Fatalf("seed-store: %v (%s)", err, out)
	}
	if out, err := execRoot(t, "create", name, "--engine", "firecracker", "--image-store", store); err != nil {
		t.Fatalf("create --image-store: %v (%s)", err, out)
	}

	// Over ssh, not `podman exec`: a microVM has no container to exec into.
	if got := sshCapture(t, host, name, "podman images docker.io/library/busybox --format '{{.Repository}}'"); !strings.Contains(got, "busybox") {
		t.Errorf("the guest's nested podman did not see busybox from the --image-store: %q", got)
	}
	// Registered is not the same as usable: the ids the store records have to be the ids
	// this engine resolves, or the layers are there and the image still will not run.
	if got := sshCapture(t, host, name, "podman run --rm --pull=never docker.io/library/busybox echo fc-store-ok 2>&1"); !strings.Contains(got, "fc-store-ok") {
		t.Errorf("running from the --image-store in a microVM printed %q", got)
	}
}

// from the seeded store via nested podman, without pulling — end to end.
func TestCLIImageStoreUseLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	store := fmt.Sprintf("csgostore%d%s", os.Getpid(), runID)
	name := boxName("store")
	t.Cleanup(func() {
		_, _ = execRoot(t, "destroy", name, "-f")
		_, _ = execRoot(t, "rm-store", store, "-f")
	})

	if out, err := execRoot(t, "create-store", store); err != nil {
		t.Fatalf("create-store: %v (%s)", err, out)
	}
	if out, err := execRoot(t, "seed-store", store, "docker.io/library/busybox"); err != nil {
		t.Fatalf("seed-store: %v (%s)", err, out)
	}
	if out, err := execRoot(t, "create", name, "--engine", "podman", "--image-store", store); err != nil {
		t.Fatalf("create --image-store: %v (%s)", err, out)
	}

	// The nested (rootless) podman sees busybox from the mounted store — no pull.
	got := inBox(ctx, r, host, name, "podman images docker.io/library/busybox --format '{{.Repository}}' 2>/dev/null")
	if !strings.Contains(got, "busybox") {
		t.Errorf("nested podman did not see busybox from the --image-store: %q", got)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// waitInBox polls a shell test inside the sandbox until it succeeds or times out
// (for first-boot-async artifacts like the --repo clone).
func waitInBox(t *testing.T, r *run.Exec, host hostenv.Host, name, sh string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := r.Run(context.Background(), run.Opts{}, "podman", "exec", "--user", host.User,
			"--workdir", "/home/"+host.User, objName(name), "bash", "-lc", sh); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %q in %s", sh, name)
}

// hostGitInit creates a host source repo with an identity and an initial commit.
func hostGitInit(t *testing.T, r *run.Exec, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		if _, err := r.Run(ctx, run.Opts{Dir: dir}, append([]string{"git"}, args...)...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

// TestCLIRepoPushLive: `push` fast-forwards a host-side commit into the sandbox's
// --repo checkout (the reverse of the fetch the engine test covers).
func TestCLIRepoPushLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	src := filepath.Join(shareDir(t, host), "proj")
	hostGitInit(t, r, src)

	name := boxName("push")
	createBox(t, r, name, "--repo", src)
	waitInBox(t, r, host, name, "test -d ~/proj/.git", 90*time.Second)

	// A new host-side commit, then push it into the sandbox.
	if _, err := r.Run(ctx, run.Opts{Dir: src}, "git", "commit", "--allow-empty", "-m", "host-side change"); err != nil {
		t.Fatalf("host commit: %v", err)
	}
	if _, err := execRoot(t, "push", name); err != nil {
		t.Fatalf("push: %v", err)
	}
	if log := inBox(ctx, r, host, name, "git -C ~/proj log --oneline"); !strings.Contains(log, "host-side change") {
		t.Errorf("sandbox checkout missing pushed commit:\n%s", log)
	}
}

// TestCLISnapshotShareLive: `create --snapshot <dir>` shares a frozen copy of a
// directory into the sandbox.
func TestCLISnapshotShareLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	snap := filepath.Join(shareDir(t, host), "snapdata")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "hello.txt"), []byte("frozen-hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := boxName("snap")
	createBox(t, r, name, "--snapshot", snap)
	waitInBox(t, r, host, name, "test -f ~/snapdata/hello.txt", 90*time.Second)

	if got := inBox(ctx, r, host, name, "cat ~/snapdata/hello.txt"); !strings.Contains(got, "frozen-hi") {
		t.Errorf("snapshot content in sandbox = %q, want frozen-hi", got)
	}
}

// TestCLIFirecrackerCrossEngineLive: create a real microVM via the CLI, prove its
// per-instance disks exist, and prove cross-engine reachability — a podman
// container resolves the firecracker VM by name on the shared fabric. Skips
// without /dev/kvm or the cached FC artifacts.
func TestCLIFirecrackerCrossEngineLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	instDir := fcInstancesDir(t, host)

	fbox := boxName("xfc")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", fbox, "-f") })
	step(t, "booting firecracker microVM %s (takes ~30s)…", fbox)
	start := time.Now()
	if out, err := execRoot(t, "create", fbox, "--engine", "firecracker"); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	step(t, "microVM %s booted (%s)", fbox, time.Since(start).Round(time.Second))
	// The per-instance reflink rootfs and the seed.ext4 disk exist on disk.
	for _, disk := range []string{"rootfs.ext4", "seed.ext4"} {
		if !fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, fbox), disk)) {
			t.Errorf("firecracker instance missing %s", disk)
		}
	}

	// A podman container resolves the firecracker VM by name (cross-engine fabric).
	pbox := boxName("xpod")
	createBox(t, r, pbox)
	got := strings.TrimSpace(inBox(ctx, r, host, pbox, "getent hosts "+fbox+" | awk '{print $1}'"))
	if got == "" {
		t.Errorf("podman box could not resolve firecracker box %s by name (cross-engine fabric)", fbox)
	}
}

// TestCLIGroupIsolationLive is the test the isolation boundary never had: it
// asserts DENIAL, live, on both planes.
//
// The network plane rests on one Podman option (isolate=true) whose enforcement
// belongs to netavark, so it can regress on an upgrade with nothing in the code
// changing. The credential plane is why per-group keys exist: if the network
// ever stops isolating, a shared key would turn a reachability bug into an
// immediate breach.
func TestCLIGroupIsolationLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	ga, gb := boxName("ga"), boxName("gb")
	t.Cleanup(func() {
		for _, g := range []string{ga, gb} {
			_, _ = execRoot(t, "group", "rm", g, "-f")
		}
	})
	// The same sandbox NAME in both groups — impossible before groups existed,
	// and the case a fleet harness actually wants.
	const member = "worker"
	for _, g := range []string{ga, gb} {
		if out, err := execRoot(t, "create", member, "--group", g, "--engine", "podman"); err != nil {
			t.Fatalf("create %s.%s: %v (%s)", member, g, err, out)
		}
	}

	ipOf := func(g string) string {
		return strings.TrimSpace(run.Output(ctx, r, "podman", "inspect", member+"."+g,
			"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"))
	}
	ipA, ipB := ipOf(ga), ipOf(gb)
	if ipA == "" || ipB == "" || ipA == ipB {
		t.Fatalf("expected two distinct member addresses, got %q and %q", ipA, ipB)
	}

	// Raw IP across groups must be refused: separate bridges alone would NOT do
	// this, which is the whole reason the networks are created isolated.
	inA := func(sh string) string {
		return strings.TrimSpace(run.Output(ctx, r, "podman", "exec", member+"."+ga, "bash", "-lc", sh))
	}
	if got := inA("timeout 5 bash -c '</dev/tcp/" + ipB + "/22' 2>/dev/null && echo REACHABLE || echo BLOCKED"); got != "BLOCKED" {
		t.Errorf("cross-group raw IP %s -> %s = %q, want BLOCKED", ga, gb, got)
	}
	// Isolation must not cost outbound internet (which --internal would).
	if got := inA("timeout 10 curl -sfI https://api.github.com >/dev/null && echo OK || echo BROKEN"); got != "OK" {
		t.Errorf("group member lost outbound internet: %q", got)
	}

	// The credential plane: group A's key must be refused by a group B sandbox.
	portB, err := execRoot(t, "port", member+"."+gb)
	if err != nil {
		t.Fatal(err)
	}
	ssh := func(keyGroup string) string {
		key := filepath.Join(os.Getenv("CS_SANDBOX_TIER_DIR"), "groups", keyGroup, "id_cs-sandbox_user")
		return run.Output(ctx, r, "ssh", "-i", key, "-p", strings.TrimSpace(portB),
			"-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null", "-o", "BatchMode=yes",
			"-o", "ConnectTimeout=10", host.User+"@127.0.0.1", "echo AUTHOK")
	}
	if out := ssh(ga); strings.Contains(out, "AUTHOK") {
		t.Errorf("group %s's key authenticated to a %s sandbox — the credential boundary is gone", ga, gb)
	}
	if out := ssh(gb); !strings.Contains(out, "AUTHOK") {
		t.Errorf("a group's own key must work against its own sandbox, got %q", out)
	}
}

// TestCLIGroupSameNameLive: identity is (group, name) end to end — records,
// podman objects and the generated ssh aliases all keep the two apart.
func TestCLIGroupSameNameLive(t *testing.T) {
	r, _ := liveSetup(t)
	ctx := context.Background()
	ga, gb := boxName("sa"), boxName("sb")
	t.Cleanup(func() {
		for _, g := range []string{ga, gb} {
			_, _ = execRoot(t, "group", "rm", g, "-f")
		}
	})
	for _, g := range []string{ga, gb} {
		if out, err := execRoot(t, "create", "dup", "--group", g, "--engine", "podman"); err != nil {
			t.Fatalf("create dup.%s: %v (%s)", g, err, out)
		}
	}
	// Both exist as distinct podman objects.
	for _, g := range []string{ga, gb} {
		if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "container", "exists", "dup."+g); err != nil {
			t.Errorf("container dup.%s missing", g)
		}
	}
	// A bare reference is ambiguous and must be refused rather than guessed.
	if _, err := execRoot(t, "port", "dup"); err == nil {
		t.Error("a bare ambiguous name must not resolve")
	}
	// Qualified references work.
	for _, g := range []string{ga, gb} {
		if _, err := execRoot(t, "port", "dup."+g); err != nil {
			t.Errorf("port dup.%s: %v", g, err)
		}
	}
}

// hostCredFile is the credential filename each agent keeps in its host profile
// dir. A new agent needs a line here; the tests below fail loudly rather than
// quietly skipping an agent they do not recognise — silently covering less than
// it claims is exactly how the microVM seed lost opencode's credentials.
var hostCredFile = map[string]string{
	"claude":   ".credentials.json",
	"codex":    "auth.json",
	"opencode": "auth.json",
}

// synthAgentHome redirects $HOME to a temp home holding a synthetic credential
// for EVERY agent, and returns it.
//
// Reading the developer's real logins made these tests cover whatever happened
// to be signed in — which on most machines is not opencode, the one agent whose
// credentials the microVM seed silently dropped. Synthesizing them means every
// agent is exercised on every host, with no skips to hide behind.
//
// XDG_CACHE_HOME stays pinned to the real cache: the firecracker artifacts and
// the host-global network fabric live there, and a fresh HOME would hide both.
func synthAgentHome(t *testing.T, host hostenv.Host) {
	t.Helper()
	home, err := os.MkdirTemp(host.Home, ".cs-sandbox-agenthome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	for _, a := range seed.AgentNames() {
		cf, ok := hostCredFile[a]
		if !ok {
			t.Fatalf("agent %q has no credential filename recorded in hostCredFile", a)
		}
		dir := filepath.Join(home, ".cs-"+a)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, cf), []byte(`{"synthetic":"`+a+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Podman's rootless store, its config, the firecracker artifacts and the
	// host-global fabric are all HOME-derived; pin each back to the real one so
	// only the agent profiles move.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(host.Home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(host.Home, ".local", "share"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(host.Home, ".config"))
	t.Setenv("HOME", home)
}

// TestCLIAgentLoginSeedsEveryAgentLive covers the contract shared by all three
// agents, driven by seed.AgentNames() so a fourth needs no new test: the host
// credential is snapshotted into the instance seed, and installed at 0600 in the
// profile dir the wrapper binds. The bespoke tests above additionally cover what
// is specific to claude and opencode; codex previously had no coverage at all.
func TestCLIAgentLoginSeedsEveryAgentLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	synthAgentHome(t, host)
	agents := seed.AgentNames()
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")

	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			name := boxName("seed" + agent)
			out := createBox(t, r, name, "--inherit-agent-login", agent)
			if !strings.Contains(out, "agent login: "+agent) {
				t.Errorf("create should report the inherited login, got:\n%s", out)
			}
			cf := hostCredFile[agent]
			if !fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", agent, cf)) {
				t.Errorf("%s: host credential was not snapshotted into the seed", agent)
			}
			got := strings.TrimSpace(inBox(ctx, r, host, name,
				"stat -c %a ~/.cs-"+agent+"/"+cf+" 2>/dev/null"))
			if got != "600" {
				t.Errorf("%s: ~/.cs-%s/%s inside the sandbox = %q, want mode 600", agent, agent, cf, got)
			}
		})
	}
}

// TestCLIAgentLoginInheritedFirecrackerLive is the engine half, and the gap that
// let a real bug ship: every agent-login test ran on podman, whose seed the
// entrypoint reads straight from disk. A microVM instead gets its credentials
// packed into seed.ext4, and that packing step iterated a hardcoded
// {"claude", "codex"} — so `--inherit-agent-login opencode --engine firecracker`
// reported success and produced a VM with no login, invisibly.
//
// One VM carries every available agent: booting one costs ~30s, three would cost
// three times that for no extra signal.
func TestCLIAgentLoginInheritedFirecrackerLive(t *testing.T) {
	_, host := liveSetup(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	agents := seed.AgentNames()
	instDir := fcInstancesDir(t, host)
	synthAgentHome(t, host)
	name := boxName("fclogin")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })

	step(t, "booting firecracker microVM %s with %s (takes ~30s)…", name, strings.Join(agents, ","))
	start := time.Now()
	if out, err := execRoot(t, "create", name, "--engine", "firecracker",
		"--inherit-agent-login", strings.Join(agents, ",")); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	step(t, "microVM %s booted (%s)", name, time.Since(start).Round(time.Second))

	for _, agent := range agents {
		cf := hostCredFile[agent]
		if !fileExists(filepath.Join(state.Dir(instDir, state.DefaultGroup, name), "seed", agent, cf)) {
			t.Errorf("%s: host credential was not snapshotted into the seed", agent)
		}
		// The assertion that matters: it survived the trip through seed.ext4 and
		// was installed by the guest's PID 1.
		got := strings.TrimSpace(sshCapture(t, host, name, "stat -c %a ~/.cs-"+agent+"/"+cf+" 2>/dev/null"))
		if got != "600" {
			t.Errorf("%s: ~/.cs-%s/%s inside the microVM = %q, want mode 600", agent, agent, cf, got)
		}
	}
}

// TestGatewayResolvesAMicroVMMemberLive: the gateway's whole purpose is reaching
// members BY NAME through one published port. A group has two resolvers —
// aardvark, which containers get by default and which knows container names, and
// the fabric's dnsmasq, which serves microVM names — and a gateway given the
// wrong one is reachable-but-nameless: `curl <ip>` works while `getent hosts
// <member>` does not. That failure is invisible on a podman-only group, whose
// members ARE containers, which is why it survived until a firecracker campaign
// tried to use it.
//
// Asserted from inside the gateway, because that is where the operator's
// `ssh -L 8080:<member>:<port> <group>-gw` resolves the name.
func TestGatewayResolvesAMicroVMMemberLive(t *testing.T) {
	r, host := liveSetup(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	fcInstancesDir(t, host)
	// Short names deliberately: group and member share one 108-byte socket-path
	// budget, and two full boxName()s overrun it — as ValidInstancePath will
	// tell you, which is the check existing to stop that reaching a microVM.
	group := "gwdns-" + runID
	name := boxName("m")
	ctx := context.Background()
	t.Cleanup(func() { _, _ = execRoot(t, "group", "rm", group, "-f") })

	step(t, "booting a microVM in group %s (takes ~30s)…", group)
	if out, err := execRoot(t, "create", name, "--group", group, "--engine", "firecracker"); err != nil {
		t.Fatalf("create: %v (out=%q)", err, out)
	}

	// The name the operator would use: the member's bare in-group name.
	gw := fcnet.KeepaliveFor(state.NetworkName(group))
	out, err := r.Run(ctx, run.Opts{ReadOnly: true},
		"podman", "exec", gw, "getent", "hosts", name)
	if err != nil || !strings.Contains(out.Stdout, name) {
		t.Fatalf("the gateway cannot resolve member %q — it was given the wrong resolver\n"+
			"  getent: %v %q", name, err, out.Stdout)
	}
	// And the address it answers with is the member's, not something stale.
	in, err := state.Load(paths.Instances(), group, name)
	if err != nil {
		t.Fatal(err)
	}
	if in.FCIP != "" && !strings.Contains(out.Stdout, in.FCIP) {
		t.Errorf("gateway resolved %s to %q, want the member's address %s", name, out.Stdout, in.FCIP)
	}
}
