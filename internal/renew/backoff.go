package renew

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lock"
)

// How long to leave a slot alone after a renewal that failed, and why that has to
// survive the process.
//
// Every attempt is a real model turn billed to somebody's subscription, so a login
// that is genuinely broken must not be retried on every tick: at a 30-second poll
// that is ~2,900 turns a day against something that cannot work. The table below
// settles a permanently broken login at 48.
//
// The state cannot live in memory, which is where it started. A renewer that begins
// with a zero failure count renews immediately on its first tick however many times
// its predecessor already failed, and nothing else on disk can stand in for it — an
// expiry says when to try, never how often trying has already failed. Three
// ordinary things reset it: a renewer that died and was restarted by the next
// create, a destroy that stopped the renewer before a create started a new one, and
// create's own one-shot renewal, which does not go through the loop at all.
//
// It is also what makes supervision safe to add. Under `--restart=always`, or a
// systemd unit, a renewer that crashes mid-renewal would otherwise come back and
// renew again at once — crash, restart, turn, crash — spending real money in a loop.
//
// Host-global, beside the per-slot locks rather than under the renewer's own
// directory, because brokenness is a property of the credential: two instances
// roots renewing one login must not each keep their own idea of how badly it is
// going and retry twice as often.

// backoff is the wait after each consecutive failure, with the last value
// repeating.
//
// The ceiling is deliberately not longer. Once a credential is actually past
// expiry a retry is the most likely thing to work, because the client must refresh
// to serve any request at all — so backing off for hours would turn a recoverable
// state into a dead one.
var backoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// SlotRetry is one slot's retry state, as it survives on disk.
type SlotRetry struct {
	// Fails is the consecutive failure count, which indexes backoff.
	Fails int `json:"fails"`
	// NextAt is the earliest the slot may be attempted again. Absolute rather
	// than a remaining duration, so a renewer that was down for the whole wait
	// comes back ready rather than starting it over.
	NextAt      time.Time `json:"next_at,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastOK      time.Time `json:"last_ok,omitzero"`
	LastErr     string    `json:"last_err,omitempty"`
}

type retryFile struct {
	Slots map[string]SlotRetry `json:"slots"`
}

func retryPath(stateDir string) string { return filepath.Join(stateDir, "retry.json") }

// ReadRetry returns the retry state for every slot. A missing file is no state
// rather than an error: nothing has failed yet.
func ReadRetry(stateDir string) map[string]SlotRetry {
	b, err := os.ReadFile(retryPath(stateDir))
	if err != nil {
		return map[string]SlotRetry{}
	}
	var doc retryFile
	if err := json.Unmarshal(b, &doc); err != nil || doc.Slots == nil {
		return map[string]SlotRetry{}
	}
	return doc.Slots
}

// UpdateRetry applies fn to one slot's state and writes the result.
//
// Read-modify-write under a lock, because the renewer's loop and create's one-shot
// renewal both record into this file and are different processes. The lock is the
// host-global one for the same reason the file is.
func UpdateRetry(stateDir, slot string, fn func(*SlotRetry)) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	l := lock.NewAt(filepath.Join(stateDir, "retry.lock"))
	if err := l.Acquire(); err != nil {
		return err
	}
	defer l.Release()

	slots := ReadRetry(stateDir)
	st := slots[slot]
	fn(&st)
	slots[slot] = st

	b, err := json.MarshalIndent(retryFile{Slots: slots}, "", "  ")
	if err != nil {
		return err
	}
	tmp := retryPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, retryPath(stateDir))
}

// RecordFailure advances the backoff for a slot and remembers why.
func RecordFailure(stateDir, slot string, now time.Time, cause error) {
	_ = UpdateRetry(stateDir, slot, func(st *SlotRetry) {
		st.Fails++
		st.LastAttempt = now
		st.LastErr = cause.Error()
		st.NextAt = now.Add(backoffFor(st.Fails))
	})
}

// RecordSuccess clears the backoff for a slot.
func RecordSuccess(stateDir, slot string, now time.Time) {
	_ = UpdateRetry(stateDir, slot, func(st *SlotRetry) {
		st.Fails = 0
		st.LastAttempt = now
		st.LastOK = now
		st.LastErr = ""
		st.NextAt = time.Time{}
	})
}

// Blocked reports how long a slot must still be left alone, or zero if it may be
// attempted now.
func Blocked(stateDir, slot string, now time.Time) time.Duration {
	st := ReadRetry(stateDir)[slot]
	if st.NextAt.IsZero() || !now.Before(st.NextAt) {
		return 0
	}
	return st.NextAt.Sub(now)
}

func backoffFor(fails int) time.Duration {
	if fails <= 0 {
		return backoff[0]
	}
	if fails > len(backoff) {
		return backoff[len(backoff)-1]
	}
	return backoff[fails-1]
}

// describeRetry is the "3 consecutive failures, next attempt in 10m" line, for a
// caller reporting why nothing is happening.
func describeRetry(st SlotRetry, now time.Time) string {
	if st.Fails == 0 {
		return ""
	}
	if left := st.NextAt.Sub(now); left > 0 {
		return fmt.Sprintf("%d consecutive failures, next attempt in %s", st.Fails, left.Round(time.Second))
	}
	return fmt.Sprintf("%d consecutive failures", st.Fails)
}
