// Package renew keeps a lent host login from going stale under a sandbox.
//
// The lender swaps a loan token for the host's real credential on every model
// call and refreshes nothing, by design — internal/lend/cred.go says why. The
// cost of that design is that a login nothing renews goes stale, so a long
// unattended sandbox run, which is the case the tool exists for, dies partway
// through with the host's login reported as expired.
//
// This closes that gap the only way the design allows: the agent that owns a
// login is the thing that may renew it, so the renewer runs a real turn through
// that agent's own client shortly before the credential expires. The client
// spends its own refresh token and rewrites its own file, and the lender's
// per-request read picks the new value up with no restart and no daemon state.
// The renewer holds no refresh token and implements no vendor's sign-in.
//
// Three properties are load-bearing, and each was a wrong turn first:
//
//   - It runs on the HOST. The obvious home for this is the lender, which is
//     already long-lived and already reads these files — but the lender is a
//     container that mounts the agent home read-only, so a client started in
//     there cannot write the credential it was launched to refresh.
//   - It is HOST-GLOBAL, where a lender is per group. There is one ~/.cs-claude
//     per host, so a renewer per group would put two clients into the same few
//     minutes and race the token rotation. Whichever writer lost would leave a
//     dead refresh token behind and log the human out of their own agent, which
//     is the single failure the lending design is built to exclude.
//   - It VERIFIES. A client refreshes on its own threshold, not on ours: a
//     attempt sent too early spends a real turn, finds a valid token, refreshes
//     nothing, and exits 0. So every attempt re-reads the expiry, and a run
//     that did not move it is a failure however well the command went.
package renew

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/lock"
	"github.com/codesweep-ai/sandbox/internal/state"
)

const (
	// minPoll is how closely the renewer watches once a window is near. A check
	// is one file read and one JSON parse, so watching this closely costs
	// nothing measurable, and being late costs a sandbox a whole turn.
	minPoll = 30 * time.Second

	// idlePoll is the interval while every credential is hours from its window.
	idlePoll = 5 * time.Minute

	// attemptTimeout bounds one renew command. Cold client starts were measured at
	// 2.8s (claude -p) and 6.6s (codex exec), so a minute is generous while
	// still leaving most of Claude's four-minute window unspent.
	attemptTimeout = 60 * time.Second
)

// Config is everything the renewer needs. Every path arrives as a parameter, the
// way internal/lend takes them, so a test drives the whole loop against a
// temporary home without touching the real one.
type Config struct {
	// Home is the agent-login home whose profiles are kept alive: the same one
	// the lender reads, or nothing renewed here is the credential being lent.
	Home    string
	KeysDir string
	InstDir string

	// Dir is the renewer's own working directory: pidfile, status, log, and the
	// empty directory renew commands run in. One per instances root.
	Dir string

	// StateDir holds the per-slot single-flight locks, and is host-global where
	// Dir is not. What a attempt contends for is the credential, and there is one
	// of those per host however many instances roots exist — see paths.RenewerState.
	StateDir string

	// Image is the sandbox image a renewal runs in, and ImageErr says why there
	// is none when there is none. A renewal needs the image rather than a client
	// on this host's PATH — see container.go.
	Image    string
	ImageErr string

	Log *slog.Logger

	// Now and Run are seams for tests: the clock, and what actually performs a
	// renewal against a staged credential.
	Now func() time.Time
	Run func(context.Context, lend.RenewSpec, string) error
}

// Status is what the renewer publishes for `doctor` to render.
//
// It exists because the failure this whole mechanism has to avoid is a silent
// one. A renewer that runs, spends turns and extends nothing looks exactly like a
// renewer that is working, so what it last did has to be readable from outside
// the process. Updated is as important as the rest: a status file that has
// stopped moving says the renewer is dead or wedged, which no field inside it
// could report for itself.
type Status struct {
	PID     int          `json:"pid"`
	Updated time.Time    `json:"updated"`
	Slots   []SlotStatus `json:"slots"`
}

// SlotStatus is what the renewer knows about one lent credential.
type SlotStatus struct {
	Slot string `json:"slot"`
	// ExpiresAt is when the credential goes stale; RefreshDeadline is when
	// refreshing stops being able to help at all. The second is absent for a
	// slot whose credential states no such bound.
	// omitzero, not omitempty: a struct is never "empty", so omitempty leaves a
	// never-attempted slot claiming a last attempt in the year 1.
	ExpiresAt       time.Time `json:"expires_at,omitzero"`
	RefreshDeadline time.Time `json:"refresh_deadline,omitzero"`
	LastAttempt     time.Time `json:"last_attempt,omitzero"`
	LastOK          time.Time `json:"last_ok,omitzero"`
	// Err is why the last attempt did not renew the login, including the case
	// where the command itself succeeded and the expiry did not move.
	Err string `json:"err,omitempty"`
}

// Run drives the renewer until ctx is done. It is the body of `cs-sandbox renewer`.
func Run(ctx context.Context, cfg Config) error {
	k, err := newRenewer(cfg)
	if err != nil {
		return err
	}
	k.cfg.Log.Info("keeping lent logins alive",
		slog.String("home", cfg.Home), slog.String("dir", cfg.Dir))
	for {
		wait := k.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

type renewer struct {
	cfg Config
}

func newRenewer(cfg Config) (*renewer, error) {
	cfg, err := cfg.prepare(true)
	if err != nil {
		return nil, err
	}
	return &renewer{cfg: cfg}, nil
}

// prepare fills in the seams and makes the directories. needInstDir is false for
// the one-shot path, which is handed the slot rather than discovering it.
func (cfg Config) prepare(needInstDir bool) (Config, error) {
	if cfg.Home == "" || cfg.Dir == "" || cfg.StateDir == "" {
		return cfg, errors.New("renewer: Home, Dir and StateDir are all required")
	}
	if needInstDir && cfg.InstDir == "" {
		return cfg, errors.New("renewer: InstDir is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// Where credentials are staged for a renewal. Owner-only: for as long as one
	// runs, a real credential lives under here.
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "stage"), 0o700); err != nil {
		return cfg, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// tick looks at every lent login once and returns how long to wait.
func (k *renewer) tick(ctx context.Context) time.Duration {
	slots := k.lentRenewableSlots()
	if len(slots) == 0 {
		// Nothing is borrowed. destroy stops the renewer, so this is the window
		// between the last loan going away and that happening, plus the case of
		// a renewer left behind by a create that failed.
		k.publish(nil)
		return idlePoll
	}
	now := k.cfg.Now()
	// Capped at idlePoll even when the nearest window is hours away. Sleeping
	// through to it would be cheaper by a rounding error and would leave the
	// renewer blind in between: a sandbox created since the last tick, a
	// credential replaced by somebody signing in again, a clock that jumped. A
	// file read every five minutes is the price of not having to reason about
	// any of that.
	wait := idlePoll
	out := make([]SlotStatus, 0, len(slots))
	for _, s := range slots {
		// Checked between slots, so a shutdown that arrives mid-tick finishes the
		// renewal already running and starts no further one. Without this, a
		// signal could still be followed by a fresh attempt for every remaining
		// slot, since renewNow deliberately ignores cancellation.
		if ctx.Err() != nil {
			break
		}
		st, next := k.consider(ctx, s, now)
		out = append(out, st)
		wait = min(wait, next)
	}
	k.publish(out)
	return max(wait, minPoll)
}

// consider decides what to do about one slot, and when to look again.
func (k *renewer) consider(ctx context.Context, s lend.Slot, now time.Time) (SlotStatus, time.Duration) {
	st := SlotStatus{Slot: s.ID}
	// Read rather than remembered: the backoff outlives this process, so that a
	// renewer restarted by the next create does not begin as though nothing had
	// ever failed. See backoff.go.
	retry := ReadRetry(k.cfg.StateDir)[s.ID]
	st.LastAttempt, st.LastOK, st.Err = retry.LastAttempt, retry.LastOK, retry.LastErr

	exp, ok, err := s.ExpiresAt(k.cfg.Home, k.cfg.KeysDir)
	if err != nil {
		// The credential cannot be read at all. Nothing a attempt fixes, and the
		// lender is reporting it per request already, so this only records it.
		st.Err = err.Error()
		return st, idlePoll
	}
	if !ok {
		return st, idlePoll
	}
	st.ExpiresAt = exp
	if dl, ok, err := s.RefreshDeadline(k.cfg.Home, k.cfg.KeysDir); err == nil && ok {
		st.RefreshDeadline = dl
	}

	remaining := exp.Sub(now)
	if remaining > s.RenewWithin() {
		// Outside the client's window, where a attempt would refresh nothing. Wake
		// when it opens.
		return st, remaining - s.RenewWithin()
	}
	if wait := Blocked(k.cfg.StateDir, s.ID, now); wait > 0 {
		return st, wait
	}

	st.LastAttempt = now
	err = k.attempt(ctx, s, exp)
	switch {
	case err == nil:
		RecordSuccess(k.cfg.StateDir, s.ID, k.cfg.Now())
		st.LastOK, st.Err = k.cfg.Now(), ""
		k.cfg.Log.Info("renewed", slog.String("slot", s.ID))
		return st, minPoll
	case errors.Is(err, errBusy):
		// Another holder of the per-slot lock is on it. Not a failure, and not
		// something to count against the slot.
		return st, minPoll
	default:
		RecordFailure(k.cfg.StateDir, s.ID, k.cfg.Now(), err)
		after := ReadRetry(k.cfg.StateDir)[s.ID]
		st.Err = after.LastErr
		k.cfg.Log.Error("could not renew", slog.String("slot", s.ID),
			slog.Int("consecutive_failures", after.Fails),
			slog.String("retry", describeRetry(after, k.cfg.Now())), slog.Any("err", err))
		return st, max(after.NextAt.Sub(k.cfg.Now()), minPoll)
	}
}

// errBusy means another renewer, or another attempt for the same slot, holds the
// per-slot lock.
var errBusy = errors.New("a attempt for this slot is already running")

// attempt runs the client's own renew command and then proves it worked.
//
// The proof is the whole point. The command exiting 0 says only that a turn
// completed; the client decides for itself whether the token needed refreshing,
// and when it decides no, everything looks successful and nothing was extended.
// So the expiry is read again and has to have moved.
func (k *renewer) attempt(ctx context.Context, s lend.Slot, before time.Time) error {
	_, err := renewNow(ctx, k.cfg, s, before, false)
	return err
}

// renewNow runs a slot's client under the per-slot lock and proves the expiry
// moved.
//
// Shared by the renewer's timer and by create's one-shot freshen, because the
// verification is the part neither of them may skip: a client decides for itself
// whether to refresh, so a command that exits 0 says nothing about whether the
// credential was extended.
//
// wait picks the contention behaviour, and the two callers genuinely differ. The
// renewer is happy to skip a slot another holder is already renewing and look
// again in thirty seconds; create cannot proceed until the credential is good, so
// it waits for the other holder and then re-reads.
//
// ran reports whether a client was actually started, which is false when the
// credential turned out to be renewed already.
func renewNow(ctx context.Context, cfg Config, s lend.Slot, before time.Time, wait bool) (ran bool, err error) {
	spec, ok := s.RenewSpec()
	if !ok {
		return false, fmt.Errorf("nothing renews the %s slot", s.ID)
	}

	// Single-flight per slot, host-global: the lock is a file, so it holds
	// against a second renewer and a concurrent create as well as against a
	// second goroutine.
	l := lock.NewAt(filepath.Join(cfg.StateDir, s.ID+".attempt.lock"))
	if wait {
		if err := l.Acquire(); err != nil {
			return false, err
		}
	} else {
		held, err := l.TryAcquire()
		if err != nil {
			return false, err
		}
		if !held {
			return false, errBusy
		}
	}
	defer l.Release()

	// Asked again now that nobody else can be writing it. Whoever held the lock
	// may have just renewed the very credential this call was about to spend a
	// turn on.
	if after, ok, err := s.ExpiresAt(cfg.Home, cfg.KeysDir); err == nil && ok && after.After(before) {
		return false, nil
	}

	// Deliberately NOT cancelled by shutdown, only by its own timeout.
	//
	// This is the one place in the renewer where being killed is worse than being
	// slow. A client interrupted between the server rotating its refresh token
	// and the client writing the new one leaves a dead token on disk, and the
	// human is logged out of their own agent — which is the single failure the
	// whole lending design is built to exclude. The lender makes the same trade
	// for the same reason (see drainLender: "a model composing a reply is the
	// long case, and cutting one off costs the caller the whole turn"), and here
	// the cost of cutting one off is higher than a lost turn.
	//
	// Bounded regardless: attemptTimeout still applies, so shutdown waits for a
	// known-short operation rather than an open-ended one.
	// The host's credential is copied into a staging directory, renewed there, and
	// only the rotated values are copied back. The stage holds the real thing
	// while this runs, so it is owner-only and goes away either way.
	src := s.Source(cfg.Home, cfg.KeysDir)
	hostDoc, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("cannot read the host's %s credential to renew it: %w", s.ID, err)
	}
	stage := filepath.Join(cfg.Dir, "stage", s.ID)
	_ = os.RemoveAll(stage) // a stage left by a killed run must not be reused
	defer os.RemoveAll(stage)
	if err := stageCredential(stage, spec, hostDoc); err != nil {
		return false, fmt.Errorf("cannot stage the %s credential: %w", s.ID, err)
	}

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptTimeout)
	defer cancel()
	cfg.Log.Info("renewing", slog.String("slot", s.ID),
		slog.String("client", spec.Bin+" "+strings.Join(spec.Args, " ")),
		slog.Time("expires", before))
	if err := cfg.run(rctx, spec, stage); err != nil {
		return true, err
	}

	fresh, err := readStaged(stage, spec)
	if err != nil {
		return true, fmt.Errorf("renewing the %s login: %w", s.ID, err)
	}
	merged, err := s.MergeRefreshed(hostDoc, fresh)
	if err != nil {
		return true, err
	}
	// Checked before the host's file is touched. A renewal that ran cleanly and
	// rotated nothing must not overwrite a working credential with a copy of
	// itself, and the client decides for itself whether to refresh.
	if !movedForward(s, hostDoc, merged) {
		return true, fmt.Errorf("the %s client ran and exited cleanly, but the login's expiry did not move: "+
			"the client refreshes on its own threshold, so this was sent too early — or that threshold changed", s.ID)
	}
	if err := writeHostCredential(src, merged); err != nil {
		return true, fmt.Errorf("cannot write the renewed %s credential to %s: %w", s.ID, src, err)
	}

	after, _, err := s.ExpiresAt(cfg.Home, cfg.KeysDir)
	if err != nil {
		return true, fmt.Errorf("the %s login was renewed, but cannot be read back: %w", s.ID, err)
	}
	if !after.After(before) {
		return true, fmt.Errorf("the %s login's expiry did not move (still %s) after a renewal that appeared to work",
			s.ID, after.Local().Format(time.RFC3339))
	}
	return true, nil
}

// run performs one renewal: the container by default, or whatever a test injected.
func (cfg Config) run(ctx context.Context, spec lend.RenewSpec, stage string) error {
	if cfg.Run != nil {
		return cfg.Run(ctx, spec, stage)
	}
	return runInContainer(ctx, cfg, spec, stage)
}

// movedForward reports whether the merged document carries a later expiry than the
// host's current one.
//
// Asked of the documents rather than of the file, so the host's credential is never
// replaced by a renewal that achieved nothing. Unparseable either way means "cannot
// prove it moved", which is the careful answer.
func movedForward(s lend.Slot, hostDoc, merged []byte) bool {
	was, err := s.ExpiryOf(hostDoc)
	if err != nil {
		return false
	}
	now, err := s.ExpiryOf(merged)
	if err != nil {
		return false
	}
	return now.After(was)
}

// lentRenewableSlots is the set of slots some sandbox on this host is currently
// borrowing and something can renew.
//
// Derived from the loan records rather than from state of its own, which is the
// same enumeration stopLenderIfIdle uses to decide the lender is idle. A renewer
// that kept its own list could disagree with it.
func (k *renewer) lentRenewableSlots() []lend.Slot {
	insts, err := state.List(k.cfg.InstDir)
	if err != nil {
		k.cfg.Log.Error("cannot list sandboxes", slog.Any("err", err))
		return nil
	}
	var ids []string
	for _, in := range insts {
		loans, err := lend.ReadLoans(state.Dir(k.cfg.InstDir, in.Group, in.Name))
		if err != nil {
			// Said rather than skipped. A loan file this cannot read is a
			// sandbox whose borrowed credential will not be kept alive, which is
			// the exact shape of silent failure this package exists to remove.
			k.cfg.Log.Error("cannot read a sandbox's loans, so its credential will not be kept alive",
				slog.String("sandbox", in.Name), slog.String("group", in.Group), slog.Any("err", err))
			continue
		}
		for _, ln := range loans {
			if slices.Contains(ids, ln.Slot) {
				continue
			}
			if s, ok := lend.SlotByID(ln.Slot); ok && s.Renewable() {
				ids = append(ids, ln.Slot)
			}
		}
	}
	slices.Sort(ids) // stable status output across ticks
	out := make([]lend.Slot, 0, len(ids))
	for _, id := range ids {
		if s, ok := lend.SlotByID(id); ok {
			out = append(out, s)
		}
	}
	return out
}

// publish writes the status file, replacing it atomically so a reader never
// sees half of one.
func (k *renewer) publish(slots []SlotStatus) {
	st := Status{PID: os.Getpid(), Updated: k.cfg.Now(), Slots: slots}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := StatusPath(k.cfg.Dir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		k.cfg.Log.Error("cannot write renewer status", slog.Any("err", err))
		return
	}
	if err := os.Rename(tmp, StatusPath(k.cfg.Dir)); err != nil {
		k.cfg.Log.Error("cannot replace renewer status", slog.Any("err", err))
	}
}

// lastLine is the client's final line of output, which is where these tools put
// the reason they stopped.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		return last[:300] + "…"
	}
	return last
}

// StatusPath, pidPath and LogPath name the renewer's files inside its own dir.
func StatusPath(dir string) string { return filepath.Join(dir, "status.json") }
func LogPath(dir string) string    { return filepath.Join(dir, "renew.log") }
func pidPath(dir string) string    { return filepath.Join(dir, "renew.pid") }

// ReadStatus reads what the running renewer last published. A missing file is
// not an error: it means no renewer has run here, which is a thing a caller
// reports rather than a thing that went wrong.
func ReadStatus(dir string) (Status, bool, error) {
	b, err := os.ReadFile(StatusPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return Status{}, false, nil
		}
		return Status{}, false, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, false, fmt.Errorf("%s is not readable as renewer status: %w", StatusPath(dir), err)
	}
	return st, true, nil
}

// Start launches the renewer as a detached host child, unless one is already
// running here.
//
// Detached and released, the way forward.Start launches its ssh child: create
// exits as soon as the sandbox is built, and what keeps a login alive has to
// outlive it. There is nothing else on the host that lives long enough — the
// lender is a container, and a container cannot write the credential.
func Start(dir, bin string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Two creates in the same second would otherwise both find no renewer and
	// both start one, which is the race a host-global renewer exists to avoid.
	l := lock.NewAt(filepath.Join(dir, "start.lock"))
	if err := l.Acquire(); err != nil {
		return err
	}
	defer l.Release()

	if Running(dir) {
		return nil
	}
	logf, err := os.OpenFile(LogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(bin, "renewer")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // detach into its own group
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the credential renewer: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // keep running after this process returns

	// A renewer that died on startup must not be recorded as running, or the next
	// create finds a pidfile, starts nothing, and nothing keeps the login alive.
	time.Sleep(600 * time.Millisecond)
	if !alive(pid) {
		return fmt.Errorf("the credential renewer exited immediately; see %s", LogPath(dir))
	}
	// The start time goes in beside the pid, because a pid alone is not an
	// identity: pids are reused, and a recycled one makes Running() say yes about
	// somebody else's process — so the next create would start no renewer and
	// nothing would renew anything. Linux only; elsewhere this is empty and the
	// check falls back to liveness alone, which is what every other detached child
	// in this repo does.
	rec := strconv.Itoa(pid) + "\n"
	if started, ok := procStarted(pid); ok {
		rec += started + "\n"
	}
	return os.WriteFile(pidPath(dir), []byte(rec), 0o600)
}

// Stop ends the renewer and forgets its status, waiting for it to actually go.
//
// Waiting matters here, unlike in forward, where an interrupted `ssh -N` costs
// nothing. A renewer may be part-way through a renewal, and SIGKILL at that moment
// is the one thing that can invalidate the refresh token it was renewing. So the
// budget is set by whether a renewal is actually in flight: nothing running means
// a prompt exit and a short wait, and a renewal running means waiting for it.
// SIGKILL stays as a last resort for a renewer that has stopped responding
// altogether, the way killFirecracker escalates.
//
// Best effort throughout: a renewer that is already gone is the desired state, not
// a failure.
func Stop(dir, lockDir string) {
	pid := readPID(dir)
	defer func() {
		_ = os.Remove(pidPath(dir))
		_ = os.Remove(StatusPath(dir))
	}()
	if pid <= 0 || !alive(pid) {
		return
	}
	budget := stopGrace
	if attemptInFlight(lockDir) {
		budget = attemptTimeout + stopGrace
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// stopGrace is how long a renewer with nothing in flight gets to exit. It is
// sleeping between ticks almost always, so this is the time to notice a signal.
const stopGrace = 2 * time.Second

// attemptInFlight reports whether some renewal holds its slot's lock right now.
//
// Asked by trying to take each one: the locks are files, so this works across
// processes, which is the only frame of reference that answers the question — the
// renewal in progress belongs to a different process than the one stopping it.
func attemptInFlight(lockDir string) bool {
	if lockDir == "" {
		return true // cannot tell, so assume the careful answer
	}
	for _, id := range lend.SlotIDs(lend.Login) {
		s, ok := lend.SlotByID(id)
		if !ok || !s.Renewable() {
			continue
		}
		l := lock.NewAt(filepath.Join(lockDir, id+".attempt.lock"))
		held, err := l.TryAcquire()
		if err != nil {
			return true
		}
		if !held {
			return true
		}
		l.Release()
	}
	return false
}

// Running reports whether a renewer is alive here.
func Running(dir string) bool { return alive(readPID(dir)) }

// readPID returns the recorded pid, and only if the process there is still the
// one that was recorded.
func readPID(dir string) int {
	b, err := os.ReadFile(pidPath(dir))
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return 0
	}
	if len(lines) > 1 {
		want := strings.TrimSpace(lines[1])
		if got, ok := procStarted(pid); ok && got != want {
			return 0 // the pid was reused; this is not our renewer
		}
	}
	return pid
}

// procStarted is the process's start time as the kernel reports it, which with the
// pid is an identity that survives reuse.
//
// Field 22 of /proc/<pid>/stat, in clock ticks since boot. Read as an opaque
// string, because nothing here needs to interpret it — only to notice that it
// changed. The comm field can contain spaces and parentheses, so the fields are
// counted from the last ')' rather than from the start of the line.
func procStarted(pid int) (string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	line := string(b)
	close := strings.LastIndex(line, ")")
	if close < 0 {
		return "", false
	}
	fields := strings.Fields(line[close+1:])
	// After the comm field, stat continues at field 3 (state), so field 22 is
	// index 19 here.
	if len(fields) < 20 {
		return "", false
	}
	return fields[19], true
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
