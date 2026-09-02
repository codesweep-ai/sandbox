//go:build agents_replay

// The replay tier: the credential matrix again, with the model turns served
// from committed cassettes instead of providers.
//
//	make test-agents-shared   the cells that hold a copy of the credential
//	make test-agents-lent     the cells that borrow one through the lender
//	make test-agents-replay   both
//
// It boots real sandboxes and runs real agent binaries, so it needs podman and
// the agents image — but it holds no credential, reaches no provider, and costs
// nothing. That is what makes it the tier that can run on every push.
//
// The two profiles differ by one hop, and it is the hop worth having a second
// profile for. A shared cell reaches the recorder itself. A lent cell reaches
// the lender, which swaps a loan token for the host's credential and forwards
// to the recorder — so the lending machinery is exercised on every replay,
// rather than being short-circuited by a cassette in front of it.
//
// What a replay proves is that the harness carried an agent through a whole
// turn to an answer. It does not re-derive the model's answer, which was
// decided once, when the cassette was recorded.
package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentReplay replays every cell that has been recorded.
//
// Serial: each cell boots a real sandbox, and they share one host's memory, one
// network fabric and one pool of gateway ports.
func TestAgentReplay(t *testing.T) {
	r, host := liveSetup(t)
	startLiveLender(t, fabricatedAgentHome(t))
	store := cassetteStore(t)
	proxy := startVCR(t, "replay", store)
	// Asked once, of the image every cell is about to boot, and compared with
	// what each cassette says it was recorded against.
	versions := agentCLIVersions(t, r, image(t))

	replayed := 0
	for _, c := range liveCases() {
		if !hasCassette(t, c) {
			continue
		}
		replayed++
		t.Run(c.name(), func(t *testing.T) {
			assertCassetteAgent(t, c, store, versions)
			assertCassetteRuleset(t, c, store)
			runAgentCase(t, r, host, c, true)
		})
	}
	if replayed == 0 {
		t.Skip("no cassette under test/cassettes: record one with `make fixtures`")
	}
	// After every cell, because one cs-vcr serves them all and its accounting
	// is per session. A single statement about the whole tier is also the one
	// worth making: nothing was spent, and the cassettes answered what was asked.
	assertSpentNothing(t, proxy)
	reportMisses(t, proxy)
}

// assertCassetteAgent settles what a cell's cassette can still be asked to
// prove, from the claim the recording left beside it.
//
// A SKIP for a cassette recorded against a different build of its agent CLI,
// and that is the whole reason the claim exists. An agent carries its own
// system prompt and tool list, so a bump rewrites every request it sends: the
// cassette misses on all of them, and the tier reports a dozen prompt diffs and
// a miss ratio instead of the one version that moved. Worse, it reports them
// after booting every sandbox, which is where the minutes are.
//
// Skipped rather than failed because the fixture is STALE, not broken. The one
// gesture that fixes it — re-recording against the new image — cannot be made
// by the run that wants it, and a bump is a deliberate two-step here: the pins
// move in one commit, the tier that carries them is published by another, and
// the cassettes can only be re-recorded once that tier is adopted. Failing in
// between would make the whole matrix red for the duration of a procedure that
// is going exactly to plan. What must not happen is failing SILENTLY, so the
// skip names both versions and the command that ends it.
//
// A version the claim does not carry, or one this image does not report, skips
// the comparison rather than guessing: a cassette from before the claim existed
// replays perfectly well, and refusing it would fail on a working fixture.
//
// The outcome is checked after, and fails: a recording that never finished is a
// broken fixture rather than a stale one. Second because when the agent has
// moved the cassette is stale by construction and nothing else about it is
// worth asserting.
func assertCassetteAgent(t *testing.T, c liveCase, store string, versions map[string]string) {
	t.Helper()
	claim, ok := readRecordingClaim(t, store, c)
	if !ok {
		return // recorded before the claim existed
	}
	if was, now := claim.CLIVersion, versions[c.cli]; was != "" && now != "" && was != now {
		t.Skipf("this cassette was recorded against %s %s and the image carries %s, "+
			"so every request in it would miss\n"+
			"re-record it with: make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/%s'",
			c.cli, was, now, c.name())
	}
	if claim.Outcome != recordingSettled {
		t.Fatalf("this cassette came out of a recording that ended %q, and the replay below "+
			"asserts a completed turn\n"+
			"re-record it with: make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/%s'",
			claim.Outcome, c.name())
	}
}

// assertCassetteRuleset fails a cell whose cassette was recorded under a
// normalization ruleset this cs-vcr build no longer applies.
//
// Worth its one process, because the failure it replaces is the worst shape a
// failure comes in. A cassette keyed under a superseded ruleset does not miss a
// few entries: the keys mean something else now, so every model call misses at
// once. The agent then waits out its whole turn for an answer that can never
// arrive, and the output says nothing about why. cs-vcr can answer the question
// outright, in one call that boots nothing and contacts nobody, so it is asked
// before a sandbox is created.
//
// A hard failure rather than a skip. A cassette that cannot be replayed is a
// broken fixture, not an absent one, and skipping would let the fixtures rot
// silently — which is exactly how they would get here.
func assertCassetteRuleset(t *testing.T, c liveCase, store string) {
	t.Helper()
	bin, err := exec.LookPath("cs-vcr")
	if err != nil {
		return // startVCR already skipped the tier on this, and owns the message
	}
	config := filepath.Join(t.TempDir(), "verify.yaml")
	writeSecret(t, config, []byte("cassettes: "+store+"\n"))
	out, err := exec.Command(bin, "cassette", "verify", "--config", config, c.name()).CombinedOutput()
	if err == nil {
		return
	}
	t.Fatalf("this cs-vcr build cannot replay the committed cassette:\n%s\n"+
		"re-record it with: make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/%s'",
		strings.TrimSpace(string(out)), c.name())
}
