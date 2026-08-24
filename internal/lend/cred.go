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
	p := filepath.Join(home, ".cs-claude", ".credentials.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return "", nil, missing("Claude", p, "cs-claude")
	}
	var doc struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // epoch milliseconds
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", nil, fmt.Errorf("%s is not readable as a Claude login: %w", p, err)
	}
	if doc.OAuth.AccessToken == "" {
		return "", nil, missing("Claude", p, "cs-claude")
	}
	if doc.OAuth.ExpiresAt > 0 {
		if exp := time.UnixMilli(doc.OAuth.ExpiresAt); time.Now().After(exp) {
			return "", nil, fmt.Errorf("the host's Claude login expired at %s — run 'cs-claude' on the host to refresh it",
				exp.Local().Format(time.RFC3339))
		}
	}
	return doc.OAuth.AccessToken, map[string]string{"anthropic-beta": "oauth-2025-04-20"}, nil
}

// readCodexLogin returns the ChatGPT access token Codex keeps in the cs-codex
// profile, with the account id that has to travel beside it.
func readCodexLogin(home, _ string) (string, map[string]string, error) {
	p := filepath.Join(home, ".cs-codex", "auth.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return "", nil, missing("Codex", p, "cs-codex login")
	}
	var doc struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", nil, fmt.Errorf("%s is not readable as a Codex login: %w", p, err)
	}
	if doc.Tokens.AccessToken == "" {
		return "", nil, missing("Codex", p, "cs-codex login")
	}
	extra := map[string]string{}
	if doc.Tokens.AccountID != "" {
		extra["chatgpt-account-id"] = doc.Tokens.AccountID
	}
	return doc.Tokens.AccessToken, extra, nil
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
