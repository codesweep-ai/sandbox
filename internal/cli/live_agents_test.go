//go:build live_agents

// The two tiers that reach a real provider: the credential matrix, and the
// recording that lets the replay tier run it for nothing.
//
//	make test-live-agents    drive every cell against its provider
//	make fixtures            drive them through a cs-vcr in record mode
//
// Both spend money and quota on every run, and both are deliberately outside
// CI, outside `make check` and outside test-integration: a provider being slow
// or rate-limiting is not a defect in this repository.
//
// Credentials come from .env at the repository root, which is git-ignored, and
// never from the developer's own profiles. The suite builds a throwaway agent
// home and points CS_SANDBOX_AGENT_HOME at it, so a lent or shared key is one
// this file put there. Host LOGINS are the exception: a login cannot be
// fabricated against a real provider, so those cases read the real profile
// through a symlink and skip themselves when it is absent.
//
// The matrix, the driver and the recorder all live in agentmatrix_test.go,
// which the replay tier shares.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveAgentCredentialMatrix drives every combination end to end, against
// the real provider, with nothing in between.
func TestLiveAgentCredentialMatrix(t *testing.T) {
	env := liveEnv(t)
	r, host := liveSetup(t)
	startLiveLender(t, liveAgentHome(t, env))

	for _, c := range liveCases() {
		t.Run(c.name(), func(t *testing.T) {
			if why := c.available(env); why != "" {
				t.Skip(why)
			}
			runAgentCase(t, r, host, c, false)
		})
	}
}

// TestLiveAgentRecordsCassettes is `make fixtures` as a test: it drives each
// cell through a cs-vcr in record mode and leaves the cassettes where the
// replay tier reads them.
//
// A test rather than a script, because a recording and its replay have to agree
// on every byte the agent sends, and the only way to guarantee that is for both
// to come out of one driver. A script would be a second description of the same
// run, free to drift from the first.
//
// It records every cell by default. Narrow it with -run when re-recording one,
// since each costs a real model turn:
//
//	make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/codex-openai-lent'
func TestLiveAgentRecordsCassettes(t *testing.T) {
	if os.Getenv("CS_SANDBOX_RECORD") == "" {
		t.Skip("recording spends tokens and overwrites a committed cassette: set CS_SANDBOX_RECORD=1")
	}
	env := liveEnv(t)
	r, host := liveSetup(t)
	startLiveLender(t, liveAgentHome(t, env))
	store := cassetteStore(t)
	proxy := startVCR(t, "record", store)

	recorded := 0
	var truncated []string
	for _, c := range liveCases() {
		t.Run(c.name(), func(t *testing.T) {
			if why := c.available(env); why != "" {
				skipUnlessStrict(t, "%s", why)
				return
			}
			// Recording REPLACES this cell's cassette rather than adding to it,
			// and the old one goes before the sandbox boots.
			//
			// cs-vcr refuses to record into a cassette keyed under a superseded
			// normalization ruleset, for the same reason it refuses to replay
			// one. Left in place, that refusal arrives once per request from
			// inside the recorder, where the agent cannot see it: the turn
			// simply never gets an answer, and the run waits out its whole
			// timeout without saying why.
			//
			// Safe to remove outright. A re-recording supersedes what it
			// replaces by definition, and the copy it replaces is in git.
			dir := filepath.Join(store, c.name())
			if err := os.RemoveAll(dir); err != nil {
				t.Fatalf("clear %s before re-recording: %v", dir, err)
			}
			runAgentCase(t, r, host, c, true)
			if recordedTruncated(t, proxy, c.name()) {
				truncated = append(truncated, c.name())
			}
			recorded++
			t.Logf("recorded %s into %s — commit it", c.name(), dir)
		})
	}
	if recorded == 0 {
		t.Skip("no cell could be recorded: every credential this matrix needs is missing")
	}
	// Said once, at the end, where it is read. A per-case log line about a
	// truncation scrolls past under -v; a list at the bottom is what somebody
	// still has on screen when they decide whether to commit.
	if len(truncated) > 0 {
		t.Logf("%d of %d recording(s) had a response the client cut off: %s\n"+
			"  Usually harmless. `make test-agents-replay` is what decides, and it is the next thing to run.",
			len(truncated), recorded, strings.Join(truncated, ", "))
	}
	t.Logf("recorded %d cassette(s). Replay them before committing:  make test-agents-replay", recorded)
}

// strictSwitch turns a skipped cell into a failure. A host that holds every
// credential and means to re-record the whole matrix wants it: recording none
// of them reports the same green as recording all of them.
const strictSwitch = "CS_SANDBOX_STRICT"

// skipUnlessStrict reports a cell this host cannot cover, and says which
// credential it wanted. Under strictSwitch it fails instead.
func skipUnlessStrict(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(strictSwitch) != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}
