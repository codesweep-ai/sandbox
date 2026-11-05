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
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/covemit"
)

// The turn DRIVERS and the agent wrappers run inside the guest, which is Linux, and they
// are written for it (GNU coreutils, /proc, tmux). Off Linux they would fail for
// environmental reasons rather than contract violations, so CI's macOS leg skips them.
//
// The cs-<agent>-remote scripts are the other half and are NOT skipped: they run on the
// HOST, which this project also supports on macOS, and every construct they used that
// only Linux has was replaced with one that both have (SBX-038). The tests for that half
// run everywhere, which is the only way the macOS host path stays working.
func skipUnlessLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("agent-tool driver scripts target the Linux guest (GNU coreutils, /proc); skipping on %s", runtime.GOOS)
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
	// A run inside a lent sandbox inherits its meter, and a script under test would read the real
	// lender. A test that wants a meter names one in extraEnv, which comes later and wins.
	cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin",
		"CS_METER_URL=", "CS_METER_TOKEN="), extraEnv...)
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
//
// opencode reads no auth file and no inline-auth variable, so a seeded API key reaches it inside
// the config the wrapper composes rather than being written beside the session db. The profile
// turns opencode's shared background service off, because that service keeps the environment of
// whichever client started it and would never see the profile's; an owner who turned it back on
// keeps that choice.
func TestOpenCodeWrapperProfile(t *testing.T) {
	skipUnlessLinux(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the wrapper composes the config with jq")
	}
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
		"echo \"content: $OPENCODE_CONFIG_CONTENT\"\n"+
		"echo \"key: $FIREWORKS_API_KEY\"\n")
	ocDir := filepath.Join(home, ".cs-opencode")
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.json"),
		[]byte(`{"model":"fireworks-ai/accounts/fireworks/models/kimi-k3"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "auth.json"),
		[]byte(`{"fireworks-ai":{"type":"api","key":"seeded"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "env"), []byte("FIREWORKS_API_KEY=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, exit := runScriptStdin(t, home, bin, openCodeNoEndpoints, "", "cs-opencode")
	if exit != 0 {
		t.Fatalf("cs-opencode exit %d: %s", exit, out)
	}
	for _, want := range []string{
		"argv: \n",
		"config: " + ocDir,
		"db: " + filepath.Join(ocDir, "opencode.db"),
		`{"provider":{"fireworks-ai":{"options":{"apiKey":"seeded"}}}}`, // the seeded key, inline
		"key: from-env-file", // the profile env file is sourced for provider keys
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wrapper output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--auto") {
		t.Errorf("non-yolo launch must not pass --auto:\n%s", out)
	}

	// The service switch is written once, where only the account can read it.
	service := filepath.Join(ocDir, "service.json")
	raw, err := os.ReadFile(service)
	if err != nil {
		t.Fatalf("no service.json: every client would join the shared background service: %v", err)
	}
	var cfg struct {
		Disabled bool `json:"disabled"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || !cfg.Disabled {
		t.Errorf("service.json = %s, want the background service disabled", raw)
	}
	if m := mode(t, service); m.Perm() != 0o600 {
		t.Errorf("service.json mode = %o, want 600", m.Perm())
	}
	if err := os.WriteFile(service, []byte(`{"disabled": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exit := runScript(t, home, bin, "cs-opencode"); exit != 0 {
		t.Fatalf("second launch exit %d", exit)
	}
	if raw, _ := os.ReadFile(service); string(raw) != `{"disabled": false}` {
		t.Errorf("the wrapper rewrote an owner's service.json: %s", raw)
	}

	// YOLO adds --auto to the TUI and to `run`, but never to a subcommand that would reject
	// the flag.
	if err := os.WriteFile(filepath.Join(ocDir, ".yolo"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		args     []string
		wantArgv string
	}{
		{"tui", nil, "argv: --auto\n"},
		{"run", []string{"run", "do it"}, "argv: run --auto do it\n"},
		{"tui on a server", []string{"--server", "http://127.0.0.1:4096", "-s", "ses_x"},
			"argv: --auto --server http://127.0.0.1:4096 -s ses_x\n"},
		{"session", []string{"session", "list"}, "argv: session list\n"},
		{"mini", []string{"mini"}, "argv: mini\n"},
		{"serve", []string{"serve", "--port", "1234"}, "argv: serve --port 1234\n"},
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

// TestOpenCodeWrapperImportsCarriedSignIns: the sign-ins `--inherit-agent-login opencode`
// carries are imported into the profile's database on the first start that finds them, and
// only then. opencode keeps sign-ins in its database, so the file is emptied rather than kept
// beside it; it is not removed, because the guest installs a seeded credential again on any
// boot that does not find it. A failed import keeps them for the next start, and the import
// names the file, never the secret.
func TestOpenCodeWrapperImportsCarriedSignIns(t *testing.T) {
	skipUnlessLinux(t)
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(home, "calls")
	writeStub(t, bin, "opencode", `#!/bin/sh
echo "$*" >> "`+calls+`"
[ "$1 $2" = "auth import" ] && exit "${STUB_IMPORT_EXIT:-0}"
exit 0
`)
	ocDir := filepath.Join(home, ".cs-opencode")
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	signIns := filepath.Join(ocDir, "credentials.json")
	const exported = `[{"id":"cred_1","integrationID":"fireworks-ai","label":"default","active":true,"value":{"type":"key","key":"fw-secret"}}]`
	if err := os.WriteFile(signIns, []byte(exported), 0o600); err != nil {
		t.Fatal(err)
	}
	imports := func() int {
		raw, _ := os.ReadFile(calls)
		return strings.Count(string(raw), "auth import "+signIns+"\n")
	}

	// A failed import keeps the sign-ins, and says so.
	out, exit := runScriptStdin(t, home, bin, append(openCodeNoEndpoints, "STUB_IMPORT_EXIT=1"), "", "cs-opencode", "run", "x")
	if exit != 0 || !strings.Contains(out, "could not import") {
		t.Fatalf("a failed import must be reported and must not stop the agent; exit %d: %s", exit, out)
	}
	if got, _ := os.ReadFile(signIns); string(got) != exported {
		t.Fatalf("a failed import lost the sign-ins: %s", got)
	}

	// The next start imports them, and empties the file.
	if out, exit := runScriptStdin(t, home, bin, openCodeNoEndpoints, "", "cs-opencode", "run", "x"); exit != 0 {
		t.Fatalf("cs-opencode exit %d: %s", exit, out)
	}
	if got := imports(); got != 2 {
		t.Fatalf("auth import ran %d times; want once per start until one succeeds", got)
	}
	if got, _ := os.ReadFile(signIns); strings.TrimSpace(string(got)) != "[]" {
		t.Errorf("credentials.json = %s after an import, want []", got)
	}
	if m := mode(t, signIns); m.Perm() != 0o600 {
		t.Errorf("credentials.json mode = %o, want 600", m.Perm())
	}

	// Once imported, they are not imported again.
	if out, exit := runScriptStdin(t, home, bin, openCodeNoEndpoints, "", "cs-opencode", "run", "x"); exit != 0 {
		t.Fatalf("cs-opencode exit %d: %s", exit, out)
	}
	if got := imports(); got != 2 {
		t.Errorf("auth import ran again on a start after the import")
	}
	if raw, _ := os.ReadFile(calls); strings.Contains(string(raw), "fw-secret") {
		t.Errorf("the secret reached an argv:\n%s", raw)
	}
	covemit.Prove(t, "auth-provisioning", "opencode", "", "scripts")
}

// installOpenCodeTurnStubs fakes the driver's dependencies: a tmux whose session is always
// alive with its server and TUI windows, a curl answering the server API (openCodeAPIStub),
// and a cs-opencode whose `run` exits $STUB_RUN_EXIT. Each logs its calls to
// $STUB_DIR/invocations, and `run` keeps the prompt it read on stdin in $STUB_DIR/prompt.
func installOpenCodeTurnStubs(t *testing.T, bin string) {
	t.Helper()
	writeStub(t, bin, "opencode", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "tmux", `#!/bin/sh
[ -n "$STUB_DIR" ] && echo "tmux $*" >> "$STUB_DIR/invocations"
case "$1" in
  list-windows) printf 'server\ntui\n' ;;
esac
exit 0
`)
	writeStub(t, bin, "cs-opencode", `#!/bin/sh
echo "cs-opencode $*" >> "$STUB_DIR/invocations"
[ "$1" = run ] && cat > "$STUB_DIR/prompt"
echo "stub-response"
exit "${STUB_RUN_EXIT:-0}"
`)
	writeStub(t, bin, "curl", openCodeAPIStub(`echo '{"version":"2.2.5"}'; exit 0`))
}

// openCodeAPIStub is a curl that answers the opencode server API as the driver asks it, and
// logs each call. health is the shell body that answers GET /api/info. The final argument is
// the URL for every call the driver makes, and a POST's body, read from stdin, is kept in
// $STUB_DIR/body.
//
// The first message-list call is the driver's pre-submit anchor (pre.json); the ones after the
// turn read the history back (message.json, then page-<cursor>.json for each cursor it
// follows). Serving them separately is what lets a test model a history that CHANGED SHAPE
// during the turn.
func openCodeAPIStub(health string) string {
	return `#!/bin/sh
for last; do :; done
echo "curl $*" >> "$STUB_DIR/invocations"
case "$*" in *--data-binary*) cat > "$STUB_DIR/body" ;; esac
case "$last" in
  */api/info) ` + health + ` ;;
  */api/session/active) cat "$STUB_DIR/active.json" 2>/dev/null || echo '{"data":{}}'; exit 0 ;;
  *cursor=*) cat "$STUB_DIR/page-${last##*cursor=}.json"; exit 0 ;;
  */api/session/*/message*)
    if [ ! -f "$STUB_DIR/.msg_called" ]; then touch "$STUB_DIR/.msg_called"; cat "$STUB_DIR/pre.json"; else cat "$STUB_DIR/message.json"; fi
    exit 0 ;;
esac
printf '{"data":{"id":"%s"}}\n' "` + openCodeTestSessionID + `"
exit 0
`
}

// ocMsg is one opencode message as the server lists it. body is the rest of its fields.
func ocMsg(id, kind string, created int64, body string) string {
	if body != "" {
		body = "," + body
	}
	return fmt.Sprintf(`{"id":%q,"type":%q,"time":{"created":%d}%s}`, id, kind, created, body)
}

// ocText is the content of an assistant message that says text.
func ocText(text string) string {
	return fmt.Sprintf(`"content":[{"type":"text","text":%q}]`, text)
}

// ocPage is one page of the message list, which the server returns newest first, built from
// messages given oldest first. next, when set, is the cursor to the page of older ones.
func ocPage(next string, oldestFirst ...string) string {
	msgs := slices.Clone(oldestFirst)
	slices.Reverse(msgs)
	page := `{"data":[` + strings.Join(msgs, ",") + `]`
	if next != "" {
		page += fmt.Sprintf(`,"cursor":{"next":%q}`, next)
	}
	return page + "}"
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
	// A cs-vcr base URL: the prefix names the provider the model runs on, then
	// the cassette, and OpenCode appends nothing after the version.
	const url = "http://vcr:8080/c/fireworks/demo/v1"

	t.Run("unset leaves the invocation alone", func(t *testing.T) {
		writeConfig(`{"model":"fireworks-ai/accounts/fireworks/models/kimi-k3"}`)
		out, exit := runScriptStdin(t, home, bin, openCodeNoEndpoints, "", "cs-opencode", "run", "x")
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
			out, exit := runScriptStdin(t, home, bin, append(openCodeNoEndpoints, "OPENCODE_BASE_URL="+url), "", "cs-opencode", "run", "x")
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

	// A config content the caller composed themselves is merged last, so what it says wins and
	// what it leaves out still comes from the wrapper.
	t.Run("a caller's own config content is merged last", func(t *testing.T) {
		writeConfig(`{"model":"fireworks-ai/accounts/fireworks/models/kimi-k3"}`)
		mine := `{"mine":true,"provider":{"fireworks-ai":{"options":{"baseURL":"http://theirs/v1"}}}}`
		out, exit := runScriptStdin(t, home, bin,
			append(openCodeNoEndpoints, "OPENCODE_BASE_URL="+url, "OPENCODE_CONFIG_CONTENT="+mine), "", "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		for _, want := range []string{`"mine":true`, `"baseURL":"http://theirs/v1"`} {
			if !strings.Contains(out, want) {
				t.Errorf("want %s in the merged content:\n%s", want, out)
			}
		}
	})

	// No model means no provider to name, and saying so beats composing a block
	// keyed on an empty string.
	t.Run("no pinned model says so", func(t *testing.T) {
		writeConfig(`{}`)
		out, exit := runScriptStdin(t, home, bin, append(openCodeNoEndpoints, "OPENCODE_BASE_URL="+url), "", "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		if !strings.Contains(out, "names no model") {
			t.Errorf("want a diagnostic naming the gap:\n%s", out)
		}
	})

	// The openai and anthropic providers each have a variable of their own, and the wrapper
	// carries it, because opencode reads none. A lent key for either provider reaches the
	// lender this way, whatever model is pinned.
	t.Run("the provider variables point their own providers", func(t *testing.T) {
		writeConfig(`{"model":"fireworks-ai/accounts/fireworks/models/kimi-k3"}`)
		out, exit := runScriptStdin(t, home, bin, append(openCodeNoEndpoints,
			"OPENAI_BASE_URL=http://lender:2500/openai", "ANTHROPIC_BASE_URL=http://lender:2500/anthropic"), "", "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		want := `{"provider":{"openai":{"options":{"baseURL":"http://lender:2500/openai"}},` +
			`"anthropic":{"options":{"baseURL":"http://lender:2500/anthropic"}}}}`
		if !strings.Contains(out, want) {
			t.Errorf("want %s:\n%s", want, out)
		}
		covemit.Prove(t, "auth-provisioning", "opencode", "", "scripts")
	})

	// OPENCODE_BASE_URL names the endpoint for the provider the session uses, so it wins over
	// that provider's own variable.
	t.Run("OPENCODE_BASE_URL wins for the pinned provider", func(t *testing.T) {
		writeConfig(`{"model":"openai/gpt-5"}`)
		out, exit := runScriptStdin(t, home, bin, append(openCodeNoEndpoints,
			"OPENAI_BASE_URL=http://lender:2500/openai", "OPENCODE_BASE_URL="+url), "", "cs-opencode", "run", "x")
		if exit != 0 {
			t.Fatalf("cs-opencode exit %d: %s", exit, out)
		}
		if want := `{"provider":{"openai":{"options":{"baseURL":"` + url + `"}}}}`; !strings.Contains(out, want) {
			t.Errorf("want %s:\n%s", want, out)
		}
	})
}

// openCodeNoEndpoints clears every variable cs-opencode composes a config from, so a test is
// not decided by the environment it runs in: a lent sandbox sets ANTHROPIC_BASE_URL.
var openCodeNoEndpoints = []string{"OPENCODE_BASE_URL=", "OPENAI_BASE_URL=", "ANTHROPIC_BASE_URL=", "OPENCODE_CONFIG_CONTENT="}

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
			env:  []string{"OPENAI_BASE_URL=http://vcr:8080/c/openai/demo/v1", "OPENAI_API_KEY=sk-not-a-real-key"},
			wantArgv: `argv: -c model_provider="cs-proxy" -c model_providers.cs-proxy=` +
				`{name="cs-proxy", base_url="http://vcr:8080/c/openai/demo/v1", env_key="OPENAI_API_KEY", wire_api="responses", stream_max_retries=10} exec do it`,
		},
		{
			// The budget depends on the key's tier, so both counts can be set, and an
			// empty value hands the count back to codex. A value that is not a number
			// is dropped: inside the provider block it would stop codex from starting.
			name: "the retry budget can be set",
			env: []string{"OPENAI_BASE_URL=http://vcr:8080/v1", "OPENAI_API_KEY=sk-not-a-real-key",
				"CS_CODEX_STREAM_MAX_RETRIES=40", "CS_CODEX_REQUEST_MAX_RETRIES=8"},
			wantArgv: `wire_api="responses", stream_max_retries=40, request_max_retries=8} exec do it`,
		},
		{
			name: "an empty budget leaves codex its default",
			env: []string{"OPENAI_BASE_URL=http://vcr:8080/v1", "OPENAI_API_KEY=sk-not-a-real-key",
				"CS_CODEX_STREAM_MAX_RETRIES="},
			wantArgv:    `wire_api="responses"} exec do it`,
			notWantArgv: "max_retries",
		},
		{
			name: "a budget that is not a number is dropped",
			env: []string{"OPENAI_BASE_URL=http://vcr:8080/v1", "OPENAI_API_KEY=sk-not-a-real-key",
				"CS_CODEX_STREAM_MAX_RETRIES=lots"},
			wantArgv:    `wire_api="responses"} exec do it`,
			notWantArgv: ", stream_max_retries",
		},
		{
			// No key means the subscription path, which authenticates as codex
			// itself rather than with a header. The empty assignment is not
			// decoration: the test inherits the developer's environment, and a
			// key there would otherwise decide this case for it.
			name: "a subscription asks for codex's own auth",
			env:  []string{"OPENAI_BASE_URL=http://vcr:8080/c/chatgpt/demo", "OPENAI_API_KEY="},
			wantArgv: `argv: -c model_provider="cs-proxy" -c model_providers.cs-proxy=` +
				`{name="cs-proxy", base_url="http://vcr:8080/c/chatgpt/demo", requires_openai_auth=true, wire_api="responses", stream_max_retries=10} exec do it`,
			notWantArgv: "env_key",
		},
		{
			// A lent login is also told where ChatGPT's own backend is: the
			// lender's second name, which answers the one call Codex 0.156 and
			// later need to start (SBX-084).
			name: "a lent login is told where ChatGPT's backend is",
			env: []string{"OPENAI_BASE_URL=http://cs-lender:2500", "OPENAI_API_KEY=",
				"CS_CODEX_CHATGPT_BASE_URL=http://cs-lender-chatgpt:2500/backend-api"},
			wantArgv: `-c chatgpt_base_url="http://cs-lender-chatgpt:2500/backend-api" exec do it`,
		},
		{
			name:        "no backend variable, no backend override",
			env:         []string{"OPENAI_BASE_URL=http://vcr:8080/c/chatgpt/demo", "OPENAI_API_KEY=", "CS_CODEX_CHATGPT_BASE_URL="},
			wantArgv:    `requires_openai_auth=true`,
			notWantArgv: "chatgpt_base_url",
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

// TestOpenCodeTurnCompletionSemantics: a turn is judged by the `idle` message opencode records
// when the session stops, never by run's exit code. run exits 1 after a step that failed mid-stream
// and was retried to a success, and a session's own record is the one that knows how it ended.
func TestOpenCodeTurnCompletionSemantics(t *testing.T) {
	skipUnlessLinux(t)
	user := ocMsg("msg_u", "user", 1, `"text":"do the thing"`)
	reply := ocMsg("msg_r", "assistant", 2, ocText("stub-response"))
	idle := func(outcome, err string) string {
		body := fmt.Sprintf(`"outcome":%q`, outcome)
		if err != "" {
			body += `,"error":` + err
		}
		return ocMsg("msg_i", "idle", 3, body)
	}
	unauthorized := `{"type":"provider.auth","message":"Invalid API key provided","status":401}`
	throttled := `{"type":"provider.rate-limit","message":"Rate limit exceeded","status":429,"retryAfterMs":6500}`
	// The lender's refusal under a hard token budget. OpenCode 2.2.5 records it as
	// provider.invalid-request, measured on 2026-11-04 against a live lender, and the same body as
	// a 429 as provider.rate-limit, which the driver otherwise reads as a throttle. OpenCode's own
	// budget kind is held too, for a later version that maps the body to it. Only the marker in
	// the message decides, and a malformed request, a 400 as well, stays other.
	const refusal = `"message":"cs-sandbox token budget spent: sandbox web has used 2.00M of its 2.00M output tokens. The operator can raise it on the host.","status":400`
	budgetSpent := `{"type":"provider.invalid-request",` + refusal + `}`
	budgetKind := `{"type":"provider.budget-limit",` + refusal + `}`
	budgetThrottle := `{"type":"provider.rate-limit",` + strings.Replace(refusal, `"status":400`, `"status":429`, 1) + `}`
	badRequest := `{"type":"provider.invalid-request","message":"max_tokens: 0 is below the minimum of 1","status":400}`

	for _, tc := range []struct {
		name, page, runExit string
		wantExit            int
		wantInOut           string
	}{
		// The happy path emits the turn's text plus the session-id sentinel.
		{"success", ocPage("", user, reply, idle("succeeded", "")), "0", 0, "__CS_OPENCODE_SESSION_ID__ " + openCodeTestSessionID},
		// The session recorded the provider's refusal, whatever the client said.
		{"provider-error", ocPage("", user, ocMsg("msg_r", "assistant", 2, `"content":[],"error":`+unauthorized),
			idle("failed", unauthorized)), "1", 5, "failure class=unauthorized retry_after=-"},
		{"provider-error-despite-exit-0", ocPage("", user, idle("failed", unauthorized)), "0", 5, "failure class=unauthorized retry_after=-"},
		{"token-budget-spent", ocPage("", user, idle("failed", budgetSpent)), "0", 5, "failure class=spent retry_after=-"},
		{"token-budget-spent-as-a-budget-kind", ocPage("", user, idle("failed", budgetKind)), "0", 5, "failure class=spent retry_after=-"},
		{"token-budget-spent-as-a-throttle", ocPage("", user, idle("failed", budgetThrottle)), "0", 5, "failure class=spent retry_after=-"},
		{"malformed-request", ocPage("", user, idle("failed", badRequest)), "0", 5, "failure class=other retry_after=-"},
		// The wait opencode asks for travels as milliseconds, and a caller is told whole seconds.
		{"throttled", ocPage("", user, idle("failed", throttled)), "1", 5, "failure class=throttled retry_after=7"},
		// A step that failed mid-stream and was retried leaves run exiting 1 on a turn that
		// succeeded.
		{"run-exit-nonzero-on-a-turn-that-succeeded", ocPage("", user, reply, idle("succeeded", "")), "1", 0, "stub-response"},
		// The client failed before the session recorded anything: its own stderr says why.
		{"run-exit-nonzero-with-nothing-recorded", ocPage(""), "1", 5, "turn failed (opencode run exit 1)"},
		{"no-outcome-despite-exit-0", ocPage("", user, reply), "0", 5, "no-outcome-this-turn"},
		// An interrupt the context hook did not make is a failed turn that no provider refused.
		{"interrupted", ocPage("", user, idle("interrupted", "")), "0", 5, "failure class=other retry_after=-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-opencode-remote")
			installOpenCodeTurnStubs(t, bin)
			stubDir := t.TempDir()
			writeSnapshots(t, stubDir, ocPage(""), tc.page)
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
			// Turn output is read back from the session API, never from the client's stdout.
			if tc.wantExit == 0 && !strings.Contains(out, "stub-response") {
				t.Fatalf("success output missing the session's assistant text: %s", out)
			}
			// Every turn end leaves a line on this machine, a good one included, so a caller
			// can tell that an older failure no longer applies.
			line := lastTurnLine(t, home, "opencode")
			if line.exit != tc.wantExit {
				t.Fatalf("turn log says exit %d; the driver exited %d", line.exit, tc.wantExit)
			}
			if strings.Contains(tc.wantInOut, "failure class=") {
				wantTurnLogAgrees(t, home, "opencode", tc.wantInOut)
			} else if line.class != "-" {
				t.Fatalf("turn log names class %q for a turn no provider refused", line.class)
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

// TestOpenCodeTurnAnchorsOnMessageIdentity: a turn is delimited by the ID of the newest
// message that preceded it, never by a message COUNT or by position: compaction adds messages
// of its own, and a long turn spans more than one page of the list.
func TestOpenCodeTurnAnchorsOnMessageIdentity(t *testing.T) {
	skipUnlessLinux(t)
	say := func(id, text string, created int64) string { return ocMsg(id, "assistant", created, ocText(text)) }
	ask := func(id string, created int64) string { return ocMsg(id, "user", created, `"text":"ask"`) }
	done := func(id string, created int64) string { return ocMsg(id, "idle", created, `"outcome":"succeeded"`) }
	// Year 2100, so it always sorts after the turn's start timestamp.
	const future = int64(4102444800000)

	for _, tc := range []struct {
		name, pre, post string
		pages           map[string]string
		wantExit        int
		wantIn          string
		wantNotIn       []string
	}{
		{
			// A compaction ran inside the turn. Its summary is not the turn's answer.
			name: "a compaction during the turn",
			pre:  ocPage("", ask("msg_a", 1), say("msg_b", "OLD REPLY", 2), done("msg_c", 3)),
			post: ocPage("", ask("msg_a", 1), say("msg_b", "OLD REPLY", 2), done("msg_c", 3), ask("msg_d", 4),
				ocMsg("msg_s", "compaction", 5, `"status":"completed","reason":"auto","summary":"SUMMARY"`),
				say("msg_e", "NEW REPLY", 6), done("msg_f", 7)),
			wantExit: 0, wantIn: "NEW REPLY", wantNotIn: []string{"OLD REPLY", "SUMMARY"},
		},
		{
			// The turn added nothing. Checking the session's last idle message would pass on
			// the PREVIOUS turn's and report a silent empty success.
			name:     "turn produced nothing",
			pre:      ocPage("", say("msg_a", "PREVIOUS TURN", 1), done("msg_b", 2)),
			post:     ocPage("", say("msg_a", "PREVIOUS TURN", 1), done("msg_b", 2)),
			wantExit: 5, wantIn: "no-outcome-this-turn", wantNotIn: []string{"PREVIOUS TURN"},
		},
		{
			// The anchor is gone from the history: creation time still bounds the turn.
			name:     "anchor gone",
			pre:      ocPage("", ask("msg_gone", 1)),
			post:     ocPage("", say("msg_old", "OLD REPLY", 1), say("msg_new", "FALLBACK REPLY", future), done("msg_i", future+1)),
			wantExit: 0, wantIn: "FALLBACK REPLY", wantNotIn: []string{"OLD REPLY"},
		},
		{
			// The turn is longer than a page: the read follows the cursor back to the anchor.
			name: "a turn longer than one page",
			pre:  ocPage("", ask("msg_a", 1), say("msg_b", "OLD REPLY", 2), done("msg_c", 3)),
			post: ocPage("older", say("msg_f", "SECOND PAGE TEXT", 6), done("msg_g", 7)),
			pages: map[string]string{
				"older": ocPage("oldest", done("msg_c", 3), ask("msg_d", 4), say("msg_e", "FIRST PAGE TEXT", 5)),
			},
			wantExit: 0, wantIn: "FIRST PAGE TEXT\n\nSECOND PAGE TEXT", wantNotIn: []string{"OLD REPLY"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-opencode-remote")
			installOpenCodeTurnStubs(t, bin)
			stubDir := t.TempDir()
			writeSnapshots(t, stubDir, tc.pre, tc.post)
			for cursor, page := range tc.pages {
				if err := os.WriteFile(filepath.Join(stubDir, "page-"+cursor+".json"), []byte(page), 0o600); err != nil {
					t.Fatal(err)
				}
			}
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
			writeSnapshots(t, stubDir, ocPage(""), ocPage(""))
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
			// Wait's error is the kill's doing; only that the runner exited counts.
			done := make(chan struct{})
			go func() { _ = runner.Wait(); close(done) }()
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

// TestRemoteBreaksALockItsOwnerNoLongerHolds: a turn lock is a DIRECTORY, and nothing
// removes it when its holder is killed outright. The kernel used to do that, while the
// lock was flock(1) on a file — and flock is util-linux, so a macOS host could not take
// the lock at all and every turn dispatched from one was told its session was busy
// (SBX-038).
//
// The owner pid recorded inside the directory is what replaces release-on-death: a caller
// that finds that process gone takes the lock rather than waiting for it. Without that,
// one hard-killed turn makes its session unusable for CS_<AGENT>_LOCK_WAIT — 900 seconds
// by default — which is worse than what flock did, not a port of it.
//
// Not skipped off Linux: this is the host half of the tools, and the point of the change
// is that the host can be a Mac.
func TestRemoteBreaksALockItsOwnerNoLongerHolds(t *testing.T) {
	for _, fam := range remoteFamilies {
		t.Run(fam.agent, func(t *testing.T) {
			home, bin := agentHome(t, fam.prefix)
			writeStub(t, bin, "scp", "#!/bin/sh\nexit 0\n")
			name := "stale-lock"
			if err := os.WriteFile(filepath.Join(home, fam.prefix+"-sessions", name+fam.mapSuffix),
				[]byte(fam.mapValue+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// A pid that has run and been reaped, rather than an invented number: an
			// invented one can be alive on the machine running this, and the test would
			// then assert the opposite of what it says.
			reaped := exec.Command("sh", "-c", "exit 0")
			if err := reaped.Run(); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(home, fam.prefix+"-locks", name+".lock")
			if err := os.MkdirAll(lock, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(lock, "owner"),
				[]byte(strconv.Itoa(reaped.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			// One second of patience: a lock that was waited on rather than broken would
			// end this turn with exit 4 and the "busy" message, not with a dispatch.
			lockWait := "CS_" + strings.ToUpper(fam.agent) + "_LOCK_WAIT=1"
			out, exit := runScriptStdin(t, home, bin, []string{lockWait}, "",
				"cs-"+fam.agent+"-remote", "--resume", name, "-H", "host", "hello")
			if exit != 0 {
				t.Fatalf("a lock whose owner is gone must not hold a turn up: exit %d: %s", exit, out)
			}
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Errorf("the turn finished but its lock is still there: %v", err)
			}
			covemit.Prove(t, "interrupt", fam.agent, "", "scripts")
		})
	}
}

// TestRemoteBackgroundTurnRuns: a -b turn has to START, wherever its caller runs. Apple's
// nohup exits without exec'ing anything under a LaunchDaemon, which is how a self-hosted
// Actions runner runs, and the launch used it there: every campaign turn dispatched from
// one printed "PID unknown" and never ran. So the runner must record a real pid and write
// its footer, and a caller that kills its own process group on return — as agent CLIs do
// — must not take the turn with it.
//
// Not skipped off Linux, and CI's macOS runners are LaunchDaemons: this is the test that
// fails there if the launch ever depends again on a program launchd can refuse.
func TestRemoteBackgroundTurnRuns(t *testing.T) {
	for _, fam := range remoteFamilies {
		t.Run(fam.agent, func(t *testing.T) {
			home, bin := agentHome(t, fam.prefix)
			// Slow enough that the runner is still alive when the launcher looks for its
			// pid, which the runner removes when the turn ends.
			writeStub(t, bin, "ssh", "#!/bin/sh\nsleep 0.3\nexit 0\n")
			writeStub(t, bin, "scp", "#!/bin/sh\nexit 0\n")
			// nohup as Apple's behaves under a LaunchDaemon, so a launch that leans on it
			// fails on every host that runs this, not only on a Mac.
			writeStub(t, bin, "nohup", "#!/bin/sh\necho \"nohup: can't detach from console\" >&2\nexit 127\n")
			name := "bg-contract"
			if err := os.WriteFile(filepath.Join(home, fam.prefix+"-sessions", name+fam.mapSuffix),
				[]byte(fam.mapValue+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			launch := exec.Command(agentTool("cs-"+fam.agent+"-remote"), "--resume", name, "-H", "host", "-b", "hello")
			launch.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin")
			launch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			out, err := launch.CombinedOutput()
			if err != nil {
				t.Fatalf("-b launch: %v: %s", err, out)
			}
			_ = syscall.Kill(-launch.Process.Pid, syscall.SIGKILL)
			if !regexp.MustCompile(`Running:\s+background \(PID [0-9]+\)`).Match(out) {
				t.Fatalf("the runner recorded no pid, so it never started: %s", out)
			}

			logPath := filepath.Join(home, fam.prefix+"-logs", name+".log")
			for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				data, _ := os.ReadFile(logPath)
				if strings.Contains(string(data), "finished (exit 0)") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the turn wrote no footer after its caller's group was killed:\n%s", data)
				}
			}
			out2, exit := runScript(t, home, bin, "cs-"+fam.agent+"-remote-output", name, "-s")
			if got := strings.TrimSpace(strings.SplitN(out2, "\n", 2)[0]); got != "finished" || exit != 0 {
				t.Errorf("after the turn, -s = %q exit %d; want finished exit 0", got, exit)
			}
			covemit.Prove(t, "status-contract", fam.agent, "", "scripts")
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
      if [ -n "$STUB_ASSISTANT" ]; then
        printf '%s\n' "$STUB_ASSISTANT" >> "$STUB_DIR/session.jsonl"
      else
        printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"STUB ANSWER"}]}}' >> "$STUB_DIR/session.jsonl"
      fi
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

// TestClaudeTurnReportsAProviderFailure: Claude Code ends a turn the provider refused the way
// it ends any other. It writes the refusal as an assistant message, then the completion
// marker, so a driver that prints the turn's text exits 0 with "API Error: …" as the model's
// reply. The message is flagged, and the flag is what the driver has to read. The entries
// are what Claude Code 2.1.258 wrote against a provider answering 429, 529, 401 and 400.
func TestClaudeTurnReportsAProviderFailure(t *testing.T) {
	skipUnlessLinux(t)
	entry := func(kind string, status int, text string) string {
		return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"error":%q,"apiErrorStatus":%d,`+
			`"message":{"content":[{"type":"text","text":%q}]}}`, kind, status, text)
	}
	for _, tc := range []struct {
		name, assistant string
		wantExit        int
		wantIn          string
	}{
		{"rate limit", entry("rate_limit", 429, "API Error: Request rejected (429) · This request would exceed your rate limit."),
			5, "failure class=throttled retry_after=-"},
		{"overloaded", entry("server_error", 529, "API Error: Repeated 529 Overloaded errors. The API is at capacity."),
			5, "failure class=capacity retry_after=-"},
		// A stream that broke after it began carries no status. The same session is sent the
		// prompt again at once, so the wait is 0 rather than a back-off (SBX-073).
		{"a response that broke mid-stream",
			`{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"error":"server_error",` +
				`"message":{"content":[{"type":"text","text":"API Error: Server error mid-response. The response above may be incomplete."}]}}`,
			5, "failure class=capacity retry_after=0"},
		{"a 500 refused at the start", entry("server_error", 500, "API Error: 500 Internal server error"),
			5, "failure class=capacity retry_after=-"},
		{"bad credential", entry("authentication_failed", 401, "Invalid API key · Fix external API key"),
			5, "failure class=unauthorized retry_after=-"},
		{"context too long", entry("invalid_request", 400, "Prompt is too long · the request is ~250000 tokens (limit 200000)"),
			5, "failure class=context retry_after=-"},
		// The lender's refusal under a hard token budget, in the shape Claude Code 2.1.258 recorded
		// it. Only the marker tells it apart from the malformed request below: both are 400s.
		{"token budget spent", entry("unknown", 400, "API Error: 400 cs-sandbox token budget spent: sandbox web has used 2.00M of its 2.00M output tokens. The operator can raise it on the host."),
			5, "failure class=spent retry_after=-"},
		{"a malformed request", entry("unknown", 400, "API Error: 400 max_tokens: 0 is below the minimum of 1"),
			5, "failure class=other retry_after=-"},
		// Claude Code retries by itself. An error it got past is not the turn's outcome.
		{"an error the turn recovered from", entry("server_error", 529, "API Error: 529 Overloaded.") + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"RECOVERED ANSWER"}]}}`,
			0, "RECOVERED ANSWER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := os.WriteFile(filepath.Join(stubDir, "pane.txt"), []byte("  auto mode on\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "STUB_ASSISTANT=" + tc.assistant, "CS_CLAUDE_STALL_SECS=0"}, "do the thing\n",
				"cs-claude-turn", "--uuid", claudeTestUUID, "--projects", projects, "--timeout", "30")
			if exit != tc.wantExit {
				t.Fatalf("exit = %d; want %d: %s", exit, tc.wantExit, out)
			}
			if !strings.Contains(out, tc.wantIn) {
				t.Fatalf("output missing %q: %s", tc.wantIn, out)
			}
			if tc.wantExit == 5 {
				wantTurnLogAgrees(t, home, "claude", tc.wantIn)
			}
			covemit.Prove(t, "turn-driver-semantics", "claude", "", "scripts")
		})
	}
}

// TestClaudeTurnWaitsOutClaudesOwnRetries: Claude Code retries a throttled call ten times by
// itself, for minutes. While it waits the pane reads "Retrying in 14s · attempt 3/10", nothing
// is appended to the session file, and "esc to interrupt" is absent. Read as idle, that let
// the stall watchdog report a throttled member as a stalled one, and a fleet harness answers
// a stall with a restart. The retry here outlasts the stall limit, and the turn has to end
// as the provider failure it is.
func TestClaudeTurnWaitsOutClaudesOwnRetries(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-claude-remote")
	writeStub(t, bin, "claude", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
	writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) exit 0 ;;
  capture-pane)
    if [ -f "$STUB_DIR/.submitted" ] && [ ! -f "$STUB_DIR/.gave-up" ]; then
      printf '%s\n' "* 429 This request would exceed your rate limit. · Retrying in 14s · attempt 3/10"
    fi
    printf '  bypass permissions on\n'
    exit 0 ;;
  send-keys)
    if [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      ( sleep 4
        printf '%s\n' "$STUB_ASSISTANT" >> "$STUB_DIR/session.jsonl"
        printf '%s\n' '{"type":"system","subtype":"turn_duration"}' >> "$STUB_DIR/session.jsonl"
        touch "$STUB_DIR/.gave-up" ) >/dev/null 2>&1 &
    fi
    exit 0 ;;
esac
exit 0
`)
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
	refused := `{"type":"assistant","isApiErrorMessage":true,"error":"rate_limit","apiErrorStatus":429,` +
		`"message":{"content":[{"type":"text","text":"API Error: Request rejected (429)"}]}}`
	out, exit := runScriptStdin(t, home, bin,
		// A stall limit of 2s against a retry of 4s: the watchdog gets its chance and must pass it up.
		[]string{"STUB_DIR=" + stubDir, "STUB_ASSISTANT=" + refused, "CS_CLAUDE_STALL_SECS=2"}, "do the thing\n",
		"cs-claude-turn", "--uuid", claudeTestUUID, "--projects", projects, "--timeout", "30")
	if exit != 5 || !strings.Contains(out, "failure class=throttled") {
		t.Fatalf("exit = %d; want 5 and class=throttled, not a stall: %s", exit, out)
	}
	covemit.Prove(t, "turn-driver-semantics", "claude", "", "scripts")
}

// TestClaudeTurnCutsShortARejectedCredential: Claude Code retries a 401 ten times, for about
// three minutes, as it would a rate limit. Waiting cannot fix a credential, so the driver
// reports it at once. It ends the TUI rather than sending Esc: in vim mode the first Esc only
// leaves insert mode, and a Claude left retrying writes its completion marker into the next
// turn. The next turn resumes the conversation from the session file. A rate limit shows
// the same retry line and is waited out.
func TestClaudeTurnCutsShortARejectedCredential(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct {
		name, retryLine string
		wantExit        int
		wantIn          string
	}{
		{"401", "* 401 invalid x-api-key · Retrying in 17s · attempt 3/10", 5, "failure class=unauthorized retry_after=-"},
		{"403", "* 403 Your key may not use this model · Retrying in 2s · attempt 1/10", 5, "failure class=unauthorized"},
		{"a rate limit is waited out", "* 429 This request would exceed your rate limit. · Retrying in 14s · attempt 3/10", 2, "did not complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude-remote")
			writeStub(t, bin, "claude", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "tmux", `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
case "$1" in
  has-session) exit 0 ;;
  capture-pane)
    [ -f "$STUB_DIR/.submitted" ] && printf '> do the thing\n%s\n' "$RETRY_LINE"
    printf '  bypass permissions on\n'
    exit 0 ;;
  send-keys) touch "$STUB_DIR/.submitted"; exit 0 ;;
esac
exit 0
`)
			stubDir := t.TempDir()
			projects := filepath.Join(home, "projects")
			if err := os.MkdirAll(projects, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projects, claudeTestUUID+".jsonl"), []byte(""), 0o600); err != nil {
				t.Fatal(err)
			}
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "RETRY_LINE=" + tc.retryLine, "CS_CLAUDE_STALL_SECS=0"}, "do the thing\n",
				"cs-claude-turn", "--uuid", claudeTestUUID, "--projects", projects, "--timeout", "4")
			if exit != tc.wantExit || !strings.Contains(out, tc.wantIn) {
				t.Fatalf("exit = %d; want %d with %q: %s", exit, tc.wantExit, tc.wantIn, out)
			}
			calls, _ := os.ReadFile(filepath.Join(stubDir, "calls"))
			if ended := strings.Contains(string(calls), "kill-session"); ended != (tc.wantExit == 5) {
				t.Errorf("the TUI ended = %v; tmux calls:\n%s", ended, calls)
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

// claudeDialogTmux is a tmux whose pane shows dialog.txt until the driver sends Escape, then
// the ready composer. The first $STUB_DIR/loading captures show Claude still starting. The
// Enter that submits the prompt ends the turn with STUB ANSWER.
const claudeDialogTmux = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
case "$1" in
  has-session) exit 0 ;;
  capture-pane)
    n=$(cat "$STUB_DIR/loading" 2>/dev/null || echo 0)
    if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$STUB_DIR/loading"; printf ' ✻ Welcome to Claude Code\n'; exit 0; fi
    if [ -f "$STUB_DIR/.dismissed" ]; then printf '❯ \n  bypass permissions on\n'; else cat "$STUB_DIR/dialog.txt"; fi
    exit 0 ;;
  send-keys)
    for last; do :; done
    if [ "$last" = Escape ]; then
      n=$(cat "$STUB_DIR/ignore" 2>/dev/null || echo 0)
      if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$STUB_DIR/ignore"; exit 0; fi
      touch "$STUB_DIR/.dismissed"; exit 0
    fi
    if [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"STUB ANSWER"}]}}' >> "$STUB_DIR/session.jsonl"
      printf '%s\n' '{"type":"system","subtype":"turn_duration"}' >> "$STUB_DIR/session.jsonl"
    fi
    exit 0 ;;
esac
exit 0
`

// TestClaudeTurnAnswersADialogOrSaysWhoMust: Claude Code shows new one-time dialogs from
// release to release. Read as a screen still loading, one failed the turn after a minute with
// "last state: unknown", and nothing said a person was needed or what for (SBX-075). A dialog
// the driver knows is refused with its own key and the turn goes on, and the turn log says
// so. Any other screen asking for a choice fails the turn at once, quoting the screen, and
// the driver presses nothing on it.
func TestClaudeTurnAnswersADialogOrSaysWhoMust(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct {
		name, dialog    string
		wantExit        int
		wantIn, reason  string
		wantEscape      bool
		ignore, loading int
	}{
		{"the fullscreen renderer offer is refused",
			"Try the new fullscreen renderer? It redraws the whole screen.\n❯ 1. Yes, try it\n  2. Not now\nEnter to confirm · Esc to cancel\n",
			0, "STUB ANSWER", "dismissed a dialog: Try the new fullscreen renderer?", true, 0, 0},
		// A busy host: Claude takes keys only seconds after it draws the dialog (SBX-090).
		{"the offer is refused once Claude takes keys",
			"Try the new fullscreen renderer? It redraws the whole screen.\n❯ 1. Yes, try it\n  2. Not now\nEnter to confirm · Esc to cancel\n",
			0, "STUB ANSWER", "dismissed a dialog: Try the new fullscreen renderer?", true, 4, 0},
		// The dialog opens between the driver's looks at one screen. Each look has to be
		// at the same capture, or the second sees a choice the first did not know (SBX-103).
		{"an offer that opens while the driver reads the screen is refused",
			"Try the new fullscreen renderer? It redraws the whole screen.\n❯ 1. Yes, try it\n  2. Not now\nEnter to confirm · Esc to cancel\n",
			0, "STUB ANSWER", "dismissed a dialog: Try the new fullscreen renderer?", true, 0, 2},
		{"a dialog nobody has seen needs a person",
			"Share usage data to help improve Claude Code?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel\n",
			3, "needs a person: Share usage data to help improve Claude Code? | ❯ 1. Yes | 2. No",
			"needs a person: Share usage data", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-claude-remote")
			writeStub(t, bin, "claude", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "tmux", claudeDialogTmux)
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
			if err := os.WriteFile(filepath.Join(stubDir, "dialog.txt"), []byte(tc.dialog), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "ignore"), []byte(strconv.Itoa(tc.ignore)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "loading"), []byte(strconv.Itoa(tc.loading)), 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "CS_CLAUDE_STALL_SECS=0"}, "do the thing\n",
				"cs-claude-turn", "--uuid", claudeTestUUID, "--projects", projects, "--timeout", "30")
			if exit != tc.wantExit || !strings.Contains(out, tc.wantIn) {
				t.Fatalf("exit = %d; want %d with %q: %s", exit, tc.wantExit, tc.wantIn, out)
			}
			if took := time.Since(start); took > 20*time.Second {
				t.Errorf("the driver took %s over it, as though waiting the screen out", took)
			}
			calls, _ := os.ReadFile(filepath.Join(stubDir, "calls"))
			if pressed := strings.Contains(string(calls), "Escape"); pressed != tc.wantEscape {
				t.Errorf("Escape sent = %v; tmux calls:\n%s", pressed, calls)
			}
			if l := lastTurnLine(t, home, "claude"); l.exit != tc.wantExit || !strings.Contains(l.reason, tc.reason) {
				t.Errorf("turn log line = %+v; want exit %d and a reason holding %q", l, tc.wantExit, tc.reason)
			}
		})
	}
}

// codexDialogTmux is a tmux whose pane shows dialog.txt until the driver sends "2", then the
// ready composer. The first $STUB_DIR/loading captures show Codex still loading its model
// catalog. The Enter that submits the prompt ends the turn with STUB ANSWER.
const codexDialogTmux = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
case "$1" in
  has-session) [ -f "$STUB_DIR/.launched" ] && exit 0 || exit 1 ;;
  new-session)
    touch "$STUB_DIR/.launched"
    : > "$FRESH_ROLLOUT"
    exit 0 ;;
  capture-pane)
    n=$(cat "$STUB_DIR/loading" 2>/dev/null || echo 0)
    if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$STUB_DIR/loading"; printf '  model: loading\n  › Ask Codex to do anything\n  ? for shortcuts\n'; exit 0; fi
    if [ -f "$STUB_DIR/.dismissed" ]; then printf '  › Ask Codex to do anything\n  gpt-5.6-sol medium · /work\n'; else cat "$STUB_DIR/dialog.txt"; fi
    exit 0 ;;
  send-keys)
    for last; do :; done
    if [ "$last" = 2 ]; then
      n=$(cat "$STUB_DIR/ignore" 2>/dev/null || echo 0)
      if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$STUB_DIR/ignore"; exit 0; fi
      touch "$STUB_DIR/.dismissed"; exit 0
    fi
    if [ -f "$STUB_DIR/.dismissed" ] && [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' '{"payload":{"type":"agent_message","message":"STUB ANSWER"}}' >> "$FRESH_ROLLOUT"
      printf '%s\n' '{"payload":{"type":"task_complete"}}' >> "$FRESH_ROLLOUT"
    fi
    exit 0 ;;
esac
exit 0
`

// TestCodexTurnAnswersADialogOrSaysWhoMust: Codex 0.157 offers a newer model at startup when
// the configured one has a successor. Its footer carries the " · " the ready check keys on,
// so the driver read the dialog as ready, pasted the prompt into it, and no turn started
// (SBX-085). The offer is declined with "2", which keeps the configured model, and the turn
// goes on. A migration with no way to decline, or any other modal, fails the turn at once,
// quoting the screen, and the driver presses nothing on it.
func TestCodexTurnAnswersADialogOrSaysWhoMust(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct {
		name, dialog    string
		wantExit        int
		wantIn, reason  string
		wantAnswer      bool
		ignore, loading int
	}{
		{"a newer model is declined",
			"  Meet GPT-6 Sol\n\n  Codex just got an upgrade.\n\n  › 1. Try new model\n    2. Use existing model\n\n  enter/esc confirm · ctrl + c quit\n",
			0, "STUB ANSWER", "answered a dialog: Use existing model", true, 0, 0},
		// A busy host: Codex takes keys only seconds after it draws the dialog (SBX-090).
		{"a newer model is declined once Codex takes keys",
			"  Meet GPT-6 Sol\n\n  Codex just got an upgrade.\n\n  › 1. Try new model\n    2. Use existing model\n\n  enter/esc confirm · ctrl + c quit\n",
			0, "STUB ANSWER", "answered a dialog: Use existing model", true, 4, 0},
		// Codex draws its main screen first and opens the offer once its model catalog loads,
		// which can be between the driver's looks at one screen. Each look has to be at the
		// same capture, or the second sees a choice the first did not know (SBX-103).
		{"a newer model offered while the driver reads the screen is declined",
			"  Meet GPT-6 Sol\n\n  Codex just got an upgrade.\n\n  › 1. Try new model\n    2. Use existing model\n\n  enter/esc confirm · ctrl + c quit\n",
			0, "STUB ANSWER", "answered a dialog: Use existing model", true, 0, 2},
		{"a migration that cannot be declined needs a person",
			"  GPT-5.4 Mini is no longer available\n\n  Codex now uses GPT-6 Luna in place of GPT-5.4 Mini.\n\n  enter/esc continue · ctrl + c quit\n",
			3, "needs a person: GPT-5.4 Mini is no longer available",
			"needs a person: GPT-5.4 Mini is no longer available", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-codex-remote")
			sess := filepath.Join(home, ".cs-codex", "sessions")
			if err := os.MkdirAll(sess, 0o700); err != nil {
				t.Fatal(err)
			}
			fresh := filepath.Join(sess, "rollout-2026-01-02-33333333-3333-4333-8333-333333333333.jsonl")
			stubDir := t.TempDir()
			writeStub(t, bin, "codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "cs-codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "tmux", codexDialogTmux)
			if err := os.WriteFile(filepath.Join(stubDir, "dialog.txt"), []byte(tc.dialog), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "ignore"), []byte(strconv.Itoa(tc.ignore)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "loading"), []byte(strconv.Itoa(tc.loading)), 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "FRESH_ROLLOUT=" + fresh, "CS_CODEX_STALL_SECS=0"},
				"do the thing\n", "cs-codex-turn", "--tmux", "codextoken", "--timeout", "30")
			if exit != tc.wantExit || !strings.Contains(out, tc.wantIn) {
				t.Fatalf("exit = %d; want %d with %q: %s", exit, tc.wantExit, tc.wantIn, out)
			}
			if took := time.Since(start); took > 20*time.Second {
				t.Errorf("the driver took %s over it, as though waiting the screen out", took)
			}
			calls, _ := os.ReadFile(filepath.Join(stubDir, "calls"))
			answered := false
			for line := range strings.SplitSeq(string(calls), "\n") {
				if strings.HasPrefix(line, "send-keys") && strings.HasSuffix(line, " 2") {
					answered = true
				}
			}
			if answered != tc.wantAnswer {
				t.Errorf("answered with 2 = %v; tmux calls:\n%s", answered, calls)
			}
			if !tc.wantAnswer && strings.Contains(string(calls), "paste-buffer") {
				t.Errorf("the prompt was pasted into a screen that needs a person:\n%s", calls)
			}
			if l := lastTurnLine(t, home, "codex"); l.exit != tc.wantExit || !strings.Contains(l.reason, tc.reason) {
				t.Errorf("turn log line = %+v; want exit %d and a reason holding %q", l, tc.wantExit, tc.reason)
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

// TestCodexTurnReportsAProviderFailure: codex writes task_complete for a turn the provider
// refused as well as for one the model finished, with the refusal under `error`. A driver that
// only counts the event exits 0 with an empty reply, and a fleet harness then reads a throttled
// member as one that stopped. The first three events are what codex 0.152.1 wrote against a stub
// provider; the last carries the message a provider sent a live campaign.
func TestCodexTurnReportsAProviderFailure(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct{ name, errJSON, wantLine string }{
		{"rate limit inside the stream",
			`{"message":"rate limit exceeded: Rate limit reached for gpt-5 on tokens per min (TPM): Limit 500000, Used 498000. Please try again in 12.52s.","codex_error_info":"rate_limit_exceeded"}`,
			"failure class=throttled retry_after=12.52"},
		{"HTTP 429",
			`{"message":"exceeded retry limit, last status: 429 Too Many Requests","codex_error_info":{"response_too_many_failed_attempts":{"http_status_code":429}}}`,
			"failure class=throttled retry_after=-"},
		{"HTTP 503",
			`{"message":"unexpected status 503 Service Unavailable: upstream down, url: http://x/v1/responses","codex_error_info":"other"}`,
			"failure class=capacity retry_after=-"},
		{"model at capacity",
			`{"message":"Selected model is at capacity. Please try a different model.","codex_error_info":"other"}`,
			"failure class=capacity retry_after=-"},
		// Measured live: every call dropped for five minutes, and this is what codex recorded.
		{"no provider answered",
			`{"message":"stream disconnected before completion: error sending request for url (http://cs-lender:2500/responses)","codex_error_info":"other"}`,
			"failure class=unreachable retry_after=-"},
		// The lender's refusal under a hard token budget, in the shape codex 0.152.1 recorded it:
		// the whole body as the message, and no status, so the marker is all there is.
		{"token budget spent",
			`{"message":"{\"error\":{\"type\":\"budget_exhausted\",\"source\":\"cs-sandbox lend\",\"message\":\"cs-sandbox token budget spent: sandbox web has used 1.00G of its 1.00G cached-input tokens. The operator can raise it on the host.\"}}\n","codex_error_info":"other"}`,
			"failure class=spent retry_after=-"},
		{"a malformed request",
			`{"message":"{\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Unsupported value: 'reasoning.effort' does not support 'max' with this model.\"}}\n","codex_error_info":"other"}`,
			"failure class=other retry_after=-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-codex-remote")
			sess := filepath.Join(home, ".cs-codex", "sessions")
			if err := os.MkdirAll(sess, 0o700); err != nil {
				t.Fatal(err)
			}
			fresh := filepath.Join(sess, "rollout-2026-01-02-"+codexFailedTurnID+".jsonl")
			stubDir := t.TempDir()
			writeStub(t, bin, "codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "cs-codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) [ -f "$STUB_DIR/.launched" ] && exit 0 || exit 1 ;;
  new-session)
    touch "$STUB_DIR/.launched"
    printf '%s\n' "$SESSION_META" > "$FRESH_ROLLOUT"
    exit 0 ;;
  capture-pane) printf '  gpt-5 default · /work\n\n'; exit 0 ;;
  send-keys)
    if [ -f "$STUB_DIR/.launched" ] && [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' "$TASK_COMPLETE" >> "$FRESH_ROLLOUT"
    fi
    exit 0 ;;
esac
exit 0
`)
			out, exit := runScriptStdin(t, home, bin,
				[]string{
					"STUB_DIR=" + stubDir,
					"FRESH_ROLLOUT=" + fresh,
					`SESSION_META={"type":"session_meta","payload":{"id":"` + codexFailedTurnID + `"}}`,
					`TASK_COMPLETE={"type":"event_msg","payload":{"type":"task_complete","last_agent_message":null,"error":` + tc.errJSON + `}}`,
					"CS_CODEX_STALL_SECS=0",
				},
				"do the thing\n", "cs-codex-turn", "--tmux", "codextoken", "--timeout", "30")
			if exit != 5 {
				t.Fatalf("exit = %d; want 5 (turn failed): %s", exit, out)
			}
			if !strings.Contains(out, tc.wantLine) {
				t.Fatalf("output missing %q: %s", tc.wantLine, out)
			}
			// The TUI outlives a failed turn, so the caller still needs the id to resume it.
			if !strings.Contains(out, "__CS_CODEX_SESSION_ID__ "+codexFailedTurnID) {
				t.Fatalf("a failed turn dropped the session id: %s", out)
			}
			// The same class and wait are left on this machine, for a caller that was not the one
			// that started the turn.
			wantTurnLogAgrees(t, home, "codex", tc.wantLine)
			covemit.Prove(t, "turn-driver-semantics", "codex", "", "scripts")
		})
	}
}

const codexFailedTurnID = "33333333-3333-4333-8333-333333333333"

// TestCodexTurnCutsShortARejectedCredential: codex keeps one retry count for every failure, so
// it retries a 401 as it would a dropped stream, doubling its wait each time. A key that can
// never work then holds the turn for minutes, and nothing is written to the rollout while it
// does. The pane is the only place that says why, so the driver reads it, stops codex, and
// reports the refusal at once. A reconnect for any other reason is waited out: that is what
// the retries are for.
func TestCodexTurnCutsShortARejectedCredential(t *testing.T) {
	skipUnlessLinux(t)
	for _, tc := range []struct {
		name, reason string
		wantExit     int
		wantIn       string
	}{
		{"401", "Unexpected status 401 Unauthorized: Incorrect API key provided, url: http://cs-lender:2500/responses",
			5, "failure class=unauthorized retry_after=-"},
		{"403", "Unexpected status 403 Forbidden: this key may not use the model, url: http://x", 5, "failure class=unauthorized"},
		{"an outage is waited out", "Unexpected status 503 Service Unavailable: upstream down, url: http://x", 2, "did not complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-codex-remote")
			sess := filepath.Join(home, ".cs-codex", "sessions")
			if err := os.MkdirAll(sess, 0o700); err != nil {
				t.Fatal(err)
			}
			fresh := filepath.Join(sess, "rollout-2026-01-02-"+codexFailedTurnID+".jsonl")
			stubDir := t.TempDir()
			writeStub(t, bin, "codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "cs-codex", "#!/bin/sh\nexit 0\n")
			writeStub(t, bin, "tmux", `#!/bin/sh
case "$1" in
  has-session) [ -f "$STUB_DIR/.launched" ] && exit 0 || exit 1 ;;
  new-session)
    touch "$STUB_DIR/.launched"
    printf '%s\n' "$SESSION_META" > "$FRESH_ROLLOUT"
    exit 0 ;;
  capture-pane)
    if [ -f "$STUB_DIR/.submitted" ]; then
      printf '  > do the thing\n* Reconnecting... 3/10 (12s * esc to interrupt)\n  - %s\n' "$REASON"
    fi
    printf '  gpt-5 default · /work\n\n'
    exit 0 ;;
  send-keys)
    echo "$*" >> "$STUB_DIR/keys"
    [ -f "$STUB_DIR/.launched" ] && touch "$STUB_DIR/.submitted"
    exit 0 ;;
esac
exit 0
`)
			out, exit := runScriptStdin(t, home, bin,
				[]string{
					"STUB_DIR=" + stubDir, "FRESH_ROLLOUT=" + fresh, "REASON=" + tc.reason,
					`SESSION_META={"type":"session_meta","payload":{"id":"` + codexFailedTurnID + `"}}`,
					"CS_CODEX_STALL_SECS=0",
				},
				"do the thing\n", "cs-codex-turn", "--tmux", "codextoken", "--timeout", "4")
			if exit != tc.wantExit || !strings.Contains(out, tc.wantIn) {
				t.Fatalf("exit = %d; want %d with %q: %s", exit, tc.wantExit, tc.wantIn, out)
			}
			keys, _ := os.ReadFile(filepath.Join(stubDir, "keys"))
			if stopped := strings.Contains(string(keys), "Escape"); stopped != (tc.wantExit == 5) {
				t.Errorf("codex stopped = %v; keys sent:\n%s", stopped, keys)
			}
			// codex is still running, so the caller needs the id to send the next turn to it.
			if tc.wantExit == 5 && !strings.Contains(out, "__CS_CODEX_SESSION_ID__ "+codexFailedTurnID) {
				t.Errorf("a refused turn dropped the session id: %s", out)
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

// openCodeStatefulTmux is a tmux whose session exists only once new-session has run, with a
// server window from then on and a TUI window once new-window has run, logging every call.
const openCodeStatefulTmux = `#!/bin/sh
echo "tmux $*" >> "$STUB_DIR/invocations"
case "$1" in
  has-session) [ -f "$STUB_DIR/.server_up" ] || exit 1 ;;
  new-session) touch "$STUB_DIR/.server_up" ;;
  new-window) touch "$STUB_DIR/.tui_up" ;;
  list-windows)
    [ -f "$STUB_DIR/.server_up" ] && echo server
    [ -f "$STUB_DIR/.tui_up" ] && echo tui ;;
  list-sessions) exit 0 ;;
esac
exit 0
`

// TestOpenCodeTurnStartsOneServerForANewSession: a new session is created on the server the
// driver starts in its tmux session, and the TUI a human attaches to is opened on it.
//
// One server per token: a second one started beside it, and killed again, is what strands a
// server on the port when a turn dies in between. It then answers every health check while no
// window shows it, so the next turn attaches to it and runs in the directory it was started in.
//
// The contract is stated as what the driver ran: one tmux session whose window runs `serve` on
// loopback, no `serve` anywhere else, the session created for the caller's directory, the TUI
// opened on that session, and the prompt and the server password in no argv.
func TestOpenCodeTurnStartsOneServerForANewSession(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	stubDir := t.TempDir()
	installOpenCodeTurnStubs(t, bin)
	writeStub(t, bin, "tmux", openCodeStatefulTmux)
	// The port reads as free before the server is started and as served after: a health probe
	// that always succeeded would look like a squatter to reclaim_port.
	writeStub(t, bin, "curl", openCodeAPIStub(`[ -f "$STUB_DIR/.server_up" ] || exit 7; echo '{"version":"2.2.5"}'; exit 0`))
	writeStub(t, bin, "ss", "#!/bin/sh\nexit 0\n")
	writeSnapshots(t, stubDir, ocPage(""), ocPage("",
		ocMsg("msg_r", "assistant", 2, ocText("stub-response")), ocMsg("msg_i", "idle", 3, `"outcome":"succeeded"`)))

	// No --uuid: this is the new-session path. The workdir has to exist, because a session is
	// bound to it.
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

	for line := range strings.SplitSeq(calls, "\n") {
		if strings.HasPrefix(line, "cs-opencode ") && strings.Contains(line, " serve") {
			t.Errorf("the driver started a server outside its tmux session, which is what strands one:\n  %s", line)
		}
	}
	if got := strings.Count(calls, "tmux new-session"); got != 1 {
		t.Errorf("tmux new-session called %d times; want exactly one", got)
	}
	if !strings.Contains(calls, "cs-opencode serve --hostname 127.0.0.1 --port ") {
		t.Errorf("the tmux session does not run the server on loopback:\n%s", calls)
	}
	// Created on the server's own API, bound to the directory the caller named rather than to
	// whatever the server's cwd happened to be.
	body, _ := os.ReadFile(filepath.Join(stubDir, "body"))
	if !strings.Contains(string(body), `"directory": "`+workdir+`"`) {
		t.Errorf("the session was not created for the caller's workdir: %s", body)
	}
	// Opened on the new session, so the warm TUI shows the driven turn.
	if !strings.Contains(calls, "-n tui") || !strings.Contains(calls, "-s "+openCodeTestSessionID) {
		t.Errorf("the TUI was never opened on the new session:\n%s", calls)
	}
	// The prompt reached run on its stdin, and neither it nor the password is in an argv.
	if prompt, _ := os.ReadFile(filepath.Join(stubDir, "prompt")); string(prompt) != "do the thing\n" {
		t.Errorf("run got %q on stdin, want the prompt", prompt)
	}
	pw, err := os.ReadFile(filepath.Join(home, ".cs-opencode", "server-password"))
	if err != nil || len(pw) < 32 {
		t.Fatalf("no server password was made: %q, %v", pw, err)
	}
	for _, secret := range []string{string(pw), "do the thing"} {
		if strings.Contains(calls, secret) {
			t.Errorf("%q reached an argv:\n%s", secret, calls)
		}
	}
	for _, f := range []string{"server-password", "curlrc"} {
		if m := mode(t, filepath.Join(home, ".cs-opencode", f)); m.Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", f, m.Perm())
		}
	}
	covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
}

// TestOpenCodeTurnReclaimsAStrandedServer: a server holding the derived port with no tmux
// session behind it is taken back, rather than driven.
//
// The port is a hash of the session token, so anything listening on it belongs to this
// session. One that no tmux session stands behind is a turn that died before it could clean
// up. Without this the driver attaches to it and the turn runs against a server nobody can
// see, or its own server cannot bind; and nothing else clears it, because a restart kills a
// tmux session and this process is not in one.
//
// The squatter is a real process, because `kill` is a shell builtin and a stub on PATH would
// never run. The test asserts the driver signalled the pid that ss named, by watching that
// process exit. It does not answer the profile's password, as a server left by an older
// password or an older opencode would not, and it is reclaimed all the same.
func TestOpenCodeTurnReclaimsAStrandedServer(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	stubDir := t.TempDir()
	installOpenCodeTurnStubs(t, bin)

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

	writeStub(t, bin, "tmux", openCodeStatefulTmux)
	// ss names the squatter while it lives, and reports nothing once it is gone.
	writeStub(t, bin, "ss", fmt.Sprintf(`#!/bin/sh
kill -0 %d 2>/dev/null || exit 0
echo 'LISTEN 0 512 127.0.0.1:21453 0.0.0.0:* users:(("opencode",pid=%d,fd=22))'
`, pid, pid))
	writeStub(t, bin, "curl", openCodeAPIStub(`[ -f "$STUB_DIR/.server_up" ] || exit 22; echo '{"version":"2.2.5"}'; exit 0`))
	writeSnapshots(t, stubDir, ocPage(""), ocPage("",
		ocMsg("msg_r", "assistant", 2, ocText("stub-response")), ocMsg("msg_i", "idle", 3, `"outcome":"succeeded"`)))

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
// every prompt on that session rather than at launch. The caller is not always in a position
// to know: cs-campaign asks every turn to run in `/workspace`, which a Firecracker member does
// not have.
func TestOpenCodeTurnFallsBackFromAMissingWorkdir(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	stubDir := t.TempDir()
	installOpenCodeTurnStubs(t, bin)
	writeStub(t, bin, "tmux", openCodeStatefulTmux)
	writeStub(t, bin, "curl", openCodeAPIStub(`[ -f "$STUB_DIR/.server_up" ] || exit 7; echo '{"version":"2.2.5"}'; exit 0`))
	writeStub(t, bin, "ss", "#!/bin/sh\nexit 0\n")
	writeSnapshots(t, stubDir, ocPage(""), ocPage("",
		ocMsg("msg_r", "assistant", 2, ocText("stub-response")), ocMsg("msg_i", "idle", 3, `"outcome":"succeeded"`)))

	out, exit := runScriptStdin(t, home, bin, []string{"STUB_DIR=" + stubDir}, "do the thing\n",
		"cs-opencode-turn", "--tmux", "stubtoken", "--workdir", "/workspace")
	if exit != 0 {
		t.Fatalf("a missing workdir is a fallback, not a failure; exit = %d: %s", exit, out)
	}
	if !strings.Contains(out, "does not exist") {
		t.Errorf("the fallback is never silent, because the turn then runs somewhere else:\n%s", out)
	}
	// The session is bound to $HOME, the directory the warning names, and never to the one
	// that is not there: opencode accepts that one and then fails every prompt on it.
	body, _ := os.ReadFile(filepath.Join(stubDir, "body"))
	if strings.Contains(string(body), "/workspace") || !strings.Contains(string(body), `"directory": "`+home+`"`) {
		t.Errorf("the session was not bound to $HOME: %s", body)
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
	for _, tc := range []struct{ tool, prefix, id string }{
		{"cs-opencode-remote", ".cs-opencode-remote", openCodeTestSessionID},
		{"cs-codex-remote", ".cs-codex-remote", "01998c4a-0000-7000-8000-000000000000"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			home, bin := agentHome(t, tc.prefix)
			trace := filepath.Join(home, "trace")
			contended := filepath.Join(home, "contended")
			// ssh stands in for the remote driver. A driven turn (the call carrying --tmux)
			// records its argv, holds until the next turn has found the lock taken, and then
			// emits the sentinel the caller learns the id from.
			//
			// Held on that event rather than for a fixed time, because a fixed time is a bet
			// on how fast a loaded runner starts a process. The bet was a 300ms head start and
			// a one-second hold, and a macOS runner under -race lost it: the second turn
			// started before the first had written the session's token, and failed outright.
			// The hold still gives up after 10s, so a turn that never waits is reported below
			// rather than hanging the test.
			writeStub(t, bin, "ssh", `#!/bin/sh
case "$*" in
  *--tmux*)
    echo "$@" >> `+trace+`
    cat >/dev/null
    i=0
    while [ ! -f `+contended+` ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i + 1)); done
    echo '__CS_OPENCODE_SESSION_ID__ `+tc.id+`'
    echo '__CS_CODEX_SESSION_ID__ `+tc.id+`'
    ;;
  *) exit 0 ;;
esac
`)
			// The lock is a directory, so a turn that finds it taken is a mkdir that failed.
			writeStub(t, bin, "mkdir", `#!/bin/sh
/bin/mkdir "$@" && exit 0
rc=$?
case "$*" in *.lock) : >> `+contended+` ;; esac
exit $rc
`)
			writeStub(t, bin, "uuidgen", "#!/bin/sh\necho 11111111-2222-3333-4444-555555555555\n")
			// The remote tool now sends the driver that sits beside it, so there is one to
			// send here, where there used to be none in the test's empty home.
			writeStub(t, bin, "scp", "#!/bin/sh\nexit 0\n")

			first := make(chan string, 1)
			go func() {
				out, _ := runScript(t, home, bin, tc.tool, "--new", "--name", "s1", "first turn")
				first <- out
			}()
			// The second turn starts once the first is inside its driven call, so the first
			// holds the lock and has written the session's token. The second then blocks
			// where a campaign's next dispatch would.
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(trace); err == nil {
					break
				}
				select {
				case out := <-first:
					t.Fatalf("the first turn ended without driving a turn:\n%s", out)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the first turn did not drive a turn within 10s")
				}
			}
			if out, exit := runScript(t, home, bin, tc.tool, "--resume", "s1", "second turn"); exit != 0 {
				t.Fatalf("second turn: exit %d: %s", exit, out)
			}
			<-first
			if _, err := os.Stat(contended); err != nil {
				t.Errorf("the second turn never found the first holding the lock, so the turns " +
					"did not overlap and nothing here was tested")
			}

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
