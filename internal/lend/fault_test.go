package lend

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// faultedServer is a lender for one sandbox, g/box, borrowing openai in front of
// up, with its faults read from a real instances directory.
func faultedServer(t *testing.T, up string) (inst string, call func() (*http.Response, string, error)) {
	t.Helper()
	withSlots(t, Slot{ID: "openai", Kind: Key, Origin: up, Header: "authorization", Prefix: "Bearer ",
		AuthEnvs: []string{"OPENAI_API_KEY"}, BaseEnv: "OPENAI_BASE_URL", read: keyReader("openai"), where: keyPath("openai")})
	home := hostProfile(t)
	root := t.TempDir()
	inst = filepath.Join(root, "g", "box")
	if err := os.MkdirAll(inst, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := TokenPrefix + "box_openai_deadbeef"
	s := New(Config{Home: home, KeysDir: KeysDir(home), Callers: CallersAny, Faults: NewFileFaults(root),
		Loans: fixedLoans{tok: {Token: tok, Slot: "openai", Kind: Key, Group: "g", Name: "box"}},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return inst, func() (*http.Response, string, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body), nil
	}
}

// TestAnArmedFaultAnswersInTheProvidersPlace: a fault exists so that a throttle can be tried
// without a provider, so the provider must not be called while one is armed, and traffic has
// to flow again by itself once the count is spent. The answer is marked, so nothing that reads
// it takes it for the provider's.
func TestAnArmedFaultAnswersInTheProvidersPlace(t *testing.T) {
	up := newUpstream(t)
	inst, call := faultedServer(t, up.URL)
	if err := WriteFaults(inst, []Fault{{ID: "f1", Status: 429, RetryAfter: 7, Count: 2}}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		resp, body, err := call()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "7" || resp.Header.Get(FaultHeader) != "f1" {
			t.Fatalf("call %d: got %d, Retry-After %q, fault %q", i, resp.StatusCode,
				resp.Header.Get("Retry-After"), resp.Header.Get(FaultHeader))
		}
		if !strings.Contains(body, "injected") {
			t.Errorf("the body does not say the failure was injected: %s", body)
		}
	}
	if up.calls != 0 {
		t.Fatalf("the provider was called %d time(s) while a fault was armed", up.calls)
	}
	resp, _, err := call()
	if err != nil || resp.StatusCode != http.StatusOK || up.calls != 1 {
		t.Fatalf("a spent fault did not let traffic through: status %v, err %v, upstream calls %d", resp, err, up.calls)
	}
}

// TestAFaultCanCarryTheProvidersOwnBody: a provider reports a rate limit INSIDE a 200 stream,
// and that is the form codex retries, so a status alone cannot reproduce it. The operator
// supplies the event and the lender serves it as it is, knowing no provider's shapes.
func TestAFaultCanCarryTheProvidersOwnBody(t *testing.T) {
	up := newUpstream(t)
	inst, call := faultedServer(t, up.URL)
	event := "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n"
	if err := WriteFaults(inst, []Fault{{ID: "f2", Status: 200, Count: 1, ContentType: "text/event-stream", Body: event}}); err != nil {
		t.Fatal(err)
	}
	resp, body, err := call()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || body != event {
		t.Fatalf("got %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

// TestAFaultCoversOnlyTheSlotAndSandboxItNames: one member of a fleet is throttled while the
// rest run, which is the case a harness has to get right. A fault on another slot, or in
// another sandbox's directory, must not touch this one's calls.
func TestAFaultCoversOnlyTheSlotAndSandboxItNames(t *testing.T) {
	up := newUpstream(t)
	inst, call := faultedServer(t, up.URL)
	if err := WriteFaults(inst, []Fault{{ID: "other-slot", Slot: "anthropic", Status: 503, Count: 5}}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(inst), "neighbour")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFaults(other, []Fault{{ID: "other-box", Status: 503, Count: 5}}); err != nil {
		t.Fatal(err)
	}
	if resp, _, err := call(); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("a fault armed elsewhere reached this sandbox: %v %v", resp, err)
	}
}

// TestAHangWithNoStatusDropsTheCall: a provider that accepts a call and never answers is what
// a stall watchdog exists for, and it looks like a connection that closes with nothing sent.
func TestAHangWithNoStatusDropsTheCall(t *testing.T) {
	up := newUpstream(t)
	inst, call := faultedServer(t, up.URL)
	if err := WriteFaults(inst, []Fault{{ID: "f3", Hang: "50ms", Count: 1}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if resp, _, err := call(); err == nil {
		t.Fatalf("a dropped call was answered with %d", resp.StatusCode)
	}
	if took := time.Since(start); took < 50*time.Millisecond {
		t.Errorf("the call was dropped after %s; it was to be held 50ms", took)
	}
	if up.calls != 0 {
		t.Errorf("the provider was called")
	}
}

func TestAFaultIsValidatedBeforeItIsArmed(t *testing.T) {
	for name, f := range map[string]Fault{
		"no answer and no hang": {Count: 1},
		"no calls":              {Status: 429},
		"not a status":          {Status: 42, Count: 1},
		"not a duration":        {Hang: "soon", Count: 1},
		"unknown slot":          {Status: 429, Count: 1, Slot: "nope"},
	} {
		if f.Validate() == nil {
			t.Errorf("%s: accepted %+v", name, f)
		}
	}
}

// The file sits beside the loans and names the sandbox's slots, so it is owner-only as they are.
func TestTheFaultsFileIsOwnerOnlyAndGoesWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFaults(dir, []Fault{{ID: "f", Status: 429, Count: 1}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, FaultsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("faults.json: %v, mode %v; want 0600", err, info.Mode().Perm())
	}
	if err := WriteFaults(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FaultsFile)); !os.IsNotExist(err) {
		t.Errorf("clearing left the file behind: %v", err)
	}
}
