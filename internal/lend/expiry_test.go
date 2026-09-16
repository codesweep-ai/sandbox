package lend

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeClaudeCred writes a credential file with the two clocks set.
func writeClaudeCred(t *testing.T, home string, expiresAt, refreshExpiresAt time.Time) {
	t.Helper()
	dir := filepath.Join(home, ".cs-claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok","expiresAt":%d,"refreshTokenExpiresAt":%d}}`,
		expiresAt.UnixMilli(), refreshExpiresAt.UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// forgeAccessToken builds an unsigned JWT carrying exp, which is all codexExpiry
// reads. The signature is deliberately nonsense: nothing here verifies one.
func forgeAccessToken(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + enc(payload) + ".not-a-signature"
}

func writeCodexAuth(t *testing.T, home, accessToken string) {
	t.Helper()
	dir := filepath.Join(home, ".cs-codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`{"tokens":{"access_token":%q,"account_id":"acct"}}`, accessToken)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCarriesTwoSeparateClocks(t *testing.T) {
	home := t.TempDir()
	exp := time.Now().Add(3 * time.Hour).Truncate(time.Millisecond)
	deadline := time.Now().Add(200 * time.Hour).Truncate(time.Millisecond)
	writeClaudeCred(t, home, exp, deadline)

	s, _ := SlotByID("claude")
	got, ok, err := s.ExpiresAt(home, "")
	if err != nil || !ok {
		t.Fatalf("ExpiresAt: ok=%v err=%v", ok, err)
	}
	if !got.Equal(exp) {
		t.Errorf("expiry = %s, want %s", got, exp)
	}
	gotDL, ok, err := s.RefreshDeadline(home, "")
	if err != nil || !ok {
		t.Fatalf("RefreshDeadline: ok=%v err=%v", ok, err)
	}
	if !gotDL.Equal(deadline) {
		t.Errorf("deadline = %s, want %s", gotDL, deadline)
	}
	// The point of reading both: they must not be the same number. A renewer that
	// conflated them would renew confidently right past the one that matters.
	if gotDL.Equal(got) {
		t.Error("the refresh deadline and the access-token expiry are the same value")
	}
}

func TestCodexExpiryComesFromInsideTheToken(t *testing.T) {
	home := t.TempDir()
	exp := time.Now().Add(240 * time.Hour).Truncate(time.Second)
	writeCodexAuth(t, home, forgeAccessToken(t, exp))

	s, _ := SlotByID("codex")
	got, ok, err := s.ExpiresAt(home, "")
	if err != nil || !ok {
		t.Fatalf("ExpiresAt: ok=%v err=%v", ok, err)
	}
	if !got.Equal(exp) {
		t.Errorf("expiry = %s, want %s", got, exp)
	}
	// auth.json states no refresh-token expiry, and "absent" has to be
	// distinguishable from "zero" or a caller reports a deadline of 1970.
	if _, ok, err := s.RefreshDeadline(home, ""); ok || err != nil {
		t.Errorf("RefreshDeadline: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestAKeyHasNothingToExpireAndThatIsNotAnError(t *testing.T) {
	for _, id := range []string{"anthropic", "openai", "fireworks"} {
		s, _ := SlotByID(id)
		if _, ok, err := s.ExpiresAt(t.TempDir(), t.TempDir()); ok || err != nil {
			t.Errorf("%s: ExpiresAt ok=%v err=%v, want false/nil", id, ok, err)
		}
		if s.Renewable() {
			t.Errorf("%s: a key reports itself renewable", id)
		}
	}
}

func TestAnExpiredCodexLoginIsReportedAsItselfNotAs401(t *testing.T) {
	home := t.TempDir()
	writeCodexAuth(t, home, forgeAccessToken(t, time.Now().Add(-time.Hour)))

	s, _ := SlotByID("codex")
	_, _, err := s.Read(home, "")
	if err == nil {
		t.Fatal("an expired Codex token was lent, so the sandbox would see an upstream 401")
	}
	if !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "cs-codex") {
		t.Errorf("error does not name the problem or the remedy: %v", err)
	}
}

func TestAnUndecodableCodexTokenIsStillLent(t *testing.T) {
	// Not understanding a credential's shape is not evidence that it is dead.
	// Refusing here would break lending the first time the token format moves.
	home := t.TempDir()
	writeCodexAuth(t, home, "not-a-jwt-at-all")

	s, _ := SlotByID("codex")
	secret, extra, err := s.Read(home, "")
	if err != nil {
		t.Fatalf("an opaque token was withheld: %v", err)
	}
	if secret != "not-a-jwt-at-all" {
		t.Errorf("secret = %q", secret)
	}
	if extra["chatgpt-account-id"] != "acct" {
		t.Errorf("account id did not travel: %v", extra)
	}
}

func TestEveryRenewableSlotHasAThresholdInsideItsOwnLifetime(t *testing.T) {
	// A zero threshold would mean "attempt only once already expired", which is a
	// gap rather than a renewer.
	for _, id := range SlotIDs(Login) {
		s, _ := SlotByID(id)
		if !s.Renewable() {
			continue
		}
		if s.RenewWithin() <= 0 {
			t.Errorf("%s: RenewWithin = %s", id, s.RenewWithin())
		}
		if s.expires == nil {
			t.Errorf("%s: renewable but states no expiry, so a renewer cannot time anything", id)
		}
	}
}

func TestJWTExpiryRejectsWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{"not a jwt", "abc"},
		{"two segments", "aaa.bbb"},
		{"payload not base64", "aaa.!!!.ccc"},
		{"payload not json", "aaa." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".ccc"},
		{"no exp claim", "aaa." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".ccc"},
	} {
		if _, err := jwtExpiry(tc.token); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestJWTExpiryToleratesPadding(t *testing.T) {
	// Unpadded is what a JWT uses, but a producer that pads is not malformed.
	payload := base64.URLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	if !strings.Contains(payload, "=") {
		t.Skip("this payload happens not to need padding")
	}
	got, err := jwtExpiry("h." + payload + ".s")
	if err != nil {
		t.Fatalf("padded payload rejected: %v", err)
	}
	if got.Unix() != 1700000000 {
		t.Errorf("exp = %d", got.Unix())
	}
}
