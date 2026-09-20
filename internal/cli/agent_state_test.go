package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/covemit"
)

// stateTmux is a tmux that holds the sessions a test names, each with the pane a test wrote
// for it, and records every call so a test can say what was NOT done to them.
const stateTmux = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
target=""; prev=""
for a in "$@"; do [ "$prev" = "-t" ] && target="$a"; prev="$a"; done
case "$1" in
  ls) for s in $SESSIONS; do echo "$s"; done; exit 0 ;;
  has-session) for s in $SESSIONS; do [ "$s" = "$target" ] && exit 0; done; exit 1 ;;
  capture-pane) cat "$STUB_DIR/pane-$target" 2>/dev/null; exit 0 ;;
esac
exit 0
`

// runState runs one driver's --state against the named sessions and their panes.
func runState(t *testing.T, script string, panes map[string]string, env []string, args ...string) (string, string) {
	t.Helper()
	home, bin := agentHome(t, ".cs-state")
	writeStub(t, bin, "tmux", stateTmux)
	stubDir := t.TempDir()
	var names []string
	for name, pane := range panes {
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(stubDir, "pane-"+name), []byte(pane), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env = append(env, "STUB_DIR="+stubDir, "SESSIONS="+strings.Join(names, " "))
	out, exit := runScriptStdin(t, home, bin, env, "", script, append([]string{"--state"}, args...)...)
	if exit != 0 {
		t.Fatalf("%s --state exit = %d; it answers with a word and always exits 0: %s", script, exit, out)
	}
	calls, _ := os.ReadFile(filepath.Join(stubDir, "calls"))
	return strings.TrimSpace(out), string(calls)
}

// TestTurnDriversSayWhetherAnAgentIsInATurn: a harness needs to know whether a member is
// working, and the process table cannot say. An agent can start a turn of its own, when a
// background command finishes, and no turn driver wraps that turn. The drivers already judge
// working from idle off the screen, so --state is that same judgement, asked by anyone, of
// any turn. The panes are the ones each CLI prints, and one case per state is the whole
// contract: a caller acts on the word.
func TestTurnDriversSayWhetherAnAgentIsInATurn(t *testing.T) {
	skipUnlessLinux(t)
	const codexReady = "  gpt-5 default · /work\n"
	const claudeReady = "  bypass permissions on (shift+tab to cycle)\n"
	for _, tc := range []struct {
		name, script, session, pane, want string
	}{
		{"codex in a turn nobody is driving", "cs-codex-turn", "cs-codex-a", "• Working (12s • esc to interrupt)\n" + codexReady, "busy"},
		{"codex waiting out a provider error", "cs-codex-turn", "cs-codex-a",
			"• Reconnecting... 3/10 (12s • esc to interrupt)\n  └ stream closed\n" + codexReady, "busy retrying"},
		{"codex idle", "cs-codex-turn", "cs-codex-a", "› Ask Codex to do anything\n" + codexReady, "idle"},
		{"codex at the trust prompt", "cs-codex-turn", "cs-codex-a", "Do you trust the contents of this directory?\n", "blocked"},
		{"codex at sign-in", "cs-codex-turn", "cs-codex-a", "Welcome to Codex\n  Sign in with ChatGPT\n", "blocked"},
		{"codex on a screen nobody has seen", "cs-codex-turn", "cs-codex-a", "something new\n", "unknown"},

		{"claude in a turn nobody is driving", "cs-claude-turn", "cs-claude-a", "✻ Cooking… (41s · esc to interrupt)\n" + claudeReady, "busy"},
		// No "esc to interrupt" on this one, which is how it was once read as idle.
		{"claude waiting out a provider error", "cs-claude-turn", "cs-claude-a",
			"✻ 429 rate limited · Retrying in 14s · attempt 3/10\n" + claudeReady, "busy retrying"},
		{"claude idle", "cs-claude-turn", "cs-claude-a", "❯ \n" + claudeReady, "idle"},
		{"claude at sign-in", "cs-claude-turn", "cs-claude-a", "Login expired · Please run /login\n", "blocked"},
		{"claude at a tool approval", "cs-claude-turn", "cs-claude-a", "Do you want to proceed?\n" + claudeReady, "blocked"},
		{"claude on a screen nobody has seen", "cs-claude-turn", "cs-claude-a", "something new\n", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, calls := runState(t, tc.script, map[string]string{tc.session: tc.pane}, nil)
			if got != tc.want {
				t.Errorf("state = %q; want %q", got, tc.want)
			}
			// Asking must never be what starts or disturbs a session.
			for _, never := range []string{"new-session", "send-keys", "kill-session", "paste-buffer"} {
				if strings.Contains(calls, never) {
					t.Errorf("--state ran tmux %s:\n%s", never, calls)
				}
			}
			covemit.Prove(t, "turn-driver-semantics", strings.TrimSuffix(strings.TrimPrefix(tc.script, "cs-"), "-turn"), "", "scripts")
		})
	}
}

// TestStateAnswersForEverySessionOfAFamily: the caller this exists for probes a sandbox
// knowing only the agent's family. The session names live on the machine that started the
// turns. So with no handle the answer covers every session of the family, and it is the one
// a caller must respect most: one busy session among idle ones is a busy member.
func TestStateAnswersForEverySessionOfAFamily(t *testing.T) {
	skipUnlessLinux(t)
	idle := "  gpt-5 default · /work\n"
	busy := "• Working (3s • esc to interrupt)\n" + idle
	for _, tc := range []struct {
		name  string
		panes map[string]string
		args  []string
		want  string
	}{
		{"no session at all", map[string]string{}, nil, "absent"},
		{"only another family's session", map[string]string{"cs-claude-x": busy}, nil, "absent"},
		{"all idle", map[string]string{"cs-codex-a": idle, "cs-codex-b": idle}, nil, "idle"},
		{"one busy among idle", map[string]string{"cs-codex-a": idle, "cs-codex-b": busy}, nil, "busy"},
		{"blocked outranks idle", map[string]string{"cs-codex-a": idle, "cs-codex-b": "Do you trust the contents of this directory?\n"}, nil, "blocked"},
		{"a handle narrows it to one", map[string]string{"cs-codex-a": idle, "cs-codex-b": busy}, []string{"--tmux", "a"}, "idle"},
		{"a handle nothing answers to", map[string]string{"cs-codex-a": idle}, []string{"--tmux", "gone"}, "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := runState(t, "cs-codex-turn", tc.panes, nil, tc.args...); got != tc.want {
				t.Errorf("state = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestOpenCodeStateAsksTheServer: opencode's driver never reads a screen. It asks the TUI's
// own server, where a session listed under /session/status is in a turn. --state asks the
// same two questions, so a TUI whose server does not answer is unknown rather than idle.
func TestOpenCodeStateAsksTheServer(t *testing.T) {
	skipUnlessLinux(t)
	const other = "ses_0000000000000000000000other"
	for _, tc := range []struct {
		name, health, status string
		args                 []string
		want                 string
	}{
		{"no turn listed", "ok", `{}`, nil, "idle"},
		{"a turn listed", "ok", `{"` + openCodeTestSessionID + `":{"type":"busy"}}`, nil, "busy"},
		{"the server does not answer", "down", `{}`, nil, "unknown"},
		{"asked about one session, and another is busy", "ok", `{"` + other + `":{"type":"busy"}}`,
			[]string{"--tmux", "a", "--uuid", openCodeTestSessionID}, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-state")
			writeStub(t, bin, "tmux", stateTmux)
			writeStub(t, bin, "curl", `#!/bin/sh
for a in "$@"; do url="$a"; done
case "$url" in
  */global/health) [ "$HEALTH" = ok ] && exit 0 || exit 7 ;;
  */session/status) [ "$HEALTH" = ok ] || exit 7; printf '%s' "$STATUS"; exit 0 ;;
esac
exit 22
`)
			stubDir := t.TempDir()
			out, exit := runScriptStdin(t, home, bin,
				[]string{"STUB_DIR=" + stubDir, "SESSIONS=cs-opencode-a", "HEALTH=" + tc.health, "STATUS=" + tc.status},
				"", "cs-opencode-turn", append([]string{"--state"}, tc.args...)...)
			if got := strings.TrimSpace(out); exit != 0 || got != tc.want {
				t.Errorf("state = %q, exit %d; want %q, exit 0", got, exit, tc.want)
			}
			covemit.Prove(t, "turn-driver-semantics", "opencode", "", "scripts")
		})
	}
}
