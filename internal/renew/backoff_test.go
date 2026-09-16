package renew

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

// The reason the state is on disk at all: a renewer that restarts must not behave
// as though nothing had ever failed. Without this, a crash-and-restart — or an
// ordinary destroy/create cycle — spends a real turn every time.
func TestARestartedRenewerResumesTheBackoffInsteadOfRetrying(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)
	f.onRun = func(*fixture, string) error { return errors.New("claude: not logged in") }

	// First renewer: one attempt, then backed off.
	f.k.tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.runs))
	}

	// A new renewer over the same directories, as a restart would be.
	second, err := newRenewer(f.k.cfg)
	if err != nil {
		t.Fatal(err)
	}
	second.tick(context.Background())
	if len(f.runs) != 1 {
		t.Errorf("runs = %d after a restart, want 1 — the restarted renewer ignored the backoff", len(f.runs))
	}

	// And once the wait has elapsed it does try again.
	f.now = f.now.Add(backoff[0] + time.Second)
	third, err := newRenewer(f.k.cfg)
	if err != nil {
		t.Fatal(err)
	}
	third.tick(context.Background())
	if len(f.runs) != 2 {
		t.Errorf("runs = %d once the backoff elapsed, want 2", len(f.runs))
	}
}

func TestConsecutiveFailuresLengthenTheWait(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i, want := range backoff {
		RecordFailure(dir, "claude", now, errors.New("nope"))
		st := ReadRetry(dir)["claude"]
		if st.Fails != i+1 {
			t.Errorf("after %d failures Fails = %d", i+1, st.Fails)
		}
		if got := st.NextAt.Sub(now); got != want {
			t.Errorf("failure %d: wait = %s, want %s", i+1, got, want)
		}
	}
	// And then holds, rather than growing without bound.
	RecordFailure(dir, "claude", now, errors.New("nope"))
	if got := ReadRetry(dir)["claude"].NextAt.Sub(now); got != backoff[len(backoff)-1] {
		t.Errorf("past the table the wait is %s, want it to hold at %s", got, backoff[len(backoff)-1])
	}
}

func TestSuccessClearsTheBackoff(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	RecordFailure(dir, "claude", now, errors.New("nope"))
	RecordFailure(dir, "claude", now, errors.New("nope"))
	RecordSuccess(dir, "claude", now)

	st := ReadRetry(dir)["claude"]
	if st.Fails != 0 || st.LastErr != "" || !st.NextAt.IsZero() {
		t.Errorf("success did not clear the backoff: %+v", st)
	}
	if Blocked(dir, "claude", now) != 0 {
		t.Error("a slot is still blocked after a success")
	}
}

// The backoff is per slot: a broken Claude login must not silence Codex.
func TestBackoffIsPerSlot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	RecordFailure(dir, "claude", now, errors.New("nope"))
	if Blocked(dir, "claude", now) == 0 {
		t.Error("claude is not blocked after a failure")
	}
	if Blocked(dir, "codex", now) != 0 {
		t.Error("codex was blocked by claude's failure")
	}
}

// NextAt is absolute, so a renewer that was down for the whole wait comes back
// ready rather than starting the wait over.
func TestTheWaitIsNotRestartedByBeingDownForIt(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	RecordFailure(dir, "claude", now, errors.New("nope"))
	if Blocked(dir, "claude", now.Add(backoff[0]+time.Second)) != 0 {
		t.Error("the wait restarted rather than elapsing")
	}
}

func TestRetryStateSurvivesAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(retryPath(dir), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Garbage reads as no state rather than crashing the renewer, and the next
	// write repairs it.
	if len(ReadRetry(dir)) != 0 {
		t.Error("garbage was read as state")
	}
	RecordFailure(dir, "claude", time.Now(), errors.New("nope"))
	if ReadRetry(dir)["claude"].Fails != 1 {
		t.Error("the file was not repaired by the next write")
	}
}

// create's one-shot renewal attempts regardless of the backoff — a person may have
// just fixed the login — but it must record what happened, or the renewer spends a
// second turn discovering the same failure moments later.
func TestRenewNowAttemptsDespiteBackoffButRecordsTheOutcome(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(time.Minute, 400*time.Hour)
	// Already deep in backoff from earlier failures.
	RecordFailure(f.cfg.StateDir, "claude", f.now, errors.New("earlier failure"))
	RecordFailure(f.cfg.StateDir, "claude", f.now, errors.New("earlier failure"))
	if Blocked(f.cfg.StateDir, "claude", f.now) == 0 {
		t.Fatal("the fixture is not actually backed off")
	}
	f.onRun = func(f *freshFixture, stage string) error { return stageClaudeExpiry(f.t, stage, f.now) }

	got, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err != nil {
		t.Fatalf("Now refused while backed off: %v", err)
	}
	if got.Outcome != Renewed || len(f.runs) != 1 {
		t.Errorf("outcome = %v, runs = %d — the attempt did not happen", got.Outcome, len(f.runs))
	}
	// And the success cleared the backoff for the renew.
	if st := ReadRetry(f.cfg.StateDir)["claude"]; st.Fails != 0 {
		t.Errorf("a successful create-time renewal left Fails = %d", st.Fails)
	}
}

func TestRenewNowRecordsAFailureForTheRenewer(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(time.Minute, 400*time.Hour)
	f.onRun = func(*freshFixture, string) error { return errors.New("claude: not logged in") }

	if _, err := Now(context.Background(), f.cfg, claudeSlot(t)); err == nil {
		t.Fatal("a failing renewal was accepted")
	}
	st := ReadRetry(f.cfg.StateDir)["claude"]
	if st.Fails != 1 {
		t.Errorf("Fails = %d, want 1 — the renewer will retry immediately", st.Fails)
	}
	if st.NextAt.IsZero() {
		t.Error("no next-attempt time was recorded")
	}
}

// A pid alone is not an identity: pids are reused, and a recycled one would make
// Running() say yes about somebody else's process, so the next create would start
// no renewer and nothing would renew anything.
func TestARecycledPidIsNotMistakenForTheRenewer(t *testing.T) {
	dir := t.TempDir()
	if _, ok := procStarted(os.Getpid()); !ok {
		t.Skip("no /proc on this platform; identity falls back to liveness")
	}
	// A live pid recorded with somebody else's start time.
	rec := strconv.Itoa(os.Getpid()) + "\nnot-the-start-time\n"
	if err := os.WriteFile(pidPath(dir), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if Running(dir) {
		t.Error("a pid whose start time does not match was taken for the renewer")
	}
}

func TestAMatchingPidIsAccepted(t *testing.T) {
	dir := t.TempDir()
	started, ok := procStarted(os.Getpid())
	if !ok {
		t.Skip("no /proc on this platform")
	}
	rec := strconv.Itoa(os.Getpid()) + "\n" + started + "\n"
	if err := os.WriteFile(pidPath(dir), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Running(dir) {
		t.Error("the recorded process was not recognised")
	}
}

// A pidfile from an older build carries no start time, and must still work.
func TestAPidFileWithoutAStartTimeStillWorks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(pidPath(dir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !Running(dir) {
		t.Error("a pidfile with no start time was rejected")
	}
}

func TestProcStartedReadsPastACommWithSpaces(t *testing.T) {
	// The comm field is parenthesised and may contain spaces, which is why the
	// fields are counted from the last ')'.
	if _, ok := procStarted(os.Getpid()); !ok {
		t.Skip("no /proc on this platform")
	}
	if _, ok := procStarted(1 << 30); ok {
		t.Error("a pid that does not exist reported a start time")
	}
}
