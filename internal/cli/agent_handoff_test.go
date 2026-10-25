package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The context hook asks an agent to hand off when its context crosses a line, and the turn
// driver reports a turn that ended that way as a hand-off. The line, the file and the ask
// come from the environment; nothing here knows what the handoff holds.

const handoffClaudeUUID = "3f9c2a10-7b1e-4d2c-9a5e-0c1d2e3f4a5b"
const handoffCodexUUID = "019a1b2c-3d4e-7f80-91a2-b3c4d5e6f7a8"

// handoffHome is agentHome with the family's own turn driver on PATH, which is where the
// hook takes its reading from, and a record over or under the line.
func handoffHome(t *testing.T, family string, tokens int) (home, bin string) {
	t.Helper()
	skipUnlessLinux(t)
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("the hook and the drivers read records with python3, which is not on this host")
	}
	home, bin = agentHome(t, ".cs-"+family)
	driver, err := filepath.Abs(agentTool("cs-" + family + "-turn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driver, filepath.Join(bin, "cs-"+family+"-turn")); err != nil {
		t.Fatal(err)
	}
	hook, err := filepath.Abs(agentTool("cs-context-hook"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hook, filepath.Join(bin, "cs-context-hook")); err != nil {
		t.Fatal(err)
	}
	writeHandoffRecord(t, home, family, tokens)
	return home, bin
}

func writeHandoffRecord(t *testing.T, home, family string, tokens int) {
	t.Helper()
	var path, body string
	switch family {
	case "claude":
		path = filepath.Join(home, ".cs-claude", "projects", "-work", handoffClaudeUUID+".jsonl")
		body = fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":%d,"output_tokens":1}}}`+"\n", tokens-3)
	case "codex":
		path = filepath.Join(home, ".cs-codex", "sessions", "2026", "10", "20", "rollout-2026-10-20T10-00-00-"+handoffCodexUUID+".jsonl")
		body = fmt.Sprintf(`{"timestamp":"2026-10-20T10:00:01.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"output_tokens":1},"model_context_window":258400}}}`+"\n", tokens-1)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func handoffSession(family string) string {
	if family == "claude" {
		return handoffClaudeUUID
	}
	return handoffCodexUUID
}

// runHook feeds one hook event to cs-context-hook and returns what it printed.
func runHook(t *testing.T, home, bin, family string, env []string, event map[string]any) string {
	t.Helper()
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	out, exit := runScriptStdin(t, home, bin, env, string(b)+"\n", "cs-context-hook", family)
	if exit != 0 {
		t.Fatalf("cs-context-hook %s exited %d; a hook never fails the tool call: %s", family, exit, out)
	}
	return strings.TrimSpace(out)
}

func postTool(family string) map[string]any {
	return map[string]any{"hook_event_name": "PostToolUse", "session_id": handoffSession(family),
		"tool_name": "Bash", "tool_response": "ok"}
}

// handoffEnv sets the line at 60000 tokens and the handoff file under the fake home.
func handoffEnv(home string) []string {
	return []string{"CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE=" + filepath.Join(home, "work", "handoff.md")}
}

// TestTheContextHookIsOffUnlessALineIsSet: a member with no line set runs exactly as before.
// The hook prints nothing and leaves nothing behind, and --context reads as it did.
func TestTheContextHookIsOffUnlessALineIsSet(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 90_000)
			for _, env := range [][]string{nil, {"CS_HANDOFF_TOKENS="}, {"CS_HANDOFF_TOKENS=0"}} {
				if out := runHook(t, home, bin, family, env, postTool(family)); out != "" {
					t.Errorf("env %v: the hook printed %q with no line set", env, out)
				}
			}
			if _, err := os.Stat(filepath.Join(home, ".cs-turns")); err == nil {
				t.Errorf("the hook wrote under ~/.cs-turns with no line set")
			}
			out, _ := runScriptStdin(t, home, bin, nil, "", "cs-"+family+"-turn", "--context")
			if strings.Contains(out, "handoff=") {
				t.Errorf("--context names a hand-off line when none is set: %s", out)
			}
		})
	}
}

// TestTheContextHookAsksOverTheLineThenEndsTheTurn: under the line nothing is said. Over it
// the ask rides on every tool result until the handoff file is newer than the first ask,
// then the agent is told once more to end its turn, and after two more tool calls the hook
// ends the turn itself. The first ask is recorded for the driver.
func TestTheContextHookAsksOverTheLineThenEndsTheTurn(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 50_000)
			env := handoffEnv(home)
			if out := runHook(t, home, bin, family, env, postTool(family)); out != "" {
				t.Fatalf("under the line, the hook said %q", out)
			}
			if _, err := os.Stat(filepath.Join(home, ".cs-turns", "handoff-ask")); err == nil {
				t.Fatalf("under the line, the hook recorded an ask")
			}
			writeHandoffRecord(t, home, family, 61_000)
			for i := range 2 {
				out := runHook(t, home, bin, family, env, postTool(family))
				if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "over the hand-off line of 60000") ||
					!strings.Contains(out, "end your turn") {
					t.Fatalf("call %d over the line: the hook said %q, not the ask", i+1, out)
				}
				if !strings.Contains(out, `"hookEventName": "PostToolUse"`) {
					t.Fatalf("the ask is not addressed to the post-tool event: %s", out)
				}
			}
			ask, err := os.ReadFile(filepath.Join(home, ".cs-turns", "handoff-ask"))
			if err != nil {
				t.Fatalf("the hook asked and recorded no ask: %v", err)
			}
			var rec struct {
				Sid    string  `json:"sid"`
				Tokens int     `json:"tokens"`
				H      int     `json:"h"`
				At     float64 `json:"at"`
			}
			if err := json.Unmarshal(ask, &rec); err != nil || rec.Sid != handoffSession(family) || rec.H != 60_000 || rec.Tokens < 61_000 || rec.At == 0 {
				t.Fatalf("the ask record reads %s (%v)", ask, err)
			}
			// The agent writes its handoff after the ask.
			time.Sleep(20 * time.Millisecond)
			handoff := filepath.Join(home, "work", "handoff.md")
			if err := os.MkdirAll(filepath.Dir(handoff), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(handoff, []byte("done: a, b; next: c\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				out := runHook(t, home, bin, family, env, postTool(family))
				if !strings.Contains(out, "Your handoff is written. End your turn now.") {
					t.Fatalf("call %d after the handoff: the hook said %q, not the nudge", i+1, out)
				}
			}
			out := runHook(t, home, bin, family, env, postTool(family))
			if !strings.Contains(out, `"continue": false`) {
				t.Fatalf("after two more tool calls the hook said %q; it ends the turn itself", out)
			}
		})
	}
}

// TestTheContextHookAsksForAHandoffAgainInALaterTurn: a harness can keep a session that
// handed off, as when its work is not committed yet, and start another turn in it. The driver
// counts only an ask made in its own turn, so a later turn still over the line is asked again
// and records a new ask. Rewriting the handoff then nudges and stops, as in the first turn.
// Within a turn, the driven file is older than the ask and changes nothing.
func TestTheContextHookAsksForAHandoffAgainInALaterTurn(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 50_000)
			session := "cs-" + family + "-driven"
			// The hook runs in a pane of the driven session.
			writeStub(t, bin, "tmux", "#!/bin/sh\n[ \"$1\" = display-message ] && echo "+session+"\nexit 0\n")
			env := append(handoffEnv(home), "TMUX_PANE=%7")
			wantAsk := func(when string) {
				t.Helper()
				out := runHook(t, home, bin, family, env, postTool(family))
				if !strings.Contains(out, "over the hand-off line of 60000") || strings.Contains(out, "handoff is written") ||
					strings.Contains(out, `"continue": false`) {
					t.Fatalf("%s: the hook said %q, not the ask", when, out)
				}
			}
			wantNudgeThenStop := func(when string) {
				t.Helper()
				for i := range 2 {
					if out := runHook(t, home, bin, family, env, postTool(family)); !strings.Contains(out, "Your handoff is written. End your turn now.") {
						t.Fatalf("%s, call %d: the hook said %q, not the nudge", when, i+1, out)
					}
				}
				if out := runHook(t, home, bin, family, env, postTool(family)); !strings.Contains(out, `"continue": false`) {
					t.Fatalf("%s, after two more tool calls: the hook said %q; it ends the turn itself", when, out)
				}
			}

			startDrivenTurn(t, home, session, time.Now().Add(-time.Minute))
			if out := runHook(t, home, bin, family, env, postTool(family)); out != "" {
				t.Fatalf("under the line, the hook said %q", out)
			}
			writeHandoffRecord(t, home, family, 61_000)
			wantAsk("the first turn over the line")
			first := readHandoffAsk(t, home)
			writeHandoffFile(t, home, time.Now())
			wantNudgeThenStop("the first turn after the handoff")

			// The harness keeps the session and starts another turn in it, still over the line.
			started := startDrivenTurn(t, home, session, time.Now())
			for i := range 2 {
				wantAsk(fmt.Sprintf("call %d of the later turn", i+1))
			}
			again := readHandoffAsk(t, home)
			if again.Session != session || again.At <= first.At || again.At <= float64(started.UnixNano())/1e9 {
				t.Fatalf("the later turn's ask record is %+v, after %+v in a turn started at %s; want a new ask in this turn",
					again, first, started.Format(time.RFC3339Nano))
			}
			writeHandoffFile(t, home, time.Now())
			wantNudgeThenStop("the later turn after the handoff is rewritten")
		})
	}
}

// startDrivenTurn plays a turn driver starting a turn: it rewrites ~/.cs-turns/driven to name
// session, dated at, and returns the date the file holds.
func startDrivenTurn(t *testing.T, home, session string, at time.Time) time.Time {
	t.Helper()
	driven := filepath.Join(home, ".cs-turns", "driven")
	if err := os.MkdirAll(filepath.Dir(driven), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driven, []byte(session+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(driven, at, at); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(driven)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// writeHandoffFile plays the agent writing the handoff file handoffEnv names, dated at.
func writeHandoffFile(t *testing.T, home string, at time.Time) {
	t.Helper()
	handoff := filepath.Join(home, "work", "handoff.md")
	if err := os.MkdirAll(filepath.Dir(handoff), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff, []byte("done: a, b; next: c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(handoff, at, at); err != nil {
		t.Fatal(err)
	}
}

// handoffAsk is the record the hook leaves in ~/.cs-turns/handoff-ask for the driver.
type handoffAsk struct {
	Session string  `json:"session"`
	Sid     string  `json:"sid"`
	Tokens  int     `json:"tokens"`
	At      float64 `json:"at"`
}

func readHandoffAsk(t *testing.T, home string) handoffAsk {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".cs-turns", "handoff-ask"))
	if err != nil {
		t.Fatalf("the hook recorded no ask: %v", err)
	}
	var rec handoffAsk
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the ask record reads %s: %v", b, err)
	}
	return rec
}

// TestTheContextHookAsksInTheCallersWords: the ask is the caller's sentence when it gives
// one, with the numbers filled in. The hook holds no words about what a handoff is for.
func TestTheContextHookAsksInTheCallersWords(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 50_000)
			env := append(handoffEnv(home), "CS_HANDOFF_ASK=Over {h} at {tokens}: hand off as section 4 says.")
			if out := runHook(t, home, bin, family, env, postTool(family)); out != "" {
				t.Fatalf("under the line, the hook said %q", out)
			}
			writeHandoffRecord(t, home, family, 61_000)
			if out := runHook(t, home, bin, family, env, postTool(family)); !strings.Contains(out, "Over 60000 at 61") || !strings.Contains(out, "section 4") {
				t.Fatalf("the custom ask was not used: %s", out)
			}
		})
	}
}

// TestTheContextHookNeverAsksASessionThatStartsOverTheLine: a line below what a fresh
// session costs would hand off forever. A session whose first reading is already over the
// line is never asked, and --context says so with floor=<tokens>.
func TestTheContextHookNeverAsksASessionThatStartsOverTheLine(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 70_000)
			env := handoffEnv(home)
			for i := range 3 {
				if out := runHook(t, home, bin, family, env, postTool(family)); out != "" {
					t.Fatalf("call %d: a session that started over the line was asked: %s", i+1, out)
				}
			}
			if _, err := os.Stat(filepath.Join(home, ".cs-turns", "handoff-ask")); err == nil {
				t.Fatalf("an ask was recorded for a session that started over the line")
			}
			out, _ := runScriptStdin(t, home, bin, env, "", "cs-"+family+"-turn", "--context", "--uuid", handoffSession(family))
			if !strings.Contains(out, " handoff=60000 floor=70") {
				t.Fatalf("--context = %q; want the line and the floor reading", strings.TrimSpace(out))
			}
			// Another session's floor is not this one's.
			out, _ = runScriptStdin(t, home, bin, env, "", "cs-"+family+"-turn", "--context", "--uuid", "00000000-0000-0000-0000-000000000000")
			if strings.Contains(out, "floor=70") {
				t.Fatalf("--context for another session shows this session's floor: %q", strings.TrimSpace(out))
			}
		})
	}
}

// TestTheContextHookSkipsSubAgentsAndOtherSessions: a sub-agent's tool calls carry an
// agent_id and are never answered, and a CLI session the agent started itself is not the
// one the driver drives, so it is never asked to hand off over the agent's file.
func TestTheContextHookSkipsSubAgentsAndOtherSessions(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 50_000)
			env := handoffEnv(home)
			// The session's first reading is under the line; the next ones are over it.
			if out := runHook(t, home, bin, family, env, postTool(family)); out != "" {
				t.Fatalf("under the line, the hook said %q", out)
			}
			writeHandoffRecord(t, home, family, 70_000)
			sub := postTool(family)
			sub["agent_id"] = "a1b2c3"
			if out := runHook(t, home, bin, family, env, sub); out != "" {
				t.Fatalf("a sub-agent was asked: %s", out)
			}
			turns := filepath.Join(home, ".cs-turns")
			if err := os.MkdirAll(turns, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(turns, "driven"), []byte("cs-"+family+"-driven\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			writeStub(t, bin, "tmux", "#!/bin/sh\necho cs-"+family+"-nested\n")
			nested := append(append([]string(nil), env...), "TMUX_PANE=%7")
			if out := runHook(t, home, bin, family, nested, postTool(family)); out != "" {
				t.Fatalf("a session other than the driven one was asked: %s", out)
			}
			writeStub(t, bin, "tmux", "#!/bin/sh\necho cs-"+family+"-driven\n")
			if out := runHook(t, home, bin, family, nested, postTool(family)); !strings.Contains(out, "additionalContext") {
				t.Fatalf("the driven session was not asked: %q", out)
			}
		})
	}
}

// TestTheContextHookPointsAtTheHandoffAfterACompaction: compaction is the safety net behind
// the hand-off. A session that starts again after one is told to read the handoff file, if
// there is one, before carrying on. Any other session start is left alone.
func TestTheContextHookPointsAtTheHandoffAfterACompaction(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		t.Run(family, func(t *testing.T) {
			home, bin := handoffHome(t, family, 1_000)
			env := handoffEnv(home)
			start := map[string]any{"hook_event_name": "SessionStart", "session_id": handoffSession(family), "source": "compact"}
			out := runHook(t, home, bin, family, env, start)
			if !strings.Contains(out, "just compacted") || !strings.Contains(out, filepath.Join(home, "work", "handoff.md")) ||
				!strings.Contains(out, `"hookEventName": "SessionStart"`) {
				t.Fatalf("after a compaction the hook said %q", out)
			}
			start["source"] = "startup"
			if out := runHook(t, home, bin, family, env, start); out != "" {
				t.Fatalf("a plain session start was answered: %s", out)
			}
		})
	}
}

// handoffTmux plays tmux and, on Enter, the agent: a first tool call under the line, then
// one over it, each run through the real hook the way the CLI would, then the handoff when
// the test says the agent obeyed, and the marker that ends the turn. Each turn appends to the
// record, so a second turn with the same STUB_DIR is a later turn of a session kept alive.
// The session ~/.cs-turns/driven names, cs-<family>-outer, is alive only when
// STUB_DRIVEN_ALIVE is 1.
const handoffTmux = `#!/bin/sh
case "$1" in
  new-session) : > "$STUB_DIR/alive" ;;
  has-session)
    case "$3" in
      *-outer) [ "$STUB_DRIVEN_ALIVE" = 1 ] || exit 1 ;;
      *) [ -f "$STUB_DIR/alive" ] || exit 1 ;;
    esac ;;
  capture-pane) printf '%s\n' '❯ ' '  bypass permissions on (shift+tab to cycle)' ;;
  display-message) echo "${STUB_PANE_SESSION:-cs-claude-$STUB_UUID}" ;;
  send-keys)
    case "$*" in
      *Enter*)
        mkdir -p "$STUB_PROJECTS"
        rec="$STUB_PROJECTS/$STUB_UUID.jsonl"
        event="{\"hook_event_name\":\"PostToolUse\",\"session_id\":\"$STUB_UUID\",\"tool_name\":\"Bash\",\"tool_response\":\"ok\"}"
        printf '%s\n' '{"type":"assistant","message":{"model":"m","usage":{"input_tokens":1,"cache_read_input_tokens":1000,"output_tokens":1}}}' >> "$rec"
        if [ "$STUB_AGENT" = obeys ]; then printf '%s' "$event" | cs-context-hook claude > "$STUB_DIR/hook1.out"; fi
        printf '%s\n' '{"type":"assistant","message":{"model":"m","usage":{"input_tokens":1,"cache_read_input_tokens":70000,"output_tokens":1}}}' >> "$rec"
        if [ "$STUB_AGENT" = obeys ]; then
          printf '%s' "$event" | cs-context-hook claude > "$STUB_DIR/hook2.out"
          mkdir -p "$(dirname "$CS_HANDOFF_FILE")" && echo "done: a; next: b" > "$CS_HANDOFF_FILE"
        fi
        printf '%s\n' '{"type":"system","subtype":"turn_duration"}' >> "$rec" ;;
    esac ;;
esac
exit 0
`

// runHandoffTurn drives one Claude turn to its end with the stubbed tmux and agent.
func runHandoffTurn(t *testing.T, agent string, env ...string) (home string, out string, exit int) {
	t.Helper()
	home, bin := handoffHome(t, "claude", 1_000)
	out, exit = driveHandoffTurn(t, home, bin, t.TempDir(), agent, env...)
	return home, out, exit
}

// driveHandoffTurn drives one Claude turn in home with the stubbed tmux and agent. The stub
// directory holds the session the stubbed tmux keeps, so a second call with the same stub
// drives a later turn of that session.
func driveHandoffTurn(t *testing.T, home, bin, stub, agent string, env ...string) (out string, exit int) {
	t.Helper()
	writeStub(t, bin, "tmux", handoffTmux)
	writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
	projects := filepath.Join(home, ".cs-claude", "projects")
	const uuid = "11111111-2222-3333-4444-555555555555"
	env = append(append([]string(nil), env...), "STUB_DIR="+stub, "STUB_PROJECTS="+filepath.Join(projects, "-work"),
		"STUB_UUID="+uuid, "STUB_AGENT="+agent)
	return runScriptStdin(t, home, bin, env, "do the thing\n",
		"cs-claude-turn", "--uuid", uuid, "--projects", projects, "--timeout", "5")
}

// TestATurnThatHandedOffIsReportedAsOne: the driver reports a turn that ended because the
// agent handed off, as the hook asked it to in that session and that turn, with class
// handoff on an exit-0 line, and says so on stderr. A turn that ends any other way, even
// with a handoff file on disk and an older ask, is the plain turn it was.
func TestATurnThatHandedOffIsReportedAsOne(t *testing.T) {
	t.Run("the agent hands off when asked", func(t *testing.T) {
		scratch := t.TempDir()
		handoff := filepath.Join(scratch, "handoff.md")
		home, out, exit := runHandoffTurn(t, "obeys", "CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE="+handoff)
		if exit != 0 {
			t.Fatalf("exit=%d: a hand-off is a turn that ended well: %s", exit, out)
		}
		if !strings.Contains(out, "cs-claude-turn: handoff at 70") || !strings.Contains(out, "over the line of 60000") {
			t.Fatalf("the driver did not say the turn was a hand-off: %s", out)
		}
		l := lastTurnLine(t, home, "claude")
		if l.exit != 0 || l.class != "handoff" || l.retry != "-" || !strings.Contains(l.reason, "handoff at") {
			t.Fatalf("turn log line %+v; want exit 0, class handoff", l)
		}
		if b, _ := os.ReadFile(filepath.Join(home, ".cs-turns", "driven")); strings.TrimSpace(string(b)) != "cs-claude-11111111-2222-3333-4444-555555555555" {
			t.Fatalf("the driver did not name the session it drove: %q", b)
		}
	})
	t.Run("a handoff file with no ask in this turn is not a hand-off", func(t *testing.T) {
		scratch := t.TempDir()
		handoff := filepath.Join(scratch, "handoff.md")
		if err := os.WriteFile(handoff, []byte("from an earlier turn\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		home, _, exit := runHandoffTurn(t, "ignores", "CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE="+handoff)
		if exit != 0 {
			t.Fatalf("exit=%d", exit)
		}
		if l := lastTurnLine(t, home, "claude"); l.class != "-" {
			t.Fatalf("turn log line %+v; want a plain turn", l)
		}
	})
	t.Run("an ask from an earlier turn does not count", func(t *testing.T) {
		scratch := t.TempDir()
		handoff := filepath.Join(scratch, "handoff.md")
		home, bin := handoffHome(t, "claude", 1_000)
		turns := filepath.Join(home, ".cs-turns")
		if err := os.MkdirAll(turns, 0o700); err != nil {
			t.Fatal(err)
		}
		stale := fmt.Sprintf(`{"session":"cs-claude-11111111-2222-3333-4444-555555555555","sid":"x","tokens":70000,"h":60000,"at":%d}`, time.Now().Unix()-600)
		if err := os.WriteFile(filepath.Join(turns, "handoff-ask"), []byte(stale), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(handoff, []byte("written after that ask\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, exit := driveHandoffTurn(t, home, bin, t.TempDir(), "ignores", "CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE="+handoff)
		if exit != 0 {
			t.Fatalf("exit=%d", exit)
		}
		if l := lastTurnLine(t, home, "claude"); l.class != "-" {
			t.Fatalf("turn log line %+v; an ask from before this turn was read as this turn's", l)
		}
	})
}

// TestALaterTurnThatRewritesItsHandoffIsAHandOff: a harness can keep a session that handed
// off and start another turn in it, as when its work was not committed. That turn is still
// over the line, so the hook asks again and records the ask in this turn. An agent that then
// rewrites its handoff ends the later turn as a hand-off too.
func TestALaterTurnThatRewritesItsHandoffIsAHandOff(t *testing.T) {
	home, bin := handoffHome(t, "claude", 1_000)
	stub := t.TempDir()
	handoff := filepath.Join(t.TempDir(), "handoff.md")
	// The harness runs the driver from outside the session, so a pane this test run inherited
	// is not where it runs. A pane in the driven session would make the second driver nested.
	env := noLineage("CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE="+handoff)
	for turn := 1; turn <= 2; turn++ {
		if turn == 2 {
			// The driver dates a turn's start in whole seconds. A later turn the harness
			// starts begins in a later second than the first turn's ask.
			time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
		}
		out, exit := driveHandoffTurn(t, home, bin, stub, "obeys", env...)
		if exit != 0 || !strings.Contains(out, "cs-claude-turn: handoff at 70") {
			t.Fatalf("turn %d: exit=%d; the agent was asked and handed off: %s", turn, exit, out)
		}
		if said, _ := os.ReadFile(filepath.Join(stub, "hook2.out")); !strings.Contains(string(said), "over the hand-off line of 60000") {
			t.Fatalf("turn %d: over the line the hook said %q, not the ask", turn, said)
		}
	}
	lines := readTurnLog(t, home, "claude")
	if len(lines) != 2 || lines[0].class != "handoff" || lines[1].class != "handoff" || lines[1].exit != 0 {
		t.Fatalf("turn log %+v; want two exit-0 turns of class handoff", lines)
	}
}

// runEndedOpenCodeTurn drives one OpenCode turn whose last message carries the error errJSON,
// as the stubbed API reports it, with the hook's ask on disk for this driver's session and
// this turn. handoff is "fresh" (written after the ask), "stale" (written before it) or
// "none". No floor is recorded, so the session started under the line.
func runEndedOpenCodeTurn(t *testing.T, handoff, errJSON string) (home, out string, exit int) {
	t.Helper()
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-opencode-remote")
	installOpenCodeTurnStubs(t, bin)
	writeStub(t, bin, "tmux", nestedTmux)
	stub := t.TempDir()
	writeSnapshots(t, stub, "[]", `[{"info":{"role":"user","time":{"created":1}}},`+
		`{"info":{"role":"assistant","time":{"created":2},"error":`+errJSON+`},"parts":[{"type":"text","text":"handing off"}]}]`)
	file := filepath.Join(stub, "handoff.md")
	turns := filepath.Join(home, ".cs-turns")
	if err := os.MkdirAll(turns, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix() + 300
	ask := fmt.Sprintf(`{"session":"cs-opencode-stubtoken","sid":%q,"tokens":70000,"h":60000,"at":%d}`, openCodeTestSessionID, at)
	if err := os.WriteFile(filepath.Join(turns, "handoff-ask"), []byte(ask), 0o600); err != nil {
		t.Fatal(err)
	}
	if handoff != "none" {
		if err := os.WriteFile(file, []byte("done: a; next: b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(at+300, 0)
		if handoff == "stale" {
			when = time.Unix(at-300, 0)
		}
		if err := os.Chtimes(file, when, when); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE=" + file, "TMUX_PANE=%7",
		"STUB_DIR=" + stub, "STUB_PANE_SESSION=operator"}
	out, exit = runScriptStdin(t, home, bin, env, "do the thing\n", "cs-opencode-turn",
		"--tmux", "stubtoken", "--uuid", openCodeTestSessionID)
	return home, out, exit
}

// TestAnOpenCodeTurnTheHookAbortedIsAHandOff: after the handoff is written, the OpenCode hook
// ends a turn whose agent keeps making tool calls by aborting the session, and opencode then
// records MessageAbortedError on the turn. With a fresh handoff after this turn's ask, that is
// a hand-off the hook ended: exit 0, class handoff, the turn's text and session id printed. An
// abort with no handoff, or one older than the ask, and any other error, stays a failed turn.
func TestAnOpenCodeTurnTheHookAbortedIsAHandOff(t *testing.T) {
	const aborted = `{"name":"MessageAbortedError","data":{"message":"Aborted"}}`
	t.Run("an abort after a fresh handoff", func(t *testing.T) {
		home, out, exit := runEndedOpenCodeTurn(t, "fresh", aborted)
		if exit != 0 {
			t.Fatalf("exit=%d: the hook ended a turn that had handed off: %s", exit, out)
		}
		if !strings.Contains(out, "cs-opencode-turn: handoff at 70000 tokens, over the line of 60000; the hook ended the turn") {
			t.Fatalf("the driver did not say the hook ended a hand-off: %s", out)
		}
		if !strings.Contains(out, "handing off") || !strings.Contains(out, "__CS_OPENCODE_SESSION_ID__ "+openCodeTestSessionID) {
			t.Fatalf("the turn's text or session id is missing: %s", out)
		}
		if strings.Contains(out, "turn failed") {
			t.Fatalf("a hand-off was reported as a failure: %s", out)
		}
		l := lastTurnLine(t, home, "opencode")
		if l.exit != 0 || l.class != "handoff" || l.retry != "-" || !strings.Contains(l.reason, "the hook ended the turn") {
			t.Fatalf("turn log line %+v; want exit 0, class handoff", l)
		}
	})
	for _, tc := range []struct{ name, handoff, err, class string }{
		{"an abort with no handoff", "none", aborted, "other"},
		{"an abort with a handoff older than the ask", "stale", aborted, "other"},
		{"a provider error after a fresh handoff", "fresh",
			`{"name":"APIError","data":{"message":"Too Many Requests","statusCode":429}}`, "throttled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, out, exit := runEndedOpenCodeTurn(t, tc.handoff, tc.err)
			if exit != 5 {
				t.Fatalf("exit=%d; want 5, a failed turn: %s", exit, out)
			}
			if strings.Contains(out, "handoff at") {
				t.Fatalf("a failed turn was reported as a hand-off: %s", out)
			}
			l := lastTurnLine(t, home, "opencode")
			if l.exit != 5 || l.class != tc.class || !strings.Contains(l.reason, "session postcheck") {
				t.Fatalf("turn log line %+v; want exit 5, class %s", l, tc.class)
			}
		})
	}
}

// nestedTmux plays tmux for the codex and opencode drivers: a session that is already up, a
// ready screen, a pane in the session STUB_PANE_SESSION names, and, on the codex submit, the
// turn's answer. The session ~/.cs-turns/driven names, cs-<family>-outer, is alive only when
// STUB_DRIVEN_ALIVE is 1.
const nestedTmux = `#!/bin/sh
case "$1" in
  has-session)
    case "$3" in
      *-outer) [ "$STUB_DRIVEN_ALIVE" = 1 ] || exit 1 ;;
    esac ;;
  display-message) echo "$STUB_PANE_SESSION" ;;
  capture-pane) echo "gpt-5 default · /work" ;;
  send-keys)
    if [ -n "$STUB_ROLLOUT" ] && [ ! -f "$STUB_DIR/.submitted" ]; then
      touch "$STUB_DIR/.submitted"
      printf '%s\n' '{"payload":{"type":"agent_message","message":"done"}}' >> "$STUB_ROLLOUT"
      printf '%s\n' '{"payload":{"type":"task_complete"}}' >> "$STUB_ROLLOUT"
    fi ;;
esac
exit 0
`

// runNestedTurn drives one turn of the family's driver to its end under a line, with
// ~/.cs-turns/driven naming cs-<family>-outer, the session the harness drives. where is the
// environment that says where the driver was started from: TMUX_PANE with the session tmux
// puts it in (STUB_PANE_SESSION), or CS_PARENT_TMUX_SESSION, as a remote tool passes it over
// ssh, and whether the outer session is alive (STUB_DRIVEN_ALIVE=1). An ask in this driver's
// own session, dated after the turn starts, and a newer handoff file are on disk, so a driver
// that keeps its line reports a hand-off. It returns the session this driver drives.
func runNestedTurn(t *testing.T, family string, where ...string) (home, own, out string, exit int) {
	t.Helper()
	// OpenCode's turn reads its session over the stubbed API, so it needs no record on disk.
	var bin string
	if family == "opencode" {
		skipUnlessLinux(t)
		home, bin = agentHome(t, ".cs-opencode-remote")
	} else {
		home, bin = handoffHome(t, family, 1_000)
	}
	stub := t.TempDir()
	handoff := filepath.Join(stub, "handoff.md")
	env := append([]string{"CS_HANDOFF_TOKENS=60000", "CS_HANDOFF_FILE=" + handoff, "STUB_DIR=" + stub}, where...)
	var args []string
	switch family {
	case "claude":
		const uuid = "11111111-2222-3333-4444-555555555555"
		own = "cs-claude-" + uuid
		projects := filepath.Join(home, ".cs-claude", "projects")
		writeStub(t, bin, "tmux", handoffTmux)
		writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
		env = append(env, "STUB_PROJECTS="+filepath.Join(projects, "-work"), "STUB_UUID="+uuid, "STUB_AGENT=ignores")
		args = []string{"--uuid", uuid, "--projects", projects, "--timeout", "5"}
	case "codex":
		own = "cs-codex-codextoken"
		writeStub(t, bin, "tmux", nestedTmux)
		writeStub(t, bin, "codex", "#!/bin/sh\nexit 0\n")
		writeStub(t, bin, "cs-codex", "#!/bin/sh\nexit 0\n")
		rollout := filepath.Join(home, ".cs-codex", "sessions", "2026", "10", "20", "rollout-2026-10-20T10-00-00-"+handoffCodexUUID+".jsonl")
		env = append(env, "STUB_ROLLOUT="+rollout, "CS_CODEX_STALL_SECS=0")
		args = []string{"--tmux", "codextoken", "--uuid", handoffCodexUUID, "--timeout", "30"}
	case "opencode":
		own = "cs-opencode-stubtoken"
		installOpenCodeTurnStubs(t, bin)
		writeStub(t, bin, "tmux", nestedTmux)
		writeSnapshots(t, stub, "[]", `[{"info":{"role":"user","time":{"created":1}}},`+
			`{"info":{"role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"done"}]}]`)
		args = []string{"--tmux", "stubtoken", "--uuid", openCodeTestSessionID}
	}
	turns := filepath.Join(home, ".cs-turns")
	if err := os.MkdirAll(turns, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(turns, "driven"), []byte("cs-"+family+"-outer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix() + 300
	ask := fmt.Sprintf(`{"session":%q,"sid":"x","tokens":70000,"h":60000,"at":%d}`, own, at)
	if err := os.WriteFile(filepath.Join(turns, "handoff-ask"), []byte(ask), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff, []byte("done: a; next: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Unix(at+300, 0)
	if err := os.Chtimes(handoff, later, later); err != nil {
		t.Fatal(err)
	}
	out, exit = runScriptStdin(t, home, bin, env, "do the thing\n", "cs-"+family+"-turn", args...)
	return home, own, out, exit
}

// TestADriverStartedInsideTheDrivenSessionHasNoLine: an agent can start a driver of its own
// from inside the session the harness drives, and that driver inherits the line. Kept, it
// would take ~/.cs-turns/driven, and the hook would then skip the harness's session. So it
// runs with no line: driven still names the outer session, and its turn is never a hand-off.
func TestADriverStartedInsideTheDrivenSessionHasNoLine(t *testing.T) {
	for _, family := range []string{"claude", "codex", "opencode"} {
		t.Run(family, func(t *testing.T) {
			home, _, out, exit := runNestedTurn(t, family, "TMUX_PANE=%7", "STUB_PANE_SESSION=cs-"+family+"-outer")
			wantNestedTurn(t, family, home, out, exit)
		})
	}
}

// TestADriverStartedOverSSHFromTheDrivenSessionHasNoLine: an agent's remote tool starts its
// driver through ssh to this same machine, so the driver has no TMUX_PANE. The tool passes
// the session it was run from as CS_PARENT_TMUX_SESSION, and a driver whose parent is the
// driven session is nested just as one started in its pane is.
func TestADriverStartedOverSSHFromTheDrivenSessionHasNoLine(t *testing.T) {
	for _, family := range []string{"claude", "codex", "opencode"} {
		t.Run(family, func(t *testing.T) {
			home, _, out, exit := runNestedTurn(t, family, "CS_PARENT_TMUX_SESSION=cs-"+family+"-outer")
			wantNestedTurn(t, family, home, out, exit)
		})
	}
}

// TestADriverThatFindsTheDrivenSessionAliveHasNoLine: any wrapper breaks the lineage, such as
// a tmux session the agent starts for a batch, so a driver can arrive with no pane and no
// parent that names the driven session. The harness ends a session before it starts the next,
// so a driven session still alive under another name means this driver is nested.
func TestADriverThatFindsTheDrivenSessionAliveHasNoLine(t *testing.T) {
	for _, family := range []string{"claude", "codex", "opencode"} {
		t.Run(family, func(t *testing.T) {
			home, _, out, exit := runNestedTurn(t, family, noLineage("STUB_DRIVEN_ALIVE=1")...)
			wantNestedTurn(t, family, home, out, exit)
		})
	}
}

// TestADeadDrivenSessionIsTakenOver: a driven file that names a session no longer alive is
// what a fresh start finds, after a hand-off or a restart. The driver takes driven and keeps
// its line.
func TestADeadDrivenSessionIsTakenOver(t *testing.T) {
	for _, family := range []string{"claude", "codex", "opencode"} {
		t.Run(family, func(t *testing.T) {
			home, own, out, exit := runNestedTurn(t, family, noLineage()...)
			wantKeptLine(t, family, home, own, out, exit)
		})
	}
}

// noLineage is an environment with no pane and no parent session, whatever the test run
// itself inherited, and extra on top.
func noLineage(extra ...string) []string {
	return append([]string{"TMUX_PANE=", "CS_PARENT_TMUX_SESSION="}, extra...)
}

// wantNestedTurn holds a nested driver to its contract: driven still names the outer session,
// the driver says it runs with no line, its turn is no hand-off, and its turn-log line is in
// <family>-nested.log. The harness reads the last lines of <family>.log as the driven
// session's, so a nested turn's line there could be read as the end of the driven turn.
func wantNestedTurn(t *testing.T, family, home, out string, exit int) {
	t.Helper()
	if exit != 0 {
		t.Fatalf("exit=%d: %s", exit, out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".cs-turns", "driven")); strings.TrimSpace(string(b)) != "cs-"+family+"-outer" {
		t.Fatalf("a nested driver took the driven session: %q", b)
	}
	if !strings.Contains(out, "cs-"+family+"-turn: started inside the driven session cs-"+family+"-outer: this one has no hand-off line") {
		t.Fatalf("the driver did not say it runs with no line: %s", out)
	}
	if strings.Contains(out, "handoff at") {
		t.Fatalf("a nested driver reported a hand-off: %s", out)
	}
	if l := lastTurnLine(t, home, family+"-nested"); l.exit != 0 || l.class != "-" {
		t.Fatalf("nested turn log line %+v; want a plain exit-0 turn", l)
	}
	if lines := readTurnLog(t, home, family); len(lines) != 0 {
		t.Fatalf("a nested driver wrote to %s.log, which the harness reads as the driven session's: %+v", family, lines)
	}
}

// TestATmuxPaneInAnotherSessionKeepsTheLine: TMUX_PANE leaks into environments that are not
// inside the driven session, such as a test run from an operator's own tmux. A pane in any
// other session is not nesting, so the driver names its own session and keeps its line.
func TestATmuxPaneInAnotherSessionKeepsTheLine(t *testing.T) {
	for _, family := range []string{"claude", "codex", "opencode"} {
		t.Run(family, func(t *testing.T) {
			home, own, out, exit := runNestedTurn(t, family, "TMUX_PANE=%7", "STUB_PANE_SESSION=operator")
			wantKeptLine(t, family, home, own, out, exit)
		})
	}
}

// wantKeptLine holds a driver that is not nested to its contract: it names its own session in
// driven, says nothing of nesting, and keeps its line, so its turn is the hand-off on disk.
func wantKeptLine(t *testing.T, family, home, own, out string, exit int) {
	t.Helper()
	if exit != 0 {
		t.Fatalf("exit=%d: %s", exit, out)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".cs-turns", "driven")); strings.TrimSpace(string(b)) != own {
		t.Fatalf("driven names %q; want the driver's own session %s", b, own)
	}
	if strings.Contains(out, "started inside the driven session") {
		t.Fatalf("a driver that is not nested was read as nesting: %s", out)
	}
	if l := lastTurnLine(t, home, family); l.exit != 0 || l.class != "handoff" {
		t.Fatalf("turn log line %+v; the line was not kept", l)
	}
}

// TestARemoteToolNamesItsTmuxSessionToTheDriver: a remote tool starts its driver through ssh,
// even to this same machine, and the driver then has no TMUX_PANE to tell it where it was
// started. Run from a pane, the tool puts that pane's session in front of the remote command
// as CS_PARENT_TMUX_SESSION, quoted so the driver gets the name whole. Run from no pane, as
// the harness runs it, the command is what it always was.
func TestARemoteToolNamesItsTmuxSessionToTheDriver(t *testing.T) {
	// A quote and a space in the name hold the quoting: the remote shell must hand the
	// driver this exact string.
	const pane = "it's outer"
	for _, fam := range remoteFamilies {
		for _, inPane := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/in a pane %v", fam.agent, inPane), func(t *testing.T) {
				home, bin := agentHome(t, fam.prefix)
				sent := filepath.Join(home, "sent")
				// ssh records the remote command of the turn, the one call that carries
				// --format, and drops the rest.
				writeStub(t, bin, "ssh", "#!/bin/sh\ncase \"$*\" in *--format*) printf '%s' \"$2\" > "+sent+"; cat >/dev/null ;; esac\nexit 0\n")
				writeStub(t, bin, "scp", "#!/bin/sh\nexit 0\n")
				writeStub(t, bin, "tmux", "#!/bin/sh\n[ \"$1\" = display-message ] && echo \""+pane+"\"\nexit 0\n")
				name := "parent-contract"
				if err := os.WriteFile(filepath.Join(home, fam.prefix+"-sessions", name+fam.mapSuffix),
					[]byte(fam.mapValue+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				where := "TMUX_PANE="
				if inPane {
					where = "TMUX_PANE=%7"
				}
				if out, exit := runScriptStdin(t, home, bin, []string{where}, "", "cs-"+fam.agent+"-remote",
					"--resume", name, "-H", "host", "hello"); exit != 0 {
					t.Fatalf("exit %d: %s", exit, out)
				}
				b, err := os.ReadFile(sent)
				if err != nil {
					t.Fatalf("the turn never reached ssh: %v", err)
				}
				cmd := string(b)
				if !inPane {
					if strings.Contains(cmd, "CS_PARENT_TMUX_SESSION") {
						t.Fatalf("a tool run from no pane named a parent session: %s", cmd)
					}
					return
				}
				if !strings.HasPrefix(cmd, "CS_PARENT_TMUX_SESSION=") {
					t.Fatalf("the remote command does not start with the parent session: %s", cmd)
				}
				// Run the command as the remote shell would, with a driver that prints what
				// it was given.
				remote := t.TempDir()
				writeStub(t, remote, "cs-"+fam.agent+"-turn", "#!/bin/sh\nprintf '%s' \"$CS_PARENT_TMUX_SESSION\"\n")
				sh := exec.Command("/bin/sh", "-c", cmd)
				sh.Env = []string{"PATH=" + remote + ":/usr/bin:/bin"}
				got, err := sh.Output()
				if err != nil {
					t.Fatalf("the remote command does not run: %v: %s", err, cmd)
				}
				if string(got) != pane {
					t.Fatalf("the driver was given %q; want %q from: %s", got, pane, cmd)
				}
			})
		}
	}
}

// The tests below hold the hook's reading on Claude Code, which writes a model call to its
// record only once the call's whole response has ended. A post-tool hook often runs before
// that, so the record's newest call is the one before the call that made this tool call.
// Their records have the shape Claude Code 2.1.283 writes: one record per content block, each
// with the call's message id and its whole usage, and each tool's result in a user record
// after the block that asked for it. Codex and OpenCode take their readings another way.

// dataFile is a tool result shaped like the recorded session's data files, 400 numbered lines
// of nine short words. Claude Code counted one of those, 28,480 characters, at about 12,400
// tokens.
func dataFile() string {
	words := strings.Fields("amber basalt cedar delta ember fjord granite harbor iris juniper kestrel lagoon " +
		"meadow nimbus orchid pebble quartz raven sable tundra umber violet willow xenon yarrow zephyr")
	var b strings.Builder
	for n := 1; n <= 400; n++ {
		fmt.Fprintf(&b, "line %03d:", n)
		for i := range 9 {
			b.WriteString(" " + words[(n*7+i*11)%len(words)])
		}
		b.WriteString("\n")
	}
	return b.String()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// claudeBlock is one tool call of a model call whose usage totals total, as its own record.
func claudeBlock(t *testing.T, msgID string, total int, toolID, command string) string {
	return mustJSON(t, map[string]any{"type": "assistant", "isSidechain": false, "message": map[string]any{
		"id": msgID, "model": "claude-opus-5-5", "stop_reason": "tool_use",
		"usage": map[string]any{"input_tokens": 2, "cache_creation_input_tokens": 0,
			"cache_read_input_tokens": total - 136, "output_tokens": 134},
		"content": []any{map[string]any{"type": "tool_use", "id": toolID, "name": "Bash",
			"input": map[string]any{"command": command}}}}})
}

func bashResponse(stdout string) map[string]any {
	return map[string]any{"stdout": stdout, "stderr": "", "interrupted": false, "isImage": false, "noOutputExpected": false}
}

// claudeResult is the user record that carries one tool's result.
func claudeResult(t *testing.T, toolID, text string) string {
	return mustJSON(t, map[string]any{"type": "user", "isSidechain": false, "message": map[string]any{
		"role": "user", "content": []any{map[string]any{"tool_use_id": toolID, "type": "tool_result",
			"content": text, "is_error": false}}}, "toolUseResult": bashResponse(text)})
}

// claudeRecordPath is where the driver and the hook find the test session's record.
func claudeRecordPath(home string) string {
	return filepath.Join(home, ".cs-claude", "projects", "-work", handoffClaudeUUID+".jsonl")
}

func writeClaudeRecord(t *testing.T, home string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(claudeRecordPath(home), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// claudeBash is the post-tool event Claude Code sends for one Bash call.
func claudeBash(home, toolID, command string, response map[string]any) map[string]any {
	return map[string]any{"hook_event_name": "PostToolUse", "session_id": handoffClaudeUUID,
		"transcript_path": claudeRecordPath(home), "tool_name": "Bash", "tool_use_id": toolID,
		"tool_input": map[string]any{"command": command}, "tool_response": response}
}

// askedAt reads the figure of the first ask the hook recorded, or fails.
func askedAt(t *testing.T, home string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".cs-turns", "handoff-ask"))
	if err != nil {
		t.Fatalf("the hook recorded no ask: %v", err)
	}
	var rec struct {
		Tokens int `json:"tokens"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the ask record reads %s: %v", b, err)
	}
	return rec.Tokens
}

// TestTheClaudeHookCountsTheCallItsRecordDoesNotHoldYet: a recorded session at a line of
// 62,000, reading one data file per call. The call that read f05 carried 45,658 tokens. The next call appended f05 to the
// answers and read f06, and carried 57,981; the call after that carried 70,127 of input. Read
// as the record's newest call plus f06 alone, at f06 the hook stood at 52,805 and asked one
// file late. Whether or not the record has caught up when the hook runs, the reading at f06
// is the context the next call carries, within the estimate's error.
func TestTheClaudeHookCountsTheCallItsRecordDoesNotHoldYet(t *testing.T) {
	const nextCallInput = 70_127
	file := dataFile()
	for _, caughtUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("record caught up %v", caughtUp), func(t *testing.T) {
			home, bin := handoffHome(t, "claude", 1_000)
			env := []string{"CS_HANDOFF_TOKENS=62000", "CS_HANDOFF_FILE=" + filepath.Join(home, "work", "handoff.md")}
			read := []string{
				claudeBlock(t, "msg_f05", 45_658, "toolu_f04_append", `echo "f04 KEY04KESTREL" >> ~/probe/work/answers.txt`),
				claudeResult(t, "toolu_f04_append", ""),
				claudeBlock(t, "msg_f05", 45_658, "toolu_f05_read", "cat ~/probe/data/f05.txt"),
				claudeResult(t, "toolu_f05_read", file),
			}
			// The append runs while the call still streams its next block, so the record
			// never holds that call at the append's hook. This is the session's first
			// reading, and it is under the line.
			writeClaudeRecord(t, home, read...)
			appendCmd := `echo "f05 KEY05YARROW" >> ~/probe/work/answers.txt`
			if out := runHook(t, home, bin, "claude", env, claudeBash(home, "toolu_f05_append", appendCmd, bashResponse(""))); out != "" {
				t.Fatalf("at the append, under the line, the hook said %q", out)
			}
			if caughtUp {
				writeClaudeRecord(t, home, append(read,
					claudeBlock(t, "msg_f06", 57_981, "toolu_f05_append", appendCmd),
					claudeResult(t, "toolu_f05_append", ""),
					claudeBlock(t, "msg_f06", 57_981, "toolu_f06_read", "cat ~/probe/data/f06.txt"))...)
			}
			out := runHook(t, home, bin, "claude", env, claudeBash(home, "toolu_f06_read", "cat ~/probe/data/f06.txt", bashResponse(file)))
			if !strings.Contains(out, "over the hand-off line of 62000") {
				t.Fatalf("at f06 the hook said %q; the next call carries %d, over the line", out, nextCallInput)
			}
			if got := askedAt(t, home); got < nextCallInput-2_000 || got > nextCallInput+2_000 {
				t.Fatalf("the hook asked at %d; the next call carries %d", got, nextCallInput)
			}
		})
	}
}

// TestTheClaudeHookCountsEarlierCallsOfTheSameResponse: one response reads two files, and
// Claude Code runs the second read while neither the call nor the first file is in the
// record. The second read's reading counts the first file, as the next call will.
func TestTheClaudeHookCountsEarlierCallsOfTheSameResponse(t *testing.T) {
	home, bin := handoffHome(t, "claude", 1_000)
	env := []string{"CS_HANDOFF_TOKENS=50000", "CS_HANDOFF_FILE=" + filepath.Join(home, "work", "handoff.md")}
	file := dataFile()
	writeClaudeRecord(t, home,
		claudeBlock(t, "msg_task", 33_384, "toolu_task", "cat ~/probe/task.md"),
		claudeResult(t, "toolu_task", strings.Repeat("Read each file and record its word. ", 50)))
	if out := runHook(t, home, bin, "claude", env, claudeBash(home, "toolu_f04", "cat ~/probe/data/f04.txt", bashResponse(file))); out != "" {
		t.Fatalf("at the first read, under the line, the hook said %q", out)
	}
	out := runHook(t, home, bin, "claude", env, claudeBash(home, "toolu_f05", "cat ~/probe/data/f05.txt", bashResponse(file)))
	if !strings.Contains(out, "over the hand-off line of 50000") {
		t.Fatalf("at the second read the hook said %q; both files are in the next call", out)
	}
	if got := askedAt(t, home); got < 33_384+2*11_000 {
		t.Fatalf("the hook asked at %d, which leaves out one of the two files", got)
	}
}

// TestTheClaudeHookCountsOnlyThePreviewOfASavedOutput: Claude Code saves a Bash output over
// its limit to a file and shows the model a preview of the first 2 KB, while the hook's event
// still carries the whole output. The reading counts what the model sees.
func TestTheClaudeHookCountsOnlyThePreviewOfASavedOutput(t *testing.T) {
	home, bin := handoffHome(t, "claude", 1_000)
	env := []string{"CS_HANDOFF_TOKENS=55000", "CS_HANDOFF_FILE=" + filepath.Join(home, "work", "handoff.md")}
	writeClaudeRecord(t, home, claudeBlock(t, "msg_ls", 50_000, "toolu_ls", "ls"), claudeResult(t, "toolu_ls", "f01.txt"))
	if out := runHook(t, home, bin, "claude", env, claudeBash(home, "toolu_pwd", "pwd", bashResponse("/work\n"))); out != "" {
		t.Fatalf("at the first call, under the line, the hook said %q", out)
	}
	saved := bashResponse(strings.Repeat("     1\tline 001: nimbus delta xenon tundra raven\n", 600))
	saved["persistedOutputPath"] = filepath.Join(filepath.Dir(claudeRecordPath(home)), handoffClaudeUUID, "tool-results", "b1.txt")
	saved["persistedOutputSize"] = 30_911
	if out := runHook(t, home, bin, "claude", env, claudeBash(home, "toolu_catn", "cat -n f01.txt", saved)); out != "" {
		t.Fatalf("a saved output was counted whole: the hook said %q", out)
	}
}
