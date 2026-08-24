package lend

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
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
}

// MintGuest fabricates the credential a lent sandbox holds for this slot.
//
// A key slot has no file: an API key travels in an environment variable, which
// is already the shape its client expects, so there is nothing to reconstruct.
func (s Slot) MintGuest(sandbox string) (GuestCredential, error) {
	nonce, err := nonceHex()
	if err != nil {
		return GuestCredential{}, err
	}
	label := TokenPrefix + sandbox + "_" + s.ID + "_" + nonce
	if s.guestFile == "" {
		return GuestCredential{Wire: label, Label: label}, nil
	}
	wire, doc, err := s.guestDoc(label, nonce)
	if err != nil {
		return GuestCredential{}, err
	}
	return GuestCredential{Agent: s.ID, File: s.guestFile, Doc: doc, Wire: wire, Label: label}, nil
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
func claudeCredentials(_, _ string) (string, []byte, error) {
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
			"subscriptionType": "cs-sandbox-loan",
		},
	}
	b, err := json.Marshal(doc)
	return access, b, err
}

// The form of an Anthropic OAuth credential: a prefixed, opaque, 108-character
// token, in two flavours.
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
func codexAuth(label, nonce string) (string, []byte, error) {
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
