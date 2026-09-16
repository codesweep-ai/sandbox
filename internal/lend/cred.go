package lend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Reading the host's real credentials.
//
// Every read happens per request rather than at startup, and nothing here
// refreshes anything. The agent that owns a login is the thing that renews it,
// in the way its vendor supports; the lender only ever reads the current value,
// so it holds no refresh token, implements no vendor's sign-in, and cannot
// invalidate a session by racing the agent that owns it. The cost is that a
// login nothing has refreshed goes stale, which this reports as itself rather
// than as an upstream 401.

// readClaudeLogin returns the OAuth access token Claude Code keeps in the
// cs-claude profile.
//
// The extra header is the one Claude Code sends with an OAuth token and not
// with an API key. A sandbox pointed here sends neither, because it is holding
// a loan token and does not know what it stands for — so the shape has to be
// restored on this side.
func readClaudeLogin(home, _ string) (string, map[string]string, error) {
	o, p, err := readClaudeOAuth(home)
	if err != nil {
		return "", nil, err
	}
	if o.AccessToken == "" {
		return "", nil, missing("Claude", p, "cs-claude")
	}
	if o.ExpiresAt > 0 {
		if exp := time.UnixMilli(o.ExpiresAt); time.Now().After(exp) {
			return "", nil, fmt.Errorf("the host's Claude login expired at %s — run 'cs-claude' on the host to refresh it",
				exp.Local().Format(time.RFC3339))
		}
	}
	return o.AccessToken, map[string]string{"anthropic-beta": "oauth-2025-04-20"}, nil
}

// claudeOAuth is the part of Claude Code's credential file this tool reads.
//
// The refresh token is deliberately not a field. Nothing on this side has any
// use for it — see the header comment — and a field that does not exist cannot
// be logged, copied or serialized by a later mistake.
type claudeOAuth struct {
	AccessToken string `json:"accessToken"`
	ExpiresAt   int64  `json:"expiresAt"` // epoch milliseconds
	// RefreshTokenExpiresAt is the end of the refresh chain: the point past
	// which refreshing cannot help and only an interactive sign-in can. See
	// Slot.RefreshDeadline.
	RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt"` // epoch milliseconds
}

// readClaudeOAuth reads the credential file, returning the path beside the
// document so every caller's error can name the file rather than the field.
func readClaudeOAuth(home string) (claudeOAuth, string, error) {
	p := filepath.Join(home, ".cs-claude", ".credentials.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return claudeOAuth{}, p, missing("Claude", p, "cs-claude")
	}
	var doc struct {
		OAuth claudeOAuth `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return claudeOAuth{}, p, fmt.Errorf("%s is not readable as a Claude login: %w", p, err)
	}
	return doc.OAuth, p, nil
}

// readCodexLogin returns the ChatGPT access token Codex keeps in the cs-codex
// profile, with the account id that has to travel beside it.
func readCodexLogin(home, _ string) (string, map[string]string, error) {
	t, p, err := readCodexTokens(home)
	if err != nil {
		return "", nil, err
	}
	if t.AccessToken == "" {
		return "", nil, missing("Codex", p, "cs-codex login")
	}
	// Staleness reported as itself, the way the Claude slot reports it, which is
	// what the header comment above claims this package does. It did not hold
	// for Codex until this check existed: the token carries its expiry inside
	// itself rather than in a field beside it, so an expired one was forwarded
	// and came back as an upstream 401 — the one shape of failure a reader
	// cannot act on, because it is also what a revoked token, a wrong account
	// and a provider outage look like.
	//
	// Only a decoded expiry in the past refuses. A token this build cannot
	// decode is forwarded rather than withheld: not understanding a credential's
	// shape is not evidence that it is dead, and refusing on that would break
	// lending the first time the provider changes the token format.
	if exp, err := jwtExpiry(t.AccessToken); err == nil && time.Now().After(exp) {
		return "", nil, fmt.Errorf("the host's Codex login expired at %s — run 'cs-codex' on the host to refresh it",
			exp.Local().Format(time.RFC3339))
	}
	extra := map[string]string{}
	if t.AccountID != "" {
		extra["chatgpt-account-id"] = t.AccountID
	}
	return t.AccessToken, extra, nil
}

// codexTokens is the part of Codex's auth.json this tool reads. As with
// claudeOAuth, the refresh token is deliberately absent.
type codexTokens struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

// readCodexTokens reads auth.json, returning the path beside the document.
func readCodexTokens(home string) (codexTokens, string, error) {
	p := filepath.Join(home, ".cs-codex", "auth.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return codexTokens{}, p, missing("Codex", p, "cs-codex login")
	}
	var doc struct {
		Tokens codexTokens `json:"tokens"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return codexTokens{}, p, fmt.Errorf("%s is not readable as a Codex login: %w", p, err)
	}
	return doc.Tokens, p, nil
}

// keyReader reads one provider key file. The whole file is the key, trimmed,
// so writing one is a shell redirect and reading one needs no parser.
func keyReader(provider string) func(string, string) (string, map[string]string, error) {
	return func(_, keysDir string) (string, map[string]string, error) {
		p := filepath.Join(keysDir, provider)
		data, err := os.ReadFile(p)
		if err != nil {
			return "", nil, fmt.Errorf("no %s key to lend: %s does not exist — %s", provider, p, saveKeyHint(provider, p))
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", nil, fmt.Errorf("no %s key to lend: %s is empty — %s", provider, p, saveKeyHint(provider, p))
		}
		return key, nil, nil
	}
}

func keyPath(provider string) func(string, string) string {
	return func(_, keysDir string) string { return filepath.Join(keysDir, provider) }
}

// saveKeyHint is the remedy, in the form of the command that fixes it. The
// variable named is the one that provider's own clients read, which is where a
// caller who has a key already has it.
func saveKeyHint(provider, path string) string {
	env := map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY"}[provider]
	if env == "" {
		env = strings.ToUpper(provider) + "_API_KEY"
	}
	return fmt.Sprintf("save one with:  mkdir -p %s && printf %%s \"$%s\" > %s && chmod 600 %s",
		filepath.Dir(path), env, path, path)
}

// missing is the "no login to lend" error, which names the command that creates
// one rather than the file that is absent.
func missing(agent, path, loginCmd string) error {
	return fmt.Errorf("no host %s login to lend: %s does not exist — run '%s' on the host first", agent, path, loginCmd)
}

// Read returns the real credential this slot lends, and the headers that have
// to travel beside it. Callers outside the proxy use it to copy a key into a
// sandbox rather than lend it.
func (s Slot) Read(home, keysDir string) (string, map[string]string, error) {
	return s.read(home, keysDir)
}

// Available reports whether the host holds what this slot lends, so `create`
// fails at the moment the flag is typed rather than at the first model call
// from inside a sandbox, where the error surfaces as the agent claiming it is
// signed out.
func (s Slot) Available(home, keysDir string) error {
	_, _, err := s.read(home, keysDir)
	return err
}

// Source is the file this slot's credential is read from, for reporting.
func (s Slot) Source(home, keysDir string) string { return s.where(home, keysDir) }
