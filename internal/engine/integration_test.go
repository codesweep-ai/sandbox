//go:build integration || smoke

// Integration tests exercise the real podman/firecracker engines on a capable
// Linux/KVM host. Run with:
//
//	go test -tags integration ./internal/engine/ -v
//
// They create real, namespaced sandboxes (csgotest-*) and clean up after.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
)

func testDeps(t *testing.T) Deps {
	t.Helper()
	requirePodman(t, image())
	h, err := hostenv.Detect()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return Deps{
		Runner:       &run.Exec{},
		Host:         h,
		InstDir:      dir,
		TierDir:      filepath.Join(dir, "tier-keys"),
		Image:        image(),
		Network:      "cs-sandbox-net",
		SSHBind:      "127.0.0.1",
		TZ:           "America/Los_Angeles",
		StartTimeout: 90,
	}
}

func image() string {
	if v := os.Getenv("CS_SANDBOX_IMAGE"); v != "" {
		return v
	}
	return "localhost/cs-sandbox:44"
}

func TestPodmanCreateLive(t *testing.T) {
	ctx := context.Background()
	d := testDeps(t)
	if err := d.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatal(err)
	}
	p := NewPodman(d)
	name := uniqName("csgotest")

	// Belt-and-suspenders cleanup even if Create fails midway.
	t.Cleanup(func() {
		_ = p.Remove(context.Background(), name, true)
	})

	inst, err := p.Create(ctx, CreateSpec{Name: name, Type: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.Port < 2200 || inst.Port >= 2300 {
		t.Errorf("podman port %d outside container pool", inst.Port)
	}

	// Container is running.
	running := run.Output(ctx, d.Runner, "podman", "inspect", obj(d.group(), name), "--format", "{{.State.Running}}")
	if running != "true" {
		t.Fatalf("container not running (State.Running=%q)", running)
	}

	// It reached readiness; the dev user was created by the entrypoint.
	waitFile(t, d, name, "/run/cs-sandbox-ready", 90*time.Second)
	if _, err := d.Runner.Run(ctx, run.Opts{}, "podman", "exec", obj(d.group(), name), "id", "-u", d.Host.User); err != nil {
		t.Errorf("dev user %q not created inside the sandbox: %v", d.Host.User, err)
	}

	// End-to-end trust proof: ssh in with the U tier key (an
	// agent sandbox authorizes H+U+G) and land as the dev user, proving the seed
	// trust material works and sshd drops to the developer.
	who := sshWhoami(ctx, d, inst.Port)
	if who != d.Host.User {
		t.Errorf("ssh whoami = %q, want %q (ssh-as-dev-user via U tier key)", who, d.Host.User)
	}

	// The trust seed is present with the expected trust material.
	seedDir := filepath.Join(d.InstanceDir(name), "seed")
	assertFile(t, filepath.Join(seedDir, "authorized_keys"))
	assertFile(t, filepath.Join(seedDir, "ssh_config"))
	assertFile(t, filepath.Join(seedDir, "id_cs-sandbox_agent")) // agent holds G
	// ssh_config carries the peer guard.
	cfg, _ := os.ReadFile(filepath.Join(seedDir, "ssh_config"))
	if !strings.Contains(string(cfg), "Match exec") {
		t.Errorf("ssh_config missing peer guard:\n%s", cfg)
	}
}

// TestPodmanExecDeliversStdinLive: input piped into `exec` has to arrive inside
// the sandbox. This is the assertion the argv-level tests cannot make, and the
// one that was missing while `podman exec` ran without -i and silently ate
// every piped payload.
func TestPodmanExecDeliversStdinLive(t *testing.T) {
	ctx := context.Background()
	d := testDeps(t)
	if err := d.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatal(err)
	}
	p := NewPodman(d)
	name := uniqName("csgotest")
	t.Cleanup(func() { _ = p.Remove(context.Background(), name, true) })

	if _, err := p.Create(ctx, CreateSpec{Name: name, Type: "agent"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitFile(t, d, name, "/run/cs-sandbox-ready", 90*time.Second)

	// Exec hands the host process's own stdio to podman (run.Opts.Interactive),
	// so the payload has to be staged on os.Stdin itself.
	const marker = "STDIN_MARKER"
	restore := stdinFrom(t, marker+"\n")
	err := p.Exec(ctx, name, ExecIO{Argv: []string{"sh", "-c", "cat > /tmp/stdin-probe"}})
	restore()
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	// Read the payload back out of band: Exec streams to the real stdout, which
	// the test cannot capture, but the file the command wrote is the proof.
	got := run.Output(ctx, d.Runner, "podman", "exec", obj(d.group(), name), "cat", "/tmp/stdin-probe")
	if strings.TrimSpace(got) != marker {
		t.Errorf("stdin delivered into the sandbox = %q, want %q", got, marker)
	}
}

// stdinFrom points os.Stdin at a file holding payload, and returns the undo.
// A real file rather than a pipe: no writer goroutine, and the command sees a
// clean EOF. Safe because the suite runs serially (make's -p 1) and no test
// here calls t.Parallel.
func stdinFrom(t *testing.T, payload string) func() {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = f
	return func() {
		os.Stdin = saved
		f.Close()
	}
}

func TestPodmanSoloWithholdsKeyLive(t *testing.T) {
	ctx := context.Background()
	d := testDeps(t)
	if err := d.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureTierKeys(ctx); err != nil {
		t.Fatal(err)
	}
	p := NewPodman(d)
	name := uniqName("csgotestsolo")
	t.Cleanup(func() { _ = p.Remove(context.Background(), name, true) })

	if _, err := p.Create(ctx, CreateSpec{Name: name, Type: "agent", Solo: true}); err != nil {
		t.Fatalf("Create solo: %v", err)
	}
	seedDir := filepath.Join(d.InstanceDir(name), "seed")
	// A --solo agent gets NO tier private key (no outbound auth).
	if _, err := os.Stat(filepath.Join(seedDir, "id_cs-sandbox_agent")); !os.IsNotExist(err) {
		t.Errorf("SECURITY: solo agent must not hold the agent tier key")
	}
	// But authorized_keys is normal (inbound still allowed) — G present.
	ak, _ := os.ReadFile(filepath.Join(seedDir, "authorized_keys"))
	if len(ak) == 0 {
		t.Errorf("solo authorized_keys should be normal, got empty")
	}
}

// sshWhoami connects to the published SSH port with the user-tier private key
// and returns `whoami` — the developer user if the trust seed is correct.
func sshWhoami(ctx context.Context, d Deps, port int) string {
	key := filepath.Join(d.TierDir, "id_cs-sandbox_user")
	return run.Output(ctx, d.Runner, "ssh",
		"-i", key,
		"-p", fmt.Sprintf("%d", port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@127.0.0.1", d.Host.User),
		"whoami")
}

func waitFile(t *testing.T, d Deps, name, path string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := d.Runner.Run(context.Background(), run.Opts{}, "podman", "exec", obj(d.group(), name), "test", "-f", path); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("file %s did not appear in %s", path, budget)
}

func assertFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected seed file %s: %v", path, err)
	}
}
