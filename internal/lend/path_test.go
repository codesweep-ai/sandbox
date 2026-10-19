package lend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathLender is a lender with every shipped slot pointed at one test upstream,
// keeping the path each origin carries, and a read that counts.
type pathLender struct {
	srv    *httptest.Server
	lender *Server
	up     *upstream
	reads  *int
}

func newPathLender(t *testing.T) pathLender {
	t.Helper()
	up := newUpstream(t)
	home := hostProfile(t)
	if err := os.WriteFile(filepath.Join(KeysDir(home), "fireworks"), []byte("the-hosts-real-fireworks-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := new(int)
	var table []Slot
	loans := fixedLoans{}
	for _, s := range slots {
		o, err := url.Parse(s.Origin)
		if err != nil {
			t.Fatal(err)
		}
		s.Origin = up.URL + o.Path
		read := s.read
		s.read = func(home, keysDir string) (string, map[string]string, error) {
			*reads++
			return read(home, keysDir)
		}
		table = append(table, s)
		tok := TokenPrefix + "box_" + s.ID + "_1"
		loans[tok] = Loan{Token: tok, Slot: s.ID, Kind: s.Kind, Name: "box"}
	}
	withSlots(t, table...)
	l := New(Config{Home: home, KeysDir: KeysDir(home), Loans: loans, Callers: CallersAny})
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	return pathLender{srv: srv, lender: l, up: up, reads: reads}
}

// raw sends one request line exactly as written. http.NewRequest and curl
// resolve dot segments before sending, so they cannot send the paths this is
// about.
func (p pathLender) raw(t *testing.T, slot, target string) (int, errorBody) {
	t.Helper()
	c, err := net.Dial("tcp", p.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: cs-lender\r\nAuthorization: Bearer %sbox_%s_1\r\nConnection: close\r\n\r\n",
		target, TokenPrefix, slot)
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%s: %v", target, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	return res.StatusCode, eb
}

// A path an upstream could resolve differently from the lender is refused
// before anything reads it: the backend check, the fault, the credential and
// the upstream all read the path, and the first is what R146a turns on. Sent
// on, "/../wham/accounts/check" reaches chatgpt.com as
// /backend-api/codex/../wham/accounts/check with the host's real token, which a
// hop that resolves dot segments delivers to the backend R146a blocks.
func TestAnUncleanPathIsRefusedAndNeverForwarded(t *testing.T) {
	p := newPathLender(t)
	cases := []struct{ slot, target string }{
		{"codex", "/../wham/accounts/check"},
		{"codex", "/%2e%2e/wham/tasks"},
		{"codex", "/%2E%2e/wham/tasks"},
		{"codex", "/.%2e/wham/tasks"},
		{"codex", "/responses/../../wham/x"},
		{"codex", "/./responses"},
		{"codex", "/responses/."},
		{"codex", "/..;/wham/tasks"},
		{"codex", "/backend-api/../responses"},
		{"codex", "/x/../backend-api/wham/usage"},
		{"claude", "/v1/../api/oauth/profile"},
		{"claude", "/v1/messages/%2e%2e/%2e%2e/x"},
		{"anthropic", "/v1/..%2f..%2fx"},
		{"anthropic", "/v1%2fmessages"},
		{"anthropic", "/v1//messages"},
		{"openai", `/v1/..\..\x`},
		{"openai", "/v1/%5c..%5cx"},
		{"openai", "/v1/%252e%252e/x"},
		{"fireworks", "/v1/chat/completions/../../../x"},
		{"fireworks", "/x/../healthz"},
		{"fireworks", "*"},
	}
	for _, c := range cases {
		status, eb := p.raw(t, c.slot, c.target)
		if status != http.StatusBadRequest || eb.Error.Type != "unclean_path" || eb.Error.Source != "cs-sandbox lend" {
			t.Errorf("%s %s: status %d, error %+v, want 400 unclean_path", c.slot, c.target, status, eb.Error)
		}
	}
	if p.up.calls != 0 {
		t.Errorf("an unclean path reached the upstream %d time(s), last as %q", p.up.calls, p.up.gotURI)
	}
	if *p.reads != 0 {
		t.Errorf("the credential was read %d time(s) for paths that were refused", *p.reads)
	}
	if st := p.lender.Snapshot(); st.Refused != len(cases) || st.Lent != 0 || st.Blocked != 0 || st.Answered != 0 {
		t.Errorf("counted %+v, want %d refused and nothing else", st, len(cases))
	}
}

// The paths real clients send still reach the provider as they were sent. A
// dot inside a segment is not a dot segment, and an escaped colon is how an
// SDK sends a fine-tuned model's id.
func TestACleanPathStillReachesTheUpstreamUnchanged(t *testing.T) {
	p := newPathLender(t)
	for _, c := range []struct{ slot, target, want string }{
		{"claude", "/v1/messages?beta=true", "/v1/messages"},
		{"anthropic", "/v1/messages/count_tokens", "/v1/messages/count_tokens"},
		{"codex", "/responses", "/backend-api/codex/responses"},
		{"openai", "/v1/models/gpt-4.1", "/v1/models/gpt-4.1"},
		{"openai", "/v1/models/ft%3Agpt-4o%3Aacme%3Aabc123", "/v1/models/ft:gpt-4o:acme:abc123"},
		{"openai", "/v1/models/gpt-4%2e1", "/v1/models/gpt-4.1"},
		{"fireworks", "/v1/models/", "/inference/v1/models/"},
	} {
		before := p.up.calls
		if status, eb := p.raw(t, c.slot, c.target); status != http.StatusOK {
			t.Errorf("%s %s: status %d, error %+v", c.slot, c.target, status, eb.Error)
			continue
		}
		if p.up.calls != before+1 || p.up.gotPath != c.want {
			t.Errorf("%s %s reached the upstream as %q, want %q", c.slot, c.target, p.up.gotPath, c.want)
		}
		uri, _, _ := strings.Cut(p.up.gotURI, "?")
		if got, err := url.PathUnescape(uri); err != nil || got != c.want {
			t.Errorf("%s %s was sent as %q, which does not decode to %q", c.slot, c.target, p.up.gotURI, c.want)
		}
	}
}

// The checks behind the refusal read the path as it resolves, so a path that
// got past it still could not climb out of the version or into the backend.
func TestThePathChecksReadTheResolvedPath(t *testing.T) {
	for _, c := range []struct{ version, in, want string }{
		{"/v1", "/v1/../../x", "/v1/x"},
		{"/v1", "/v1/messages", "/v1/messages"},
		{"", "/responses/../../wham", "/wham"},
		{"", "/v1/models/", "/v1/models/"},
	} {
		if got := ensureVersion(c.version, c.in); got != c.want {
			t.Errorf("ensureVersion(%q, %q) = %q, want %q", c.version, c.in, got, c.want)
		}
	}
	for in, want := range map[string]bool{
		"/x/../backend-api/wham/usage": true,
		"/backend-api/../responses":    false,
		"/backend-api":                 true,
		"/backend-apix":                false,
	} {
		if underBackend(in) != want {
			t.Errorf("underBackend(%q) = %v, want %v", in, !want, want)
		}
	}
}
