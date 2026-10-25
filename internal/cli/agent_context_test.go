package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTurnDriversSayHowFullTheContextIs: a harness that wants a member to hand off its work
// before the agent compacts its conversation has to know how full that conversation is, and
// only the agent's own record says. Each family keeps a different record, so each driver reads
// its own and all three print the same line. The records here have the shapes the CLIs write:
// a compaction marker, a model call after it, and a side thread or a summary call that must not
// be read as the conversation. A caller parses the line, so the line is the contract.
func TestTurnDriversSayHowFullTheContextIs(t *testing.T) {
	skipUnlessLinux(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("the drivers read records with python3, which is not on this host")
	}
	const claudeUUID = "70496c7b-2038-433e-9acf-8eaa3708e0bc"
	const codexUUID = "01a14b6f-fa85-7190-99b3-44508d7b9cbc"
	claudeRecord := strings.Join([]string{
		`{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":2,"cache_creation_input_tokens":33767,"cache_read_input_tokens":0,"output_tokens":280}}}`,
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-17T19:56:00.000Z","compactMetadata":{"trigger":"auto","preTokens":47156,"postTokens":3103}}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued"}}`,
		`{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":5,"cache_creation_input_tokens":1000,"cache_read_input_tokens":34000,"output_tokens":512}}}`,
		// A subagent's call is a side thread with its own context, not the conversation's.
		`{"type":"assistant","isSidechain":true,"message":{"model":"claude-haiku-4-5","usage":{"input_tokens":90000,"output_tokens":1}}}`,
	}, "\n") + "\n"
	claudeJustCompacted := `{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":2,"cache_creation_input_tokens":40000,"cache_read_input_tokens":0,"output_tokens":100}}}` + "\n" +
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-10-17T19:56:00.000Z","compactMetadata":{"trigger":"auto","preTokens":47156,"postTokens":3103}}` + "\n"
	codexRecord := strings.Join([]string{
		`{"timestamp":"2026-10-17T19:56:32.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol"}}`,
		`{"timestamp":"2026-10-17T19:56:40.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":39000,"output_tokens":300},"model_context_window":258400}}}`,
		`{"timestamp":"2026-10-17T19:57:00.000Z","type":"compacted","payload":{"message":"summary","replacement_history":[]}}`,
		`{"timestamp":"2026-10-17T19:57:05.000Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10400,"output_tokens":89},"model_context_window":258400}}}`,
	}, "\n") + "\n"

	writeFile := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// opencode keeps its record in SQLite. The two tables carry only the columns the driver
	// reads: a top-level session, a subagent session under it, a model call, the summary
	// call a compaction writes, and one model call after it.
	openCodeDB := func(t *testing.T, home string, afterSummary bool) {
		t.Helper()
		db := filepath.Join(home, ".cs-opencode", "opencode.db")
		if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
			t.Fatal(err)
		}
		after := ""
		if afterSummary {
			after = `ins("m4", "ses_main", 4, {"role":"assistant","modelID":"kimi-k3","tokens":{"total":17924,"input":17000,"output":924,"cache":{"read":0,"write":0}}})`
		}
		script := `
import json, sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.execute("CREATE TABLE session (id TEXT, parent_id TEXT, time_updated INTEGER)")
db.execute("CREATE TABLE message (id TEXT, session_id TEXT, time_created INTEGER, data TEXT)")
db.execute("INSERT INTO session VALUES ('ses_main', NULL, 100), ('ses_child', 'ses_main', 200)")
def ins(i, s, t, d): db.execute("INSERT INTO message VALUES (?,?,?,?)", (i, s, t, json.dumps(d)))
ins("m1", "ses_main", 1, {"role":"assistant","modelID":"kimi-k3","tokens":{"total":32853,"input":32000,"output":853,"cache":{"read":0,"write":0}}})
ins("m2", "ses_main", 2, {"role":"user"})
ins("m3", "ses_main", 3, {"role":"assistant","summary":True,"mode":"compaction","time":{"created":1792267196000},"tokens":{"total":3998,"input":2950,"output":528,"cache":{"read":0,"write":0}}})
ins("c1", "ses_child", 5, {"role":"assistant","modelID":"kimi-k3","tokens":{"total":90000,"input":90000,"output":0,"cache":{"read":0,"write":0}}})
` + after + `
db.commit()
`
		if out, err := exec.Command("python3", "-c", script, db).CombinedOutput(); err != nil {
			t.Fatalf("building the opencode fixture: %v: %s", err, out)
		}
	}

	for _, tc := range []struct {
		name, script string
		args         []string
		setup        func(t *testing.T, home string)
		want         string
	}{
		{"claude reads the call after the compaction", "cs-claude-turn", nil, func(t *testing.T, home string) {
			writeFile(t, filepath.Join(home, ".cs-claude", "projects", "-work", claudeUUID+".jsonl"), claudeRecord)
		}, "tokens=35517 window=- compactions=1 compacted=1792266960 model=claude-opus-5"},
		{"claude just compacted reads the summary's size", "cs-claude-turn", []string{"--uuid", claudeUUID}, func(t *testing.T, home string) {
			writeFile(t, filepath.Join(home, ".cs-claude", "projects", "-work", claudeUUID+".jsonl"), claudeJustCompacted)
		}, "tokens=3103 window=- compactions=1 compacted=1792266960 model=claude-opus-5"},
		{"claude with no record", "cs-claude-turn", nil, func(*testing.T, string) {}, "absent"},
		{"codex reads the window it records", "cs-codex-turn", nil, func(t *testing.T, home string) {
			writeFile(t, filepath.Join(home, ".cs-codex", "sessions", "2026", "10", "17", "rollout-2026-10-17T19-56-32-"+codexUUID+".jsonl"), codexRecord)
		}, "tokens=10489 window=258400 compactions=1 compacted=1792267020 model=gpt-5.6-sol"},
		{"codex with no rollout for that session", "cs-codex-turn", []string{"--uuid", "00000000-0000-0000-0000-000000000000"}, func(t *testing.T, home string) {
			writeFile(t, filepath.Join(home, ".cs-codex", "sessions", "2026", "10", "17", "rollout-2026-10-17T19-56-32-"+codexUUID+".jsonl"), codexRecord)
		}, "absent"},
		{"opencode skips the summary call and the subagent", "cs-opencode-turn", nil, func(t *testing.T, home string) {
			openCodeDB(t, home, true)
		}, "tokens=17924 window=- compactions=1 compacted=1792267196 model=kimi-k3"},
		{"opencode just compacted is not known yet", "cs-opencode-turn", nil, func(t *testing.T, home string) {
			openCodeDB(t, home, false)
		}, "tokens=- window=- compactions=1 compacted=1792267196 model=kimi-k3"},
		{"opencode with no database", "cs-opencode-turn", nil, func(*testing.T, string) {}, "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, bin := agentHome(t, ".cs-context")
			tc.setup(t, home)
			out, exit := runScriptStdin(t, home, bin, []string{"OPENCODE_DB=", "CS_TURN_LOG="}, "", tc.script, append([]string{"--context"}, tc.args...)...)
			if exit != 0 {
				t.Fatalf("%s --context exit = %d; it answers with a line and always exits 0: %s", tc.script, exit, out)
			}
			if got := strings.TrimSpace(out); got != tc.want {
				t.Errorf("%s --context = %q, want %q", tc.script, got, tc.want)
			}
			// A question is not a turn. A turn log line from it would read as a turn that ended.
			if _, err := os.Stat(filepath.Join(home, ".cs-turns")); err == nil {
				t.Errorf("%s --context wrote a turn log; only a turn may", tc.script)
			}
		})
	}
}
