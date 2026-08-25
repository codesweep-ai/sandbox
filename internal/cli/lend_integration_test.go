//go:build integration || smoke

package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Credential lending, end to end through a real sandbox.
//
// The claim under test is one sentence: what the sandbox holds is worthless,
// and what reaches the provider is the host's real credential. Only a live
// sandbox can prove it, because the loan travels through the seed, the guest's
// environment, the rootless network and the lender's peer check — and any one of
// those can be wrong while every unit test still passes.
//
// A stand-in provider stands where api.anthropic.com would, so the assertion is
// on the headers a provider received without a real API being called.

// seenRequest is what the stand-in provider was sent.
type seenRequest struct {
	mu     sync.Mutex
	header http.Header
	path   string
	calls  int
}

func (s *seenRequest) snapshot() (http.Header, string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header.Clone(), s.path, s.calls
}

// standInProvider answers on loopback, where only the lender can reach it.
func standInProvider(t *testing.T) (url string, seen *seenRequest) {
	t.Helper()
	seen = &seenRequest{header: http.Header{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		seen.header, seen.path, seen.calls = r.Header.Clone(), r.URL.Path, seen.calls+1
		seen.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"stand_in":true}`)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + l.Addr().String(), seen
}

// lendingHost writes a host home holding a login and a key to lend, and points
// the agent-login lookup at it. Nothing real is read, and nothing is written to
// the developer's own profiles.
func lendingHost(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	put := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(".cs-claude/.credentials.json", fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"REAL-HOST-TOKEN","expiresAt":%d}}`,
		time.Now().Add(time.Hour).UnixMilli()))
	put(".cs-keys/anthropic", "REAL-HOST-KEY\n")
	t.Setenv("CS_SANDBOX_AGENT_HOME", home)
	return home
}

// startLender runs a lender in this process, on an address a sandbox can reach,
// with its upstreams pointed at the stand-in. create finds it by probing the
// address rather than starting one, which is what a host running a lender under
// a service manager gets too.
func startLender(t *testing.T, home, upstream string) string {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: lend.New(lend.Config{
		Home:      home,
		KeysDir:   lend.KeysDir(home),
		Loans:     lend.NewFileLoans(paths.Instances()),
		LocalOnly: true,
		Origins:   map[string]string{"anthropic": upstream, "claude": upstream},
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	addr := "0.0.0.0:" + port
	t.Setenv("CS_SANDBOX_LEND_ADDR", addr)
	return addr
}

// TestCLILendKeyLive: a sandbox spends a key it does not have.
func TestCLILendKeyLive(t *testing.T) {
	r, host := liveSetup(t)
	home := lendingHost(t)
	upstream, seen := standInProvider(t)
	startLender(t, home, upstream)

	name := boxName("lendkey")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	out := createBox(t, r, name, "--lend-api-key", "anthropic")
	if !strings.Contains(out, "lent: anthropic") {
		t.Errorf("create should report the loan:\n%s", out)
	}

	ctx := context.Background()
	// What the sandbox holds: a token, and a base URL pointing at the host.
	env := inBox(ctx, r, host, name, `printf '%s|%s' "$ANTHROPIC_API_KEY" "$ANTHROPIC_BASE_URL"`)
	token, base, _ := strings.Cut(env, "|")
	if !strings.HasPrefix(token, lend.TokenPrefix) {
		t.Fatalf("the sandbox holds %q, want a loan token", token)
	}
	if strings.Contains(token, "REAL-HOST-KEY") {
		t.Fatal("the real key reached the sandbox")
	}
	if !strings.Contains(base, engine.HostReachableName) {
		t.Errorf("base URL = %q, want the host as seen from inside", base)
	}

	// And what a provider receives when it spends it.
	body := inBox(ctx, r, host, name,
		`curl -s --max-time 10 -X POST "$ANTHROPIC_BASE_URL/v1/messages" -H "x-api-key: $ANTHROPIC_API_KEY" -d '{}'`)
	if !strings.Contains(body, "stand_in") {
		t.Fatalf("the call did not reach the provider through the lender: %q\n%s",
			body, hostReachability(ctx, r, host, name))
	}
	got, path, calls := seen.snapshot()
	if calls == 0 {
		t.Fatal("the provider was never called")
	}
	if got.Get("X-Api-Key") != "REAL-HOST-KEY" {
		t.Errorf("provider saw x-api-key %q, want the host's real key", got.Get("X-Api-Key"))
	}
	if strings.Contains(got.Get("X-Api-Key"), lend.TokenPrefix) {
		t.Error("the loan token reached the provider")
	}
	if path != "/v1/messages" {
		t.Errorf("provider path = %q, want the client's own path", path)
	}
}

// hostReachability reports how the sandbox resolves the host, for a failure
// whose cause is which address this engine published rather than anything about
// the loan.
//
// The name is podman's to publish, and what it publishes is not the same
// everywhere: pasta's host mapping where podman runs natively, slirp4netns' own
// address on an older one, and a route to the Mac rather than to the VM under a
// podman machine. A test that fails on a host nobody can log into has to say
// which of those it got.
func hostReachability(ctx context.Context, r *run.Exec, host hostenv.Host, name string) string {
	return "the sandbox resolves the host as:\n" +
		inBox(ctx, r, host, name,
			`getent ahosts `+engine.HostReachableName+`; grep -i internal /etc/hosts; `+
				`curl -s -o /dev/null --max-time 10 "$ANTHROPIC_BASE_URL/healthz"; `+
				`echo "curl exit $?"`)
}

// TestCLILendLoginLive: a sandbox spends the host's agent login, in the header
// shape that login travels in — which the sandbox cannot know, because it holds
// a token rather than a credential.
func TestCLILendLoginLive(t *testing.T) {
	r, host := liveSetup(t)
	home := lendingHost(t)
	upstream, seen := standInProvider(t)
	startLender(t, home, upstream)

	name := boxName("lendlogin")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	createBox(t, r, name, "--lend-agent-login", "claude")

	ctx := context.Background()
	// A login is seeded as the agent's own credential file, so the client stays
	// on the code path it takes when it is signed in.
	cred := inBox(ctx, r, host, name, `cat ~/.cs-claude/.credentials.json 2>/dev/null`)
	// The fabricated token takes the form the provider issues, so the client
	// cannot tell it from a real one, and says "loan" so a person can.
	if !strings.Contains(cred, "sk-ant-oat01-loan-") {
		t.Fatalf("the sandbox's credentials file does not hold a loan: %q", cred)
	}
	if strings.Contains(cred, "REAL-HOST-TOKEN") {
		t.Fatal("the host's real login reached the sandbox")
	}
	// What the client sends is what the file told it to send.
	token := strings.TrimSpace(inBox(ctx, r, host, name,
		`python3 -c 'import json;print(json.load(open("/home/"+__import__("os").environ["USER"]+"/.cs-claude/.credentials.json"))["claudeAiOauth"]["accessToken"])' 2>/dev/null`))
	if token == "" {
		t.Fatal("could not read the seeded access token")
	}
	body := inBox(ctx, r, host, name,
		`curl -s --max-time 10 -X POST "$ANTHROPIC_BASE_URL/v1/messages" -H "authorization: Bearer `+token+`" -d '{}'`)
	if !strings.Contains(body, "stand_in") {
		t.Fatalf("the call did not reach the provider through the lender: %q", body)
	}
	got, _, _ := seen.snapshot()
	if got.Get("Authorization") != "Bearer REAL-HOST-TOKEN" {
		t.Errorf("provider saw authorization %q, want the host's real login", got.Get("Authorization"))
	}
	// The shape the login travels in, restored on the host's side.
	if !strings.Contains(got.Get("Anthropic-Beta"), "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, want the OAuth beta the login needs", got.Get("Anthropic-Beta"))
	}

	// The sandbox never held the credential, so there is nothing in it to find.
	if g := inBox(ctx, r, host, name, `grep -rc REAL-HOST ~/.cs-claude/ 2>/dev/null | grep -v ':0' | wc -l`); strings.TrimSpace(g) != "0" {
		t.Errorf("the host's credential is in the sandbox's claude profile")
	}
	if g := inBox(ctx, r, host, name, `env | grep -c REAL-HOST || true`); strings.TrimSpace(g) != "0" {
		t.Errorf("the host's credential is in the sandbox's environment (%s matches)", strings.TrimSpace(g))
	}
}

// TestCLILendSideCallsBlockedLive: the half of an agent's traffic a base URL
// does not govern. A sandbox must not reach a host the lender fronts, and must
// still reach everything else.
func TestCLILendSideCallsBlockedLive(t *testing.T) {
	r, host := liveSetup(t)
	home := lendingHost(t)
	upstream, _ := standInProvider(t)
	startLender(t, home, upstream)

	name := boxName("lendside")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	createBox(t, r, name, "--lend-api-key", "anthropic")

	ctx := context.Background()
	// A tunnel to a fronted host is refused, and curl reports the refusal
	// rather than hanging or succeeding.
	blocked := inBox(ctx, r, host, name,
		`curl -sS --max-time 10 https://api.anthropic.com/v1/messages 2>&1 | head -1`)
	if !strings.Contains(blocked, "403") {
		t.Errorf("a direct call to a fronted host = %q, want a refused tunnel", blocked)
	}
	// An agent's tools share its environment, so everything else still works.
	other := inBox(ctx, r, host, name,
		`curl -sS --max-time 20 -o /dev/null -w '%{http_code}' https://github.com/ 2>&1 | tail -1`)
	if strings.TrimSpace(other) != "200" {
		t.Errorf("an ordinary https call through the tunnel = %q, want 200", strings.TrimSpace(other))
	}
}

// TestCLILendRevokedByDestroyLive: destroying the sandbox is the revocation.
// There is no other, which is the property that keeps a loan from outliving
// what it was lent to.
func TestCLILendRevokedByDestroyLive(t *testing.T) {
	r, host := liveSetup(t)
	home := lendingHost(t)
	upstream, _ := standInProvider(t)
	addr := startLender(t, home, upstream)

	name := boxName("lendrevoke")
	createBox(t, r, name, "--lend-api-key", "anthropic")
	ctx := context.Background()
	token := strings.TrimSpace(inBox(ctx, r, host, name, `printf '%s' "$ANTHROPIC_API_KEY"`))
	if !strings.HasPrefix(token, lend.TokenPrefix) {
		t.Fatalf("no loan token in the sandbox: %q", token)
	}

	loans := filepath.Join(state.Dir(paths.Instances(), state.DefaultGroup, name), "loans.json")
	if !fileExists(loans) {
		t.Fatalf("no loan record at %s", loans)
	}
	if out, err := execRoot(t, "destroy", name, "-f"); err != nil {
		t.Fatalf("destroy: %v (%s)", err, out)
	}
	if fileExists(loans) {
		t.Error("the loan record outlived the sandbox")
	}

	// The token is now worth nothing, from the host itself.
	probe := lend.ProbeAddr(addr)
	req, err := http.NewRequest(http.MethodPost, "http://"+probe+"/v1/messages", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", token)
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a destroyed sandbox's token = HTTP %d, want 401", res.StatusCode)
	}
}

// TestCLILendFirecrackerLive: the same claim under the other engine. One image,
// one fabric, the same flags — a feature that works under Podman and not under
// Firecracker is unfinished.
func TestCLILendFirecrackerLive(t *testing.T) {
	_, host := liveSetup(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	fcInstancesDir(t, host)
	home := lendingHost(t)
	upstream, seen := standInProvider(t)
	startLender(t, home, upstream)

	name := boxName("lendfc")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	step(t, "booting firecracker microVM %s (takes ~30s)…", name)
	if out, err := execRoot(t, "create", name, "--engine", "firecracker", "--lend-api-key", "anthropic"); err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	body := sshCapture(t, host, name,
		`curl -s --max-time 10 -X POST "$ANTHROPIC_BASE_URL/v1/messages" -H "x-api-key: $ANTHROPIC_API_KEY" -d '{}'`)
	if !strings.Contains(body, "stand_in") {
		t.Fatalf("a microVM did not reach the provider through the lender: %q", body)
	}
	got, _, _ := seen.snapshot()
	if got.Get("X-Api-Key") != "REAL-HOST-KEY" {
		t.Errorf("provider saw x-api-key %q, want the host's real key", got.Get("X-Api-Key"))
	}
}

// standInVCR is a cs-vcr in shape rather than in substance: it strips the
// /c/<name> prefix its base URL carries and forwards the rest, headers
// untouched, to the lender.
//
// The real recorder lives in another repository, and what is under test here is
// this repository's half of the composition: that a sandbox pointed at a
// cassette still reaches the lender, still carries its loan token there, and
// needs no different environment than one pointed straight at it.
func standInVCR(t *testing.T, lenderAddr string) string {
	t.Helper()
	target, err := url.Parse("http://" + lend.ProbeAddr(lenderAddr))
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		p := pr.In.URL.Path
		if rest := strings.SplitN(strings.TrimPrefix(p, "/c/"), "/", 2); strings.HasPrefix(p, "/c/") && len(rest) == 2 {
			pr.Out.URL.Path = "/" + rest[1]
		}
		pr.SetURL(target)
		pr.Out.Host = target.Host
	}}
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: rp}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return engine.HostReachableName + ":" + port
}

// TestCLILendThroughCassetteLive: a cassette in front of the lender changes one
// thing about the sandbox — the base URL — and nothing about the credential.
func TestCLILendThroughCassetteLive(t *testing.T) {
	r, host := liveSetup(t)
	home := lendingHost(t)
	upstream, seen := standInProvider(t)
	addr := startLender(t, home, upstream)
	vcr := standInVCR(t, addr)

	name := boxName("lendcass")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
	out := createBox(t, r, name, "--lend-api-key", "anthropic", "--cassette", "demo", "--vcr", vcr)
	if !strings.Contains(out, "providers.anthropic.base_url") {
		t.Errorf("create should print the cs-vcr stanza to paste:\n%s", out)
	}

	ctx := context.Background()
	base := strings.TrimSpace(inBox(ctx, r, host, name, `printf '%s' "$ANTHROPIC_BASE_URL"`))
	if !strings.HasSuffix(base, "/c/demo") {
		t.Errorf("base URL = %q, want it to name the cassette", base)
	}
	body := inBox(ctx, r, host, name,
		`curl -s --max-time 10 -X POST "$ANTHROPIC_BASE_URL/v1/messages" -H "x-api-key: $ANTHROPIC_API_KEY" -d '{}'`)
	if !strings.Contains(body, "stand_in") {
		t.Fatalf("the call did not reach the provider through the recorder and the lender: %q", body)
	}
	got, path, _ := seen.snapshot()
	if got.Get("X-Api-Key") != "REAL-HOST-KEY" {
		t.Errorf("provider saw x-api-key %q, want the host's real key", got.Get("X-Api-Key"))
	}
	// The cassette's addressing is stripped before the provider sees the path.
	if path != "/v1/messages" {
		t.Errorf("provider path = %q, want the client's own path", path)
	}
}
