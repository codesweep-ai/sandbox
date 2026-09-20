package lend

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// answering is an upstream whose next answer the test chooses.
type answering struct {
	*httptest.Server
	mu     sync.Mutex
	status int
}

func (a *answering) set(status int) { a.mu.Lock(); a.status = status; a.mu.Unlock() }

func newAnswering(t *testing.T) *answering {
	t.Helper()
	a := &answering{status: http.StatusOK}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		status := a.status
		a.mu.Unlock()
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "12")
			w.Header().Set("X-Ratelimit-Remaining-Tokens", "0")
		}
		w.Header().Set("Set-Cookie", "session=not-for-a-log")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"a body that must not be logged"}}`)
	}))
	t.Cleanup(a.Close)
	return a
}

// lentServer is a lender with one openai loan in front of up, its log captured.
func lentServer(t *testing.T, up string, now func() time.Time) (*Server, func() int, *bytes.Buffer) {
	t.Helper()
	withSlots(t, Slot{ID: "openai", Kind: Key, Origin: up, Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL", read: keyReader("openai"), where: keyPath("openai")})
	home := hostProfile(t)
	tok := TokenPrefix + "box_openai_deadbeef"
	var log lockedBuffer
	s := New(Config{Home: home, KeysDir: KeysDir(home), Callers: CallersAny, Now: now,
		Loans: fixedLoans{tok: {Token: tok, Slot: "openai", Kind: Key, Name: "box"}},
		Log:   slog.New(slog.NewTextHandler(&log, nil))})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	call := func() int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	return s, call, &log.Buffer
}

// lockedBuffer is a log sink the handler goroutines and the test can share.
type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

// TestTheLenderRecordsWhatTheUpstreamAnswered: a lender that logs the request and not the
// answer writes the same line for a call that worked and one the provider throttled, so a
// fleet at its limit looks healthy from the host. The answer is recorded from its status and
// the provider's limit headers. The body is a caller's conversation and never reaches a log,
// and neither does a header that is not about limits.
func TestTheLenderRecordsWhatTheUpstreamAnswered(t *testing.T) {
	up := newAnswering(t)
	s, call, log := lentServer(t, up.URL, nil)

	call()
	up.set(http.StatusTooManyRequests)
	if got := call(); got != http.StatusTooManyRequests {
		t.Fatalf("the caller got %d; the upstream's 429 has to pass through unchanged", got)
	}
	up.set(http.StatusServiceUnavailable)
	call()
	up.set(http.StatusUnauthorized)
	call()
	up.Close() // nothing answers at all
	call()

	want := Outcomes{OK: 1, Throttled: 1, Errors: 1, Refused: 1, Failed: 1}
	if got := s.Snapshot().Slots["openai"]; got != want {
		t.Errorf("outcomes = %+v; want %+v", got, want)
	}
	if sum := s.Snapshot().Summary(); !strings.Contains(sum, "openai: ok 1 · throttled 1 · refused 1 · 5xx 1 · no answer 1") {
		t.Errorf("the summary does not say what the slot's upstream answered:\n%s", sum)
	}
	text := log.String()
	for _, want := range []string{
		"msg=answered sandbox=box slot=openai status=200",
		"msg=answered sandbox=box slot=openai status=429",
		"retry-after=12", "x-ratelimit-remaining-tokens=0",
		`msg="the upstream is throttling this slot" slot=openai`,
		`msg="the upstream is failing this slot" slot=openai`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log is missing %q:\n%s", want, text)
		}
	}
	for _, never := range []string{"must not be logged", "not-for-a-log", "the-hosts-real-openai-key"} {
		if strings.Contains(text, never) {
			t.Errorf("the log carries %q:\n%s", never, text)
		}
	}
}

// TestAThrottledSlotWarnsOnceAnInterval: every answer has its own line, so the warning is the
// one a person is meant to notice. A fleet at its limit is throttled many times a minute, and
// a warning per answer is a log nobody reads. The ones in between are counted into the next.
func TestAThrottledSlotWarnsOnceAnInterval(t *testing.T) {
	up := newAnswering(t)
	up.set(http.StatusTooManyRequests)
	var mu sync.Mutex
	clock := time.Unix(1_800_000_000, 0)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	_, call, log := lentServer(t, up.URL, now)

	for range 5 {
		call()
	}
	if n := strings.Count(log.String(), "is throttling this slot"); n != 1 {
		t.Fatalf("5 throttled answers inside one interval warned %d times; want 1:\n%s", n, log.String())
	}
	mu.Lock()
	clock = clock.Add(troubleEvery + time.Second)
	mu.Unlock()
	call()
	text := log.String()
	if n := strings.Count(text, "is throttling this slot"); n != 2 {
		t.Fatalf("the next interval warned %d times in all; want 2:\n%s", n, text)
	}
	// The four that were not said, and the one that was.
	if !strings.Contains(text, "answers=5") {
		t.Errorf("the second warning does not count the answers since the first:\n%s", text)
	}
}
