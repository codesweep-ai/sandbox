package lend

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"
)

// What a lent sandbox is given.
//
// A loan is seeded in the shape the agent's own sign-in leaves behind, with
// fabricated values, rather than in the shape a gateway uses. Both would work
// today, and the file is chosen anyway: a client reading its own credential
// file takes the code path it always takes, whatever that path becomes in a
// release nobody has shipped yet. The alternative asks a client to keep
// treating two auth modes alike, which is a promise nobody made.
//
// One rule covers every slot: fabricate in the form the real credential has.
// What that form IS differs by provider and is not ours to choose. Codex signs
// in with a pair of JWTs, so a lent Codex holds forged ones. Anthropic issues
// an opaque token with a vendor prefix, so a lent Claude holds one of those.
// Copying Codex's shape into Claude's file would be less faithful rather than
// more consistent.
//
// Nothing fabricated here is plausible on inspection. Every value says what it
// is, so a token that turns up anywhere is recognisable as this tool's and
// useless to whoever found it.

// GuestCredential is a file to seed into an agent's profile, and the value the
// agent will send once it reads it.
type GuestCredential struct {
	Agent string // the profile directory: ~/.cs-<agent>
	File  string // the file inside it
	Doc   []byte // its content
	Wire  string // what the client will present, and what the lender matches on
	Label string // a short name for that value, for logs and errors

	// Extra are further files in the same profile. A client that keeps the
	// account it is signed in as apart from the token it signs in with needs
	// both, or it holds a working credential it cannot name.
	Extra []GuestFile
}

// GuestFile is one file to seed into an agent's profile.
type GuestFile struct {
	File string // relative to the profile directory
	Doc  []byte // its content
}

// MintGuest fabricates the credential a lent sandbox holds for this slot.
//
// A key slot has no file: an API key travels in an environment variable, which
// is already the shape its client expects, so there is nothing to reconstruct.
func (s Slot) MintGuest(sandbox, home string) (GuestCredential, error) {
	nonce, err := nonceHex()
	if err != nil {
		return GuestCredential{}, err
	}
	label := TokenPrefix + sandbox + "_" + s.ID + "_" + nonce
	if s.guestFile == "" {
		return GuestCredential{Wire: label, Label: label}, nil
	}
	wire, doc, err := s.guestDoc(label, nonce, home)
	if err != nil {
		return GuestCredential{}, err
	}
	g := GuestCredential{Agent: s.ID, File: s.guestFile, Doc: doc, Wire: wire, Label: label}
	if s.guestProfile != nil {
		extra, err := s.guestProfile(home)
		if err != nil {
			return GuestCredential{}, err
		}
		g.Extra = extra
	}
	return g, nil
}

func nonceHex() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint a loan: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// claudeCredentials is what `claude` writes when it signs in.
//
// The tokens are opaque rather than structured, because that is what Anthropic
// issues: one segment, a vendor prefix, and 108 characters. The fabricated pair
// takes the same form, with `loan` where a real one would have random bytes, so
// anyone reading the file sees what it is.
//
// expiresAt is far out because the sandbox must not try to refresh: it holds no
// refresh token that would work, and the attempt would reach a host the lender
// refuses. The host's own login is the thing that gets refreshed, by whatever
// signed it in.
func claudeCredentials(_, _, home string) (string, []byte, error) {
	access, err := loanOpaque(claudeAccessPrefix, claudeTokenLen)
	if err != nil {
		return "", nil, err
	}
	refresh, err := loanOpaque(claudeRefreshPrefix, claudeTokenLen)
	if err != nil {
		return "", nil, err
	}
	doc := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      access,
			"refreshToken":     refresh,
			"expiresAt":        time.Now().Add(loanLifetime).UnixMilli(),
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": claudeSubscriptionType(home),
		},
	}
	b, err := json.Marshal(doc)
	return access, b, err
}

// claudeSubscriptionType is the plan the loan spends, taken from the host.
//
// The tokens beside it stay fabricated and still say "loan", so the credential
// remains recognisable as this tool's. The plan is different in kind: it is not
// a secret and not a thing to forge, it is a fact about the subscription the
// request will be billed to, and a client that reads it is asking which plan it
// is on rather than who to trust.
//
// Stating it wrong has a visible cost. A value Claude Code does not know is not
// treated as an unknown plan, it falls through to the label it uses for API
// billing, so a sandbox spending a Max subscription reported itself as "API"
// and its owner could not tell the loan was working from the one screen that
// should have said so.
//
// The fallback keeps a host whose credential predates this readable rather than
// failing the create: an absent or unreadable file means the plan is simply not
// known here.
func claudeSubscriptionType(home string) string {
	const unknown = "cs-sandbox-loan"
	if home == "" {
		return unknown
	}
	data, err := os.ReadFile(filepath.Join(home, ".cs-claude", ".credentials.json"))
	if err != nil {
		return unknown
	}
	var doc struct {
		OAuth struct {
			SubscriptionType string `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.OAuth.SubscriptionType == "" {
		return unknown
	}
	return doc.OAuth.SubscriptionType
}

// claudeProfile carries the host account's identity in beside the loan.
//
// Claude Code keeps the account it is signed in as in .claude.json, apart from
// the token in .credentials.json. Seeding only the token therefore produced a
// sandbox holding a working credential it could not name: `claude auth status`
// answered with a null email and a null organisation, which reads as a broken
// login rather than a working loan.
//
// Only oauthAccount is carried, and it is carried verbatim. Curating the fields
// would be guessing at what a client release reads, and the ones that matter
// here — the address, the organisation, the account it belongs to — are what
// identifies the subscription being spent anyway.
//
// This is the host's own identity going into a sandbox, and it travels for one
// reason: --lend-agent-login named the login it belongs to. R3 holds either
// way, because a sandbox nobody lent a login to still receives none of this.
// A host with no oauthAccount seeds nothing rather than failing the create.
func claudeProfile(home string) ([]GuestFile, error) {
	if home == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".cs-claude", ".claude.json"))
	if err != nil {
		return nil, nil
	}
	var doc struct {
		OAuthAccount json.RawMessage `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil
	}
	if len(doc.OAuthAccount) == 0 || string(doc.OAuthAccount) == "null" {
		return nil, nil
	}
	out, err := json.Marshal(map[string]any{"oauthAccount": doc.OAuthAccount})
	if err != nil {
		return nil, fmt.Errorf("carry the Claude account into the loan: %w", err)
	}
	// Seeded under its own name rather than as .claude.json: the guest merges
	// it into that file, which the client owns and rewrites as it runs, and a
	// seed named for the destination invites replacing it instead.
	return []GuestFile{{File: claudeAccountFile, Doc: out}}, nil
}

// The form of an Anthropic OAuth credential: a prefixed, opaque, 108-character
// token, in two flavours.
// claudeAccountFile is the seeded account document, merged into .claude.json by
// the guest at every boot.
const claudeAccountFile = "account.json"

const (
	claudeAccessPrefix  = "sk-ant-oat01-"
	claudeRefreshPrefix = "sk-ant-ort01-"
	claudeTokenLen      = 108
)

// loanOpaque fabricates a credential shaped like an opaque vendor token: the
// prefix the client expects, the word that says what it really is, and random
// characters to the length the real one has.
//
// The marker sits at the front rather than buried, so a value found in a
// sandbox, a log or a support thread reads as a loan before it reads as a
// credential.
func loanOpaque(prefix string, total int) (string, error) {
	const marker = "loan-"
	// Never trade away the randomness to hit a length.
	fill := max(total-len(prefix)-len(marker), 32)
	b := make([]byte, fill) // base64url expands, so this is trimmed below
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint a loan: %w", err)
	}
	return prefix + marker + base64.RawURLEncoding.EncodeToString(b)[:fill], nil
}

// codexAuth is what `codex` writes after a ChatGPT sign-in: an auth mode, a
// pair of JWTs and the account they belong to.
//
// The tokens are forged rather than fabricated loosely, because Codex decodes
// them and treats anything it cannot read as signed out. Only the structure is
// borrowed. Every identity claim names this tool, the account id is not the
// host's, and the signature is random bytes: nothing here would survive
// verification, and nothing here is meant to leave this host.
func codexAuth(label, nonce, _ string) (string, []byte, error) {
	now := time.Now()
	account := loanAccountID(nonce)
	auth := map[string]any{
		"amr":                       []string{"pwd"},
		"chatgpt_account_id":        account,
		"chatgpt_account_user_id":   "cs-sandbox-loan",
		"chatgpt_compute_residency": "no_constraint",
		"chatgpt_plan_type":         "cs-sandbox-loan",
		"chatgpt_user_id":           "cs-sandbox-loan",
		"localhost":                 true,
		"poid":                      "cs-sandbox-loan",
		"user_id":                   "cs-sandbox-loan",
	}
	claims := map[string]any{
		// The protocol fields the client reads to decide it is signed in.
		"aud":                         []string{"https://api.openai.com/v1"},
		"client_id":                   "cs-sandbox-loan",
		"iss":                         "https://auth.openai.com",
		"scp":                         []string{"openid", "profile", "email", "offline_access"},
		"sub":                         "cs-sandbox-loan",
		"iat":                         now.Unix(),
		"nbf":                         now.Unix(),
		"exp":                         now.Add(loanLifetime).Unix(),
		"jti":                         nonce,
		"https://api.openai.com/auth": auth,
		"https://api.openai.com/profile": map[string]any{
			"email": "loan@cs-sandbox.invalid", "email_verified": true, "name": "cs-sandbox loan",
		},
		// What this is, for anyone who finds one and decodes it.
		"cs_sandbox_loan": label,
	}
	access, err := forgeJWT(claims)
	if err != nil {
		return "", nil, err
	}
	idClaims := make(map[string]any, len(claims))
	maps.Copy(idClaims, claims)
	idClaims["aud"] = []string{"cs-sandbox-loan"}
	id, err := forgeJWT(idClaims)
	if err != nil {
		return "", nil, err
	}
	doc, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": id, "access_token": access,
			"refresh_token": label + "-refresh-not-a-credential", "account_id": account,
		},
		"last_refresh": now.UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	return access, doc, err
}

// loanAccountID is the account a lent Codex believes it is signed in as. It is
// shaped like the identifier the client expects and belongs to nobody: the
// lender replaces it with the host's real one on the way out, so the sandbox
// never learns which account pays for it.
func loanAccountID(nonce string) string {
	return "00000000-0000-4000-8000-" + nonce[:12]
}

// forgeJWT builds a token the client can decode and nothing can verify.
//
// The header names this tool as the key, and the signature is random bytes. A
// real verifier rejects it, which is the point: it is a local placeholder, and
// the only thing that ever accepts it is the lender on this host.
func forgeJWT(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "cs-sandbox-loan", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	var sig [256]byte
	if _, err := rand.Read(sig[:]); err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc(header) + "." + enc(payload) + "." + enc(sig[:]), nil
}

// loanLifetime is how long a seeded credential claims to be good for. It only
// has to outlast the sandbox: a loan is revoked by destroying that sandbox, not
// by expiring, and an expiry the client believed had passed would send it
// looking for a refresh it cannot do.
const loanLifetime = 365 * 24 * time.Hour
