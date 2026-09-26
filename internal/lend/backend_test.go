package lend

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexLoan is a lender with the shipped Codex slot pointed at a test upstream,
// and one loan minted the way create mints it: its token is the access token
// forged into the sandbox's auth.json.
type codexLoan struct {
	srv     *httptest.Server
	lender  *Server
	up      *upstream
	token   string
	account string
	inst    string
}

func newCodexLoan(t *testing.T, home string) codexLoan {
	t.Helper()
	up := newUpstream(t)
	slot, ok := SlotByID("codex")
	if !ok {
		t.Fatal("no codex slot shipped")
	}
	slot.Origin = up.URL
	withSlots(t, slot)
	nonce := "0123456789abcdef0123456789abcdef"
	token, _, err := codexAuth(TokenPrefix+"box_codex_"+nonce, nonce, home)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	inst := filepath.Join(root, "g", "box")
	if err := os.MkdirAll(inst, 0o700); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Home: home, KeysDir: KeysDir(home), Callers: CallersAny, Faults: NewFileFaults(root),
		Loans: fixedLoans{token: {Token: token, Slot: "codex", Kind: Login, Group: "g", Name: "box"}},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return codexLoan{srv: srv, lender: s, up: up, token: token, account: loanAccountID(nonce), inst: inst}
}

func (c codexLoan) call(t *testing.T, method, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("ChatGPT-Account-Id", c.account)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// Codex 0.156 and later exit at startup unless this call succeeds with the token
// they hold, and a loan's token succeeds nowhere else (R146a, SBX-084). The answer
// names the loan's own account, so the sandbox still cannot tell whose
// subscription it spends, and a concrete HTTPS origin, which Codex requires of
// it where NO_CONSTRAINT would resolve to this plain-HTTP lender.
//
// The home holds no credential at all: the answer must not depend on reading
// one, and nothing may be dialled for it.
func TestALentCodexIsToldItsOwnAccountAtStartup(t *testing.T) {
	c := newCodexLoan(t, t.TempDir())
	resp, body := c.call(t, http.MethodGet, discoveryPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var got accountsCheck
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("not the answer Codex reads: %v\n%s", err, body)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("want exactly the loan's account, got %+v", got.Accounts)
	}
	a := got.Accounts[0]
	if a.ID != c.account || got.DefaultAccount != c.account || len(got.AccountOrdering) != 1 || got.AccountOrdering[0] != c.account {
		t.Errorf("the answer names %+v, want the loan's account %s throughout", got, c.account)
	}
	if a.Origin != "https://chatgpt.com" || a.Override != "NO_CONSTRAINT" {
		t.Errorf("origin %q, override %q: Codex needs an HTTPS origin and no routing override", a.Origin, a.Override)
	}
	if c.up.calls != 0 {
		t.Errorf("the upstream was called %d time(s) for a call the lender answers", c.up.calls)
	}
	if st := c.lender.Snapshot(); st.Answered != 1 || st.Lent != 0 {
		t.Errorf("counted %+v, want one answered and nothing lent", st)
	}
}

// Everything else under the backend is refused rather than forwarded. Each of
// these went to the tunnel before and was refused there, and forwarding them
// would put the real credential on paths nobody reviewed.
func TestTheRestOfTheBackendIsRefusedAndNeverForwarded(t *testing.T) {
	c := newCodexLoan(t, hostProfile(t))
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, backendPrefix + "/wham/usage"},
		{http.MethodGet, backendPrefix + "/wham/config/bundle"},
		{http.MethodPost, discoveryPath},
		{http.MethodGet, backendPrefix},
	} {
		resp, body := c.call(t, call.method, call.path)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "backend_blocked") {
			t.Errorf("%s %s: status %d: %s", call.method, call.path, resp.StatusCode, body)
		}
	}
	if c.up.calls != 0 {
		t.Errorf("a refused backend call reached the upstream %d time(s)", c.up.calls)
	}
	if st := c.lender.Snapshot(); st.Blocked != 4 || st.Lent != 0 {
		t.Errorf("counted %+v, want four blocked and nothing lent", st)
	}
}

// The same loan's model calls are lent exactly as before: the backend is a
// prefix no model call starts with.
func TestALentCodexsModelCallsAreStillLent(t *testing.T) {
	c := newCodexLoan(t, hostProfile(t))
	resp, body := c.call(t, http.MethodPost, "/responses")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if c.up.calls != 1 || c.up.gotPath != "/responses" {
		t.Fatalf("upstream calls %d, path %q", c.up.calls, c.up.gotPath)
	}
	if got := c.up.gotHeader.Get("Authorization"); got != "Bearer REAL-CHATGPT-JWT" {
		t.Errorf("Authorization = %q, want the host's real login", got)
	}
	if got := c.up.gotHeader.Get("Chatgpt-Account-Id"); got != "acct-123" {
		t.Errorf("ChatGPT-Account-Id = %q, want the host's real account", got)
	}
}

// An armed fault stands for the provider, and the discovery is not a provider
// call. Spent on it, a fault meant for a turn would stop Codex from starting.
func TestAFaultIsNotSpentOnTheStartupCall(t *testing.T) {
	c := newCodexLoan(t, hostProfile(t))
	if err := WriteFaults(c.inst, []Fault{{ID: "f1", Status: 429, Count: 1}}); err != nil {
		t.Fatal(err)
	}
	if resp, body := c.call(t, http.MethodGet, discoveryPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("the discovery was answered %d: %s", resp.StatusCode, body)
	}
	if resp, _ := c.call(t, http.MethodPost, "/responses"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the model call got %d, want the fault that was armed for it", resp.StatusCode)
	}
}

// A loan whose token names no account cannot be answered for, and is not
// answered with a made-up one.
func TestALoanThatNamesNoAccountIsNotAnswered(t *testing.T) {
	c := newCodexLoan(t, t.TempDir())
	tok := TokenPrefix + "box_codex_not-a-jwt"
	s := New(Config{Home: t.TempDir(), Callers: CallersAny,
		Loans: fixedLoans{tok: {Token: tok, Slot: "codex", Kind: Login, Name: "box"}},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+discoveryPath, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "backend_unanswered") {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
	if c.up.calls != 0 {
		t.Errorf("the upstream was called for a loan that could not be answered")
	}
}
