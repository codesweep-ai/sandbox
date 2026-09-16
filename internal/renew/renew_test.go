package renew

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/codesweep-ai/sandbox/internal/state"
)

// fixture is a renewer wired to a temporary home and a temporary instances root,
// with the clock and the exec both under the test's control. Nothing here
// touches a real credential or starts a real client.
type fixture struct {
	t       *testing.T
	home    string
	instDir string
	dir     string
	now     time.Time
	// runs records every renewal the renewer decided to perform.
	runs   []lend.RenewSpec
	stages []string
	// onRun is what a run does to the world, standing in for the client.
	onRun func(*fixture, string) error
	k     *renewer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:       t,
		home:    t.TempDir(),
		instDir: t.TempDir(),
		dir:     t.TempDir(),
		now:     time.Now(),
	}
	// Resolution is part of the flow now, and deliberately does not consult the
	// ambient PATH, so a fixture supplies the clients the way a host does.
	cfg := Config{
		Home:     f.home,
		KeysDir:  filepath.Join(f.home, ".cs-keys"),
		InstDir:  f.instDir,
		Dir:      f.dir,
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
	k, err := newRenewer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.k = k
	return f
}

// setClaudeExpiry writes the host credential with the given remaining life.
func (f *fixture) setClaudeExpiry(remaining time.Duration) {
	f.t.Helper()
	dir := filepath.Join(f.home, ".cs-claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		f.now.Add(remaining).UnixMilli(), f.now.Add(400*time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(doc), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// setCodexExpiry writes a Codex auth.json whose token expires in remaining.
func (f *fixture) setCodexExpiry(remaining time.Duration) {
	f.t.Helper()
	dir := filepath.Join(f.home, ".cs-codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(map[string]any{"exp": f.now.Add(remaining).Unix()})
	if err != nil {
		f.t.Fatal(err)
	}
	tok := enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
	doc := fmt.Sprintf(`{"tokens":{"access_token":%q,"account_id":"acct"}}`, tok)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(doc), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// lend records a sandbox borrowing a slot, which is how the renewer learns there
// is anything to keep alive.
func (f *fixture) lend(slot string) {
	f.t.Helper()
	in := &state.Instance{Name: "box", Group: state.DefaultGroup, Type: "agent", Engine: "podman"}
	if err := state.SaveGroup(f.instDir, &state.Group{Name: state.DefaultGroup}); err != nil {
		f.t.Fatal(err)
	}
	if err := state.Save(f.instDir, in); err != nil {
		f.t.Fatal(err)
	}
	loans := []lend.Loan{{Token: "loan_x", Label: "l", Slot: slot, Kind: lend.Login}}
	if err := lend.WriteLoans(state.Dir(f.instDir, in.Group, in.Name), loans); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) status() Status {
	f.t.Helper()
	st, ok, err := ReadStatus(f.dir)
	if err != nil || !ok {
		f.t.Fatalf("status: ok=%v err=%v", ok, err)
	}
	return st
}

func TestNothingBorrowedMeansNoAttempt(t *testing.T) {
	f := newFixture(t)
	f.setClaudeExpiry(time.Minute) // deep inside the window, and irrelevant
	f.k.tick(context.Background())
	if len(f.runs) != 0 {
		t.Errorf("attemptd a credential nobody is borrowing: %v", f.runs)
	}
}

func TestOutsideTheWindowTheRenewerWaitsInsteadOfSpendingATurn(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(3 * time.Hour)

	wait := f.k.tick(context.Background())
	if len(f.runs) != 0 {
		t.Fatalf("attemptd 3h before expiry, which refreshes nothing and costs a real turn: %v", f.runs)
	}
	// It must not wake past the moment the window opens, and it must not sleep
	// longer than idlePoll either — see the comment on that cap in tick.
	s, _ := lend.SlotByID("claude")
	if until := 3*time.Hour - s.RenewWithin(); wait > until {
		t.Errorf("wait = %s, which is past the window opening at %s", wait, until)
	}
	if wait > idlePoll {
		t.Errorf("wait = %s, longer than the idle poll %s", wait, idlePoll)
	}
	if wait < minPoll {
		t.Errorf("wait = %s, tighter than the minimum poll %s", wait, minPoll)
	}
}

func TestInsideTheWindowItAttemptsAndVerifies(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)
	// A working client: it refreshes, so the expiry moves.
	f.onRun = func(f *fixture, stage string) error { return stageClaudeExpiry(f.t, stage, f.now) }

	f.k.tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.runs))
	}
	// An absolute path inside the image, not a bare name for some PATH to resolve.
	if f.runs[0].Bin != "/sandbox/home/.local/bin/cs-claude" {
		t.Errorf("ran %q, want the image's cs-claude wrapper", f.runs[0].Bin)
	}
	// And the host's credential was staged for it to find.
	if len(f.stages) != 1 {
		t.Fatalf("stages = %d", len(f.stages))
	}
	st := f.status()
	if len(st.Slots) != 1 || st.Slots[0].Err != "" {
		t.Errorf("status reports a problem after a successful renew: %+v", st.Slots)
	}
	if st.Slots[0].LastOK.IsZero() {
		t.Error("a successful renew was not recorded")
	}
}

// The failure this whole mechanism exists to catch: the command works, and
// nothing was renewed. The client decides for itself whether to refresh, so an
// exit code says nothing about whether the credential was extended.
func TestACleanRunThatRenewedNothingIsAFailure(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)
	f.onRun = func(*fixture, string) error { return nil } // exits 0, touches nothing

	f.k.tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.runs))
	}
	st := f.status()
	if len(st.Slots) != 1 {
		t.Fatalf("slots = %d", len(st.Slots))
	}
	if st.Slots[0].Err == "" {
		t.Fatal("a attempt that renewed nothing was recorded as success, which is the silent failure this must not have")
	}
	if !st.Slots[0].LastOK.IsZero() {
		t.Error("LastOK was set by a run that renewed nothing")
	}
	if got := st.Slots[0].Err; !strings.Contains(got, "did not move") {
		t.Errorf("the error does not say what went wrong: %q", got)
	}
}

func TestAFailedAttemptBacksOffInsteadOfSpinning(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)
	f.onRun = func(*fixture, string) error { return errors.New("claude: not logged in") }

	// First attempt runs.
	f.k.tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.runs))
	}
	// A second tick immediately after must not spend another turn: each one is
	// billed to somebody's subscription.
	f.k.tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("runs = %d after an immediate second tick, want 1 (backoff)", len(f.runs))
	}
	// Once the backoff has elapsed it tries again.
	f.now = f.now.Add(backoff[0] + time.Second)
	f.k.tick(context.Background())
	if len(f.runs) != 2 {
		t.Errorf("runs = %d after the backoff elapsed, want 2", len(f.runs))
	}
	if !strings.Contains(f.status().Slots[0].Err, "not logged in") {
		t.Errorf("the client's own reason was not kept: %q", f.status().Slots[0].Err)
	}
}

func TestBackoffGrowsAndThenHolds(t *testing.T) {
	for i, want := range backoff {
		if got := backoffFor(i + 1); got != want {
			t.Errorf("backoffFor(%d) = %s, want %s", i+1, got, want)
		}
	}
	if got := backoffFor(len(backoff) + 5); got != backoff[len(backoff)-1] {
		t.Errorf("backoff past the table = %s, want it to hold at %s", got, backoff[len(backoff)-1])
	}
}

func TestAnUnreadableCredentialIsRecordedNotAttemptd(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	// No credential file at all.
	f.k.tick(context.Background())
	if len(f.runs) != 0 {
		t.Errorf("attemptd a credential that cannot be read: %v", f.runs)
	}
	st := f.status()
	if len(st.Slots) != 1 || st.Slots[0].Err == "" {
		t.Errorf("an unreadable credential was not reported: %+v", st.Slots)
	}
}

func TestStatusCarriesBothClocks(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(3 * time.Hour)
	f.k.tick(context.Background())

	st := f.status()
	if len(st.Slots) != 1 {
		t.Fatalf("slots = %d", len(st.Slots))
	}
	if st.Slots[0].ExpiresAt.IsZero() {
		t.Error("the access-token expiry is not published")
	}
	// The outer clock matters most when everything looks fine, so it has to be
	// there on a healthy tick.
	if st.Slots[0].RefreshDeadline.IsZero() {
		t.Error("the refresh deadline is not published")
	}
	if st.PID == 0 || st.Updated.IsZero() {
		t.Error("status does not identify the process or when it reported")
	}
}

func TestStatusIsReplacedWholeNotInPlace(t *testing.T) {
	// A reader must never see half a document, so the file is renamed into place
	// and no .tmp is left behind.
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(3 * time.Hour)
	f.k.tick(context.Background())

	if _, err := os.Stat(StatusPath(f.dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("a temporary status file was left behind")
	}
	b, err := os.ReadFile(StatusPath(f.dir))
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("the published status is not valid JSON: %v", err)
	}
}

func TestOnlyRenewableSlotsAreKept(t *testing.T) {
	f := newFixture(t)
	// A key is lent. Nothing renews a key, so the renewer has no work.
	in := &state.Instance{Name: "box", Group: state.DefaultGroup, Type: "agent", Engine: "podman"}
	if err := state.SaveGroup(f.instDir, &state.Group{Name: state.DefaultGroup}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(f.instDir, in); err != nil {
		t.Fatal(err)
	}
	loans := []lend.Loan{{Token: "loan_x", Label: "l", Slot: "anthropic", Kind: lend.Key}}
	if err := lend.WriteLoans(state.Dir(f.instDir, in.Group, in.Name), loans); err != nil {
		t.Fatal(err)
	}
	if got := f.k.lentRenewableSlots(); len(got) != 0 {
		t.Errorf("a lent key was treated as something to keep alive: %v", got)
	}
}

func TestMissingConfigIsRefusedRatherThanGuessed(t *testing.T) {
	for _, c := range []Config{
		{Dir: "d", InstDir: "i", StateDir: "l"},
		{Home: "h", InstDir: "i", StateDir: "l"},
		{Home: "h", Dir: "d", StateDir: "l"},
		{Home: "h", Dir: "d", InstDir: "i"},
	} {
		if _, err := newRenewer(c); err == nil {
			t.Errorf("accepted an incomplete config: %+v", c)
		}
	}
}

// A renewal in flight must survive shutdown. A client killed between the server
// rotating its refresh token and the client writing the new one leaves a dead
// token on disk, which logs the human out of their own agent — the single failure
// the lending design exists to exclude.
func TestShutdownDoesNotInterruptARenewalInFlight(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.setClaudeExpiry(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	var sawCancelled bool
	// The context the runner receives is the one that decides whether a real
	// client would be killed, so the assertion is on that rather than on ctx.
	f.k.cfg.Run = func(rctx context.Context, spec lend.RenewSpec, stage string) error {
		f.runs = append(f.runs, spec)
		cancel()
		if rctx.Err() != nil {
			sawCancelled = true
		}
		return stageClaudeExpiry(t, stage, f.now)
	}

	f.k.tick(ctx)
	if len(f.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.runs))
	}
	if sawCancelled {
		t.Error("the renewal's context was cancelled by shutdown, so a real client would have been killed mid-refresh")
	}
	st := f.status()
	if st.Slots[0].Err != "" {
		t.Errorf("the renewal was recorded as failed: %s", st.Slots[0].Err)
	}
}

// After a signal it finishes what is running and starts nothing further, so the
// work a stop has to wait for is bounded by one renewal rather than by however
// many slots are lent.
func TestShutdownStartsNoFurtherRenewals(t *testing.T) {
	f := newFixture(t)
	f.lend("claude")
	f.lend("codex")
	f.setClaudeExpiry(2 * time.Minute)
	f.setCodexExpiry(2 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	f.k.cfg.Run = func(_ context.Context, spec lend.RenewSpec, _ string) error {
		f.runs = append(f.runs, spec)
		cancel() // shutdown lands during the first slot's renewal
		return nil
	}

	f.k.tick(ctx)
	if len(f.runs) != 1 {
		t.Errorf("runs = %d after a shutdown during the first renewal, want 1", len(f.runs))
	}
}

// Nothing in flight means a prompt stop, not a wait for the full renewal budget.
func TestAttemptInFlightIsFalseWhenNoLockIsHeld(t *testing.T) {
	if attemptInFlight(t.TempDir()) {
		t.Error("an empty lock directory reported a renewal in flight")
	}
}

func TestAttemptInFlightIsCarefulWhenItCannotTell(t *testing.T) {
	if !attemptInFlight("") {
		t.Error("with no lock directory the careful answer is that one may be in flight")
	}
}

// stageClaudeExpiry stands in for the client inside the container: it rewrites the
// STAGED credential, which is the only thing a real renewal touches. The renewer is
// what copies the rotated fields back to the host.
func stageClaudeExpiry(t *testing.T, stage string, now time.Time) error {
	t.Helper()
	s, _ := lend.SlotByID("claude")
	spec, _ := s.RenewSpec()
	doc := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"rotated","refreshToken":"rotated","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		now.Add(8*time.Hour).UnixMilli(), now.Add(400*time.Hour).UnixMilli())
	return os.WriteFile(filepath.Join(stage, spec.ProfileDir, spec.File), []byte(doc), 0o600)
}
