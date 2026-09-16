package renew

import (
	"context"
	"fmt"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
)

// Renewing a credential once, at the moment it is first lent.
//
// The renewer's timer cannot cover this. It starts after a loan is recorded, so
// the one moment it is guaranteed not to help is the moment the sandbox is being
// built — and that is exactly when a credential nobody has used for days is
// found to be already stale. Measured on this host: a Codex login four days past
// expiry, unnoticed, which a create would then refuse.
//
// Refusing is defensible and reads as a tool being less capable than it is: the
// renewer would renew that same credential unattended five minutes into a run, so
// declining to renew it while a person is standing there is an asymmetry with no
// reason behind it.

// Outcome is what Now did, for a caller that has to report it.
type Outcome int

const (
	// Fresh means the credential had enough life left that its client would not
	// have refreshed it, so nothing was run. Nudging here would spend a real turn
	// and change nothing.
	Fresh Outcome = iota
	// Renewed means a client was run and the expiry moved.
	Renewed
	// AlreadyRenewed means a concurrent create or the renewer renewed it while
	// this call waited on the lock, so nothing was run here.
	AlreadyRenewed
	// NotRenewable means nothing renews this slot, which is every key.
	NotRenewable
)

// Result is the result of one Freshen, with enough detail to report it.
type Result struct {
	Outcome Outcome
	// Expires is the credential's expiry after the call, so a caller can say how
	// much life the sandbox is actually starting with.
	Expires time.Time
	// WasExpired records that the credential had already lapsed when Freshen
	// looked, which is worth saying out loud rather than quietly repairing.
	WasExpired bool
}

// Now renews one slot's credential now, if it needs renewing and something
// can.
//
// It acts in exactly two cases: the credential has expired, or it is inside its
// client's own refresh window. Outside those, the client would decline to refresh
// and the turn would be wasted — see lend.Slot.RenewWithin.
//
// It refuses rather than renewing when the refresh chain itself has ended, and it
// says so in those words, because that is the one failure here whose remedy is a
// person signing in and no amount of retrying reaches it.
func Now(ctx context.Context, cfg Config, s lend.Slot) (Result, error) {
	if !s.Renewable() {
		return Result{Outcome: NotRenewable}, nil
	}
	cfg, err := cfg.prepare(false)
	if err != nil {
		return Result{}, err
	}

	exp, ok, err := s.ExpiresAt(cfg.Home, cfg.KeysDir)
	if err != nil {
		// Missing or unreadable. The error already names the file and the command
		// that creates one, so it is returned as it is.
		return Result{}, err
	}
	if !ok {
		return Result{Outcome: NotRenewable}, nil
	}

	now := cfg.Now()
	out := Result{Expires: exp, WasExpired: !exp.After(now)}
	if exp.Sub(now) > s.RenewWithin() {
		out.Outcome = Fresh
		return out, nil
	}

	// Nothing below can help once the chain has ended, and trying would spend a
	// turn to produce a worse error message than this one.
	if dl, ok, err := s.RefreshDeadline(cfg.Home, cfg.KeysDir); err == nil && ok && !dl.After(now) {
		return out, fmt.Errorf("the host's %s login can no longer be renewed: its refresh window ended at %s\n"+
			"  refreshing does not extend that, so sign in again on the host and then create this sandbox",
			s.ID, dl.Local().Format(time.RFC3339))
	}

	// Attempted whatever the backoff says, and recorded either way.
	//
	// Attempted, because a person running create is an explicit action and may
	// have just fixed the login — refusing until a timer expires would leave them
	// unable to retry the thing they came to do. Recorded, because the renewer must
	// not then spend a second turn discovering the same failure a moment later.
	ran, err := renewNow(ctx, cfg, s, exp, true)
	if err != nil {
		if ran {
			RecordFailure(cfg.StateDir, s.ID, cfg.Now(), err)
		}
		return out, fmt.Errorf("the host's %s login needed renewing before it could be lent, and that failed: %w", s.ID, err)
	}
	if ran {
		RecordSuccess(cfg.StateDir, s.ID, cfg.Now())
	}
	if after, ok, err := s.ExpiresAt(cfg.Home, cfg.KeysDir); err == nil && ok {
		out.Expires = after
	}
	if ran {
		out.Outcome = Renewed
	} else {
		out.Outcome = AlreadyRenewed
	}
	return out, nil
}
