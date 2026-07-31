package cli

// Contract tests for the OpenCode remote tool family. The kill contract shares the table test in
// remote_helpers_test.go; this file covers the opencode-specific pieces: the stale-PID bystander
// case, the -s status state table, and the cs-opencode-turn driver's completion semantics —
// in particular the mandatory postcheck that fails a turn whose attached `opencode run` exited 0
// while the provider recorded an error on the session (verified against opencode 1.18.10).

import (
	"github.com/codesweep-ai/sandbox/internal/covemit"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const opencodeTestSessionID = "ses_0123456789abcdefghij"

func opencodeToolPath(name string) string {
	return filepath.Join("..", "..", "image", "rootfs", "home", ".local", "bin", name)
}

func opencodeHome(t *testing.T) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	bin = filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-sessions", "-logs", "-pids", "-locks"} {
		if err := os.MkdirAll(filepath.Join(home, ".cs-opencode-remote"+suffix), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home, bin
}

func TestOpenCodeRemoteKillLeavesUnrelatedPIDAlone(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := opencodeHome(t)
	name := "stale-contract"
	if err := os.WriteFile(filepath.Join(home, ".cs-opencode-remote-sessions", name+".token"), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, ".cs-opencode-remote-logs", name+".log")
	if err := os.WriteFile(log, []byte("--- 2026-01-01 00:00:00 --- prompt: wait\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bystander.Process.Kill(); _, _ = bystander.Process.Wait() }()
	pidFile := filepath.Join(home, ".cs-opencode-remote-pids", name+".pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(bystander.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(opencodeToolPath("cs-opencode-remote"), "--kill", name)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--kill: %v: %s", err, out)
	}
	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was signaled despite failing runner verification: %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale PID file not cleaned: %v", err)
	}
	if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "finished (exit 130)") {
		t.Fatalf("interrupted footer missing: %v: %s", err, b)
	}
	covemit.Prove(t, "interrupt", "opencode", "", "scripts")
}

// The -s contract consumed by cs-campaign: finished=0, unknown=1, running=2, failed=3, decided
// by the authoritative footer with the PID only as a secondary liveness signal.
func TestOpenCodeRemoteOutputStatusContract(t *testing.T) {
	skipUnlessLinux(t)
	prompt := "--- 2026-01-01 00:00:00 --- prompt: task\n"
	tests := []struct {
		name, log string
		livePID   bool
		wantOut   string
		wantExit  int
	}{
		{"finished", prompt + "done\n--- 2026-01-01 00:01:00 --- finished (exit 0) ---\n", false, "finished", 0},
		{"failed", prompt + "--- 2026-01-01 00:01:00 --- finished (exit 4) ---\n", false, "failed", 3},
		{"running", prompt, true, "running", 2},
		{"crashed", prompt, false, "unknown", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := opencodeHome(t)
			name := "status-contract"
			log := filepath.Join(home, ".cs-opencode-remote-logs", name+".log")
			if err := os.WriteFile(log, []byte(tc.log), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.livePID {
				proc := exec.Command("sleep", "30")
				if err := proc.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() }()
				pidFile := filepath.Join(home, ".cs-opencode-remote-pids", name+".pid")
				if err := os.WriteFile(pidFile, []byte(strconv.Itoa(proc.Process.Pid)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(opencodeToolPath("cs-opencode-remote-output"), name, "-s")
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin")
			out, err := cmd.CombinedOutput()
			got := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
			exit := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("-s: %v: %s", err, out)
				}
				exit = exitErr.ExitCode()
			}
			if got != tc.wantOut || exit != tc.wantExit {
				t.Fatalf("-s = %q exit %d; want %q exit %d", got, exit, tc.wantOut, tc.wantExit)
			}
		})
	}
	covemit.Prove(t, "status-contract", "opencode", "", "scripts")
}

// installOpenCodeTurnStubs fakes the driver's dependencies: a tmux whose session is always
// alive, a curl that answers the health/status/session endpoints (the message payload comes
// from $STUB_DIR/message.json), an opencode for the PATH check, and a cs-opencode whose `run`
// prints canned output and exits $STUB_RUN_EXIT.
func installOpenCodeTurnStubs(t *testing.T, bin string) {
	t.Helper()
	stubs := map[string]string{
		"opencode":    "#!/bin/sh\nexit 0\n",
		"tmux":        "#!/bin/sh\nexit 0\n",
		"cs-opencode": "#!/bin/sh\necho \"stub-response\"\nexit \"${STUB_RUN_EXIT:-0}\"\n",
		// The first /message call is the driver's pre-submit snapshot and sees an
		// empty session; later calls (postcheck + output extraction) see the
		// canned conversation.
		"curl": `#!/bin/sh
for last; do :; done
case "$last" in
  */global/health) exit 0 ;;
  */session/status) echo '{}'; exit 0 ;;
  */session/*/message)
    if [ ! -f "$STUB_DIR/.msg_called" ]; then touch "$STUB_DIR/.msg_called"; echo '[]'; else cat "$STUB_DIR/message.json"; fi
    exit 0 ;;
  */session/*) printf '{"id":"%s"}\n' "` + opencodeTestSessionID + `"; exit 0 ;;
esac
exit 0
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func runOpenCodeTurn(t *testing.T, home, bin, stubDir, runExit string) (string, int) {
	t.Helper()
	cmd := exec.Command(opencodeToolPath("cs-opencode-turn"),
		"--tmux", "stubtoken", "--uuid", opencodeTestSessionID)
	cmd.Stdin = strings.NewReader("do the thing\n")
	cmd.Env = append(os.Environ(),
		"HOME="+home, "PATH="+bin+":/usr/bin:/bin",
		"STUB_DIR="+stubDir, "STUB_RUN_EXIT="+runExit)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("cs-opencode-turn: %v: %s", err, out)
	}
	return string(out), exitErr.ExitCode()
}

func TestOpenCodeTurnCompletionSemantics(t *testing.T) {
	skipUnlessLinux(t)
	okMessage := `[{"info":{"role":"user","time":{"created":1}}},` +
		`{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"stub-response"}]}]`
	providerError := `[{"info":{"role":"assistant","time":{"created":2},` +
		`"error":{"name":"APIError","data":{"message":"unauthorized","statusCode":401}}},"parts":[]}]`
	incomplete := `[{"info":{"role":"assistant","time":{"created":2}},"parts":[]}]`

	tests := []struct {
		name, message, runExit string
		wantExit               int
		wantInOut              string
	}{
		// The happy path emits the run output plus the session-id sentinel.
		{"success", okMessage, "0", 0, "__CS_OPENCODE_SESSION_ID__ " + opencodeTestSessionID},
		// run exit 0 + provider error recorded on the session = failed turn (the 1.18.10 trap:
		// attached clients do not propagate provider-side API errors).
		{"provider-error-despite-exit-0", providerError, "0", 4, "postcheck"},
		{"incomplete-despite-exit-0", incomplete, "0", 4, "postcheck"},
		// A nonzero run exit is a failed turn regardless of session state.
		{"run-exit-nonzero", okMessage, "1", 4, "turn failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := opencodeHome(t)
			installOpenCodeTurnStubs(t, bin)
			stubDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(stubDir, "message.json"), []byte(tc.message), 0o600); err != nil {
				t.Fatal(err)
			}
			out, exit := runOpenCodeTurn(t, home, bin, stubDir, tc.runExit)
			if exit != tc.wantExit {
				t.Fatalf("exit = %d; want %d: %s", exit, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantInOut) {
				t.Fatalf("output missing %q: %s", tc.wantInOut, out)
			}
			if tc.wantExit == 0 && !strings.Contains(out, "stub-response") {
				t.Fatalf("success output missing run stdout: %s", out)
			}
		})
	}
	covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
}
