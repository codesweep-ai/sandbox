package renew

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
)

// freshFixture is a Now call with the clock and the exec under the test's
// control. It shares no state with the renewer loop, which is the point: create
// calls Now without a renewer process existing at all.
type freshFixture struct {
	t      *testing.T
	home   string
	cfg    Config
	runs   []lend.RenewSpec
	stages []string
	// onRun stands in for the client.
	onRun func(*freshFixture, string) error
	now   time.Time
}

func newFreshFixture(t *testing.T) *freshFixture {
	t.Helper()
	f := &freshFixture{t: t, home: t.TempDir(), now: time.Now()}
	f.cfg = Config{
		Home:     f.home,
		KeysDir:  filepath.Join(f.home, ".cs-keys"),
		Dir:      t.TempDir(),
		StateDir: t.TempDir(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return f.now },
		Image:    "localhost/test-image:latest",
		Run: func(_ context.Context, spec lend.RenewSpec, stage string) error {
			f.runs = append(f.runs, spec)
			f.stages = append(f.stages, stage)
			if f.onRun != nil {
				return f.onRun(f, stage)
			}
			return nil
		},
	}
	return f
}

// writeClaude writes the host credential with the two clocks set relative to now.
func (f *freshFixture) writeClaude(expiresIn, deadlineIn time.Duration) {
	f.t.Helper()
	dir := filepath.Join(f.home, ".cs-claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		f.now.Add(expiresIn).UnixMilli(), f.now.Add(deadlineIn).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(doc), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func claudeSlot(t *testing.T) lend.Slot {
	t.Helper()
	s, ok := lend.SlotByID("claude")
	if !ok {
		t.Fatal("no claude slot")
	}
	return s
}

// The case this was built for: a credential nobody has used for days, which a
// create would otherwise refuse.
func TestRenewNowRenewsAnAlreadyExpiredLogin(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(-4*24*time.Hour, 400*time.Hour)
	f.onRun = func(f *freshFixture, stage string) error { return stageClaudeExpiry(f.t, stage, f.now) }

	got, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err != nil {
		t.Fatalf("Freshen: %v", err)
	}
	if got.Outcome != Renewed {
		t.Errorf("outcome = %v, want Renewed", got.Outcome)
	}
	if !got.WasExpired {
		t.Error("an expired login was not reported as having been expired")
	}
	if len(f.runs) != 1 {
		t.Errorf("runs = %d, want 1", len(f.runs))
	}
	if left := got.Expires.Sub(f.now); left < 7*time.Hour {
		t.Errorf("the reported expiry is %s out, so the new one was not read back", left)
	}
}

// A sandbox must not start with ninety seconds of credential when it could start
// with eight hours.
func TestRenewNowRenewsALoginInsideItsWindow(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(90*time.Second, 400*time.Hour)
	f.onRun = func(f *freshFixture, stage string) error { return stageClaudeExpiry(f.t, stage, f.now) }

	got, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err != nil {
		t.Fatalf("Freshen: %v", err)
	}
	if got.Outcome != Renewed {
		t.Errorf("outcome = %v, want Renewed", got.Outcome)
	}
	if got.WasExpired {
		t.Error("a valid login was reported as expired")
	}
}

// The important half. Most creates have a perfectly good credential, and its
// client would refuse to refresh one — so a attempt here costs a real turn on the
// subscription and achieves nothing.
func TestRenewNowLeavesAHealthyLoginAlone(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(6*time.Hour, 400*time.Hour)

	got, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err != nil {
		t.Fatalf("Freshen: %v", err)
	}
	if got.Outcome != Fresh {
		t.Errorf("outcome = %v, want Fresh", got.Outcome)
	}
	if len(f.runs) != 0 {
		t.Errorf("spent a turn on a credential with six hours left: %v", f.runs)
	}
}

// Past the end of the refresh chain nothing can help, so it must refuse without
// spending a turn to produce a worse message.
func TestRenewNowRefusesPastTheRefreshDeadlineWithoutRunningAnything(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(-time.Hour, -time.Minute) // expired, and the chain has ended

	_, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err == nil {
		t.Fatal("accepted a login that can no longer be renewed")
	}
	if len(f.runs) != 0 {
		t.Errorf("spent a turn on a login that cannot be renewed: %v", f.runs)
	}
	for _, want := range []string{"can no longer be renewed", "sign in again on the host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q: %v", want, err)
		}
	}
}

// A clean run that renewed nothing is a failure here too, and create has to stop
// rather than build a sandbox around a credential about to die.
func TestRenewNowFailsWhenTheClientRenewedNothing(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(time.Minute, 400*time.Hour)
	f.onRun = func(*freshFixture, string) error { return nil } // exits 0, changes nothing

	_, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err == nil {
		t.Fatal("a attempt that renewed nothing was accepted")
	}
	if !strings.Contains(err.Error(), "did not move") {
		t.Errorf("the error does not say what happened: %v", err)
	}
	if !strings.Contains(err.Error(), "before it could be lent") {
		t.Errorf("the error does not say why it mattered: %v", err)
	}
}

func TestRenewNowPassesThroughTheClientsOwnFailure(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(time.Minute, 400*time.Hour)
	f.onRun = func(*freshFixture, string) error { return errors.New("claude failed: Not logged in") }

	_, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("the client's own reason was lost: %v", err)
	}
}

// A missing credential is not this function's problem to describe: the reader
// wants the file and the command that makes one, which the slot already says.
func TestRenewNowReportsAMissingCredentialAsItself(t *testing.T) {
	f := newFreshFixture(t)
	_, err := Now(context.Background(), f.cfg, claudeSlot(t))
	if err == nil {
		t.Fatal("a missing credential was accepted")
	}
	if !strings.Contains(err.Error(), "cs-claude") {
		t.Errorf("the error does not name the remedy: %v", err)
	}
	if len(f.runs) != 0 {
		t.Errorf("tried to renew a credential that does not exist: %v", f.runs)
	}
}

func TestRenewNowIgnoresSlotsNothingRenews(t *testing.T) {
	f := newFreshFixture(t)
	for _, id := range []string{"anthropic", "openai", "fireworks"} {
		s, _ := lend.SlotByID(id)
		got, err := Now(context.Background(), f.cfg, s)
		if err != nil {
			t.Errorf("%s: %v", id, err)
		}
		if got.Outcome != NotRenewable {
			t.Errorf("%s: outcome = %v, want NotRenewable", id, got.Outcome)
		}
	}
	if len(f.runs) != 0 {
		t.Errorf("ran a client for a key: %v", f.runs)
	}
}

// Whoever held the lock may have renewed the very credential this call was about
// to spend a turn on, so the expiry is asked again once the lock is held.
func TestRenewNowDoesNotRenewWhatSomebodyElseJustRenewed(t *testing.T) {
	f := newFreshFixture(t)
	f.writeClaude(time.Minute, 400*time.Hour)
	s := claudeSlot(t)

	cfg, err := f.cfg.prepare(false)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for the concurrent holder: the credential is renewed between the
	// first read and the attempt.
	before, _, err := s.ExpiresAt(cfg.Home, cfg.KeysDir)
	if err != nil {
		t.Fatal(err)
	}
	f.writeClaude(8*time.Hour, 400*time.Hour)

	ran, err := renewNow(context.Background(), cfg, s, before, true)
	if err != nil {
		t.Fatalf("renewNow: %v", err)
	}
	if ran {
		t.Error("spent a turn on a credential that had already been renewed")
	}
	if len(f.runs) != 0 {
		t.Errorf("ran a client anyway: %v", f.runs)
	}
}
