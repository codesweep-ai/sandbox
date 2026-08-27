package cli

// Contract tests over the bundled agent tooling in image/rootfs/home — the scripts in
// .local/bin, and the profile config the OpenCode wrapper binds.
//
// Two things are pinned here. First, the `-s` status contract is identical across the
// claude, codex, and opencode remote families — an orchestrator polls all three the same
// way, so a drift in any one of them is a bug. Second, the opencode turn driver's
// completion semantics: opencode is the only agent whose turn can run to completion and
// still have failed (an attached `run` exits 0 while the provider error is recorded on the
// session), so the driver's mandatory postcheck has to stay.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/covemit"
)

// The scripts target the Linux guest/host environment (GNU coreutils, /proc). Off Linux
// they would fail for environmental reasons rather than contract violations, so CI's macOS
// leg skips them.
func skipUnlessLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("agent-tool scripts target Linux (GNU coreutils, /proc); skipping on %s", runtime.GOOS)
	}
}

const openCodeTestSessionID = "ses_04198268affeeKLgivDfdCnHrm"

func agentTool(name string) string {
	return filepath.Join("..", "..", "image", "rootfs", "home", ".local", "bin", name)
}

// agentHome builds a fake $HOME with the per-agent bookkeeping dirs and a stub bin dir
// (with an ssh that always succeeds, so nothing reaches the network).
func agentHome(t *testing.T, prefix string) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	bin = filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStub(t, bin, "ssh", "#!/bin/sh\nexit 0\n")
	for _, suffix := range []string{"-sessions", "-logs", "-pids", "-locks"} {
		if err := os.MkdirAll(filepath.Join(home, prefix+suffix), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home, bin
}

func writeStub(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// runScript runs a bundled script with HOME/PATH pointed at the fake home, returning its
// combined output and exit code.
func runScript(t *testing.T, home, bin string, name string, args ...string) (string, int) {
	t.Helper()
	return runScriptStdin(t, home, bin, nil, "", name, args...)
}

// runScriptStdin is runScript with a prompt piped in — the turn drivers read theirs there.
func runScriptStdin(t *testing.T, home, bin string, extraEnv []string, stdin, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(agentTool(name), args...)
	cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin"), extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
	return string(out), exitErr.ExitCode()
}

// TestRemoteOutputStatusContract: the `-s` probe of every remote family answers
// finished=0 / unknown=1 / running=2 / failed=3, decided by the authoritative footer with
// the PID only as a secondary liveness signal. An orchestrator polls all three families the
// same way, so the table is shared: an agent that drifts fails here.
func TestRemoteOutputStatusContract(t *testing.T) {
	skipUnlessLinux(t)
	prompt := "--- 2026-01-01 00:00:00 --- prompt: task\n"
	states := []struct {
		state, log string
		livePID    bool
		wantOut    string
		wantExit   int
	}{
		{"finished", prompt + "done\n--- 2026-01-01 00:01:00 --- finished (exit 0) ---\n", false, "finished", 0},
		// Ran to completion, but badly — distinct from both "finished" and "crashed", so a
		// driver can tell success from failure without parsing the log.
		{"failed", prompt + "--- 2026-01-01 00:01:00 --- finished (exit 5) ---\n", false, "failed", 3},
		// 130 is what --kill records for a cancelled turn.
		{"cancelled", prompt + "--- 2026-01-01 00:01:00 --- finished (exit 130) ---\n", false, "failed", 3},
		{"running", prompt, true, "running", 2},
		{"crashed", prompt, false, "unknown", 1},
	}
	for _, tool := range []struct{ name, prefix, agent string }{
		{"cs-claude-remote-output", ".cs-claude-remote", "claude"},
		{"cs-codex-remote-output", ".cs-codex-remote", "codex"},
		{"cs-opencode-remote-output", ".cs-opencode-remote", "opencode"},
	} {
		for _, tc := range states {
			t.Run(tool.name+"/"+tc.state, func(t *testing.T) {
				home, bin := agentHome(t, tool.prefix)
				name := "status-contract"
				log := filepath.Join(home, tool.prefix+"-logs", name+".log")
				if err := os.WriteFile(log, []byte(tc.log), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.livePID {
					proc := exec.Command("sleep", "30")
					if err := proc.Start(); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = proc.Process.Kill(); _, _ = proc.Process.Wait() }()
					pidFile := filepath.Join(home, tool.prefix+"-pids", name+".pid")
					if err := os.WriteFile(pidFile, []byte(strconv.Itoa(proc.Process.Pid)+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				out, exit := runScript(t, home, bin, tool.name, name, "-s")
				got := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
				if got != tc.wantOut || exit != tc.wantExit {
					t.Fatalf("-s = %q exit %d; want %q exit %d", got, exit, tc.wantOut, tc.wantExit)
				}
				covemit.Prove(t, "status-contract", tool.agent, "", "scripts")
			})
		}
	}
}

// TestOpenCodeProfileConfigFailsClosed: the shipped profile must pin a model AND disable
// upstream's auto-loaded OpenCode Zen gateway. Zen is the only provider that serves an
// anonymous caller, so it is the only thing an unusable pin can fall back to — and at
// 1.18.10 the interactive TUI does exactly that, silently selecting `opencode/big-pickle`
// when the pinned provider has no key. Sending the sandbox's code to a third party nobody
// chose is the one thing this project promises not to do, so both keys stay.
func TestOpenCodeProfileConfigFailsClosed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "image", "rootfs", "home", ".cs-opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Model            string   `json:"model"`
		DisabledProvider []string `json:"disabled_providers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("shipped opencode.json is not valid JSON: %v", err)
	}
	if cfg.Model == "" {
		t.Error("no model pinned: opencode does not otherwise resolve a deterministic model")
	}
	if !slices.Contains(cfg.DisabledProvider, "opencode") {
		t.Errorf("disabled_providers = %v, must contain \"opencode\" so an unusable pin "+
			"fails instead of falling back to the free anonymous gateway", cfg.DisabledProvider)
	}

	// The guard has to hold for BARE `opencode` too. /opt/opencode/bin is on PATH, so
	// running it instead of the cs-opencode wrapper is an easy slip — and it loads none
	// of the profile above, which used to leave the anonymous gateway reachable through
	// the front door. The XDG default config closes that without aliasing the binary.
	raw, err = os.ReadFile(filepath.Join("..", "..", "image", "rootfs", "home", ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("no XDG default config: bare `opencode` would reach the gateway: %v", err)
	}
	var xdg struct {
		DisabledProvider []string `json:"disabled_providers"`
	}
	if err := json.Unmarshal(raw, &xdg); err != nil {
		t.Fatalf("shipped ~/.config/opencode/opencode.json is not valid JSON: %v", err)
	}
	if !slices.Contains(xdg.DisabledProvider, "opencode") {
		t.Errorf("XDG disabled_providers = %v, must contain \"opencode\"", xdg.DisabledProvider)
	}
}

// TestOpenCodeWrapperProfile: the cs-opencode wrapper is what binds the isolated profile.
// A seeded credential must reach opencode as inline OPENCODE_AUTH_CONTENT rather than being
// left for opencode to read out of its data dir beside the session db.
func TestOpenCodeWrapperProfile(t *testing.T) {
	skipUnlessLinux(t)
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stub reports back the argv and the profile env it was launched with.
	writeStub(t, bin, "opencode", "#!/bin/sh\n"+
		"echo \"argv: $*\"\n"+
		"echo \"config: $OPENCODE_CONFIG_DIR\"\n"+
		"echo \"db: $OPENCODE_DB\"\n"+
		"echo \"auth: $OPENCODE_AUTH_CONTENT\"\n"+
		"echo \"key: $FIREWORKS_API_KEY\"\n")
	ocDir := filepath.Join(home, ".cs-opencode")
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "auth.json"), []byte(`{"tok":"seeded"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "env"), []byte("FIREWORKS_API_KEY=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, exit := runScript(t, home, bin, "cs-opencode")
	if exit != 0 {
		t.Fatalf("cs-opencode exit %d: %s", exit, out)
	}
	for _, want := range []string{
		"config: " + ocDir,
		"db: " + filepath.Join(ocDir, "opencode.db"),
		`auth: {"tok":"seeded"}`, // the credential is passed inline, not by path
		"key: from-env-file",     // the profile env file is sourced for provider keys
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wrapper output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--auto") {
		t.Errorf("non-yolo launch must not pass --auto:\n%s", out)
	}

	// YOLO adds --auto to the TUI and to `run`, but never to a subcommand that would
	// reject the flag.
	if err := os.WriteFile(filepath.Join(ocDir, ".yolo"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		args     []string
		wantArgv string
	}{
		{"tui", nil, "argv: --auto"},
		{"run", []string{"run", "do it"}, "argv: run --auto do it"},
		{"serve", []string{"serve", "--port", "1234"}, "argv: serve --port 1234"},
	} {
		t.Run("yolo/"+tc.name, func(t *testing.T) {
			out, exit := runScript(t, home, bin, "cs-opencode", tc.args...)
			if exit != 0 {
				t.Fatalf("cs-opencode %v exit %d: %s", tc.args, exit, out)
			}
			if !strings.Contains(out, tc.wantArgv) {
				t.Errorf("want %q:\n%s", tc.wantArgv, out)
			}
		})
	}
}

// installOpenCodeTurnStubs fakes the driver's dependencies: a tmux whose session is always
// alive, a curl answering the health/status/session endpoints (the message payload comes
// from $STUB_DIR/message.json), and a cs-opencode whose `run` exits $STUB_RUN_EXIT.
func installOpenCodeTurnStubs(t *testing.T, bin string) {
	t.Helper()
	writeStub(t, bin, "opencode", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "tmux", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "cs-opencode", "#!/bin/sh\necho \"stub-response\"\nexit \"${STUB_RUN_EXIT:-0}\"\n")
	// The first /message call is the driver's pre-submit snapshot (pre.json); the one
	// after the turn is the history it reads back (message.json). Serving them separately
	// is what lets a test model a history that CHANGED SHAPE during the turn.
	writeStub(t, bin, "curl", `#!/bin/sh
for last; do :; done
case "$last" in
  */global/health) exit 0 ;;
  */session/status) echo '{}'; exit 0 ;;
  */session/*/message)
    if [ ! -f "$STUB_DIR/.msg_called" ]; then touch "$STUB_DIR/.msg_called"; cat "$STUB_DIR/pre.json"; else cat "$STUB_DIR/message.json"; fi
    exit 0 ;;
  */session/*) printf '{"id":"%s"}\n' "`+openCodeTestSessionID+`"; exit 0 ;;
esac
exit 0
`)
}

// TestOpenCodeWrapperForwardsABaseURL: OPENCODE_BASE_URL points opencode's
// active provider at another endpoint.
//
// It cannot be OPENAI_BASE_URL. opencode's base URL belongs to the provider, and
// that variable already means its openai provider — so a model on any other
// provider ignores it, and repointing one with it would surprise whoever set it.
// The route that works for every provider is a baseURL in the config, supplied
// inline so nothing is written and an unset variable changes nothing.
//
// The provider comes from the pinned model's own slug, which is the one this
// session will use. Pinned here because getting it wrong is silent: the agent
// talks to the real provider, the proxy records nothing, and the run looks like
// a success.
func TestOpenCodeWrapperForwardsABaseURL(t *testing.T) {
	skipUnlessLinux(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the wrapper composes the provider block with jq")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStub(t, bin, "opencode", "#!/bin/sh\necho \"config: $OPENCODE_CONFIG_CONTENT\"\n")
	ocDir := filepath.Join(home, ".cs-opencode")
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ocDir, "opencode.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const url = "http://vcr:8080/c/demo/v1"

	t.Run("unset leaves the invocation alone", func(t *testing.T) {
		writeConfig(`{"model":"fireworks-ai/accounts/fireworks/models/kimi-k3"}`)
		out, exit := runScript(t, home, bin, "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		if !strings.Contains(out, "config: \n") && strings.TrimSpace(out) != "config:" {
			t.Errorf("no variable set, so no config content should be composed:\n%s", out)
		}
	})

	// The provider is whichever the pinned model names, not a fixed one: that is
	// the whole reason an environment variable cannot do this job.
	for _, tc := range []struct{ name, model, wantProvider string }{
		{"a fireworks model points the fireworks provider", "fireworks-ai/accounts/fireworks/models/kimi-k3", "fireworks-ai"},
		{"an openai model points the openai provider", "openai/gpt-5", "openai"},
		{"an anthropic model points the anthropic provider", "anthropic/claude-sonnet-5", "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(`{"model":"` + tc.model + `"}`)
			out, exit := runScriptStdin(t, home, bin, []string{"OPENCODE_BASE_URL=" + url}, "", "cs-opencode", "run", "x")
			if exit != 0 {
				t.Fatalf("cs-opencode exit %d: %s", exit, out)
			}
			want := `{"provider":{"` + tc.wantProvider + `":{"options":{"baseURL":"` + url + `"}}}}`
			if !strings.Contains(out, want) {
				t.Errorf("want %s:\n%s", want, out)
			}
			covemit.Prove(t, "auth-provisioning", "opencode", "", "scripts")
		})
	}

	// A config content the caller composed themselves is theirs.
	t.Run("a caller's own config content is kept", func(t *testing.T) {
		writeConfig(`{"model":"fireworks-ai/accounts/fireworks/models/kimi-k3"}`)
		out, exit := runScriptStdin(t, home, bin,
			[]string{"OPENCODE_BASE_URL=" + url, `OPENCODE_CONFIG_CONTENT={"mine":true}`}, "", "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		if !strings.Contains(out, `{"mine":true}`) {
			t.Errorf("the caller's own content must survive:\n%s", out)
		}
	})

	// No model means no provider to name, and saying so beats composing a block
	// keyed on an empty string.
	t.Run("no pinned model says so", func(t *testing.T) {
		writeConfig(`{}`)
		out, exit := runScriptStdin(t, home, bin, []string{"OPENCODE_BASE_URL=" + url}, "", "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		if !strings.Contains(out, "names no model") {
			t.Errorf("want a diagnostic naming the gap:\n%s", out)
		}
	})
}

// TestCodexWrapperForwardsABaseURL: OPENAI_BASE_URL points codex at an alternative
// OpenAI-compatible endpoint, the way it points every other client at one.
//
// codex reads no base-URL variable of its own — it takes a provider block — so the
// wrapper translates the variable into the `-c` overrides that declare one. Without
// this, codex is the only agent a caller cannot put behind a record/replay proxy, and
// the only one whose traffic cannot be captured and replayed for $0.
//
// The auth mode has to follow the endpoint, which is the half worth pinning: an API key
// travels in a header and the provider names the variable holding it, while a ChatGPT
// subscription travels as codex's own OAuth and needs requires_openai_auth instead.
// Getting that branch wrong fails at the first turn with an authentication error that
// says nothing about the endpoint.
func TestCodexWrapperForwardsABaseURL(t *testing.T) {
	skipUnlessLinux(t)
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStub(t, bin, "codex", "#!/bin/sh\necho \"argv: $*\"\n")

	for _, tc := range []struct {
		name        string
		env         []string
		wantArgv    string
		notWantArgv string
	}{
		{
			// The variable is the whole switch: unset, the wrapper is what it was.
			name:        "unset leaves the invocation alone",
			env:         nil,
			wantArgv:    "argv: exec do it",
			notWantArgv: "model_provider",
		},
		{
			name: "an API key names the variable holding it",
			env:  []string{"OPENAI_BASE_URL=http://vcr:8080/c/demo/v1", "OPENAI_API_KEY=sk-not-a-real-key"},
			wantArgv: `argv: -c model_provider="cs-proxy" -c model_providers.cs-proxy=` +
				`{name="cs-proxy", base_url="http://vcr:8080/c/demo/v1", env_key="OPENAI_API_KEY", wire_api="responses"} exec do it`,
		},
		{
			// No key means the subscription path, which authenticates as codex
			// itself rather than with a header. The empty assignment is not
			// decoration: the test inherits the developer's environment, and a
			// key there would otherwise decide this case for it.
			name: "a subscription asks for codex's own auth",
			env:  []string{"OPENAI_BASE_URL=http://vcr:8080/c/demo", "OPENAI_API_KEY="},
			wantArgv: `argv: -c model_provider="cs-proxy" -c model_providers.cs-proxy=` +
				`{name="cs-proxy", base_url="http://vcr:8080/c/demo", requires_openai_auth=true, wire_api="responses"} exec do it`,
			notWantArgv: "env_key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, exit := runScriptStdin(t, home, bin, tc.env, "", "cs-codex", "exec", "do it")
			if exit != 0 {
				t.Fatalf("cs-codex exit %d: %s", exit, out)
			}
			if !strings.Contains(out, tc.wantArgv) {
				t.Errorf("want %q:\n%s", tc.wantArgv, out)
			}
			if tc.notWantArgv != "" && strings.Contains(out, tc.notWantArgv) {
				t.Errorf("argv must not carry %q:\n%s", tc.notWantArgv, out)
			}
			covemit.Prove(t, "auth-provisioning", "codex", "", "scripts")
		})
	}
}

func TestOpenCodeTurnCompletionSemantics(t *testing.T) {
	skipUnlessLinux(t)
	okMessage := `[{"info":{"role":"user","time":{"created":1}}},` +
		`{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"stub-response"}]}]`
	providerError := `[{"info":{"role":"assistant","time":{"created":2},` +
		`"error":{"name":"APIError","data":{"message":"unauthorized","statusCode":401}}},"parts":[]}]`
	incomplete := `[{"info":{"role":"assistant","time":{"created":2}},"parts":[]}]`

	for _, tc := range []struct {
		name, message, runExit string
		wantExit               int
		wantInOut              string
	}{
		// The happy path emits the turn's text plus the session-id sentinel.
		{"success", okMessage, "0", 0, "__CS_OPENCODE_SESSION_ID__ " + openCodeTestSessionID},
		// run exit 0 + a provider error recorded on the session = failed turn. This is the
		// trap the postcheck exists for: attached clients do not propagate provider errors.
		{"provider-error-despite-exit-0", providerError, "0", 5, "postcheck"},
		{"incomplete-despite-exit-0", incomplete, "0", 5, "postcheck"},
		// A nonzero run exit is a failed turn regardless of session state.
		{"run-exit-nonzero", okMessage, "1", 5, "turn failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-opencode-remote")
			installOpenCodeTurnStubs(t, bin)
			stubDir := t.TempDir()
			writeSnapshots(t, stubDir, "[]", tc.message)
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "STUB_RUN_EXIT=" + tc.runExit},
				"do the thing\n",
				"cs-opencode-turn", "--tmux", "stubtoken", "--uuid", openCodeTestSessionID)
			if exit != tc.wantExit {
				t.Fatalf("exit = %d; want %d: %s", exit, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantInOut) {
				t.Fatalf("output missing %q: %s", tc.wantInOut, out)
			}
			// Turn output is read back from the session API, never from the attached
			// client's stdout (which stays empty when a TUI hosts the server).
			if tc.wantExit == 0 && !strings.Contains(out, "stub-response") {
				t.Fatalf("success output missing the session's assistant text: %s", out)
			}
			covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
		})
	}
}

// TestOpenCodeRemoteSessionsUsesSupportedSurface: `-v` reads last-activity from
// `opencode session list --format json`, never from opencode's internal SQLite schema. A raw
// `SELECT ... FROM session` works today but is not a surface upstream owes us — a table rename
// would blank the column silently. `session list` is scoped to its working directory, so the
// query has to run in the workdir recorded for the session.
func TestOpenCodeRemoteSessionsUsesSupportedSurface(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	name, sid := "listing", openCodeTestSessionID
	mapDir := filepath.Join(home, ".cs-opencode-remote-sessions")
	for file, content := range map[string]string{
		name:              sid + "\n",
		name + ".host":    "somehost\n",
		name + ".workdir": "/home/dev/api\n",
	} {
		if err := os.WriteFile(filepath.Join(mapDir, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Record what the tool asks the remote to run, and answer as opencode would.
	sshLog := filepath.Join(home, "ssh.log")
	writeStub(t, bin, "ssh", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> "+sshLog+"\n"+
		`printf '[{"id":"`+sid+`","title":"t","updated":1785605904757,"created":1,"projectId":"global","directory":"/home/dev/api"}]\n'`+"\n")

	out, exit := runScript(t, home, bin, "cs-opencode-remote-sessions", "-v")
	if exit != 0 {
		t.Fatalf("-v exit %d: %s", exit, out)
	}
	asked, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("the tool never queried the remote: %v", err)
	}
	if !strings.Contains(string(asked), "session list --format json") {
		t.Errorf("remote command should use the supported CLI surface, got: %s", asked)
	}
	if strings.Contains(string(asked), "SELECT") || strings.Contains(string(asked), "opencode db") {
		t.Errorf("remote command reaches into the internal SQLite schema: %s", asked)
	}
	if !strings.Contains(string(asked), "/home/dev/api") {
		t.Errorf("query must run in the session's recorded workdir (it is directory-scoped): %s", asked)
	}
	// 1785605904757 ms -> 2026-08-01 in whatever zone the test host runs in; assert the
	// date rendered rather than the "(not found on remote)" fallback.
	if strings.Contains(out, "not found on remote") || !strings.Contains(out, "2026-") {
		t.Errorf("last-activity not resolved from the listing:\n%s", out)
	}
}

// writeSnapshots lays down the history the stubbed API serves before the turn (pre) and
// after it (post).
func writeSnapshots(t *testing.T, stubDir, pre, post string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stubDir, "pre.json"), []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "message.json"), []byte(post), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCodeTurnAnchorsOnMessageIdentity: a turn is delimited by the ID of the last
// message that preceded it, never by a message COUNT. opencode's history is not
// append-only — compaction rewrites it and inserts `summary` messages — so a positional
// cursor returns the wrong slice as soon as the shape changes under it.
func TestOpenCodeTurnAnchorsOnMessageIdentity(t *testing.T) {
	skipUnlessLinux(t)
	msg := func(id, role, text string, created, completed int64, extra string) string {
		t := fmt.Sprintf(`"created":%d`, created)
		if completed > 0 {
			t += fmt.Sprintf(`,"completed":%d`, completed)
		}
		return fmt.Sprintf(`{"info":{"id":%q,"role":%q,%s"time":{%s}},"parts":[{"type":"text","text":%q}]}`,
			id, role, extra, t, text)
	}
	// Year 2100, so it always sorts after the turn's start timestamp.
	const future = int64(4102444800000)

	for _, tc := range []struct {
		name, pre, post string
		wantExit        int
		wantIn          string
		wantNotIn       []string
	}{
		{
			// Compaction replaced the head of the history while the turn ran. The list is
			// the same LENGTH as before, so a positional cursor yields nothing at all.
			name: "history rewritten mid-turn",
			pre: "[" + msg("msg_a", "user", "ask", 1, 0, "") + "," +
				msg("msg_b", "assistant", "OLD REPLY", 2, 3, "") + "," +
				msg("msg_c", "user", "ask again", 4, 0, "") + "]",
			post: "[" + msg("msg_sum", "assistant", "SUMMARY", 5, 5, `"summary":true,`) + "," +
				msg("msg_c", "user", "ask again", 4, 0, "") + "," +
				msg("msg_d", "assistant", "NEW REPLY", 6, 7, "") + "]",
			wantExit: 0, wantIn: "NEW REPLY", wantNotIn: []string{"OLD REPLY", "SUMMARY"},
		},
		{
			// The turn added nothing. Checking the session's last assistant message would
			// pass on the PREVIOUS turn's reply and report a silent empty success.
			name:     "turn produced nothing",
			pre:      "[" + msg("msg_a", "assistant", "PREVIOUS TURN", 1, 2, "") + "]",
			post:     "[" + msg("msg_a", "assistant", "PREVIOUS TURN", 1, 2, "") + "]",
			wantExit: 5, wantIn: "no-assistant-message-this-turn", wantNotIn: []string{"PREVIOUS TURN"},
		},
		{
			// Compaction removed the anchor itself: creation time still bounds the turn.
			name: "anchor compacted away",
			pre:  "[" + msg("msg_gone", "user", "ask", 1, 0, "") + "]",
			post: "[" + msg("msg_sum", "assistant", "SUMMARY", 1, 1, `"summary":true,`) + "," +
				msg("msg_new", "assistant", "FALLBACK REPLY", future, future+1, "") + "]",
			wantExit: 0, wantIn: "FALLBACK REPLY", wantNotIn: []string{"SUMMARY"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-opencode-remote")
			installOpenCodeTurnStubs(t, bin)
			stubDir := t.TempDir()
			writeSnapshots(t, stubDir, tc.pre, tc.post)
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "STUB_RUN_EXIT=0"}, "do the thing\n",
				"cs-opencode-turn", "--tmux", "stubtoken", "--uuid", openCodeTestSessionID)
			if exit != tc.wantExit {
				t.Fatalf("exit = %d; want %d: %s", exit, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantIn) {
				t.Fatalf("output missing %q: %s", tc.wantIn, out)
			}
			for _, no := range tc.wantNotIn {
				if strings.Contains(out, no) {
					t.Fatalf("output leaked %q from outside this turn: %s", no, out)
				}
			}
		})
	}
}

// TestOpenCodeTurnGivesUpOnAWedgedRun: an attached `run` that never returns must not
// pin the turn open. It happens for real — e.g. a run whose model cannot be resolved
// blocks with the session never going busy — so both the stall watchdog and the overall
// timeout have to bail out with exit 2 rather than hanging.
func TestOpenCodeTurnGivesUpOnAWedgedRun(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct{ name, env, wantErr string }{
		{"stall watchdog", "CS_OPENCODE_STALL_SECS=1", "turn stalled"},
		{"overall timeout", "CS_OPENCODE_TIMEOUT=1", "did not complete within"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-opencode-remote")
			installOpenCodeTurnStubs(t, bin)
			// A run that never returns, on a session the server reports as idle.
			writeStub(t, bin, "cs-opencode", "#!/bin/sh\nsleep 120\n")
			stubDir := t.TempDir()
			writeSnapshots(t, stubDir, "[]", "[]")
			env := []string{"STUB_DIR=" + stubDir, "CS_OPENCODE_TIMEOUT=60", "CS_OPENCODE_STALL_SECS=0", tc.env}
			out, exit := runScriptStdin(t, home, bin, env, "do the thing\n",
				"cs-opencode-turn", "--tmux", "stubtoken", "--uuid", openCodeTestSessionID)
			if exit != 2 {
				t.Fatalf("exit = %d; want 2: %s", exit, out)
			}
			if !strings.Contains(out, tc.wantErr) {
				t.Fatalf("output missing %q: %s", tc.wantErr, out)
			}
		})
	}
}

// remoteFamilies is the per-agent bookkeeping every remote tool shares. The mapping file a
// session is keyed by differs: claude names its tmux session by the agent's own uuid, the
// other two by a locally-generated token.
var remoteFamilies = []struct{ agent, prefix, mapSuffix, mapValue string }{
	{"claude", ".cs-claude-remote", "", "00000000-0000-4000-8000-000000000001"},
	{"codex", ".cs-codex-remote", ".token", "deadbeef"},
	{"opencode", ".cs-opencode-remote", ".token", "deadbeef"},
}

// killFixture lays down a session whose background turn is mid-flight: a mapping file, a log
// with a prompt header and no footer, and a runner script. Returns the runner file path.
func killFixture(t *testing.T, home, prefix, mapSuffix, mapValue, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, prefix+"-sessions", name+mapSuffix),
		[]byte(mapValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, prefix+"-logs", name+".log"),
		[]byte("--- 2026-01-01 00:00:00 --- prompt: wait\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, prefix+"-pids", name+".runner")
}

// TestRemoteKillStopsTheRunner: --kill has to stop this side's background runner, not just
// the remote tmux session — otherwise the runner and its PID file outlive the cancel and
// `-s` reports "running" forever. It also has to leave the authoritative footer behind, so a
// cancelled turn reads as failed rather than as a crash.
func TestRemoteKillStopsTheRunner(t *testing.T) {
	skipUnlessLinux(t)
	for _, fam := range remoteFamilies {
		t.Run(fam.agent, func(t *testing.T) {
			home, bin := agentHome(t, fam.prefix)
			name := "kill-contract"
			runnerFile := killFixture(t, home, fam.prefix, fam.mapSuffix, fam.mapValue, name)
			// The runner must look like OUR runner: cancellation checks the PID's cmdline
			// references this exact file before signalling.
			if err := os.WriteFile(runnerFile, []byte("#!/bin/bash\nsleep 30\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			runner := exec.Command("bash", runnerFile)
			if err := runner.Start(); err != nil {
				t.Fatal(err)
			}
			pidFile := filepath.Join(home, fam.prefix+"-pids", name+".pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(runner.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if out, exit := runScript(t, home, bin, "cs-"+fam.agent+"-remote", "--kill", name); exit != 0 {
				t.Fatalf("--kill exit %d: %s", exit, out)
			}
			done := make(chan error, 1)
			go func() { done <- runner.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = runner.Process.Kill()
				t.Fatal("background runner survived --kill")
			}
			if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
				t.Errorf("PID file not cleaned: %v", err)
			}
			// A cancelled turn must report failed/3, not unknown/1.
			out, exit := runScript(t, home, bin, "cs-"+fam.agent+"-remote-output", name, "-s")
			if got := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]); got != "failed" || exit != 3 {
				t.Errorf("after --kill, -s = %q exit %d; want failed exit 3", got, exit)
			}
			covemit.Prove(t, "interrupt", fam.agent, "", "scripts")
			covemit.Prove(t, "status-contract", fam.agent, "", "scripts")
		})
	}
}

// TestRemoteKillLeavesUnrelatedPIDAlone: a stale PID file whose number has been recycled by
// an unrelated process must never be signalled. Without the cmdline check, --kill would TERM
// a bystander and pkill -P its children.
func TestRemoteKillLeavesUnrelatedPIDAlone(t *testing.T) {
	skipUnlessLinux(t)
	for _, fam := range remoteFamilies {
		t.Run(fam.agent, func(t *testing.T) {
			home, bin := agentHome(t, fam.prefix)
			name := "stale-contract"
			killFixture(t, home, fam.prefix, fam.mapSuffix, fam.mapValue, name)
			// No runner file is written: this PID is a bystander that merely reuses the number.
			bystander := exec.Command("sleep", "30")
			if err := bystander.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = bystander.Process.Kill(); _, _ = bystander.Process.Wait() }()
			pidFile := filepath.Join(home, fam.prefix+"-pids", name+".pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(bystander.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if out, exit := runScript(t, home, bin, "cs-"+fam.agent+"-remote", "--kill", name); exit != 0 {
				t.Fatalf("--kill exit %d: %s", exit, out)
			}
			if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("unrelated process was signalled despite failing the runner check: %v", err)
			}
			if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
				t.Errorf("stale PID file not cleaned: %v", err)
			}
			covemit.Prove(t, "interrupt", fam.agent, "", "scripts")
		})
	}
}

const claudeTestUUID = "00000000-0000-4000-8000-000000000001"

// installClaudeTurnStubs fakes the driver's world: a tmux whose session is always alive and
// whose pane text comes from $STUB_DIR/pane.txt, and which — on the send-keys that submits
// the prompt — appends a turn_duration marker to the session JSONL, standing in for Claude
// completing the turn. $STUB_DIR/pane_after.txt, if present, becomes the pane from then on,
// which is how a screen that changes DURING the turn is modelled.
func installClaudeTurnStubs(t *testing.T, bin string) {
	t.Helper()
	writeStub(t, bin, "claude", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) exit 0 ;;
  capture-pane)
    if [ -f "$STUB_DIR/.submitted" ] && [ -f "$STUB_DIR/pane_after.txt" ]; then
      cat "$STUB_DIR/pane_after.txt"
    else
      cat "$STUB_DIR/pane.txt"
    fi
    exit 0 ;;
  send-keys)
    # The Enter that submits the prompt: Claude "answers" and ends the turn.
    if [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"STUB ANSWER"}]}}' >> "$STUB_DIR/session.jsonl"
      printf '%s\n' '{"type":"system","subtype":"turn_duration"}' >> "$STUB_DIR/session.jsonl"
    fi
    exit 0 ;;
esac
exit 0
`)
}

// TestClaudeTurnDetectsExpiredLogin: Claude can accept a prompt, append a normal-looking
// assistant message such as "Login expired · Please run /login", AND still emit the turn
// completion marker. Reporting that as a successful turn hands back an answer the model
// never produced, so the driver re-checks the screen after completion and fails instead.
func TestClaudeTurnDetectsExpiredLogin(t *testing.T) {
	skipUnlessLinux(t)
	const ready = "some output\n  auto mode on\n"
	for _, tc := range []struct {
		name, paneAfter string
		wantExit        int
		wantIn          string
	}{
		{"healthy turn", "", 0, "STUB ANSWER"},
		{"login expired mid-turn", "Login expired · Please run /login\n", 3, "login expired during the turn"},
		{"oauth screen mid-turn", "Paste code here\n", 3, "login expired during the turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude-remote")
			installClaudeTurnStubs(t, bin)
			stubDir := t.TempDir()
			projects := filepath.Join(home, "projects")
			if err := os.MkdirAll(projects, 0o700); err != nil {
				t.Fatal(err)
			}
			// The driver finds the session by <uuid>.jsonl; the stub appends to the same file.
			jsonl := filepath.Join(projects, claudeTestUUID+".jsonl")
			if err := os.WriteFile(jsonl, []byte(""), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(jsonl, filepath.Join(stubDir, "session.jsonl")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "pane.txt"), []byte(ready), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.paneAfter != "" {
				if err := os.WriteFile(filepath.Join(stubDir, "pane_after.txt"), []byte(tc.paneAfter), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "CS_CLAUDE_STALL_SECS=0"}, "do the thing\n",
				"cs-claude-turn", "--uuid", claudeTestUUID, "--projects", projects, "--timeout", "30")
			if exit != tc.wantExit {
				t.Fatalf("exit = %d; want %d: %s", exit, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantIn) {
				t.Fatalf("output missing %q: %s", tc.wantIn, out)
			}
		})
	}
}

// TestClaudeTurnReadyStates: the ready check has to recognise the status line current Claude
// Code actually prints. A --yolo sandbox runs with permissions bypassed and shows "bypass
// permissions on", not "auto mode on"; missing it strands every turn at the ready timeout.
func TestClaudeTurnReadyStates(t *testing.T) {
	skipUnlessLinux(t)
	for _, pane := range []string{"  auto mode on\n", "  bypass permissions on\n"} {
		t.Run(strings.TrimSpace(pane), func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude-remote")
			installClaudeTurnStubs(t, bin)
			stubDir := t.TempDir()
			projects := filepath.Join(home, "projects")
			if err := os.MkdirAll(projects, 0o700); err != nil {
				t.Fatal(err)
			}
			jsonl := filepath.Join(projects, claudeTestUUID+".jsonl")
			if err := os.WriteFile(jsonl, []byte(""), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(jsonl, filepath.Join(stubDir, "session.jsonl")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "pane.txt"), []byte(pane), 0o600); err != nil {
				t.Fatal(err)
			}
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "CS_CLAUDE_STALL_SECS=0"}, "do the thing\n",
				"cs-claude-turn", "--uuid", claudeTestUUID, "--projects", projects, "--timeout", "30")
			if exit != 0 || !strings.Contains(out, "STUB ANSWER") {
				t.Fatalf("pane %q not treated as ready: exit %d: %s", pane, exit, out)
			}
		})
	}
}

// TestCodexTurnReadyStates: Codex writes its status line as "<model> <effort> · <dir>", and
// the effort segment reads "default" only while none is configured. Keying readiness on that
// literal left every member carrying an explicit model_reasoning_effort un-ready until the
// startup budget ran out, whatever provider or model slug it ran. Readiness follows the
// line's shape, so an unfamiliar effort name and a foreign model slug both stay ready.
func TestCodexTurnReadyStates(t *testing.T) {
	skipUnlessLinux(t)
	for _, pane := range []string{
		"gpt-5 default · /work",     // no effort configured
		"gpt-5.6-sol xhigh · /work", // a tier added after this driver was written
		"kimi-k3 turbo · /work",     // foreign slug, effort this client does not know
	} {
		t.Run(pane, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-codex-remote")
			sess := filepath.Join(home, ".cs-codex", "sessions")
			if err := os.MkdirAll(sess, 0o700); err != nil {
				t.Fatal(err)
			}
			fresh := filepath.Join(sess, "rollout-2026-01-02-22222222-2222-4222-8222-222222222222.jsonl")
			stubDir := t.TempDir()
			writeStub(t, bin, "codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "cs-codex", "#!/bin/sh\nexit 0\n")
			// capture-pane pads to the pane height, so the status line is the last row with
			// content and not the last row emitted.
			writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) [ -f "$STUB_DIR/.launched" ] && exit 0 || exit 1 ;;
  new-session)
    touch "$STUB_DIR/.launched"
    : > "$FRESH_ROLLOUT"
    exit 0 ;;
  capture-pane) printf '  %s\n  %s\n\n\n' "$PROMPT" "$PANE"; exit 0 ;;
  send-keys)
    if [ -f "$STUB_DIR/.launched" ] && [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' '{"payload":{"type":"agent_message","message":"READY ANSWER"}}' >> "$FRESH_ROLLOUT"
      printf '%s\n' '{"payload":{"type":"task_complete"}}' >> "$FRESH_ROLLOUT"
    fi
    exit 0 ;;
esac
exit 0
`)
			out, exit := runScriptStdin(t, home, bin,
				[]string{
					"STUB_DIR=" + stubDir,
					"FRESH_ROLLOUT=" + fresh,
					"PANE=" + pane,
					"PROMPT=› Improve documentation in @filename",
					"CS_CODEX_STALL_SECS=0",
				},
				"do the thing\n", "cs-codex-turn", "--tmux", "codextoken", "--timeout", "30")
			if exit != 0 || !strings.Contains(out, "READY ANSWER") {
				t.Fatalf("pane %q not treated as ready: exit %d: %s", pane, exit, out)
			}
		})
	}
}

// TestCodexTurnBindsToItsOwnRollout: current Codex creates its rollout at TUI startup, and
// the ready screen can appear a moment before the file exists. Picking the globally newest
// *.jsonl after startup can therefore bind the turn to a PREVIOUS session — the turn then
// reports another conversation's content as its own. The driver snapshots before launching
// and takes the file that appeared since.
func TestCodexTurnBindsToItsOwnRollout(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-codex-remote")
	sess := filepath.Join(home, ".cs-codex", "sessions")
	if err := os.MkdirAll(sess, 0o700); err != nil {
		t.Fatal(err)
	}
	// A stale rollout from an earlier session, deliberately left as the newest file on disk
	// so "newest wins" would pick exactly the wrong one.
	stale := filepath.Join(sess, "rollout-2026-01-01-11111111-1111-4111-8111-111111111111.jsonl")
	staleBody := `{"payload":{"type":"agent_message","message":"STALE SESSION"}}` + "\n" +
		`{"payload":{"type":"task_complete"}}` + "\n"
	if err := os.WriteFile(stale, []byte(staleBody), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(sess, "rollout-2026-01-02-22222222-2222-4222-8222-222222222222.jsonl")
	stubDir := t.TempDir()

	// tmux: no session until new-session runs (so the driver takes the launch path), which
	// creates the fresh rollout the way Codex does at startup. The submitting send-keys then
	// appends this turn's answer and its task_complete.
	writeStub(t, bin, "codex", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "cs-codex", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) [ -f "$STUB_DIR/.launched" ] && exit 0 || exit 1 ;;
  new-session)
    touch "$STUB_DIR/.launched"
    : > "$FRESH_ROLLOUT"
    exit 0 ;;
  capture-pane) echo "gpt-5 default · /work"; exit 0 ;;
  send-keys)
    if [ -f "$STUB_DIR/.launched" ] && [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' '{"payload":{"type":"agent_message","message":"FRESH ANSWER"}}' >> "$FRESH_ROLLOUT"
      printf '%s\n' '{"payload":{"type":"task_complete"}}' >> "$FRESH_ROLLOUT"
    fi
    exit 0 ;;
esac
exit 0
`)
	// Make the stale file the newest on disk, so mtime ordering favours the wrong choice.
	if err := os.Chtimes(stale, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	out, exit := runScriptStdin(t, home, bin,
		[]string{"STUB_DIR=" + stubDir, "FRESH_ROLLOUT=" + fresh, "CS_CODEX_STALL_SECS=0"},
		"do the thing\n", "cs-codex-turn", "--tmux", "codextoken", "--timeout", "30")
	if exit != 0 {
		t.Fatalf("exit = %d: %s", exit, out)
	}
	if !strings.Contains(out, "FRESH ANSWER") {
		t.Errorf("turn did not bind to the rollout its own launch created: %s", out)
	}
	if strings.Contains(out, "STALE SESSION") {
		t.Errorf("turn reported a previous session's content as its own: %s", out)
	}
}

// TestTurnDriversRejectANonNumericTimeout: 0 legitimately means "wait
// indefinitely", and bash arithmetic reads a non-numeric value as 0 — so
// without an explicit check, `--timeout abc` would not fail, it would silently
// drop the budget and wait forever. That is the exact failure these drivers'
// watchdogs exist to prevent, so a bad value has to be rejected up front, and
// a legitimate 0 has to survive.
func TestTurnDriversRejectANonNumericTimeout(t *testing.T) {
	skipUnlessLinux(t)
	const badTimeout = "must be a non-negative integer"
	for _, tc := range []struct {
		tool string
		args []string
	}{
		{"cs-claude-turn", []string{"--uuid", claudeTestUUID}},
		{"cs-codex-turn", []string{"--tmux", "stubtoken"}},
		{"cs-opencode-turn", []string{"--tmux", "stubtoken"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude-remote")
			args := append(append([]string{}, tc.args...), "--timeout", "abc")
			out, exit := runScriptStdin(t, home, bin, nil, "prompt", tc.tool, args...)
			if exit == 0 || !strings.Contains(out, badTimeout) {
				t.Errorf("a non-numeric timeout must be rejected up front; exit %d: %s", exit, out)
			}

			// 0 is the documented "no limit" value and must get past validation.
			// It will fail later for want of a real agent, which is fine — the
			// point is that it is not turned away as a malformed timeout.
			args = append(append([]string{}, tc.args...), "--timeout", "0")
			out, _ = runScriptStdin(t, home, bin, nil, "prompt", tc.tool, args...)
			if strings.Contains(out, badTimeout) {
				t.Errorf("--timeout 0 means no limit and must be accepted: %s", out)
			}
		})
	}
}

// TestRemoteBackgroundCarriesTurnTimeout: `-b` re-execs the tool through a runner
// script that REBUILDS argv, so any flag not explicitly re-added there is dropped.
// --turn-timeout was, which meant a caller-supplied turn budget had never once
// reached the turn driver on a background dispatch — and every campaign dispatch is
// a background dispatch. The turn then ran under the driver default (claude 600s),
// whose expiry stops only the watcher while the guest keeps working, yet still exits
// 2 so the status contract reports a healthy turn as failed. Verified against a live
// member VM on 2026-08-06: the driver command line carried no --timeout at all.
func TestRemoteBackgroundCarriesTurnTimeout(t *testing.T) {
	skipUnlessLinux(t)
	for _, fam := range remoteFamilies {
		t.Run(fam.agent, func(t *testing.T) {
			home, bin := agentHome(t, fam.prefix)
			name := "timeout-contract"
			// Record what the re-exec asks ssh to run: that string is what actually
			// reaches the guest, so asserting on it tests the whole background path
			// rather than the shape of the generated script.
			sshLog := filepath.Join(home, "ssh.log")
			writeStub(t, bin, "ssh", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+sshLog+"\nexit 0\n")
			writeStub(t, bin, "scp", "#!/bin/sh\nexit 0\n")
			if err := os.WriteFile(filepath.Join(home, fam.prefix+"-sessions", name+fam.mapSuffix),
				[]byte(fam.mapValue+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, exit := runScript(t, home, bin, "cs-"+fam.agent+"-remote",
				"--resume", name, "-H", "host", "--turn-timeout", "0", "-b", "hello"); exit != 0 {
				t.Fatalf("background dispatch exit %d: %s", exit, out)
			}
			// The runner is launched detached; give it a moment to reach ssh.
			//
			// Wait on --format rather than the driver's name. deploy_driver
			// probes first with `ssh host test -x $HOME/.local/bin/cs-<a>-turn`,
			// and that line ENDS in "-turn" — so waiting for "-turn" matched the
			// PROBE, broke the loop before the dispatch was logged at all, and
			// failed the assertion below against a log holding only probes. It
			// passed wherever the dispatch happened to land inside the same 100ms
			// tick, which is why it only showed on the slowest host.
			//
			// --format is the one flag every family sets unconditionally, and no
			// ssh call before the dispatch carries it. --uuid will not do: codex
			// and opencode add it only when a UUID is already known, so it would
			// skip those two into vacuous passes. It is also independent of the
			// timeout, so a real regression still reaches the assertion below
			// rather than quietly skipping.
			var logged string
			for range 60 {
				if b, err := os.ReadFile(sshLog); err == nil && strings.Contains(string(b), "--format") {
					logged = string(b)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if logged == "" {
				t.Skip("background runner did not reach ssh in this environment")
			}
			if !strings.Contains(logged, "--timeout 0") {
				t.Fatalf("background dispatch dropped the turn timeout; ssh was asked to run:\n%s", logged)
			}
		})
	}
}

// TestClaudeWrapperAnswersTheFirstRunDialogs pins the three dialogs a fresh
// profile would otherwise stop on. Each blocks Claude Code's TUI until someone
// picks an option, so on an unattended member each one hangs the turn until its
// deadline — the failure this test exists to keep from coming back.
//
// The API-key case is the one that is easy to get wrong: an inherited profile
// can carry a `rejected` entry for the very key the member was handed, so
// approving is not enough on its own.
//
// Onboarding is the opposite trap. Answering it on a sandbox with nothing to
// sign in with hides the screen offering the sign-in choices, and claude comes
// up as "API Usage Billing", so it is answered only when a login exists.
func TestClaudeWrapperAnswersTheFirstRunDialogs(t *testing.T) {
	skipUnlessLinux(t)

	const key = "sk-ant-0000000000000000000000000000-TAIL20CHARSHEREOK"
	id := key[len(key)-20:]

	for _, tc := range []struct {
		name          string
		env           []string
		seed          string
		theme         string // ~/.cs-claude/theme, as create carried it from the host
		wantTheme     string
		wantOnboarded bool
		wantApp       []string
		wantReject    []string
	}{
		{
			// The virgin profile a member boots with: no .claude.json at all.
			name:          "a fresh profile gets every answer",
			env:           []string{"ANTHROPIC_API_KEY=" + key},
			wantTheme:     "dark",
			wantOnboarded: true,
			wantApp:       []string{id},
		},
		{
			// A theme the operator picked is theirs, not ours to overwrite.
			name:          "an existing theme survives",
			env:           []string{"ANTHROPIC_API_KEY=" + key},
			seed:          `{"theme":"light"}`,
			wantTheme:     "light",
			wantOnboarded: true,
			wantApp:       []string{id},
		},
		{
			// Inheriting a host profile that declined this key must not carry
			// the refusal into the member.
			name:          "a rejection inherited for this key is cleared",
			env:           []string{"ANTHROPIC_API_KEY=" + key},
			seed:          fmt.Sprintf(`{"customApiKeyResponses":{"approved":[],"rejected":[%q,"another-key-tail-xx"]}}`, id),
			wantTheme:     "dark",
			wantOnboarded: true,
			wantApp:       []string{id},
			// Somebody else's declined key is none of our business.
			wantReject: []string{"another-key-tail-xx"},
		},
		{
			// No key means no dialog to answer, and nothing to record. Onboarding
			// stays unanswered too: a sandbox with nothing to sign in with must
			// keep the screen that offers the sign-in choices, or claude comes up
			// as "API Usage Billing" with no way to log in.
			name:      "no key leaves a login-free profile unonboarded",
			env:       []string{"ANTHROPIC_API_KEY="},
			wantTheme: "dark",
		},
		{
			// The theme create carried off the host, so the sandbox looks like
			// the claude the operator already runs.
			name:          "the host theme carried at create is used",
			env:           []string{"ANTHROPIC_API_KEY=" + key},
			theme:         "light-daltonized",
			wantTheme:     "light-daltonized",
			wantOnboarded: true,
			wantApp:       []string{id},
		},
		{
			// A gateway token is something to sign in with, even with no
			// credentials file beside it, or the profile opens on the sign-in
			// screen while its model calls work perfectly.
			name:          "a gateway token counts as a login",
			env:           []string{"ANTHROPIC_AUTH_TOKEN=some-gateway-token"},
			wantTheme:     "dark",
			wantOnboarded: true,
		},
		{
			// A value claude does not know would reopen the picker, so it is
			// dropped rather than written through.
			name:          "a theme claude does not know falls back",
			env:           []string{"ANTHROPIC_API_KEY=" + key},
			theme:         "neon",
			wantTheme:     "dark",
			wantOnboarded: true,
			wantApp:       []string{id},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude")
			writeStub(t, bin, "claude", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "jq", "#!/bin/sh\nexec /usr/bin/jq \"$@\"\n")
			ccDir := filepath.Join(home, ".cs-claude")
			if err := os.MkdirAll(ccDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.theme != "" {
				if err := os.WriteFile(filepath.Join(ccDir, "theme"), []byte(tc.theme+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cfgPath := filepath.Join(ccDir, ".claude.json")
			if tc.seed != "" {
				if err := os.WriteFile(cfgPath, []byte(tc.seed), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if out, exit := runScriptStdin(t, home, bin, tc.env, "", "cs-claude"); exit != 0 {
				t.Fatalf("cs-claude exit %d: %s", exit, out)
			}

			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatalf("the wrapper must leave a config behind: %v", err)
			}
			var cfg struct {
				Theme                  string `json:"theme"`
				HasCompletedOnboarding bool   `json:"hasCompletedOnboarding"`
				CustomAPIKeyResponses  struct {
					Approved []string `json:"approved"`
					Rejected []string `json:"rejected"`
				} `json:"customApiKeyResponses"`
				Projects map[string]struct {
					HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
				} `json:"projects"`
			}
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("config is not JSON: %v: %s", err, raw)
			}

			if cfg.Theme != tc.wantTheme {
				t.Errorf("theme = %q, want %q", cfg.Theme, tc.wantTheme)
			}
			if cfg.HasCompletedOnboarding != tc.wantOnboarded {
				t.Errorf("hasCompletedOnboarding = %v, want %v", cfg.HasCompletedOnboarding, tc.wantOnboarded)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Projects[cwd].HasTrustDialogAccepted {
				t.Errorf("the launch dir %s must be trusted; got %+v", cwd, cfg.Projects)
			}
			if got := cfg.CustomAPIKeyResponses.Approved; !slices.Equal(got, tc.wantApp) {
				t.Errorf("approved = %v, want %v", got, tc.wantApp)
			}
			if got := cfg.CustomAPIKeyResponses.Rejected; !slices.Equal(got, tc.wantReject) {
				t.Errorf("rejected = %v, want %v", got, tc.wantReject)
			}
			covemit.Prove(t, "auth-provisioning", "claude", "", "scripts")
		})
	}
}

// TestClaudeWrapperRefusesForeignMCPServers pins that a sandbox loads no MCP
// server it did not ask for.
//
// An inherited Claude subscription carries the account's claude.ai connectors,
// so without this an agent working unattended in a disposable machine is handed
// the creator's Gmail, Calendar and Drive. It also keeps a session
// reproducible: the connectors attach on their own schedule, so the tool list
// differs between two runs of the same task, and a recorded session matches on
// the tool list.
func TestClaudeWrapperRefusesForeignMCPServers(t *testing.T) {
	skipUnlessLinux(t)

	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "the ordinary branch",
			env:  []string{"CS_CLAUDE_YOLO="},
			want: "--permission-mode auto --strict-mcp-config",
		},
		{
			// The flag has to survive the other branch too, which takes a
			// different exec line.
			name: "the yolo branch",
			env:  []string{"CS_CLAUDE_YOLO=1"},
			want: "--dangerously-skip-permissions --strict-mcp-config",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude")
			writeStub(t, bin, "claude", "#!/bin/sh\necho \"argv: $*\"\n")
			out, exit := runScriptStdin(t, home, bin, tc.env, "", "cs-claude")
			if exit != 0 {
				t.Fatalf("cs-claude exit %d: %s", exit, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("want %q:\n%s", tc.want, out)
			}
			covemit.Prove(t, "auth-provisioning", "claude", "", "scripts")
		})
	}
}

// TestOpenCodeTurnStartsOneServerForANewSession: a new session is minted on the warm TUI's
// own API, and no second server is ever started.
//
// The driver used to boot a transient `opencode serve` on the same derived port purely to
// mint a session id, kill it, and wait for the port before launching the TUI. A turn that
// died inside that window left the transient server running, and nothing cleared it: a
// restart kills the tmux session, and that process is not in one. It then answered every
// health check while the TUI it shadowed rendered nothing, so the next turn attached to it
// and ran in the directory it had been started in.
//
// The contract is therefore stated as an absence — no `serve` — plus the two calls that
// replace it: the session is minted with the caller's directory, and the TUI is navigated
// to it so a human attaching sees the driven turn.
func TestOpenCodeTurnStartsOneServerForANewSession(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	stubDir := t.TempDir()
	installOpenCodeTurnStubs(t, bin)
	// Record every cs-opencode invocation, then behave as the shared stub does.
	writeStub(t, bin, "cs-opencode", "#!/bin/sh\necho \"cs-opencode $*\" >> \"$STUB_DIR/invocations\"\n"+
		"echo \"stub-response\"\nexit \"${STUB_RUN_EXIT:-0}\"\n")
	// Stateful, so the port reads as free before the TUI is launched and as served after:
	// a health probe that always succeeded would look like a squatter to reclaim_port.
	writeStub(t, bin, "tmux", `#!/bin/sh
echo "tmux $*" >> "$STUB_DIR/invocations"
case "$1" in
  has-session) [ -f "$STUB_DIR/.tui_up" ] || exit 1 ;;
  new-session) touch "$STUB_DIR/.tui_up" ;;
esac
exit 0
`)
	// A curl that logs the request line it was given, so the mint and the navigation are
	// observable. The final argument is the URL for every call the driver makes.
	writeStub(t, bin, "curl", `#!/bin/sh
for last; do :; done
echo "curl $*" >> "$STUB_DIR/invocations"
case "$last" in
  */global/health) [ -f "$STUB_DIR/.tui_up" ] || exit 7; exit 0 ;;
  */session/status) echo '{}'; exit 0 ;;
  */session/*/message)
    if [ ! -f "$STUB_DIR/.msg_called" ]; then touch "$STUB_DIR/.msg_called"; cat "$STUB_DIR/pre.json"; else cat "$STUB_DIR/message.json"; fi
    exit 0 ;;
esac
printf '{"id":"%s"}\n' "`+openCodeTestSessionID+`"
exit 0
`)
	writeSnapshots(t, stubDir, "[]",
		`[{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"stub-response"}]}]`)

	// No --uuid: this is the new-session path, the only one that used to mint. The workdir has
	// to exist, because a session is bound to it.
	workdir := t.TempDir()
	out, exit := runScriptStdin(t, home, bin, []string{"STUB_DIR=" + stubDir}, "do the thing\n",
		"cs-opencode-turn", "--tmux", "stubtoken", "--workdir", workdir)
	if exit != 0 {
		t.Fatalf("exit = %d; want 0: %s", exit, out)
	}
	logged, err := os.ReadFile(filepath.Join(stubDir, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	calls := string(logged)

	// The absence that is the fix: no second server, on any port.
	for line := range strings.SplitSeq(calls, "\n") {
		if strings.HasPrefix(line, "cs-opencode ") && strings.Contains(line, " serve") {
			t.Errorf("the driver started a second server, which is what strands one:\n  %s", line)
		}
	}
	if got := strings.Count(calls, "tmux new-session"); got != 1 {
		t.Errorf("tmux new-session called %d times; the TUI is the only server, so exactly one", got)
	}
	// Minted on the TUI's own API, bound to the directory the caller named rather than to
	// whatever the TUI's cwd happened to be.
	if !strings.Contains(calls, "/session?directory="+workdir) {
		t.Errorf("the session was not minted for the caller's workdir:\n%s", calls)
	}
	// Navigated, so the warm TUI shows the driven session instead of the home screen.
	if !strings.Contains(calls, "/tui/select-session") {
		t.Errorf("the TUI was never navigated to the new session:\n%s", calls)
	}
	covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
}

// TestOpenCodeTurnReclaimsAStrandedServer: a server holding the derived port with no tmux
// session behind it is taken back, rather than driven.
//
// The port is a hash of the session token, so anything listening on it belongs to this
// session. One that no tmux session stands behind is a turn that died before it could clean
// up. It answers every health check, so without this the driver attaches to it and the turn
// runs against a server nobody can see; and nothing else clears it, because a restart kills
// a tmux session and this process is not in one.
//
// The squatter is a real process, because `kill` is a shell builtin and a stub on PATH would
// never run. The test asserts the driver signalled the pid that ss named, by watching that
// process exit.
func TestOpenCodeTurnReclaimsAStrandedServer(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	stubDir := t.TempDir()
	installOpenCodeTurnStubs(t, bin)
	writeStub(t, bin, "cs-opencode", "#!/bin/sh\necho \"stub-response\"\nexit 0\n")

	squatter := exec.Command("sleep", "120")
	if err := squatter.Start(); err != nil {
		t.Fatal(err)
	}
	// Reaped as it dies, in a goroutine. A killed child this process has not waited for is a
	// zombie, and `kill -0` on a zombie still succeeds — so without this the stubs below would
	// go on reporting a squatter the driver had already killed.
	died := make(chan struct{})
	go func() { _ = squatter.Wait(); close(died) }()
	t.Cleanup(func() { _ = squatter.Process.Kill() })
	pid := squatter.Process.Pid

	writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) [ -f "$STUB_DIR/.tui_up" ] || exit 1 ;;
  new-session) touch "$STUB_DIR/.tui_up" ;;
  list-sessions) exit 0 ;;
esac
exit 0
`)
	// ss names the squatter while it lives, and reports nothing once it is gone.
	writeStub(t, bin, "ss", fmt.Sprintf(`#!/bin/sh
kill -0 %d 2>/dev/null || exit 0
echo 'LISTEN 0 512 127.0.0.1:21453 0.0.0.0:* users:(("opencode",pid=%d,fd=22))'
`, pid, pid))
	// The port answers while the squatter lives, and again once the TUI is up, which is what
	// a real reclaim looks like from the driver's side.
	writeStub(t, bin, "curl", fmt.Sprintf(`#!/bin/sh
for last; do :; done
case "$last" in
  */global/health)
    [ -f "$STUB_DIR/.tui_up" ] && exit 0
    kill -0 %d 2>/dev/null && exit 0
    exit 7 ;;
  */session/status) echo '{}'; exit 0 ;;
  */session/*/message)
    if [ ! -f "$STUB_DIR/.msg_called" ]; then touch "$STUB_DIR/.msg_called"; cat "$STUB_DIR/pre.json"; else cat "$STUB_DIR/message.json"; fi
    exit 0 ;;
esac
printf '{"id":"%%s"}\n' "%s"
exit 0
`, pid, openCodeTestSessionID))
	writeSnapshots(t, stubDir, "[]",
		`[{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"stub-response"}]}]`)

	out, exit := runScriptStdin(t, home, bin, []string{"STUB_DIR=" + stubDir}, "do the thing\n",
		"cs-opencode-turn", "--tmux", "stubtoken", "--workdir", t.TempDir())
	if exit != 0 {
		t.Fatalf("a stranded server must be reclaimed, not fatal; exit = %d: %s", exit, out)
	}
	if !strings.Contains(out, "reclaiming port") {
		t.Errorf("the reclaim is never silent, so the log says which port and which pid:\n%s", out)
	}
	select {
	case <-died:
	case <-time.After(10 * time.Second):
		t.Errorf("the squatter on the derived port is still running, so it was never reclaimed")
	}
	covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
}

// TestOpenCodeTurnFallsBackFromAMissingWorkdir: a `--workdir` that does not exist runs in
// $HOME, and says so.
//
// opencode binds a session to the directory it is given, so a path that is not there fails
// every prompt on that session with `FileSystem.realPath (…) ENOENT` rather than at launch.
// The caller is not always in a position to know: cs-campaign asks every turn to run in
// `/workspace`, which a Firecracker member does not have.
func TestOpenCodeTurnFallsBackFromAMissingWorkdir(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	stubDir := t.TempDir()
	installOpenCodeTurnStubs(t, bin)
	writeStub(t, bin, "cs-opencode", "#!/bin/sh\necho \"stub-response\"\nexit 0\n")
	writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) [ -f "$STUB_DIR/.tui_up" ] || exit 1 ;;
  new-session) touch "$STUB_DIR/.tui_up" ;;
  list-sessions) exit 0 ;;
esac
exit 0
`)
	writeStub(t, bin, "curl", `#!/bin/sh
for last; do :; done
echo "curl $*" >> "$STUB_DIR/invocations"
case "$last" in
  */global/health) [ -f "$STUB_DIR/.tui_up" ] || exit 7; exit 0 ;;
  */session/status) echo '{}'; exit 0 ;;
  */session/*/message)
    if [ ! -f "$STUB_DIR/.msg_called" ]; then touch "$STUB_DIR/.msg_called"; cat "$STUB_DIR/pre.json"; else cat "$STUB_DIR/message.json"; fi
    exit 0 ;;
esac
printf '{"id":"%s"}\n' "`+openCodeTestSessionID+`"
exit 0
`)
	writeSnapshots(t, stubDir, "[]",
		`[{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"stub-response"}]}]`)

	out, exit := runScriptStdin(t, home, bin, []string{"STUB_DIR=" + stubDir}, "do the thing\n",
		"cs-opencode-turn", "--tmux", "stubtoken", "--workdir", "/workspace")
	if exit != 0 {
		t.Fatalf("a missing workdir is a fallback, not a failure; exit = %d: %s", exit, out)
	}
	if !strings.Contains(out, "does not exist") {
		t.Errorf("the fallback is never silent, because the turn then runs somewhere else:\n%s", out)
	}
	logged, err := os.ReadFile(filepath.Join(stubDir, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	// The session must not be bound to the directory that is not there: opencode accepts it
	// and then fails every prompt on that session.
	if strings.Contains(string(logged), "directory=/workspace") {
		t.Errorf("the session was bound to a directory that does not exist:\n%s", logged)
	}
	covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
}

// TestRemoteLearnsTheSessionIdBeforeTheNextTurn: a turn dispatched while the previous one is
// still running resumes that session rather than minting a new one.
//
// A campaign dispatches the next turn as soon as the previous turn's REPLY lands, and the
// reply is written before the driver that produced it exits, so the two overlap. The id is
// learned from the first turn's trailing sentinel. Reading it when the process started read
// nothing, and storing it after releasing the turn lock published it too late for the turn
// already blocked on that lock — so the second turn passed no --uuid and the remote minted a
// fresh session.
//
// Measured on CI: an opencode orchestrator ran its two turns on two sessions, and the second
// generated a title. That is a model call the cassette has no recording of at that point, so
// the replay missed and the turn died 11s in while the campaign waited out its ladder.
//
// The overlap is the test. Sequential turns pass either way, which is why this failure
// reached CI at all. cs-claude-remote is absent on purpose: it mints the id locally and has
// nothing to learn.
func TestRemoteLearnsTheSessionIdBeforeTheNextTurn(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct{ tool, prefix, id string }{
		{"cs-opencode-remote", ".cs-opencode-remote", openCodeTestSessionID},
		{"cs-codex-remote", ".cs-codex-remote", "01998c4a-0000-7000-8000-000000000000"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			home, bin := agentHome(t, tc.prefix)
			trace := filepath.Join(home, "trace")
			// ssh stands in for the remote driver. A driven turn (the call carrying --tmux)
			// records its argv, holds long enough for the next turn to start behind it, and
			// then emits the sentinel the caller learns the id from.
			writeStub(t, bin, "ssh", `#!/bin/sh
case "$*" in
  *--tmux*)
    echo "$@" >> `+trace+`
    cat >/dev/null
    sleep 1
    echo '__CS_OPENCODE_SESSION_ID__ `+tc.id+`'
    echo '__CS_CODEX_SESSION_ID__ `+tc.id+`'
    ;;
  *) exit 0 ;;
esac
`)
			writeStub(t, bin, "uuidgen", "#!/bin/sh\necho 11111111-2222-3333-4444-555555555555\n")

			first := make(chan string, 1)
			go func() {
				out, _ := runScript(t, home, bin, tc.tool, "--new", "--name", "s1", "first turn")
				first <- out
			}()
			// Long enough that the first turn holds the lock, short enough to be inside its
			// hold. The second turn then blocks where a campaign's next dispatch would.
			time.Sleep(300 * time.Millisecond)
			if out, exit := runScript(t, home, bin, tc.tool, "--resume", "s1", "second turn"); exit != 0 {
				t.Fatalf("second turn: exit %d: %s", exit, out)
			}
			<-first

			b, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal(err)
			}
			var turns []string
			for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
				if strings.Contains(line, "--tmux") {
					turns = append(turns, line)
				}
			}
			if len(turns) != 2 {
				t.Fatalf("want two driven turns, got %d:\n%s", len(turns), b)
			}
			if strings.Contains(turns[0], "--uuid") {
				t.Errorf("the first turn has no id to resume yet:\n%s", turns[0])
			}
			if !strings.Contains(turns[1], "--uuid "+tc.id) {
				t.Errorf("a turn that started behind another must resume the session that one\n"+
					"learned; without the id the remote mints a new session, whose bookkeeping\n"+
					"calls no cassette recorded:\n%s", turns[1])
			}
		})
	}
}

// imageFile reads a file from the shipped image tree, relative to image/.
func imageFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "image", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("shipped image asset missing: %v", err)
	}
	return b
}

// canonJSON re-marshals parsed JSON so two documents compare by content, not formatting.
func canonJSON(t *testing.T, raw []byte, what string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", what, err)
	}
	return m
}

// TestYoloClaudeSettingsDenyNothing: --yolo has to drop the deny RULES, not just the prompts.
//
// Claude Code enforces permissions.deny even under --dangerously-skip-permissions — the flag
// suppresses prompting, not rules — and a deny cannot be lifted by an allow at any later
// settings layer. So the guarded profile and the yolo profile have to be two different files,
// and the only difference between them may be the deny list: everything else (the allow list,
// defaultMode, and the three prompt-skipping keys the wrapper depends on) must stay identical,
// or a yolo sandbox quietly drifts into a different Claude Code than a guarded one.
func TestYoloClaudeSettingsDenyNothing(t *testing.T) {
	guarded := canonJSON(t, imageFile(t, "rootfs/home/.cs-claude/settings.json"), "guarded settings.json")
	yolo := canonJSON(t, imageFile(t, "rootfs/agent-profiles/cs-claude-settings-yolo.json"), "yolo settings")

	denyOf := func(m map[string]any, what string) []any {
		perms, ok := m["permissions"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no permissions object", what)
		}
		deny, ok := perms["deny"].([]any)
		if !ok {
			t.Fatalf("%s has no permissions.deny array", what)
		}
		return deny
	}

	// The guarded profile is the reason this test exists; if it ever stops denying anything,
	// the two variants have collapsed and the yolo one is pointless.
	if len(denyOf(guarded, "guarded settings.json")) == 0 {
		t.Error("guarded settings.json denies nothing — the yolo variant is then redundant")
	}
	// The whole point: a --yolo sandbox must not carry a rule that hard-blocks a tool call
	// with no human present to escalate to.
	if got := denyOf(yolo, "yolo settings"); len(got) != 0 {
		t.Errorf("yolo settings deny %v — under --dangerously-skip-permissions these are still "+
			"enforced, so the sandbox is not actually going yolo", got)
	}

	// Identical apart from deny. Compared as canonical JSON so formatting cannot fake a match.
	strip := func(m map[string]any) string {
		clone := maps.Clone(m)
		permClone := maps.Clone(m["permissions"].(map[string]any))
		delete(permClone, "deny")
		clone["permissions"] = permClone
		b, err := json.Marshal(clone)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if a, b := strip(guarded), strip(yolo); a != b {
		t.Errorf("the two Claude profiles differ by more than the deny list:\n guarded: %s\n yolo:    %s", a, b)
	}
}

// TestYoloSettingsInstalledByBothBootPaths: the asset is inert unless a boot path installs it,
// and there are two of them — the podman entrypoint and the microVM guest init. Updating one
// and forgetting the other ships a yolo microVM (or container) that still hard-blocks, which is
// invisible until an unattended agent dies on a denied call.
func TestYoloSettingsInstalledByBothBootPaths(t *testing.T) {
	const assetPath = "/sandbox/agent-profiles/cs-claude-settings-yolo.json"
	for _, script := range []string{"rootfs/entrypoint", "guest/init"} {
		body := string(imageFile(t, script))
		if !strings.Contains(body, assetPath) {
			t.Errorf("%s never references %s, so a --yolo instance keeps the guarded deny list", script, assetPath)
		}
		// Both directions: `rm` can keep a sandbox's data, so recreating it without --yolo has
		// to restore the guarded list rather than leaving the permissive one standing.
		if !strings.Contains(body, "/sandbox/home/.cs-claude/settings.json") {
			t.Errorf("%s never references the pristine guarded settings, so recreating a kept "+
				"sandbox without --yolo would leave the yolo rules in place", script)
		}
	}
}

// TestYoloProfilesCarryNoBlockingRules: Claude is the only agent whose profile can hard-block a
// call that --yolo was supposed to wave through, and this pins that it stays that way. Codex
// carries no deny-shaped key (--dangerously-bypass-approvals-and-sandbox overrides the approval
// and sandbox settings it does carry), and every OpenCode permission is "allow". Either one
// growing a real deny would reintroduce the Claude bug in an agent nobody thinks to check.
func TestYoloProfilesCarryNoBlockingRules(t *testing.T) {
	codex := string(imageFile(t, "rootfs/home/.cs-codex/config.toml"))
	for line := range strings.SplitSeq(codex, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if ok && strings.Contains(strings.ToLower(strings.TrimSpace(key)), "deny") {
			t.Errorf("cs-codex config.toml grew a deny-shaped key %q: --yolo passes "+
				"--dangerously-bypass-approvals-and-sandbox, which may not override it", strings.TrimSpace(key))
		}
	}

	oc := canonJSON(t, imageFile(t, "rootfs/home/.cs-opencode/opencode.json"), "opencode.json")
	perms, ok := oc["permission"].(map[string]any)
	if !ok {
		t.Fatal("shipped opencode.json has no permission object")
	}
	for k, v := range perms {
		if v != "allow" {
			t.Errorf("opencode permission %q = %v, want \"allow\": a non-allow value survives "+
				"--auto and hard-blocks a --yolo sandbox", k, v)
		}
	}
}
