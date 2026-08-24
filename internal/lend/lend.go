// Package lend is the credential lender: a host-side proxy that lets a sandbox
// call an LLM API without ever holding the credential that pays for it.
//
// A sandbox is given a loan token — an unguessable string that is worthless
// anywhere but this host — in the variable the agent already reads, plus a base
// URL pointing here. The lender resolves the token to a slot, swaps the token
// for the host's real credential in that slot's own header shape, and forwards
// to the slot's own upstream. The real credential never crosses the sandbox
// boundary, so a sandbox that is compromised, snapshotted or shared leaks
// nothing worth having.
//
// Two things can be lent, and the difference is only which slot answers:
//
//   - a token loan lends an agent's host login (Claude Code's OAuth token,
//     Codex's ChatGPT token)
//   - a key loan lends an LLM API key the host keeps in ~/.cs-keys/<provider>
//
// This package imports nothing else in this repository. Everything it needs —
// where the instances live, which home to read — arrives as a parameter, which
// is what keeps the lender extractable and what keeps a bug in it from being
// able to go looking for credentials it was not given.
package lend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenPrefix opens every loan token. It is deliberately not a provider's own
// prefix: a value that reaches a real API by mistake must be recognizable as
// this tool's, in a log or a support thread, rather than passing for a key
// somebody then goes looking for.
const TokenPrefix = "loan_"

// Kind distinguishes what a slot lends. It decides nothing about the swap —
// that is the slot's header shape — and exists so the CLI can say "login" or
// "key" in the words the person typing the flag used.
type Kind string

const (
	Login Kind = "login" // an agent's host sign-in
	Key   Kind = "key"   // an LLM API key
)

// Slot is one lendable credential: where the real one is read from, how it
// travels, and the one upstream it is good for.
//
// Origin belongs to the slot rather than to the request, and that is the rule
// the whole design rests on. A credential is valid at exactly one place, so
// resolving the token answers "which upstream" for free — and a caller that
// could name its own upstream could aim the host's real credential at a host of
// its choosing, which is a credential-exfiltration primitive rather than a
// feature.
type Slot struct {
	ID     string // "claude" | "codex" | "anthropic" | "openai"
	Kind   Kind
	Origin string // upstream base URL; the inbound path is appended to it

	// Version is the provider's API version segment, ensured exactly once on
	// the way out, or "" for an upstream that has none.
	//
	// It exists because clients disagree about who owns it. Claude Code treats
	// a base URL as the site root and posts /v1/messages; OpenCode's Anthropic
	// client treats it as already versioned and posts /messages. One base URL
	// serves both only if this side settles it.
	Version string

	// Header and Prefix are the shape the real credential travels in:
	// ("authorization", "Bearer ") or ("x-api-key", "").
	Header string
	Prefix string

	// AuthEnvs are the variables the sandbox receives the credential in, and
	// BaseEnv the one that points the agent here. All are the clients' own
	// variables: nothing in a sandbox is taught a new name for this.
	//
	// More than one, because a provider can be reached by clients that disagree
	// about what to read. An OpenAI key is OPENAI_API_KEY to anything shaped
	// like the OpenAI SDK, and CODEX_API_KEY to Codex, which attaches no header
	// at all for the former.
	AuthEnvs []string
	BaseEnv  string

	// read returns the real credential and any per-credential headers that
	// travel with it (Codex's account id). It is called per request rather than
	// cached, so a host login refreshed by the agent that owns it is picked up
	// without the lender ever holding a refresh token of its own.
	read func(home, keysDir string) (secret string, extra map[string]string, err error)

	// where names the file the credential is read from, for an error message
	// that tells the reader where to look.
	where func(home, keysDir string) string

	// guestFile is the credential file this slot's agent reads inside a
	// sandbox, relative to its profile directory, or "" for a slot whose
	// client reads an environment variable. guestDoc fabricates its content.
	//
	// Seeding the agent's own file rather than a gateway variable is what
	// keeps a lent sandbox on the code path its client always takes. See
	// guest.go.
	guestFile string
	guestDoc  func(label, nonce string) (wire string, doc []byte, err error)
}

// slots is the table. Adding a provider is an entry here and nothing else.
var slots = []Slot{
	{
		ID: "claude", Kind: Login,
		Origin: "https://api.anthropic.com", Version: "/v1",
		Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"ANTHROPIC_AUTH_TOKEN"}, BaseEnv: "ANTHROPIC_BASE_URL",
		read:      readClaudeLogin,
		where:     func(home, _ string) string { return filepath.Join(home, ".cs-claude", ".credentials.json") },
		guestFile: ".credentials.json",
		guestDoc:  claudeCredentials,
	},
	{
		ID: "codex", Kind: Login,
		// ChatGPT-mode Codex talks to this host, not to api.openai.com, and the
		// path it appends ("/responses") is the same either way — which is why
		// the origin can carry the difference and the request never has to.
		// No version segment: the subscription transport has none.
		Origin: "https://chatgpt.com/backend-api/codex",
		Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL",
		read:      readCodexLogin,
		where:     func(home, _ string) string { return filepath.Join(home, ".cs-codex", "auth.json") },
		guestFile: "auth.json",
		guestDoc:  codexAuth,
	},
	{
		ID: "anthropic", Kind: Key,
		Origin: "https://api.anthropic.com", Version: "/v1",
		Header: "x-api-key", Prefix: "",
		AuthEnvs: []string{"ANTHROPIC_API_KEY"}, BaseEnv: "ANTHROPIC_BASE_URL",
		read:  keyReader("anthropic"),
		where: keyPath("anthropic"),
	},
	{
		ID: "openai", Kind: Key,
		Origin: "https://api.openai.com", Version: "/v1",
		Header: "authorization", Prefix: "Bearer ",
		// Codex reads the second and nothing else; OpenCode reads the first.
		AuthEnvs: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}, BaseEnv: "OPENAI_BASE_URL",
		read:  keyReader("openai"),
		where: keyPath("openai"),
	},
	{
		ID: "fireworks", Kind: Key,
		Origin: "https://api.fireworks.ai/inference", Version: "/v1",
		Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"FIREWORKS_API_KEY"},
		// OpenCode is the client that reaches this provider, and its base URL
		// belongs to the provider rather than to the client. Only OpenCode's own
		// variable can point one that has no variable of its own, which is why
		// this slot names it where the others name a provider's.
		BaseEnv: "OPENCODE_BASE_URL",
		read:    keyReader("fireworks"),
		where:   keyPath("fireworks"),
	},
}

// SlotByID returns the slot with this id.
func SlotByID(id string) (Slot, bool) {
	for _, s := range slots {
		if s.ID == id {
			return s, true
		}
	}
	return Slot{}, false
}

// SlotIDs returns the slots of one kind, in table order, for a flag's help text
// and for validating what a caller typed.
func SlotIDs(k Kind) []string {
	var out []string
	for _, s := range slots {
		if s.Kind == k {
			out = append(out, s.ID)
		}
	}
	return out
}

// KeysDir is where the host keeps the LLM API keys it is willing to lend: one
// file per provider, holding the key and nothing else.
//
// A directory rather than a config file, for the same reason the agent profiles
// are directories: adding a key is `cp`, there is nothing to keep in sync, and
// what the host will lend is legible from `ls`.
func KeysDir(home string) string { return filepath.Join(home, ".cs-keys") }

// Loan is one credential lent to one sandbox. The token is the secret; the rest
// is what makes a log line or an error readable.
type Loan struct {
	// Token is the value the client presents, which for a seeded credential
	// file is the whole fabricated token. Label is a short name for it, and is
	// what logs and errors say: a kilobyte of forged JWT in a log line helps
	// nobody, and the label identifies the same loan.
	Token string `json:"token"`
	Label string `json:"label"`
	Slot  string `json:"slot"`
	Kind  Kind   `json:"kind"`

	// Group and Name are filled in by the store from the loan file's own
	// location, so a loan cannot claim to belong to a sandbox other than the one
	// whose directory holds it.
	Group string `json:"-"`
	Name  string `json:"-"`
}

// IsToken reports whether a credential value is one of ours. Used to pick the
// loan token out of whichever header the client put it in.
func IsToken(v string) bool { return strings.HasPrefix(v, TokenPrefix) }

// loansFile is the per-instance record, written by create and removed with the
// instance directory. Revocation is the sandbox lifecycle: there is no separate
// list to keep in step, and nothing to forget to clean up.
const loansFile = "loans.json"

type loansDoc struct {
	Loans []Loan `json:"loans"`
}

// WriteLoans records a sandbox's loans in its instance directory at 0600.
func WriteLoans(instanceDir string, loans []Loan) error {
	if len(loans) == 0 {
		return nil
	}
	data, err := json.MarshalIndent(loansDoc{Loans: loans}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(instanceDir, loansFile)
	if err := os.WriteFile(p, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

// ReadLoans returns the loans recorded for one instance directory, or nothing
// when that sandbox has none.
func ReadLoans(instanceDir string) ([]Loan, error) {
	data, err := os.ReadFile(filepath.Join(instanceDir, loansFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc loansDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("loans.json is not readable: %w", err)
	}
	return doc.Loans, nil
}

// Loans resolves a loan token to the loan it stands for.
type Loans interface {
	Lookup(token string) (Loan, bool)
}

// FileLoans reads the loan records under an instances directory, at
// <instances>/<group>/<name>/loans.json.
//
// It caches, because a token arrives on every request and the answer changes
// only when a sandbox is created or destroyed. A miss forces an immediate
// reload — so a sandbox works the moment create returns — and the cache expires
// on its own, which is what bounds how long a destroyed sandbox's token keeps
// working.
type FileLoans struct {
	Dir string
	TTL time.Duration

	mu      sync.Mutex
	byToken map[string]Loan
	loaded  time.Time
	// missAt is when a miss last forced a re-read, which bounds how often an
	// unrecognized token can send this to the disk.
	missAt time.Time
}

// DefaultTTL bounds how stale the loan table may be, and therefore how long a
// destroyed sandbox's token could still be honoured. Short enough that
// revocation is effectively immediate, long enough that a busy session is not
// re-reading the disk for every request in a stream.
const DefaultTTL = 2 * time.Second

// NewFileLoans returns a loan table backed by the instances directory.
func NewFileLoans(dir string) *FileLoans { return &FileLoans{Dir: dir, TTL: DefaultTTL} }

// Lookup resolves a token, reloading the table when it is stale or when the
// token is unknown to it.
func (f *FileLoans) Lookup(token string) (Loan, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ttl := f.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if f.byToken == nil || time.Since(f.loaded) > ttl {
		f.reload()
	}
	if l, ok := f.byToken[token]; ok {
		return l, true
	}
	// An unknown token is either a sandbox created since the last read or a
	// token that is not ours. Re-reading tells them apart, so a sandbox works
	// the moment create returns rather than at the end of the next window.
	//
	// Bounded, because the second case is the one an attacker controls: without
	// this, anything that can reach the port could put the lender on the disk
	// once per request by sending a token that does not exist.
	if time.Since(f.missAt) > missReloadEvery {
		f.missAt = time.Now()
		f.reload()
	}
	l, ok := f.byToken[token]
	return l, ok
}

// missReloadEvery bounds the miss-driven re-reads. Short enough that a sandbox
// created a moment ago resolves on its first call, long enough that a flood of
// unknown tokens costs one scan rather than thousands.
const missReloadEvery = 250 * time.Millisecond

func (f *FileLoans) reload() {
	table := map[string]Loan{}
	groups, _ := os.ReadDir(f.Dir)
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		names, _ := os.ReadDir(filepath.Join(f.Dir, g.Name()))
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			loans, err := ReadLoans(filepath.Join(f.Dir, g.Name(), n.Name()))
			if err != nil {
				continue
			}
			for _, l := range loans {
				l.Group, l.Name = g.Name(), n.Name()
				table[l.Token] = l
			}
		}
	}
	f.byToken = table
	f.loaded = time.Now()
}

// Env returns the environment a sandbox needs to spend this loan: the loan
// token in the variable the agent reads for a credential, and this lender in
// the one it reads for a base URL.
//
// base is where the sandbox should send its model calls. It is the lender's own
// address, or a cs-vcr in front of it — the sandbox cannot tell the difference,
// and neither variable changes when a cassette is added or dropped.
func (s Slot) Env(token, base string) []string {
	out := []string{s.BaseEnv + "=" + base}
	for _, v := range s.AuthEnvs {
		out = append(out, v+"="+token)
	}
	return out
}

// EnvNames returns the variables a slot owns, so create can refuse to have them
// set by hand as well.
func (s Slot) EnvNames() []string { return append([]string{s.BaseEnv}, s.AuthEnvs...) }
