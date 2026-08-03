//go:build integration

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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func image() string {
	if v := os.Getenv("CS_SANDBOX_IMAGE"); v != "" {
		return v
	}
	return "localhost/cs-sandbox:44"
}

// execRoot runs a fresh command tree (as production would) and returns stdout.
func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := root.Execute()
	return out.String(), err
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
	img := image()
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
func boxName(t *testing.T, tag string) string {
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
func fcInstancesDir(t *testing.T, host hostenv.Host) string {
	t.Helper()
	dir, err := os.MkdirTemp(host.Home, ".cs-sandbox-fctest-")
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
	name := boxName(t, "cxd")

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
	name := boxName(t, "reuse")
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

	// rm keeps the data and removes the sandbox from `ls`.
	if out, err := execRoot(t, "rm", name); err != nil {
		t.Fatalf("rm: %v (%s)", err, out)
	}
	if ls, _ := execRoot(t, "ls"); strings.Contains(ls, name) {
		t.Errorf("after rm, %s should be gone from ls:\n%s", name, ls)
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
	name := boxName(t, "tools")
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
	// The guest-only user-podman lives there too and is executable.
	if got := strings.TrimSpace(inBox(ctx, r, host, name, "test -x ~/.local/bin/user-podman && echo ok")); got != "ok" {
		t.Errorf("user-podman not executable in sandbox: %q", got)
	}
}

// TestCLIAgentLoginInheritedLive: --inherit-agent-login claude snapshots the host
// login into the seed and first boot installs it at 0600. Existence/mode only,
// never contents.
func TestCLIAgentLoginInheritedLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	if !fileExists(filepath.Join(host.Home, ".cs-claude", ".credentials.json")) {
		t.Skip("host has no ~/.cs-claude/.credentials.json to inherit")
	}
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	name := boxName(t, "login")
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
	if !fileExists(filepath.Join(host.Home, ".cs-opencode", "auth.json")) {
		t.Skip("host has no ~/.cs-opencode/auth.json to inherit")
	}
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	name := boxName(t, "oclogin")
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
	if !fileExists(filepath.Join(host.Home, ".cs-claude", ".credentials.json")) {
		t.Skip("host has no ~/.cs-claude/.credentials.json (nothing that could leak in)")
	}
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")

	plain := boxName(t, "nologin")
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

	one := boxName(t, "onelogin")
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

	name := boxName(t, "nokeys")
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
	name := boxName(t, "repo")
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
		name := boxName(t, "yolo"+typ)
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

	uname := boxName(t, "user")
	createBox(t, r, uname, "--type", "user")
	userSeed := filepath.Join(instDir, uname, "seed")
	if !fileExists(filepath.Join(userSeed, "id_cs-sandbox_user")) {
		t.Errorf("user sandbox missing user tier key in seed")
	}
	if fileExists(filepath.Join(userSeed, "id_cs-sandbox_agent")) {
		t.Errorf("user sandbox should not hold the agent tier key")
	}

	aname := boxName(t, "agent")
	createBox(t, r, aname, "--type", "agent")
	if !fileExists(filepath.Join(instDir, aname, "seed", "id_cs-sandbox_agent")) {
		t.Errorf("agent sandbox missing agent tier key in seed")
	}
}

// TestCLIEnvInjectionLive: `create -e KEY=VALUE` resolves the var into the seed's
// inject-env file (installed into the sandbox as ~/.ssh/environment), written 0600.
func TestCLIEnvInjectionLive(t *testing.T) {
	r, _ := liveSetup(t)
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")
	name := boxName(t, "env")
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
	a, b := boxName(t, "neta"), boxName(t, "netb")
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
	portStr, err := execRoot(t, "port", name)
	if err != nil {
		t.Fatalf("port %s: %v", name, err)
	}
	// Trust material is per group; these tests create default-group sandboxes.
	key := filepath.Join(paths.GroupKeys(state.DefaultGroup), "id_cs-sandbox_user")
	return run.Output(context.Background(), &run.Exec{}, "ssh",
		"-i", key, "-p", strings.TrimSpace(portStr),
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "-o", "LogLevel=ERROR",
		fmt.Sprintf("%s@127.0.0.1", host.User), sh)
}

// assertHostByName checks the host-by-name wiring for a live sandbox: the seed
// pins the host's name(s) to the reachable pasta address, and inside the guest a
// getaddrinfo client (gai.conf ordering) picks that IPv4 over any unreachable
// AAAA the host's resolver returns. inGuest runs a shell snippet in the sandbox.
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
	hn := host.Names[0]
	got := strings.TrimSpace(inGuest("getent ahosts " + hn + " | awk 'NR==1{print $1}'"))
	if got != "169.254.1.2" {
		t.Errorf("host name %q resolves to %q inside the sandbox, want 169.254.1.2", hn, got)
	}
}

// TestCLIHostByNameLive: a podman sandbox can reach the host by its own name.
func TestCLIHostByNameLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	if len(host.Names) == 0 {
		t.Skip("host exposes no name to map")
	}
	name := boxName(t, "hostname")
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
	name := boxName(t, "hostnamefc")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	step(t, "booting firecracker microVM %s (takes ~30s)…", name)
	start := time.Now()
	if out, err := execRoot(t, "create", name, "--engine", "firecracker"); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	step(t, "microVM %s booted (%s)", name, time.Since(start).Round(time.Second))
	assertHostByName(t, host, name, func(sh string) string { return sshCapture(t, host, name, sh) })
}

// TestCLIListShowsInstanceLive: `ls` reports a live instance with its engine.
func TestCLIListShowsInstanceLive(t *testing.T) {
	r, _ := liveSetup(t)
	name := boxName(t, "ls")
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
	name := boxName(t, "fwd")
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
	for i := 0; i < 20; i++ {
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
	name := boxName(t, "socks")
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
	for i := 0; i < 20; i++ {
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
// from the seeded store via nested podman, without pulling — end to end.
func TestCLIImageStoreUseLive(t *testing.T) {
	r, host := liveSetup(t)
	ctx := context.Background()
	store := fmt.Sprintf("csgostore%d%s", os.Getpid(), runID)
	name := boxName(t, "store")
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

	// The nested (rootful) podman sees busybox from the mounted store — no pull.
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

// TestCLIHostRouteReadOnlyLive: the sudo-free host-route paths work end-to-end.
// FCCache is isolated so host-route is guaranteed inactive — status reports down
// and refresh is rejected, both WITHOUT ever invoking sudo or touching host
// networking. (The privileged up/down roundtrip needs interactive sudo +
// systemd-resolved and is intentionally not exercised.)
func TestCLIHostRouteReadOnlyLive(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("host-route is Linux-only")
	}
	// The fabric working dir is host-global, so isolate it explicitly: an empty
	// one has no marker file, making host-route guaranteed inactive.
	t.Setenv("CS_SANDBOX_FC_NET", t.TempDir())

	out, err := execRoot(t, "host-route", "status")
	if err != nil {
		t.Fatalf("host-route status: %v", err)
	}
	if !strings.Contains(out, "down") {
		t.Errorf("status = %q, want it to report down", out)
	}
	// refresh when down is rejected cleanly, before any privileged wiring.
	if _, err := execRoot(t, "host-route", "refresh"); err == nil || !strings.Contains(err.Error(), "not up") {
		t.Errorf("host-route refresh when down err = %v, want 'not up'", err)
	}
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

	name := boxName(t, "push")
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
	name := boxName(t, "snap")
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
	instDir := os.Getenv("CS_SANDBOX_INSTANCES_DIR")

	fbox := boxName(t, "xfc")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", fbox, "-f") })
	step(t, "booting firecracker microVM %s (takes ~30s)…", fbox)
	start := time.Now()
	if out, err := execRoot(t, "create", fbox, "--engine", "firecracker"); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	step(t, "microVM %s booted (%s)", fbox, time.Since(start).Round(time.Second))
	// The per-instance reflink rootfs and the seed.ext4 disk exist on disk.
	for _, disk := range []string{"rootfs.ext4", "seed.ext4"} {
		if !fileExists(filepath.Join(instDir, fbox, disk)) {
			t.Errorf("firecracker instance missing %s", disk)
		}
	}

	// A podman container resolves the firecracker VM by name (cross-engine fabric).
	pbox := boxName(t, "xpod")
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
	ga, gb := boxName(t, "ga"), boxName(t, "gb")
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
	ssh := func(keyGroup string) (string, error) {
		key := filepath.Join(os.Getenv("CS_SANDBOX_TIER_DIR"), "groups", keyGroup, "id_cs-sandbox_user")
		return run.Output(ctx, r, "ssh", "-i", key, "-p", strings.TrimSpace(portB),
			"-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null", "-o", "BatchMode=yes",
			"-o", "ConnectTimeout=10", host.User+"@127.0.0.1", "echo AUTHOK"), nil
	}
	if out, _ := ssh(ga); strings.Contains(out, "AUTHOK") {
		t.Errorf("group %s's key authenticated to a %s sandbox — the credential boundary is gone", ga, gb)
	}
	if out, _ := ssh(gb); !strings.Contains(out, "AUTHOK") {
		t.Errorf("a group's own key must work against its own sandbox, got %q", out)
	}
}

// TestCLIGroupSameNameLive: identity is (group, name) end to end — records,
// podman objects and the generated ssh aliases all keep the two apart.
func TestCLIGroupSameNameLive(t *testing.T) {
	r, _ := liveSetup(t)
	ctx := context.Background()
	ga, gb := boxName(t, "sa"), boxName(t, "sb")
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
func synthAgentHome(t *testing.T, host hostenv.Host) string {
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
	return home
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
			name := boxName(t, "seed"+agent)
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
	name := boxName(t, "fclogin")
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
