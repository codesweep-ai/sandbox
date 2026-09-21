package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// submitTmux records every call, serves one pane, and creates the transcript of
// a submitted prompt when the test says the machine is quick enough to write it.
const submitTmux = `#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
case "$1" in
  new-session) : > "$STUB_DIR/alive" ;;
  has-session) [ -f "$STUB_DIR/alive" ] || exit 1 ;;
  list-sessions|ls) [ -f "$STUB_DIR/alive" ] && echo "$STUB_SESSION" ;;
  capture-pane) cat "$STUB_DIR/pane" 2>/dev/null ;;
  send-keys)
    case "$*" in
      *Enter*)
        echo enter >> "$STUB_DIR/enters"
        if [ -f "$STUB_DIR/quick" ]; then
          mkdir -p "$STUB_PROJECTS"
          : > "$STUB_PROJECTS/$STUB_UUID.jsonl"
        fi ;;
    esac ;;
esac
exit 0
`

// runSubmit drives one turn as far as the transcript wait and reports how many
// times Enter was pressed on the way.
func runSubmit(t *testing.T, quick bool, env ...string) (enters int, out string, exit int) {
	t.Helper()
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-claude")
	writeStub(t, bin, "tmux", submitTmux)
	writeStub(t, bin, "cs-claude", "#!/bin/sh\nexit 0\n")
	stub := t.TempDir()
	projects := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	// An idle claude: ready to take a prompt, and in no turn.
	if err := os.WriteFile(filepath.Join(stub, "pane"),
		[]byte("❯ \n  bypass permissions on (shift+tab to cycle)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if quick {
		if err := os.WriteFile(filepath.Join(stub, "quick"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const uuid = "11111111-2222-3333-4444-555555555555"
	env = append(env, "STUB_DIR="+stub, "STUB_PROJECTS="+projects,
		"STUB_SESSION=cs-claude-"+uuid, "STUB_UUID="+uuid)
	out, exit = runScriptStdin(t, home, bin, env, "do the thing\n",
		"cs-claude-turn", "--uuid", uuid, "--projects", projects, "--timeout", "1")
	b, _ := os.ReadFile(filepath.Join(stub, "enters"))
	return strings.Count(string(b), "enter"), out, exit
}

// TestASubmittedPromptIsEnteredOnce: the driver presses Enter once for a prompt
// that was taken, and only presses again when nothing came of the first.
//
// A second Enter sent beside the first is harmless only while the first is
// still being rendered. Once the prompt has gone, that keystroke sits in the
// composer, and what sits there is carried into the NEXT prompt: a turn was
// seen reaching the model with two carriage returns in front of text the
// recording holds without them, which is a different request and a cassette
// miss in a tier whose whole value is that a replay matches.
func TestASubmittedPromptIsEnteredOnce(t *testing.T) {
	enters, out, _ := runSubmit(t, true)
	if enters != 1 {
		t.Errorf("Enter pressed %d times for one accepted prompt; want 1\n%s", enters, out)
	}
}

// TestAnUnwrittenTranscriptIsWaitedForAndSaidSo: a transcript that has not
// appeared yet is a slow machine, not a lost turn.
//
// Ten seconds was the bound, and a loaded runner did not make it: the turn died
// here, the campaign read a member that had stopped, and the ladder it climbed
// diverged from the recording — so the real failure arrived as a cassette miss
// three rungs away from its cause. The driver now waits a span it names, and
// re-sends Enter once before giving up.
func TestAnUnwrittenTranscriptIsWaitedForAndSaidSo(t *testing.T) {
	enters, out, exit := runSubmit(t, false, "CS_CLAUDE_JSONL_WAIT_SECS=1")
	if exit != 3 {
		t.Fatalf("a transcript that never appears must end the turn with exit 3; got %d\n%s", exit, out)
	}
	if enters != 2 {
		t.Errorf("Enter pressed %d times; want 2: one to submit, one more before giving up\n%s", enters, out)
	}
	if !strings.Contains(out, "waiting 2s") {
		t.Errorf("the failure must say how long it waited, so a reader can tell a slow machine from a lost one:\n%s", out)
	}
}
