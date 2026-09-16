package lend

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The whole point of merging fields instead of replacing the file: the client in
// the image is a pinned version, and a field it does not write must not be erased
// from the host's credential.
func TestMergeKeepsEverythingARefreshDoesNotRotate(t *testing.T) {
	s, _ := SlotByID("claude")
	host := []byte(`{"claudeAiOauth":{
		"accessToken":"old-access","refreshToken":"old-refresh",
		"expiresAt":1000,"refreshTokenExpiresAt":9000,
		"scopes":["user:inference","user:profile"],
		"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`)
	// An older client that knows nothing of rateLimitTier or subscriptionType.
	fresh := []byte(`{"claudeAiOauth":{
		"accessToken":"new-access","refreshToken":"new-refresh",
		"expiresAt":2000,"refreshTokenExpiresAt":9000}}`)

	merged, err := s.MergeRefreshed(host, fresh)
	if err != nil {
		t.Fatalf("MergeRefreshed: %v", err)
	}
	var got struct {
		OAuth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	// Rotated.
	for k, want := range map[string]any{
		"accessToken":  "new-access",
		"refreshToken": "new-refresh",
		"expiresAt":    float64(2000),
	} {
		if got.OAuth[k] != want {
			t.Errorf("%s = %v, want %v", k, got.OAuth[k], want)
		}
	}
	// Preserved, which is the claim under test.
	if got.OAuth["rateLimitTier"] != "default_claude_max_20x" {
		t.Errorf("rateLimitTier was lost: %v", got.OAuth["rateLimitTier"])
	}
	if got.OAuth["subscriptionType"] != "max" {
		t.Errorf("subscriptionType was lost: %v", got.OAuth["subscriptionType"])
	}
	if scopes, ok := got.OAuth["scopes"].([]any); !ok || len(scopes) != 2 {
		t.Errorf("scopes were lost: %v", got.OAuth["scopes"])
	}
}

func TestMergeCarriesTheCodexFieldsAndNotTheAccount(t *testing.T) {
	s, _ := SlotByID("codex")
	host := []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,
		"tokens":{"id_token":"old-id","access_token":"old-access",
		"refresh_token":"old-refresh","account_id":"acct-real"},
		"last_refresh":"2026-09-01T00:00:00Z"}`)
	// A renewal that also reports a different account must not move the account.
	fresh := []byte(`{"tokens":{"id_token":"new-id","access_token":"new-access",
		"refresh_token":"new-refresh","account_id":"acct-WRONG"},
		"last_refresh":"2026-09-15T00:00:00Z"}`)

	merged, err := s.MergeRefreshed(host, fresh)
	if err != nil {
		t.Fatalf("MergeRefreshed: %v", err)
	}
	var got struct {
		AuthMode string         `json:"auth_mode"`
		Tokens   map[string]any `json:"tokens"`
		Last     string         `json:"last_refresh"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.Tokens["access_token"] != "new-access" || got.Tokens["refresh_token"] != "new-refresh" {
		t.Errorf("tokens were not rotated: %v", got.Tokens)
	}
	if got.Tokens["id_token"] != "new-id" {
		t.Errorf("id_token was not carried: %v", got.Tokens["id_token"])
	}
	if got.Last != "2026-09-15T00:00:00Z" {
		t.Errorf("last_refresh = %q", got.Last)
	}
	// account_id identifies the account rather than the session, and a renewal
	// has no business rewriting which account the host's login belongs to.
	if got.Tokens["account_id"] != "acct-real" {
		t.Errorf("account_id was overwritten: %v", got.Tokens["account_id"])
	}
	if got.AuthMode != "chatgpt" {
		t.Errorf("auth_mode was lost: %q", got.AuthMode)
	}
}

// A field absent from the refreshed document is the client's choice, not an
// instruction to delete the host's value.
func TestMergeLeavesAMissingFieldAlone(t *testing.T) {
	s, _ := SlotByID("claude")
	host := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1000,"refreshTokenExpiresAt":9000}}`)
	fresh := []byte(`{"claudeAiOauth":{"accessToken":"a2","expiresAt":2000}}`)

	merged, err := s.MergeRefreshed(host, fresh)
	if err != nil {
		t.Fatalf("MergeRefreshed: %v", err)
	}
	var got struct {
		OAuth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.OAuth["refreshToken"] != "r" {
		t.Errorf("an absent field deleted the host's value: %v", got.OAuth["refreshToken"])
	}
	if got.OAuth["refreshTokenExpiresAt"] != float64(9000) {
		t.Errorf("refreshTokenExpiresAt was lost: %v", got.OAuth["refreshTokenExpiresAt"])
	}
}

// A document carrying none of the rotated values is not a renewal, and must not
// be written over a working credential.
func TestMergeRefusesADocumentWithNothingToCopy(t *testing.T) {
	s, _ := SlotByID("claude")
	host := []byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":1000}}`)
	if _, err := s.MergeRefreshed(host, []byte(`{"something":"else"}`)); err == nil {
		t.Fatal("a document with no rotated values was accepted")
	}
}

func TestMergeRefusesUnreadableInput(t *testing.T) {
	s, _ := SlotByID("claude")
	good := []byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":1000}}`)
	if _, err := s.MergeRefreshed([]byte("not json"), good); err == nil {
		t.Error("an unreadable host document was accepted")
	}
	if _, err := s.MergeRefreshed(good, []byte("not json")); err == nil {
		t.Error("an unreadable renewed document was accepted")
	}
}

func TestEveryRenewableSlotNamesAnAbsoluteClientInTheImage(t *testing.T) {
	for _, id := range SlotIDs(Login) {
		s, _ := SlotByID(id)
		spec, ok := s.RenewSpec()
		if !ok {
			continue
		}
		// An absolute path in the image. A bare name would be looked up on
		// whatever PATH the container has, which is the ambiguity running in a
		// container exists to remove.
		if !strings.HasPrefix(spec.Bin, "/") {
			t.Errorf("%s: Bin = %q, want an absolute path in the image", id, spec.Bin)
		}
		// This project's own wrapper, which is the entry point that created the
		// credential — not the bare client beneath it.
		if !strings.HasPrefix(filepath.Base(spec.Bin), "cs-") {
			t.Errorf("%s: Bin = %q, want the cs-* wrapper", id, spec.Bin)
		}
		// The profile is a directory under the staged HOME, because the wrappers
		// derive it from $HOME and override any profile variable handed to them.
		if !strings.HasPrefix(spec.ProfileDir, ".cs-") {
			t.Errorf("%s: ProfileDir = %q, want a .cs-<agent> directory", id, spec.ProfileDir)
		}
		if spec.File == "" || len(spec.Fields) == 0 {
			t.Errorf("%s: incomplete spec: %+v", id, spec)
		}
		if slices.Contains(spec.Args, "--bare") {
			t.Errorf("%s: --bare never reads OAuth, so it cannot refresh anything", id)
		}
		// The expiry has to be one of the rotated fields, or nothing could tell a
		// renewal that worked from one that did not.
		if !slices.ContainsFunc(spec.Fields, func(p []string) bool {
			return strings.Contains(strings.ToLower(p[len(p)-1]), "token") || strings.Contains(p[len(p)-1], "xpires")
		}) {
			t.Errorf("%s: no rotated field looks like a token or an expiry: %v", id, s.renewFieldNames())
		}
	}
}

func TestExpiryCanBeReadFromADocument(t *testing.T) {
	s, _ := SlotByID("claude")
	got, err := s.ExpiryOf([]byte(`{"claudeAiOauth":{"expiresAt":1789514763000}}`))
	if err != nil {
		t.Fatalf("ExpiryOf: %v", err)
	}
	if got.UnixMilli() != 1789514763000 {
		t.Errorf("expiry = %d", got.UnixMilli())
	}
	if _, err := s.ExpiryOf([]byte(`{}`)); err == nil {
		t.Error("a document with no expiry was accepted")
	}
}
