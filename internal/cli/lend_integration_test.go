//go:build integration || smoke

package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
	// 0.0.0.0, not loopback. The lender dials this from its container on the
	// group's network, and a guest arrives on the host's ordinary side — where
	// a server bound to 127.0.0.1 refuses the connection (SPEC R52).
	l, err := net.Listen("tcp", "0.0.0.0:0")
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
	// Named as loopback deliberately: it is what a caller types, and create
	// moves it onto the name a container reaches this host by. The tests below
	// therefore drive the documented spelling AND that translation.
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return "http://127.0.0.1:" + port, seen
}

// lendingHost writes a host home holding a login and a key to lend, and points
// the agent-login lookup at it. Nothing real is read, and nothing is written to
// the developer's own profiles.
func lendingHost(t *testing.T, host hostenv.Host) {
	t.Helper()
	// Not t.TempDir(): the lender container mounts this home read-only, and on
	// macOS a container can only mount what is under $HOME — the podman-machine
	// share cs-sandbox commits to. shareDir already knows that rule.
	home := shareDir(t, host)
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
}

// lendUpstream is how a live test puts a provider of its own behind the lender.
//
// Through the LOAN rather than through the lender, which is both the documented
// path (R147: the origin may be named by the loan, the lender's --origin, or the
// slot) and the only one available now: the lender is a container on the group's
// network, started by create, so there is nothing for a test to configure by
// hand before it exists.
func lendUpstream(upstream string) []string {
	return []string{"--env", "ANTHROPIC_BASE_URL=" + upstream}
}

// lenderIsUp reports whether this group's lender container is still running.
func lenderIsUp(t *testing.T, r *run.Exec) bool {
	t.Helper()
	out := run.Output(context.Background(), r, "podman", "container", "inspect",
		lend.BoxName(state.NetworkName(state.DefaultGroup)), "--format", "{{.State.Running}}")
	return strings.TrimSpace(out) == "true"
}

// askTheLender puts a request to the group's lender from inside its own
// container, which is the only place it can be reached from: nothing is bound
// on the host, and no other network can route to it. Returns the status code.
func askTheLender(t *testing.T, r *run.Exec, token string) string {
	t.Helper()
	box := lend.BoxName(state.NetworkName(state.DefaultGroup))
	res, err := r.Run(context.Background(), run.Opts{ReadOnly: true}, "podman", "exec", box,
		"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-m", "10",
		"-X", "POST", "-H", "x-api-key: "+token,
		"http://"+lend.ProbeAddr(lend.DefaultBind)+"/v1/messages")
	if err != nil {
		t.Fatalf("asking the lender: %v (%s)", err, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout)
}

// TestCLILendKeyLive: a sandbox spends a key it does not have.
func TestCLILendKeyLive(t *testing.T) {
	r, host := liveSetup(t)
	lendingHost(t, host)
	upstream, seen := standInProvider(t)

	name := boxName("lendkey")
	t.Cleanup(func() { _, _ = destroyBox(t, name) })
	out := createBox(t, r, name, append([]string{"--lend-api-key", "anthropic"}, lendUpstream(upstream)...)...)
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
	if !strings.Contains(base, lend.GuestName) {
		t.Errorf("base URL = %q, want the lender's name on the group's network", base)
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

// TestCLILendKeyFirecrackerLive: the same loan, spent from a microVM.
//
// Worth its own member because the two engines reach the host by different
// routes. A container is handed the host's name by podman; a microVM guest gets
// only what the seed pins, and its traffic leaves over a tap rather than out of
// a container. Nothing else in the profile spends a credential from a microVM,
// so without this the engine that boots its own kernel is the one engine lending
// is never proven on.
func TestCLILendKeyFirecrackerLive(t *testing.T) {
	_, host := liveSetup(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("firecracker artifacts not built (run: cs-sandbox build --engine firecracker)")
	}
	// Before anything reads the instances root, because this moves it.
	fcInstancesDir(t, host)
	lendingHost(t, host)
	upstream, seen := standInProvider(t)

	name := boxName("lendkeyfc")
	t.Cleanup(func() { _, _ = destroyBox(t, name) })
	step(t, "booting firecracker microVM %s (takes ~30s)…", name)
	out, err := execRoot(t, append([]string{"create", name, "--engine", "firecracker", "--lend-api-key", "anthropic"},
		lendUpstream(upstream)...)...)
	if err != nil {
		t.Fatalf("create firecracker: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "lent: anthropic") {
		t.Errorf("create should report the loan:\n%s", out)
	}

	token := strings.TrimSpace(sshCapture(t, host, name, `printf %s "$ANTHROPIC_API_KEY"`))
	if !strings.HasPrefix(token, lend.TokenPrefix) {
		t.Fatalf("the microVM holds %q, want a loan token", token)
	}
	if strings.Contains(token, "REAL-HOST-KEY") {
		t.Fatal("the real key reached the microVM")
	}

	body := sshCapture(t, host, name,
		`curl -s --max-time 10 -X POST "$ANTHROPIC_BASE_URL/v1/messages" -H "x-api-key: $ANTHROPIC_API_KEY" -d '{}'`)
	if !strings.Contains(body, "stand_in") {
		t.Fatalf("the call did not reach the provider through the lender: %q\n%s", body,
			vmReachability(t, host, name))
	}
	if got, _, calls := seen.snapshot(); calls == 0 || got.Get("X-Api-Key") != "REAL-HOST-KEY" {
		t.Errorf("provider saw x-api-key %q after %d call(s), want the host's real key",
			got.Get("X-Api-Key"), calls)
	}
}

// vmReachability is hostReachability for a microVM, which has no container to
// exec into and answers over ssh instead.
//
// It carries the one thing its predecessor here left out and the whole question
// turns on: curl's EXIT CODE. `curl -s` prints nothing when it fails, so a name
// that does not resolve, a route that is not there, a port that refuses and a
// port that hangs all reach the assertion above as the same empty string. They
// are four different faults with four different fixes, and 6 (host), 7
// (refused) and 28 (timeout) tell them apart in one number.
//
// Written out after a CI leg where this test was the ONLY minimal reproduction
// of a microVM that could not reach its host -- no agent, no recorder, one curl
// -- and it could not say which of the four it was.
func vmReachability(t *testing.T, host hostenv.Host, name string) string {
	t.Helper()
	return "the microVM's view of the host:\n" + sshCapture(t, host, name,
		`echo "base: $ANTHROPIC_BASE_URL"; `+
			`getent ahosts `+engine.HostReachableName+` || echo "(does not resolve)"; `+
			`grep -i internal /etc/hosts; `+
			`curl -s -o /dev/null -w 'healthz: HTTP %{http_code} in %{time_total}s\n' `+
			`--max-time 10 "$ANTHROPIC_BASE_URL/healthz"; echo "curl exit $?"; `+
			`ip route 2>&1 | head -5`)
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
	lendingHost(t, host)
	upstream, seen := standInProvider(t)

	name := boxName("lendlogin")
	t.Cleanup(func() { _, _ = destroyBox(t, name) })
	createBox(t, r, name, append([]string{"--lend-agent-login", "claude"}, lendUpstream(upstream)...)...)

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
	lendingHost(t, host)
	upstream, _ := standInProvider(t)

	name := boxName("lendside")
	t.Cleanup(func() { _, _ = destroyBox(t, name) })
	createBox(t, r, name, append([]string{"--lend-api-key", "anthropic"}, lendUpstream(upstream)...)...)

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
	lendingHost(t, host)
	upstream, _ := standInProvider(t)

	name := boxName("lendrevoke")
	createBox(t, r, name, append([]string{"--lend-api-key", "anthropic"}, lendUpstream(upstream)...)...)
	ctx := context.Background()
	token := strings.TrimSpace(inBox(ctx, r, host, name, `printf '%s' "$ANTHROPIC_API_KEY"`))
	if !strings.HasPrefix(token, lend.TokenPrefix) {
		t.Fatalf("no loan token in the sandbox: %q", token)
	}

	loans := filepath.Join(state.Dir(paths.Instances(), state.DefaultGroup, name), "loans.json")
	if !fileExists(loans) {
		t.Fatalf("no loan record at %s", loans)
	}
	if out, err := destroyBox(t, name); err != nil {
		t.Fatalf("destroy: %v (%s)", err, out)
	}
	if fileExists(loans) {
		t.Error("the loan record outlived the sandbox")
	}

	// The token is now worth nothing, asked of the lender that minted it.
	//
	// Destroy stops a lender with nothing left to lend, so this may find no
	// container at all — which is the same claim, arrived at sooner: a token
	// whose lender is gone buys nothing either.
	if lenderIsUp(t, r) {
		if code := askTheLender(t, r, token); code != "401" {
			t.Errorf("a destroyed sandbox's token = HTTP %s, want 401", code)
		}
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
	lendingHost(t, host)
	upstream, seen := standInProvider(t)

	name := boxName("lendfc")
	t.Cleanup(func() { _, _ = destroyBox(t, name) })
	step(t, "booting firecracker microVM %s (takes ~30s)…", name)
	if out, err := execRoot(t, append([]string{"create", name, "--engine", "firecracker", "--lend-api-key", "anthropic"},
		lendUpstream(upstream)...)...); err != nil {
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
