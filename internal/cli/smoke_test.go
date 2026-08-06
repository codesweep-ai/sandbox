//go:build integration || smoke

// The smoke profile: the subset of the live tests small enough to run in CI on
// every host cs-sandbox is driven from — Linux, macOS, and Windows under WSL2.
//
//	make test-smoke        # this subset alone, on any host, in seconds
//	make test-integration  # everything, including this subset
//
// Most of the profile is existing live tests run verbatim — CI boots real
// sandboxes on Linux, on WSL2, and on an Intel macOS runner, against a slimmed
// image (see image/ci-slim.sh). The members defined HERE exist for the one host
// that can never do that: arm64 macOS, where the runner is an M1 VM reporting
// kern.hv_support=0, so no hypervisor exists to run a podman machine in. There
// the live members skip and these still run, which is what lets one
// `make test-smoke` be correct on every host.
//
// Two rules keep the profile honest. The subset is spelled out in the Makefile
// (SMOKE_TESTS) rather than inferred, so what CI runs is greppable. And nothing
// goes in that a unit test already covers: `ls --json`, `inspect`, the doctor
// branching and the create-flag validation are all tested with fakes already,
// and re-asserting them here would buy CI time with no signal. What is left is
// what only a real host can answer — the state root this OS actually uses, and
// an ssh config this OS's own ssh actually parses.
package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// execRoot runs a fresh command tree (as production would) and returns stdout.
// Shared by both profiles, which is why it lives in this file rather than with
// the podman-only tests.
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

// smokeHome points the whole tool at a throwaway HOME and clears every env
// override, so a test resolves the SAME state root a real user gets rather than
// one it was handed.
//
// That is the entire point of these two tests, and it is the one thing
// liveSetup cannot do: it sets CS_SANDBOX_INSTANCES_DIR to a temp dir, so the
// live suite never touches the root cs-sandbox picks for itself. ~/.ssh moves
// with HOME too, so nothing here can reach the developer's own config.
func smokeHome(t *testing.T) string {
	t.Helper()
	for _, k := range []string{
		"CS_SANDBOX_HOME", "CS_SANDBOX_INSTANCES_DIR", "CS_SANDBOX_TIER_DIR",
		"CS_SANDBOX_FC_CACHE", "CS_SANDBOX_FC_NET", "CS_SANDBOX_ASSETS_DIR",
		"CS_SANDBOX_IMAGE", "CS_SANDBOX_ENGINE", "CS_SANDBOX_GROUP",
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME",
	} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// smokeStateRoot is where a real user's sandboxes live on this OS, spelled here
// independently of internal/paths. Deriving it from the code under test would
// make the assertion agree with that code however it changed — including a
// change that put macOS state somewhere macOS cannot write.
func smokeStateRoot(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "cs-sandbox")
	}
	return filepath.Join(home, ".local", "share", "cs-sandbox")
}

// smokeRecords writes what a create leaves behind, minus the sandbox: a default
// group with one member, and a second group with a gateway. Written through the
// state package, so they are the shape the CLI reads back rather than a fixture
// pinning the file format.
func smokeRecords(t *testing.T, instDir string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, g := range []*state.Group{
		{Name: state.DefaultGroup, Created: now, TapPrefix: "fd0"},
		{Name: "smokegrp", Created: now, TapPrefix: "fd1", GWPort: 2401},
	} {
		if err := state.SaveGroup(instDir, g); err != nil {
			t.Fatal(err)
		}
	}
	for _, in := range []*state.Instance{
		{Name: "smokebox", Group: state.DefaultGroup, Type: "agent", Engine: state.Podman, Port: 2287, Created: now},
		{Name: "member", Group: "smokegrp", Type: "user", Engine: state.Podman, Port: 2288, Created: now},
	} {
		if err := state.Save(instDir, in); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSmokeListsASandboxAtTheDefaultStateRoot is TestCLIListShowsInstanceLive
// scaled down: the same command and the same assertion, over a sandbox that is
// a record rather than a running container — which is what lets it run on a
// host with no engine.
//
// What it adds over the live test is the root itself. Every other test in this
// repository redirects the state root into a temp dir, so nothing anywhere
// exercises the one a user actually gets — and on macOS that is
// ~/Library/Application Support/cs-sandbox, a path with a space in it.
func TestSmokeListsASandboxAtTheDefaultStateRoot(t *testing.T) {
	home := smokeHome(t)
	instDir := filepath.Join(smokeStateRoot(home), "instances")
	if got := paths.Instances(); got != instDir {
		t.Fatalf("default instances root = %q, want %q", got, instDir)
	}
	smokeRecords(t, instDir)

	out, err := execRoot(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	for _, want := range []string{"NAME", "STATUS", "smokebox", "member", "podman"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls output missing %q:\n%s", want, out)
		}
	}
}

// TestSmokeSSHConfigParsesOnThisHost is where the live suite's ssh assertions
// stop being reachable in CI. Those connect to a real sandbox (sshCapture, the
// ssh-whoami in TestPodmanCreateLive) and so prove the generated config works;
// this goes as far as a host with no sandbox can — the config this OS's own ssh
// is handed, and whether it accepts it.
//
// macOS is the reason it exists. Its state root contains a space, so an
// unquoted argument makes ssh reject the whole file ("keyword identityfile
// extra arguments at end of line") and every sandbox becomes unreachable by
// name — with the generated text still looking perfectly reasonable, which is
// why hostcfg's own unit tests cannot catch it.
func TestSmokeSSHConfigParsesOnThisHost(t *testing.T) {
	home := smokeHome(t)
	smokeRecords(t, filepath.Join(smokeStateRoot(home), "instances"))

	if _, err := execRoot(t, "sync-ssh-config"); err != nil {
		t.Fatalf("sync-ssh-config: %v", err)
	}
	frag := filepath.Join(home, ".ssh", "config.d", "cs-sandbox")
	fi, err := os.Stat(frag)
	if err != nil {
		t.Fatalf("no managed fragment at %s: %v", frag, err)
	}
	// It names key paths, so it is as secret as the keys themselves.
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("fragment mode = %o, want 600", fi.Mode().Perm())
	}
	cfg := filepath.Join(home, ".ssh", "config")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("~/.ssh/config was not given the Include: %v", err)
	}

	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client to validate the generated config with")
	}
	// -G resolves the alias the way a real connection would, so this covers the
	// Include wiring and the Host block together: a fragment ssh never reads
	// answers with defaults instead of failing.
	out, err := exec.Command("ssh", "-F", cfg, "-G", "smokebox").CombinedOutput()
	if err != nil {
		t.Fatalf("this host's ssh rejected the generated config: %v\n%s", err, out)
	}
	got := strings.ToLower(string(out))
	for _, want := range []string{"hostname 127.0.0.1", "port 2287"} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh -G smokebox missing %q:\n%s", want, out)
		}
	}
}

// TestSmokeHostRouteReadOnly: the sudo-free host-route paths work end-to-end.
// FCCache is isolated so host-route is guaranteed inactive — status reports down
// and refresh is rejected, both WITHOUT ever invoking sudo or touching host
// networking. (The privileged up/down roundtrip needs interactive sudo +
// systemd-resolved and is intentionally not exercised.)
//
// It joins the smoke profile unchanged: alone among the live tests it never
// needed podman or the sandbox image, so it already ran anywhere.
func TestSmokeHostRouteReadOnly(t *testing.T) {
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
