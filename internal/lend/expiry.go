package lend

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// How long a lent login has left, and what renews it.
//
// The lender does neither of these things. It reads a credential per request
// and forwards it, and cred.go says why it must never refresh one itself.
// Everything here exists for the host-side renewer, which is a different process
// on the other side of the container boundary — see internal/renew.
//
// So this file holds reads and data only: no exec, no vendor sign-in, and
// nothing that writes a credential. A package that can read every credential on
// the host is a small thing to trust; the same package able to start processes
// would not be.

// ExpiresAt is when this slot's credential goes stale.
//
// The bool is false for a slot that carries no expiry at all. An API key does
// not have one, so "there is nothing to expire" is an answer rather than a
// failure — a caller that could not tell those apart would report every key on
// the host as a credential in trouble.
func (s Slot) ExpiresAt(home, keysDir string) (time.Time, bool, error) {
	if s.expires == nil {
		return time.Time{}, false, nil
	}
	t, err := s.expires(home, keysDir)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// RefreshDeadline is when this slot's refresh chain ends: the point past which
// no amount of refreshing helps and only an interactive sign-in does.
//
// It is a different clock from ExpiresAt and it does not move. Claude's access
// token lives 8 hours and is renewed indefinitely inside a refresh-token
// lifetime of about 20 days, and refreshing does not extend that outer bound —
// measured across a real rotation, the value came back identical to the second.
// A renewer that reported only the inner clock would press on confidently into a
// wall, so the outer one is readable here.
//
// The bool is false for a slot whose credential states no such deadline, which
// includes Codex: its file carries no equivalent field.
func (s Slot) RefreshDeadline(home, keysDir string) (time.Time, bool, error) {
	if s.deadline == nil {
		return time.Time{}, false, nil
	}
	t, err := s.deadline(home, keysDir)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// Renewable reports whether some client can be asked to refresh this slot.
// False for a key, which no client renews.
func (s Slot) Renewable() bool { return s.renew != nil }

// RenewWithin is how close to expiry an attempt has to be sent to do anything.
//
// It is the client's threshold, not a preference of ours, and that inverts the
// buffer a renewer would otherwise pick for itself. Claude Code refreshes only
// in the last few minutes before expiry: measured on a live profile, the file
// was rewritten 4m56s ahead of an 8-hour expiry, while 24 sessions across the
// preceding 8 hours — fresh launches among them — rewrote nothing at all. So a
// attempt made at "30 minutes remaining" spends a real turn, finds a valid token,
// refreshes nothing, and looks like it worked.
//
// Treat these as observations of undocumented internal behaviour rather than as
// published constants. They can move with any client release, which is why the
// renewer verifies the expiry actually changed instead of trusting an exit code.
func (s Slot) RenewWithin() time.Duration { return s.renewWithin }

// ExpiryOf reads the expiry out of a credential document held in memory.
//
// Separate from ExpiresAt, which reads the file, because a renewal has to compare
// the expiry it is about to write against the one already there — and must do that
// before touching the host's credential, not after.
func (s Slot) ExpiryOf(doc []byte) (time.Time, error) {
	if s.expiryOf == nil {
		return time.Time{}, fmt.Errorf("the %s slot's credential states no expiry", s.ID)
	}
	return s.expiryOf(doc)
}

// claudeExpiry is when the host's Claude access token goes stale.
func claudeExpiry(home, _ string) (time.Time, error) {
	_, p, err := readClaudeOAuth(home)
	if err != nil {
		return time.Time{}, err
	}
	doc, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}, err
	}
	return claudeExpiryOf(doc)
}

// claudeExpiryOf is the same read against a document rather than a path.
func claudeExpiryOf(doc []byte) (time.Time, error) {
	var d struct {
		OAuth claudeOAuth `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return time.Time{}, fmt.Errorf("not readable as a Claude login: %w", err)
	}
	if d.OAuth.ExpiresAt == 0 {
		return time.Time{}, errors.New("the Claude login states no expiry")
	}
	return time.UnixMilli(d.OAuth.ExpiresAt), nil
}

// claudeRefreshDeadline is the end of the Claude refresh chain. See
// Slot.RefreshDeadline for why this is read at all.
func claudeRefreshDeadline(home, _ string) (time.Time, error) {
	o, p, err := readClaudeOAuth(home)
	if err != nil {
		return time.Time{}, err
	}
	if o.RefreshTokenExpiresAt == 0 {
		return time.Time{}, fmt.Errorf("%s states no refresh-token expiry", p)
	}
	return time.UnixMilli(o.RefreshTokenExpiresAt), nil
}

// codexExpiry decodes the exp claim of the ChatGPT access token.
//
// Codex keeps no expiry field of its own, so the claim inside the token is the
// only statement of when it dies. Only the payload is decoded and only exp is
// read; the signature is not checked, because this is not an authorization
// decision. It is the provider's own statement about a credential this host
// already holds, read to know when to ask the client to renew it.
func codexExpiry(home, _ string) (time.Time, error) {
	_, p, err := readCodexTokens(home)
	if err != nil {
		return time.Time{}, err
	}
	doc, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}, err
	}
	exp, err := codexExpiryOf(doc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", p, err)
	}
	return exp, nil
}

// codexExpiryOf is the same read against a document rather than a path.
func codexExpiryOf(doc []byte) (time.Time, error) {
	var d struct {
		Tokens codexTokens `json:"tokens"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return time.Time{}, fmt.Errorf("not readable as a Codex login: %w", err)
	}
	return jwtExpiry(d.Tokens.AccessToken)
}

// jwtExpiry reads the exp claim out of a JWT's payload without verifying it.
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("the access token is not a JWT, so it states no expiry")
	}
	// Unpadded base64url is what a JWT uses, but a producer that pads is not
	// malformed, so the padding is trimmed rather than rejected.
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("the access token's payload is not base64url: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("the access token's payload is not JSON: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("the access token carries no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}
