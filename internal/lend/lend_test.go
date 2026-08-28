package lend

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upstream is a stand-in provider that remembers what reached it. The whole
// contract of a lender is what the upstream sees, so every test here asserts on
// this rather than on the lender's internals.
type upstream struct {
	*httptest.Server
	gotHeader http.Header
	gotPath   string
	gotHost   string
	calls     int
	body      string
	sse       bool
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{body: `{"ok":true}`}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls++
		u.gotHeader = r.Header.Clone()
		u.gotPath = r.URL.Path
		u.gotHost = r.Host
		if u.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for i := range 3 {
				fmt.Fprintf(w, "data: chunk-%d\n\n", i)
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, u.body)
	}))
	t.Cleanup(u.Close)
	return u
}

// withSlots points the slot table at a test upstream for the duration of one
// test. The table is the production one everywhere else, so the shapes under
// test are the shipped shapes.
func withSlots(t *testing.T, replace ...Slot) {
	t.Helper()
	old := slots
	slots = replace
	t.Cleanup(func() { slots = old })
}

// fixedLoans is a loan table with no disk behind it.
type fixedLoans map[string]Loan

func (f fixedLoans) Lookup(tok string) (Loan, bool) { l, ok := f[tok]; return l, ok }

// hostProfile writes a host home holding the credentials a slot reads.
func hostProfile(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exp := time.Now().Add(time.Hour).UnixMilli()
	write(".cs-claude/.credentials.json",
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"the-hosts-real-login","expiresAt":%d}}`, exp))
	write(".cs-codex/auth.json",
		`{"auth_mode":"chatgpt","tokens":{"access_token":"REAL-CHATGPT-JWT","account_id":"acct-123"}}`)
	write(".cs-keys/anthropic", "the-hosts-real-key\n")
	write(".cs-keys/openai", "the-hosts-real-openai-key\n")
	return home
}

func newTestServer(t *testing.T, home string, loans Loans) *httptest.Server {
	t.Helper()
	s := New(Config{Home: home, KeysDir: KeysDir(home), Loans: loans})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// The core contract: the sandbox's worthless token goes in, the host's real
// credential comes out, and the token never reaches the provider.
func TestLoanTokenIsSwappedForTheRealCredential(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	cases := []struct {
		slot       Slot
		sendHeader string // how the client presents the loan
		wantHeader string
		wantValue  string
		wantExtra  map[string]string
	}{
		{
			slot:       Slot{ID: "claude", Kind: Login, Origin: up.URL, Header: "authorization", Prefix: "Bearer ", AuthEnvs: []string{"ANTHROPIC_AUTH_TOKEN"}, BaseEnv: "ANTHROPIC_BASE_URL", read: readClaudeLogin, where: claudeWhere},
			sendHeader: "Authorization",
			wantHeader: "Authorization", wantValue: "Bearer the-hosts-real-login",
			wantExtra: map[string]string{"Anthropic-Beta": "oauth-2025-04-20"},
		},
		{
			slot:       Slot{ID: "codex", Kind: Login, Origin: up.URL, Header: "authorization", Prefix: "Bearer ", AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL", read: readCodexLogin, where: codexWhere},
			sendHeader: "Authorization",
			wantHeader: "Authorization", wantValue: "Bearer REAL-CHATGPT-JWT",
			wantExtra: map[string]string{"Chatgpt-Account-Id": "acct-123"},
		},
		{
			slot:       Slot{ID: "anthropic", Kind: Key, Origin: up.URL, Header: "x-api-key", AuthEnvs: []string{"ANTHROPIC_API_KEY"}, BaseEnv: "ANTHROPIC_BASE_URL", read: keyReader("anthropic"), where: keyPath("anthropic")},
			sendHeader: "X-Api-Key",
			wantHeader: "X-Api-Key", wantValue: "the-hosts-real-key",
		},
		{
			slot:       Slot{ID: "openai", Kind: Key, Origin: up.URL, Header: "authorization", Prefix: "Bearer ", AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL", read: keyReader("openai"), where: keyPath("openai")},
			sendHeader: "Authorization",
			wantHeader: "Authorization", wantValue: "Bearer the-hosts-real-openai-key",
		},
	}
	for _, c := range cases {
		t.Run(c.slot.ID, func(t *testing.T) {
			withSlots(t, c.slot)
			tok := TokenPrefix + "box_" + c.slot.ID + "_deadbeef"
			srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: c.slot.ID, Kind: c.slot.Kind, Name: "box"}})

			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"hi":1}`))
			if err != nil {
				t.Fatal(err)
			}
			if c.sendHeader == "Authorization" {
				req.Header.Set("Authorization", "Bearer "+tok)
			} else {
				req.Header.Set(c.sendHeader, tok)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(res.Body)
				t.Fatalf("status %d: %s", res.StatusCode, b)
			}
			if got := up.gotHeader.Get(c.wantHeader); got != c.wantValue {
				t.Errorf("upstream %s = %q, want %q", c.wantHeader, got, c.wantValue)
			}
			for k, v := range c.wantExtra {
				if got := up.gotHeader.Get(k); got != v {
					t.Errorf("upstream %s = %q, want %q", k, got, v)
				}
			}
			// The point of the whole exercise.
			for _, h := range []string{"Authorization", "X-Api-Key", "Api-Key"} {
				if v := up.gotHeader.Get(h); strings.Contains(v, TokenPrefix) {
					t.Errorf("the loan token reached the provider in %s: %q", h, v)
				}
			}
		})
	}
}

func claudeWhere(home, _ string) string {
	return filepath.Join(home, ".cs-claude", ".credentials.json")
}
func codexWhere(home, _ string) string { return filepath.Join(home, ".cs-codex", "auth.json") }

// A credential the lender did not mint is refused in its own error shape and
// forwarded nowhere: a default upstream would make this an open relay.
func TestUnknownLoanIsRefusedAndNeverForwarded(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	withSlots(t, Slot{ID: "claude", Kind: Login, Origin: up.URL, Header: "authorization", Prefix: "Bearer ", read: readClaudeLogin, where: claudeWhere})
	srv := newTestServer(t, home, fixedLoans{})

	for _, tok := range []string{TokenPrefix + "box_claude_nope", "a-credential-from-somewhere-else", ""} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", http.NoBody)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401 (%s)", tok, res.StatusCode, body)
		}
		var eb errorBody
		if err := json.Unmarshal(body, &eb); err != nil || eb.Error.Source != "cs-sandbox lend" {
			t.Errorf("token %q: error body %s, want the lender's own shape", tok, body)
		}
	}
	if up.calls != 0 {
		t.Errorf("upstream was called %d times for refused requests, want 0", up.calls)
	}
}

// The upstream sees its own path. The origin carries the provider's prefix, so
// a client that appends /responses reaches /backend-api/codex/responses without
// the lender knowing anything about either.
func TestOriginCarriesThePathPrefix(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	withSlots(t, Slot{ID: "codex", Kind: Login, Origin: up.URL + "/backend-api/codex", Header: "authorization", Prefix: "Bearer ", read: readCodexLogin, where: codexWhere})
	tok := TokenPrefix + "box_codex_1"
	srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: "codex", Name: "box"}})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/responses", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if up.gotPath != "/backend-api/codex/responses" {
		t.Errorf("upstream path = %q, want %q", up.gotPath, "/backend-api/codex/responses")
	}
	// TLS vhosting rejects a request claiming the inbound host.
	want, _ := url.Parse(up.URL)
	if up.gotHost != want.Host {
		t.Errorf("upstream Host = %q, want %q", up.gotHost, want.Host)
	}
}

// A model answers token by token, and a proxy that buffers turns that into one
// lump. The lender must flush what it gets, when it gets it.
func TestStreamPassesThroughUnbuffered(t *testing.T) {
	up := newUpstream(t)
	up.sse = true
	home := hostProfile(t)
	withSlots(t, Slot{ID: "claude", Kind: Login, Origin: up.URL, Header: "authorization", Prefix: "Bearer ", read: readClaudeLogin, where: claudeWhere})
	tok := TokenPrefix + "box_claude_1"
	srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: "claude", Name: "box"}})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q, want text/event-stream", ct)
	}
	buf := make([]byte, 256)
	n, err := res.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read first chunk: %v", err)
	}
	// Arriving before the body is complete is the property; a buffering proxy
	// would deliver all three events at once, or nothing until the end.
	if got := string(buf[:n]); !strings.Contains(got, "data: chunk-0") {
		t.Errorf("first read = %q, want it to start with the first event", got)
	}
}

// A stale credential in the sandbox's environment must not reach a provider
// beside the real one.
func TestEveryClientCredentialIsStripped(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	withSlots(t, Slot{ID: "anthropic", Kind: Key, Origin: up.URL, Header: "x-api-key", read: keyReader("anthropic"), where: keyPath("anthropic")})
	tok := TokenPrefix + "box_anthropic_1"
	srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: "anthropic", Name: "box"}})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", http.NoBody)
	req.Header.Set("X-Api-Key", tok)
	req.Header.Set("Authorization", "Bearer a-stale-one-from-an-earlier-create")
	req.Header.Set("X-Forwarded-For", "10.89.0.7")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := up.gotHeader.Get("Authorization"); got != "" {
		t.Errorf("a second credential reached the provider: Authorization = %q", got)
	}
	if got := up.gotHeader.Get("X-Forwarded-For"); got != "" {
		t.Errorf("the sandbox's address reached the provider: X-Forwarded-For = %q", got)
	}
}

// anthropic-beta is a list a client uses for its own features, so the lender's
// value is added to it rather than written over it.
func TestExtraHeaderIsMergedNotOverwritten(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	withSlots(t, Slot{ID: "claude", Kind: Login, Origin: up.URL, Header: "authorization", Prefix: "Bearer ", read: readClaudeLogin, where: claudeWhere})
	tok := TokenPrefix + "box_claude_1"
	srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: "claude", Name: "box"}})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	got := up.gotHeader.Get("Anthropic-Beta")
	if !strings.Contains(got, "claude-code-20250219") || !strings.Contains(got, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, want the client's value and the lender's", got)
	}
}

// An expired host login is reported as itself. The alternative is an upstream
// 401, which the agent shows as "you are not signed in" inside a sandbox that
// was never signed in to begin with.
func TestExpiredHostLoginSaysSo(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".cs-claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UnixMilli()
	if err := os.WriteFile(p, fmt.Appendf(nil, `{"claudeAiOauth":{"accessToken":"x","expiresAt":%d}}`, past), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readClaudeLogin(home, "")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("readClaudeLogin on an expired token = %v, want an error naming the expiry", err)
	}
	if !strings.Contains(err.Error(), "cs-claude") {
		t.Errorf("the error should name the command that fixes it: %v", err)
	}
}

// A missing key names the file and the command that creates it, because the
// remedy is the useful half of the message.
func TestMissingKeyPrintsTheRemedy(t *testing.T) {
	home := t.TempDir()
	_, _, err := keyReader("anthropic")(home, KeysDir(home))
	if err == nil {
		t.Fatal("a missing key file should be an error")
	}
	for _, want := range []string{".cs-keys/anthropic", "ANTHROPIC_API_KEY", "chmod 600"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// A lent sandbox holds the shape its agent's own sign-in leaves behind, with a
// value that is worth nothing. This is the property the whole design rests on,
// so it is asserted per slot rather than described.
func TestMintGuestFabricatesTheAgentsOwnCredentialFile(t *testing.T) {
	for _, c := range []struct{ id, file string }{
		{"claude", ".credentials.json"},
		{"codex", "auth.json"},
	} {
		t.Run(c.id, func(t *testing.T) {
			s, _ := SlotByID(c.id)
			g, err := s.MintGuest("feature")
			if err != nil {
				t.Fatal(err)
			}
			if g.File != c.file || g.Agent != c.id {
				t.Errorf("seeded %s/%s, want %s/%s", g.Agent, g.File, c.id, c.file)
			}
			if !strings.HasPrefix(g.Label, "loan_feature_"+c.id+"_") {
				t.Errorf("label %q should name its sandbox and slot", g.Label)
			}
			// The file has to parse as JSON, or the client reads nothing.
			var doc map[string]any
			if err := json.Unmarshal(g.Doc, &doc); err != nil {
				t.Fatalf("the seeded file is not JSON: %v", err)
			}
			// And the value the client will send has to be findable in it, or
			// the lender would have nothing to match.
			if !strings.Contains(string(g.Doc), g.Wire) {
				t.Error("the value the client will present is not in the file it reads")
			}
			// Two sandboxes never share one.
			other, _ := s.MintGuest("feature")
			if other.Wire == g.Wire {
				t.Error("two mints produced the same credential")
			}
		})
	}
	// A key needs no file: an environment variable is already the shape its
	// client expects.
	s, _ := SlotByID("anthropic")
	g, err := s.MintGuest("feature")
	if err != nil {
		t.Fatal(err)
	}
	if g.File != "" || g.Doc != nil {
		t.Errorf("a key slot seeded %s, want no file", g.File)
	}
	if g.Wire != g.Label || !strings.HasPrefix(g.Wire, TokenPrefix) {
		t.Errorf("a key loan should travel as its own label, got %q", g.Wire)
	}
}

// Codex decodes its tokens and treats anything it cannot read as signed out, so
// the forged one has to be a JWT in form. It must also be obviously ours: an
// account that belongs to nobody, and a claim naming the loan.
func TestTheForgedCodexTokenIsDecodableAndObviouslyOurs(t *testing.T) {
	s, _ := SlotByID("codex")
	g, err := s.MintGuest("feature")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(g.Wire, ".")
	if len(parts) != 3 {
		t.Fatalf("the forged token has %d segments, want 3", len(parts))
	}
	var claims map[string]any
	seg, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("the payload does not decode: %v", err)
	}
	if err := json.Unmarshal(seg, &claims); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}
	if claims["cs_sandbox_loan"] != g.Label {
		t.Errorf("cs_sandbox_loan = %v, want the loan label", claims["cs_sandbox_loan"])
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if acct, _ := auth["chatgpt_account_id"].(string); !strings.HasPrefix(acct, "00000000-") {
		t.Errorf("chatgpt_account_id = %q, want an account belonging to nobody", acct)
	}
	// Far enough out that the client never goes looking for a refresh it
	// cannot do.
	exp, _ := claims["exp"].(float64)
	if time.Until(time.Unix(int64(exp), 0)) < 30*24*time.Hour {
		t.Errorf("exp is %v away, want longer than any sandbox lives", time.Until(time.Unix(int64(exp), 0)))
	}
}

// Revocation is the sandbox lifecycle: the loan record goes when the instance
// directory does, and the lender stops honouring it.
func TestLoansAreRevokedWithTheInstanceDirectory(t *testing.T) {
	dir := t.TempDir()
	inst := filepath.Join(dir, "default", "feature")
	tok := TokenPrefix + "feature_claude_1"
	if err := WriteLoans(inst, []Loan{{Token: tok, Slot: "claude", Kind: Login}}); err != nil {
		t.Fatal(err)
	}
	fl := &FileLoans{Dir: dir, TTL: time.Millisecond}
	l, ok := fl.Lookup(tok)
	if !ok {
		t.Fatal("a recorded loan should resolve")
	}
	if l.Group != "default" || l.Name != "feature" {
		t.Errorf("loan resolved to %s/%s, want default/feature", l.Group, l.Name)
	}
	if err := os.RemoveAll(inst); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := fl.Lookup(tok); ok {
		t.Error("a destroyed sandbox's loan still resolves")
	}
}

// The file the token is written to is a secret, and a mode a group can read
// would undo the whole exercise.
func TestLoanFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLoans(dir, []Loan{{Token: "loan_x_claude_1", Slot: "claude"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, loansFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("loans.json mode = %v, want 0600", fi.Mode().Perm())
	}
}

// The shipped table is what the docs and the flags promise.
func TestShippedSlots(t *testing.T) {
	for _, c := range []struct {
		id, origin, version, header, baseEnv string
		authEnvs                             []string
	}{
		{"claude", "https://api.anthropic.com", "/v1", "authorization", "ANTHROPIC_BASE_URL", []string{"ANTHROPIC_AUTH_TOKEN"}},
		{"codex", "https://chatgpt.com/backend-api/codex", "", "authorization", "OPENAI_BASE_URL", []string{"OPENAI_API_KEY"}},
		{"anthropic", "https://api.anthropic.com", "/v1", "x-api-key", "ANTHROPIC_BASE_URL", []string{"ANTHROPIC_API_KEY"}},
		// Two variables: Codex reads CODEX_API_KEY and attaches no header at
		// all for OPENAI_API_KEY, which every OpenAI-shaped client reads.
		{"openai", "https://api.openai.com", "/v1", "authorization", "OPENAI_BASE_URL", []string{"OPENAI_API_KEY", "CODEX_API_KEY"}},
		{"fireworks", "https://api.fireworks.ai/inference", "/v1", "authorization", "OPENCODE_BASE_URL", []string{"FIREWORKS_API_KEY"}},
	} {
		s, ok := SlotByID(c.id)
		if !ok {
			t.Fatalf("slot %q is missing", c.id)
		}
		if s.Origin != c.origin || s.Version != c.version || s.Header != c.header || s.BaseEnv != c.baseEnv {
			t.Errorf("slot %s = %+v, want origin %s version %s header %s base %s",
				c.id, s, c.origin, c.version, c.header, c.baseEnv)
		}
		if strings.Join(s.AuthEnvs, ",") != strings.Join(c.authEnvs, ",") {
			t.Errorf("slot %s auth variables = %v, want %v", c.id, s.AuthEnvs, c.authEnvs)
		}
	}
	if got := SlotIDs(Login); len(got) != 2 || got[0] != "claude" {
		t.Errorf("login slots = %v", got)
	}
	if got := SlotIDs(Key); len(got) != 3 || got[0] != "anthropic" || got[2] != "fireworks" {
		t.Errorf("key slots = %v", got)
	}
}

// A token that resolves to nothing must not put the lender on the disk once per
// request: anything that can reach the port could otherwise make it scan.
func TestUnknownTokensDoNotRescanPerRequest(t *testing.T) {
	dir := t.TempDir()
	fl := &FileLoans{Dir: dir, TTL: time.Hour} // a long window, so only a miss can reload

	// The first miss is allowed one scan, which is what lets a sandbox created a
	// moment ago work. It finds nothing here.
	if _, ok := fl.Lookup(TokenPrefix + "nobody_claude_ffff"); ok {
		t.Fatal("nothing is recorded yet")
	}
	// Record a loan, then spend the window on misses. None of them may scan, so
	// none of them may find it.
	tok := TokenPrefix + "box_claude_1"
	if err := WriteLoans(filepath.Join(dir, "default", "box"), []Loan{{Token: tok, Slot: "claude"}}); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		fl.Lookup(TokenPrefix + "nobody_claude_ffff")
	}
	if _, ok := fl.Lookup(tok); ok {
		t.Error("a flood of unknown tokens re-read the disk; the rescan should be bounded to one per window")
	}
}

// But a loan minted a moment ago has to work on its first call, which is what
// the bounded rescan buys back.
func TestALoanMintedAfterTheLastScanStillResolves(t *testing.T) {
	dir := t.TempDir()
	fl := &FileLoans{Dir: dir, TTL: time.Hour}
	tok := TokenPrefix + "fresh_claude_1"
	if _, ok := fl.Lookup(tok); ok {
		t.Fatal("nothing is recorded yet")
	}
	if err := WriteLoans(filepath.Join(dir, "default", "fresh"), []Loan{{Token: tok, Slot: "claude"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(missReloadEvery + 20*time.Millisecond)
	if _, ok := fl.Lookup(tok); !ok {
		t.Error("a loan written since the last scan should resolve on the next call")
	}
}

// A pid file can outlive the process it names, and the number is then whatever
// the kernel handed out next. Stop must not signal on that evidence alone.
func TestStopDoesNotSignalAPidItCannotConfirm(t *testing.T) {
	dir := t.TempDir()
	d := Daemon{Dir: dir}
	// Our own pid, recorded against an address nothing answers on: exactly the
	// shape a record left behind by a reboot has.
	if err := d.writeRecord(os.Getpid(), "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop on a stale record should succeed: %v", err)
	}
	// Signalled, this test would not be here to check.
	if _, _, alive := d.Status(); alive {
		t.Error("the record should be gone")
	}
}

// This package holds every credential on the host, and it is the one package
// that must not be able to go looking for more. Everything it reads arrives as
// a parameter, which is a property worth failing a build over rather than a
// convention worth writing down: the day someone reaches for internal/paths to
// save an argument, this stops them.
//
// It is also what keeps the lender extractable, which SPEC 10.2's design rests
// on.
func TestThisPackageImportsNothingElseInTheRepository(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	const self = "github.com/codesweep-ai/sandbox/internal/lend"
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		dep := strings.TrimSpace(line)
		if dep == self || !strings.HasPrefix(dep, "github.com/codesweep-ai/") {
			continue
		}
		t.Errorf("internal/lend imports %s; everything it needs must arrive as a parameter", dep)
	}
}

// Clients disagree about who owns the version segment of a provider's URL.
// Claude Code posts /v1/messages against a base URL it treats as the site root;
// OpenCode's Anthropic client posts /messages against one it treats as already
// versioned. One base URL serves both only if the lender settles it.
func TestTheVersionSegmentIsEnsuredExactlyOnce(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	withSlots(t, Slot{
		ID: "anthropic", Kind: Key, Origin: up.URL, Version: "/v1",
		Header: "x-api-key", read: keyReader("anthropic"), where: keyPath("anthropic"),
	})
	tok := TokenPrefix + "box_anthropic_1"
	srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: "anthropic", Name: "box"}})

	for _, c := range []struct{ sent, want string }{
		{"/v1/messages", "/v1/messages"}, // a client that versions its own path
		{"/messages", "/v1/messages"},    // one that expects the base URL to
		{"/v1", "/v1"},                   // the segment alone
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+c.sent, http.NoBody)
		req.Header.Set("X-Api-Key", tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if up.gotPath != c.want {
			t.Errorf("a client posting %s reached the provider at %s, want %s", c.sent, up.gotPath, c.want)
		}
	}
}

// An upstream with no version segment gets the client's path untouched: the
// Codex subscription transport carries its own prefix in the origin.
func TestASlotWithNoVersionLeavesThePathAlone(t *testing.T) {
	up := newUpstream(t)
	home := hostProfile(t)
	withSlots(t, Slot{
		ID: "codex", Kind: Login, Origin: up.URL + "/backend-api/codex",
		Header: "authorization", Prefix: "Bearer ", read: readCodexLogin, where: codexWhere,
	})
	tok := TokenPrefix + "box_codex_1"
	srv := newTestServer(t, home, fixedLoans{tok: {Token: tok, Slot: "codex", Name: "box"}})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/responses", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if up.gotPath != "/backend-api/codex/responses" {
		t.Errorf("upstream path = %q, want the origin's prefix and the client's path", up.gotPath)
	}
}

// Every fabricated credential takes the form the real one has, because that is
// what keeps a client on the code path it always takes. What the form IS
// differs by provider: Anthropic issues an opaque prefixed token, and Codex a
// pair of JWTs. Copying one shape into the other's file would be less faithful,
// not more consistent.
func TestEachFabricationTakesItsProvidersOwnForm(t *testing.T) {
	claude, _ := SlotByID("claude")
	g, err := claude.MintGuest("feature")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OAuth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(g.Doc, &doc); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{
		"sk-ant-oat01-": doc.OAuth.AccessToken,
		"sk-ant-ort01-": doc.OAuth.RefreshToken,
	} {
		if !strings.HasPrefix(tok, name) {
			t.Errorf("token %q does not carry the %s prefix a real one has", tok, name)
		}
		if len(tok) != claudeTokenLen {
			t.Errorf("token is %d chars, want %d, the length a real one has", len(tok), claudeTokenLen)
		}
		// One segment: an Anthropic credential is opaque, not a JWT.
		if strings.Contains(tok, ".") {
			t.Errorf("token %q is structured; a real Anthropic token is opaque", tok)
		}
		// And it says what it is before it says anything else.
		if !strings.HasPrefix(tok, name+"loan-") {
			t.Errorf("token %q should read as a loan before it reads as a credential", tok)
		}
	}
	if doc.OAuth.AccessToken != g.Wire {
		t.Error("the value the lender matches on is not the one the client will send")
	}
	if g.Wire == g.Label {
		t.Error("the wire value should take the provider's form, not the label's")
	}

	// The other form, for contrast: Codex signs in with JWTs, so its file holds
	// forged ones.
	codex, _ := SlotByID("codex")
	c, err := codex.MintGuest("feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(c.Wire, ".")) != 3 {
		t.Errorf("the Codex token is not a JWT: %q", c.Wire[:min(40, len(c.Wire))])
	}
}
