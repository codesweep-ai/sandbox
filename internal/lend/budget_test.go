package lend

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// meteredLender is a lender for one sandbox, g/box, borrowing openai in front
// of a stand-in provider that answers /responses with a recorded codex stream,
// gzipped whenever it is asked for gzip. Its budget and its usage are read and
// written in a real instances root.
type meteredLender struct {
	root, inst  string
	lender      *Server
	srv         *httptest.Server
	lent, meter Loan
	calls       atomic.Int32
	encodings   atomic.Value
	stream      []byte
}

func newMeteredLender(t *testing.T) *meteredLender {
	t.Helper()
	m := &meteredLender{stream: cassetteFile(t, "codex-login-lent", "resp", 3)}
	m.over(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		m.encodings.Store(r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write(m.stream)
			_ = gz.Close()
			return
		}
		_, _ = w.Write(m.stream)
	}))
	return m
}

// over starts the lender in front of a stand-in provider with this handler.
func (m *meteredLender) over(t *testing.T, provider http.Handler) {
	t.Helper()
	up := httptest.NewServer(provider)
	t.Cleanup(up.Close)
	m.at(t, up.URL)
}

// at starts the lender in front of whatever is at this origin.
func (m *meteredLender) at(t *testing.T, origin string) {
	t.Helper()
	withSlots(t, Slot{ID: "openai", Kind: Key, Origin: origin, Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL", read: keyReader("openai"), where: keyPath("openai")})
	m.root = t.TempDir()
	m.inst = filepath.Join(m.root, "g", "box")
	if err := os.MkdirAll(m.inst, 0o700); err != nil {
		t.Fatal(err)
	}
	m.lent = Loan{Token: TokenPrefix + "box_openai_feedface", Slot: "openai", Kind: Key, Group: "g", Name: "box"}
	m.meter = Loan{Token: TokenPrefix + "box_meter_feedface", Kind: MeterLoan, Group: "g", Name: "box"}
	id := MeterID([]Loan{m.lent, m.meter})
	m.lent.Meter, m.meter.Meter = id, id
	m.srv = m.start(t)
}

// start is a lender process over the same instances root, as a restart is.
func (m *meteredLender) start(t *testing.T) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	home := hostProfile(t)
	m.lender = New(Config{Home: home, KeysDir: KeysDir(home), Callers: CallersAny, Log: log,
		Loans: fixedLoans{m.lent.Token: m.lent, m.meter.Token: m.meter}, Meter: NewMeter(m.root, "", log)})
	srv := httptest.NewServer(m.lender)
	t.Cleanup(srv.Close)
	return srv
}

func (m *meteredLender) do(t *testing.T, method, path string, loan Loan) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, m.srv.URL+path, strings.NewReader(`{"model":"gpt-5.6-sol","input":"pong"}`))
	req.Header.Set("Authorization", "Bearer "+loan.Token)
	// What Claude Code sends, and what used to leave every answer unreadable.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (m *meteredLender) reading(t *testing.T) Reading {
	t.Helper()
	code, body := m.do(t, http.MethodGet, MeterPath, m.meter)
	if code != http.StatusOK {
		t.Fatalf("the reading answered %d: %s", code, body)
	}
	var rd Reading
	if err := json.Unmarshal([]byte(body), &rd); err != nil {
		t.Fatalf("the reading is not JSON: %v: %s", err, body)
	}
	return rd
}

func (m *meteredLender) budget(t *testing.T, b Budget) {
	t.Helper()
	if err := WriteBudget(m.inst, b); err != nil {
		t.Fatal(err)
	}
}

// TestTheLenderMetersWhatItForwards: a client that asks for gzip still gets the
// provider's content, byte for byte and uncompressed, and the meter reads its
// usage. The count is written through to the sandbox's usage record, and the
// sandbox reads it back with its meter loan.
func TestTheLenderMetersWhatItForwards(t *testing.T) {
	m := newMeteredLender(t)
	code, body := m.do(t, http.MethodPost, "/v1/responses", m.lent)
	if code != http.StatusOK || body != string(m.stream) {
		t.Fatalf("the client got %d and %d bytes, want 200 and the %d bytes the provider sent", code, len(body), len(m.stream))
	}
	if enc, _ := m.encodings.Load().(string); enc != "gzip" {
		t.Errorf("the provider was asked for %q: the lender's own transport should ask for gzip and decompress", enc)
	}
	rd := m.reading(t)
	want := map[string]int64{TypeInput: 6583, TypeCached: 1920, TypeOutput: 17}
	for typ, n := range want {
		if rd.Used[typ] != n {
			t.Errorf("used %s = %d, want %d", typ, rd.Used[typ], n)
		}
	}
	if rd.Calls != 1 || rd.Estimated != 0 || len(rd.Ask) != 0 || len(rd.Limits) != 0 {
		t.Errorf("reading = %+v, want one read call and no budget", rd)
	}
	u, err := ReadUsage(UsagePath(m.root, "g", "box", m.lent.Meter))
	if err != nil {
		t.Fatalf("the usage record: %v", err)
	}
	if len(u.Rows) != 1 || u.Rows[0].Model != "gpt-5.6-sol" || u.Rows[0].Surface != "openai.responses" || u.Rows[0].Slot != "openai" {
		t.Errorf("usage rows = %+v", u.Rows)
	}
	if fi, err := os.Stat(UsagePath(m.root, "g", "box", m.lent.Meter)); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("the usage record is %v, want 0600", fi.Mode().Perm())
	}

	// A free call is forwarded and charged nothing.
	if code, _ := m.do(t, http.MethodGet, "/models", m.lent); code != http.StatusOK {
		t.Errorf("GET /models answered %d", code)
	}
	if rd := m.reading(t); rd.Calls != 1 {
		t.Errorf("a free call was counted: %d calls", rd.Calls)
	}
}

// TestTheCountSurvivesARestartAndNotARecreate: the next lender process carries
// on from the usage record. A sandbox created again under the same name holds new
// loans, so a new meter id, and starts at zero (R155c).
func TestTheCountSurvivesARestartAndNotARecreate(t *testing.T) {
	m := newMeteredLender(t)
	m.do(t, http.MethodPost, "/v1/responses", m.lent)
	m.srv = m.start(t)
	if rd := m.reading(t); rd.Used[TypeOutput] != 17 || rd.Calls != 1 {
		t.Errorf("after a restart the reading is %+v, want the call made before it", rd)
	}
	m.lent.Token, m.meter.Token = TokenPrefix+"box_openai_0ddba11", TokenPrefix+"box_meter_0ddba11"
	id := MeterID([]Loan{m.lent, m.meter})
	m.lent.Meter, m.meter.Meter = id, id
	m.srv = m.start(t)
	if rd := m.reading(t); rd.Calls != 0 || rd.Used[TypeOutput] != 0 {
		t.Errorf("a sandbox created again started at %+v, want zero", rd)
	}
}

// TestASoftBudgetAsksAndNeverRefuses: past its limit, a soft budget names the
// type in the reading, which the turn driver and the hand-off hook act on, and
// the lender keeps forwarding.
func TestASoftBudgetAsksAndNeverRefuses(t *testing.T) {
	m := newMeteredLender(t)
	m.budget(t, Budget{Limits: map[string]int64{TypeOutput: 20, TypeCached: 1 << 30}})
	m.do(t, http.MethodPost, "/v1/responses", m.lent)
	if rd := m.reading(t); len(rd.Ask) != 0 || rd.Hard || rd.AskAt != 1 {
		t.Errorf("under its limit the reading is %+v, want nothing to ask", rd)
	}
	m.do(t, http.MethodPost, "/v1/responses", m.lent)
	rd := m.reading(t)
	if !slices.Equal(rd.Ask, []string{TypeOutput}) || !slices.Equal(rd.Spent, []string{TypeOutput}) {
		t.Errorf("past its output limit the reading is %+v, want output asked and spent", rd)
	}
	if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusOK || m.calls.Load() != 3 {
		t.Errorf("a soft budget refused a call: %d, %d calls reached the provider", code, m.calls.Load())
	}
}

// TestAHardBudgetRefusesOnceATypeIsSpent: the refusal comes before the credential
// is read, carries the marker every turn driver matches and names the type that
// ran out, and a top-up admits the next call (R153h). A free call still goes.
func TestAHardBudgetRefusesOnceATypeIsSpent(t *testing.T) {
	m := newMeteredLender(t)
	m.budget(t, Budget{Limits: map[string]int64{TypeOutput: 18}, Hard: true})
	m.do(t, http.MethodPost, "/v1/responses", m.lent)
	if rd := m.reading(t); !slices.Equal(rd.Ask, []string{TypeOutput}) || len(rd.Spent) != 0 {
		// 17 of 18 is at a hard budget's ask line, 95% of the limit rounded
		// down, and under the limit itself.
		t.Errorf("at 17 of 18 the reading is %+v, want output asked and not spent", rd)
	}
	m.do(t, http.MethodPost, "/v1/responses", m.lent)
	before := m.calls.Load()
	code, body := m.do(t, http.MethodPost, "/v1/responses", m.lent)
	if code != SpentStatus || m.calls.Load() != before {
		t.Fatalf("a spent hard budget answered %d and reached the provider %d times, want %d and none",
			code, m.calls.Load()-before, SpentStatus)
	}
	var eb errorBody
	_ = json.Unmarshal([]byte(body), &eb)
	if eb.Error.Type != "budget_exhausted" || !strings.HasPrefix(eb.Error.Message, SpentMarker+":") ||
		!strings.Contains(eb.Error.Message, "of its 18 output tokens") {
		t.Errorf("the refusal is %s", body)
	}
	if code, _ := m.do(t, http.MethodGet, "/models", m.lent); code != http.StatusOK {
		t.Errorf("a free call was refused under a spent budget: %d", code)
	}
	if rd := m.reading(t); rd.Refused != 1 {
		t.Errorf("refused = %d, want 1", rd.Refused)
	}
	m.budget(t, Budget{Limits: map[string]int64{TypeOutput: 1000}, Hard: true})
	if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusOK {
		t.Errorf("after a top-up the next call answered %d", code)
	}
}

// TestACallThatGetsNoAnswerIsCharged: the provider read the whole request, so it
// may have spent tokens on it, whichever side gave up before the headers. Each is
// charged once, as an estimate, and a hard budget then holds.
func TestACallThatGetsNoAnswerIsCharged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool // the client gives up, rather than the provider dropping the call
	}{
		{name: "the provider dropped the connection"},
		{name: "the client gave up waiting", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &meteredLender{}
			var received atomic.Int32
			held := make(chan struct{})
			m.over(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if received.Add(1) == 1 {
					close(held)
				}
				if !tc.cancel {
					panic(http.ErrAbortHandler)
				}
				<-r.Context().Done()
			}))
			m.budget(t, Budget{Limits: map[string]int64{TypeOutput: 1}, Hard: true})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.srv.URL+"/v1/responses", strings.NewReader(strings.Repeat("x", 4500)))
			req.Header.Set("Authorization", "Bearer "+m.lent.Token)
			if tc.cancel {
				go func() { <-held; cancel() }()
			}
			if resp, err := http.DefaultTransport.RoundTrip(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusBadGateway {
					t.Errorf("the client saw %d, want 502", resp.StatusCode)
				}
			}
			m.until(t, func(st Stats) bool { return st.Metered == 1 })

			rd := m.reading(t)
			if rd.Calls != 1 || rd.Estimated != 1 || rd.Used[TypeInput] != 1000 || rd.Used[TypeOutput] != estimateOutput {
				t.Errorf("reading = %+v, want one estimated call of 1000 input and %d output", rd, estimateOutput)
			}
			if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != SpentStatus {
				t.Errorf("the next call answered %d, want the budget's %d", code, SpentStatus)
			}
			if received.Load() != 1 {
				t.Errorf("the provider received %d calls, want the first alone", received.Load())
			}
		})
	}
}

// until waits for the lender's counters to say this, because a client that gives
// up has its answer before the lender has finished with the call.
func (m *meteredLender) until(t *testing.T, done func(Stats) bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if done(m.lender.Snapshot()) {
			return
		}
	}
	t.Fatalf("the lender's counters never got there: %+v", m.lender.Snapshot())
}

// TestACallThatNeverWentOutIsFree: a provider cannot spend tokens on a request it
// never had, and an agent retries these, so an outage must not spend a budget.
func TestACallThatNeverWentOutIsFree(t *testing.T) {
	failedThenFree := func(t *testing.T, m *meteredLender) {
		t.Helper()
		m.until(t, func(st Stats) bool { return st.Slots["openai"].Failed == 1 })
		if rd := m.reading(t); rd.Calls != 0 || rd.Estimated != 0 || rd.Used[TypeInput] != 0 || rd.Used[TypeOutput] != 0 {
			t.Errorf("reading = %+v, want nothing charged", rd)
		}
	}

	t.Run("nothing listens", func(t *testing.T) {
		gone := httptest.NewServer(http.NotFoundHandler())
		gone.Close()
		m := &meteredLender{}
		m.at(t, gone.URL)
		if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusBadGateway {
			t.Errorf("the client saw %d, want 502", code)
		}
		failedThenFree(t, m)
	})

	// The upload stops partway, so the provider holds a request it cannot act on.
	t.Run("the client left partway through its request", func(t *testing.T) {
		m := &meteredLender{}
		arrived := make(chan struct{})
		m.over(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(arrived)
			_, _ = io.Copy(io.Discard, r.Body)
		}))
		conn, err := net.Dial("tcp", m.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 4500\r\n\r\nhello", m.lent.Token)
		<-arrived
		_ = conn.Close()
		failedThenFree(t, m)
	})
}

// TestAnAnswerTheProxyRejectsIsChargedOnce: an upstream that switches protocols
// unasked is estimated when its 101 arrives, and the proxy then fails the call.
func TestAnAnswerTheProxyRejectsIsChargedOnce(t *testing.T) {
	m := &meteredLender{}
	m.over(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: nonsense\r\n\r\n")
	}))
	if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusBadGateway {
		t.Errorf("the client saw %d, want 502", code)
	}
	if rd := m.reading(t); rd.Calls != 1 || rd.Estimated != 1 {
		t.Errorf("reading = %+v, want the call charged exactly once", rd)
	}
}

// TestAStreamThatFailsMidAnswerSpendsTheBudget: output then a failure with no
// usage is estimated, so a hard budget holds against a run of them.
func TestAStreamThatFailsMidAnswerSpendsTheBudget(t *testing.T) {
	m := newMeteredLender(t)
	m.stream = []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"usage\":null}}\n\n")
	m.budget(t, Budget{Limits: map[string]int64{TypeOutput: 1}, Hard: true})
	if code, body := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusOK || body != string(m.stream) {
		t.Fatalf("the client got %d and %q, want the provider's answer", code, body)
	}
	if rd := m.reading(t); rd.Estimated != 1 || rd.Used[TypeOutput] != estimateOutput {
		t.Errorf("reading = %+v, want one call estimated at %d output", rd, estimateOutput)
	}
	if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != SpentStatus {
		t.Errorf("the next call answered %d, want the budget's %d", code, SpentStatus)
	}
}

// TestARateLimitInAStreamSpendsNothing: codex retries this form, 22 requests for
// one call, so a charge here would spend a budget on calls that produced nothing.
// That holds for a client that stops reading at the failure, as well as for one
// that reads to the end.
func TestARateLimitInAStreamSpendsNothing(t *testing.T) {
	throttle, err := os.ReadFile("../../test/faults/openai-rate-limit.sse")
	if err != nil {
		t.Fatal(err)
	}
	limits := Budget{Limits: map[string]int64{TypeInput: 1, TypeOutput: 1}, Hard: true}
	free := func(t *testing.T, m *meteredLender, calls int64) {
		t.Helper()
		rd := m.reading(t)
		if rd.Calls != calls || rd.Estimated != 0 || rd.Refused != 0 || rd.Used[TypeInput] != 0 || rd.Used[TypeOutput] != 0 {
			t.Errorf("reading = %+v, want %d calls charged nothing", rd, calls)
		}
	}

	t.Run("read to the end", func(t *testing.T) {
		m := newMeteredLender(t)
		m.stream = throttle
		m.budget(t, limits)
		for range 22 {
			if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusOK {
				t.Fatalf("a throttled call answered %d, want the provider's 200", code)
			}
		}
		free(t, m, 22)
	})

	t.Run("the client left at the failure", func(t *testing.T) {
		m := &meteredLender{}
		m.over(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write(throttle)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		m.budget(t, limits)
		req, _ := http.NewRequest(http.MethodPost, m.srv.URL+"/v1/responses", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+m.lent.Token)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(resp.Body, make([]byte, len(throttle))); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		m.until(t, func(st Stats) bool { return st.Metered == 1 })
		free(t, m, 1)
	})
}

// TestTheMeterLoanReadsAndLendsNothing: the lender's own namespace is never
// forwarded, and a meter loan never reaches a provider.
func TestTheMeterLoanReadsAndLendsNothing(t *testing.T) {
	m := newMeteredLender(t)
	for _, c := range []struct {
		name         string
		method, path string
		loan         Loan
	}{
		{"a meter loan on a model path", http.MethodPost, "/v1/responses", m.meter},
		{"a meter loan elsewhere in the namespace", http.MethodGet, "/cs-sandbox/loans", m.meter},
		{"a lent loan reading the count", http.MethodGet, MeterPath, m.lent},
	} {
		if code, _ := m.do(t, c.method, c.path, c.loan); code != http.StatusForbidden {
			t.Errorf("%s answered %d, want 403", c.name, code)
		}
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("%d calls reached the provider", n)
	}
}

// TestTheRefusalTripsNoOtherClass: the turn drivers class a failure by its
// words before they reach their catch-all, so the message must hold the marker
// and none of those words, for every type. A sandbox's name is the operator's,
// and one that holds such a word is left out.
func TestTheRefusalTripsNoOtherClass(t *testing.T) {
	for _, name := range []string{"web", "context-window", "rate-limit-probe"} {
		for _, typ := range Types {
			msg := strings.ToLower(spentMessage(name, Spent{Type: typ, Used: 2_000_000, Limit: 2_000_000}))
			if !strings.HasPrefix(msg, SpentMarker+":") || !strings.Contains(msg, TypeWords(typ)) {
				t.Errorf("%q: %s", name, msg)
			}
			for _, w := range classWords {
				if strings.Contains(msg, w) {
					t.Errorf("%q: the message holds %q, which a driver reads as another class: %s", name, w, msg)
				}
			}
		}
	}
}

// TestHealthzSaysWhatTheLenderEnforces: a lender that cannot write its group's
// usage directory does not claim to meter, so a create that sets a budget can
// refuse it rather than set a budget nothing applies.
func TestHealthzSaysWhatTheLenderEnforces(t *testing.T) {
	root := t.TempDir()
	if f := NewMeter(root, "g", nil).Features(); len(f) != 0 {
		t.Errorf("with no usage directory the features are %v", f)
	}
	if err := os.MkdirAll(UsageDir(root, "g"), 0o700); err != nil {
		t.Fatal(err)
	}
	if f := NewMeter(root, "g", nil).Features(); !slices.Equal(f, []string{"meter", "refuse"}) {
		t.Errorf("with a writable usage directory the features are %v", f)
	}
	s := httptest.NewServer(New(Config{Callers: CallersAny, Meter: NewMeter(root, "g", nil)}))
	defer s.Close()
	resp, err := http.Get(s.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h struct {
		OK       bool     `json:"ok"`
		Features []string `json:"features"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if !h.OK || !slices.Contains(h.Features, "meter") {
		t.Errorf("/healthz = %+v", h)
	}
}

// TestTheBudgetFileIsValidatedAndOwnerOnly: written with a rename at 0600, like
// faults.json, and refused when it names a type that does not exist.
func TestTheBudgetFileIsValidatedAndOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := WriteBudget(dir, Budget{Limits: map[string]int64{"tokens": 5}}); err == nil {
		t.Error("a budget on an unknown type was written")
	}
	if err := WriteBudget(dir, Budget{Limits: map[string]int64{TypeInput: -1}}); err == nil {
		t.Error("a negative limit was written")
	}
	b := Budget{Limits: map[string]int64{TypeCached: 1e9}, Hard: true, AskAt: 0.9}
	if err := WriteBudget(dir, b); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, BudgetFile))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("budget.json is %v, want 0600", fi.Mode().Perm())
	}
	got, err := ReadBudget(dir)
	if err != nil || got.Limits[TypeCached] != 1e9 || !got.Hard {
		t.Errorf("read back %+v, %v", got, err)
	}
	if line, _ := got.AskLine(TypeCached); line != 9e8 {
		t.Errorf("a hard budget asking at 90%% of 1G asks at %d", line)
	}
	if line, _ := (Budget{Limits: map[string]int64{TypeCached: 1e9}}).AskLine(TypeCached); line != 1e9 {
		t.Errorf("a soft budget asks at %d, want its limit", line)
	}
}

// TestAWebsocketUpgradeIsRefused: OpenCode tries OpenAI's Responses over a
// websocket before it falls back to HTTP, and every model call of a session
// would then pass inside one upgrade the meter cannot read. So a lent path is
// lent as HTTP only, and the upgrade never reaches the provider or reads a
// credential.
func TestAWebsocketUpgradeIsRefused(t *testing.T) {
	m := newMeteredLender(t)
	req, _ := http.NewRequest(http.MethodGet, m.srv.URL+"/v1/responses", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+m.lent.Token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "websocket_refused") {
		t.Errorf("an upgrade answered %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(strings.ToLower(string(body)), SpentMarker) {
		t.Error("the refusal of an upgrade reads as a spent budget")
	}
	if n := m.calls.Load(); n != 0 {
		t.Errorf("%d calls reached the provider", n)
	}
	// The same path as an ordinary request is lent, and metered.
	if code, _ := m.do(t, http.MethodPost, "/v1/responses", m.lent); code != http.StatusOK {
		t.Errorf("the POST that follows answered %d", code)
	}
}
