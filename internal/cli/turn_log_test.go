package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// turnLine is one parsed line of a driver's turn log:
// <epoch> <exit> <class|-> <retry_after|-> <reason>.
type turnLine struct {
	at     int64
	exit   int
	class  string
	retry  string
	reason string
}

func readTurnLog(t *testing.T, home, family string) []turnLine {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".cs-turns", family+".log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []turnLine
	for _, raw := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		f := strings.SplitN(raw, " ", 5)
		if len(f) != 5 {
			t.Fatalf("turn log line has %d fields, want 5: %q", len(f), raw)
		}
		at, err1 := strconv.ParseInt(f[0], 10, 64)
		exit, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("turn log line does not start with an epoch and an exit code: %q", raw)
		}
		lines = append(lines, turnLine{at, exit, f[2], f[3], f[4]})
	}
	return lines
}

func lastTurnLine(t *testing.T, home, family string) turnLine {
	t.Helper()
	lines := readTurnLog(t, home, family)
	if len(lines) == 0 {
		t.Fatalf("the %s driver ended a turn and left no line in ~/.cs-turns/%s.log", family, family)
	}
	l := lines[len(lines)-1]
	if age := time.Now().Unix() - l.at; age < 0 || age > 120 {
		t.Fatalf("turn log line is dated %ds from now", age)
	}
	return l
}

// wantTurnLogAgrees holds the log to the line the driver printed for its caller,
// "failure class=<c> retry_after=<r>": two tellings of one turn must not differ.
func wantTurnLogAgrees(t *testing.T, home, family string, exit int, classLine string) {
	t.Helper()
	l := lastTurnLine(t, home, family)
	want := fmt.Sprintf("failure class=%s retry_after=%s", l.class, l.retry)
	if l.exit != exit || !strings.Contains(classLine, want) {
		t.Fatalf("turn log says exit=%d %q; the caller was told exit=%d %q", l.exit, want, exit, classLine)
	}
	if l.reason == "-" || l.reason == "" {
		t.Fatalf("turn log gives no reason for a turn the provider refused: %+v", l)
	}
}

// TestAskingAndUsageWriteNoTurnLine: --state is a question and a usage error is a typo.
// Neither is a turn, and a harness that polls --state every few seconds must not fill the log.
func TestAskingAndUsageWriteNoTurnLine(t *testing.T) {
	skipUnlessLinux(t)
	for _, family := range []string{"codex", "claude", "opencode"} {
		script := "cs-" + family + "-turn"
		for _, args := range [][]string{{"--state"}, {"--help"}, {"--no-such-flag"}} {
			t.Run(family+" "+args[0], func(t *testing.T) {
				home, bin := agentHome(t, ".cs-state")
				writeStub(t, bin, "tmux", "#!/bin/sh\nexit 1\n")
				runScriptStdin(t, home, bin, nil, "", script, args...)
				if lines := readTurnLog(t, home, family); len(lines) != 0 {
					t.Fatalf("%s %s wrote %d turn line(s): %+v", script, args[0], len(lines), lines)
				}
			})
		}
	}
}

// TestTurnLogIsBounded: the log is append-only until it is twice its keep size, and then
// the oldest lines go. The newest line is always the last.
func TestTurnLogIsBounded(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-codex-remote")
	dir := filepath.Join(home, ".cs-turns")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var old strings.Builder
	for i := range 400 {
		fmt.Fprintf(&old, "%d 0 - - -\n", 1700000000+i)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex.log"), []byte(old.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// A launch failure is a turn end too: codex is not on PATH here, so the driver exits 3.
	_, exit := runScriptStdin(t, home, bin, []string{"PATH=" + bin + ":/usr/bin:/bin"}, "do the thing\n",
		"cs-codex-turn", "--tmux", "tok")
	if exit != 3 {
		t.Skipf("driver exited %d where the test needs a launch failure", exit)
	}
	lines := readTurnLog(t, home, "codex")
	if len(lines) != 200 {
		t.Fatalf("log holds %d lines after passing twice its keep size; want 200", len(lines))
	}
	if last := lines[len(lines)-1]; last.exit != 3 || !strings.Contains(last.reason, "codex not found") {
		t.Fatalf("newest line is not the turn that just ended: %+v", last)
	}
}

// TestAnUnwritableTurnLogChangesNothing: the log is a courtesy to a second reader. A driver
// that cannot write it reports the turn exactly as before.
func TestAnUnwritableTurnLogChangesNothing(t *testing.T) {
	skipUnlessLinux(t)
	home, bin := agentHome(t, ".cs-codex-remote")
	out, exit := runScriptStdin(t, home, bin,
		[]string{"PATH=" + bin + ":/usr/bin:/bin", "CS_TURN_LOG=/proc/nonexistent/turns.log"}, "do the thing\n",
		"cs-codex-turn", "--tmux", "tok")
	if exit != 3 || !strings.Contains(out, "codex not found") {
		t.Fatalf("exit=%d out=%s; an unwritable log changed how the turn was reported", exit, out)
	}
}
