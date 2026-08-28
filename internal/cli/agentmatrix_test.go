//go:build live_agents || agents_replay

// The agent matrix: every supported pairing of an agent with a credential, in
// both credential modes, each one driving a real agent inside a real sandbox
// and asking a real model to say one word.
//
// Three tiers share this file, and they differ only in what sits between the
// sandbox and the provider:
//
//	make test-live-agents      nothing.  Real providers, real money.
//	make fixtures              a cs-vcr recording.  Real providers, real money.
//	make test-agents-replay    a cs-vcr replaying.  No provider, no credential.
//
// One driver runs all three, because a recording and its replay have to agree
// on every byte the agent sends, and the only way to guarantee that is for both
// to come out of the same function.
//
// What is proved is an end-to-end turn, not just a credential: the agent starts,
// signs itself in with what the sandbox holds, reaches a model and answers. The
// replay tier proves the same thing for free, which is what makes it the one CI
// can run.
package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// pong is what every case asks the model for. One word, so the assertion is
// about the credential path rather than about the model.
const (
	pongPrompt = "Reply with the single word: pong"
	pongWord   = "pong"
)

// Model ids are pinned rather than defaulted, so a case that fails says
// something about the credential path instead of about a model that moved. A
// default that moves is also a cassette that stops matching for a reason no
// diff explains. Each is the cheapest one its provider offers that these
// clients can address.
const (
	// Claude Code names a model bare; OpenCode names the provider too.
	claudeModel    = "claude-fable-5"
	anthropicModel = "anthropic/" + claudeModel
	openaiModel    = "openai/gpt-5-nano"
	codexAPIModel  = "gpt-5-nano"
	// The image already pins a Fireworks model, and the Fireworks pairing uses
	// it rather than overriding: that is the path a lent Fireworks key travels,
	// via OPENCODE_BASE_URL and the pinned model's own provider.
)

// pairing is one agent spending one credential.
//
// Each becomes two cases, one per credential mode. Derived rather than written
// twice, because the two halves have to run the same command against the same
// model to be worth comparing, and a table that repeats them is a table where
// they can drift apart.
type pairing struct {
	// agent names the pairing, and run is what the sandbox is asked to do.
	agent, run string
	// slot is the lend.Slot this credential is. It decides the flag, the
	// variable the base URL travels in, and which file the host holds.
	slot string
	// provider is the cs-vcr entry this slot's upstream is served under, which
	// is the name the addressing prefix carries.
	//
	// Keyed on the slot rather than on its variable, because a provider with no
	// base-URL variable of its own borrows a client's. One vendor can be two
	// entries: a lent Codex login is a ChatGPT subscription, spent at the
	// backend cs-vcr reaches under `chatgpt`, while an OpenAI key is spent at
	// api.openai.com under `openai`.
	provider string
	// suffix is what this CLIENT appends to the base URL it is given, and it is
	// a property of the client rather than of the slot: Claude Code treats a
	// base URL as the site root and posts /v1/messages, while OpenCode treats
	// it as already versioned and posts /messages.
	//
	// It is added only in the shared mode. A lent sandbox is handed the lender,
	// which puts the version segment on itself (lend.Slot.Version) before
	// joining the upstream's path — so carrying it here as well would send
	// /v1/v1/messages.
	suffix string
	// key is the .env variable this pairing needs, or "" when it needs a login.
	// login is the host agent whose real profile it needs, or "".
	key, login string
}

// pairings is the matrix. Every combination is listed whether or not this host
// can sign in for it: a case that skips says which credential is missing, and
// that is the only way a contributor learns what one more login would cover.
func pairings() []pairing {
	// --model, and not for the cost. Claude Code takes its own default
	// otherwise, measured recording as claude-opus-5: a cassette keyed on
	// whatever that default happens to be stops matching on the day it moves,
	// for a reason no diff explains.
	claude := `cd ~ && cs-claude -p --model ` + claudeModel + ` ` + shellQuote(pongPrompt)
	opencode := func(model string) string {
		m := ""
		if model != "" {
			m = "-m " + model + " "
		}
		return `cd ~ && cs-opencode run ` + m + shellQuote(pongPrompt)
	}
	return []pairing{
		// An agent login, which only its own agent can spend.
		{agent: "claude-login", run: claude, slot: "claude", provider: "anthropic", login: "claude"},
		{
			agent: "codex-login", slot: "codex", provider: "chatgpt", login: "codex",
			run: `cd ~ && cs-codex exec --skip-git-repo-check ` + shellQuote(pongPrompt),
			// No suffix: the subscription transport has no version segment.
		},

		// An Anthropic key, which Claude Code and OpenCode can both spend.
		{agent: "claude-anthropic", run: claude, slot: "anthropic", provider: "anthropic", key: "ANTHROPIC_API_KEY"},
		{
			agent: "opencode-anthropic", run: opencode(anthropicModel), slot: "anthropic",
			provider: "anthropic", suffix: "/v1", key: "ANTHROPIC_API_KEY",
		},

		// An OpenAI key, which Codex and OpenCode can both spend.
		{
			agent: "codex-openai", slot: "openai", provider: "openai", suffix: "/v1", key: "OPENAI_API_KEY",
			run: `cd ~ && cs-codex exec --skip-git-repo-check -m ` + codexAPIModel + ` ` + shellQuote(pongPrompt),
		},
		{
			agent: "opencode-openai", run: opencode(openaiModel), slot: "openai",
			provider: "openai", suffix: "/v1", key: "OPENAI_API_KEY",
		},

		// A Fireworks key, which only OpenCode reaches, and only through the
		// pinned model's own provider. No -m: this is the path OPENCODE_BASE_URL
		// governs, and the image already pins a Fireworks model.
		{
			agent: "opencode-fireworks", run: opencode(""), slot: "fireworks",
			provider: "fireworks", suffix: "/v1", key: "FIREWORKS_API_KEY",
		},
	}
}

// liveCase is one cell of the matrix: a pairing in one credential mode.
//
// LENT means --lend-*: the sandbox holds a fabrication and the host holds the
// credential. SHARED means --inherit-*: the real credential is copied in.
type liveCase struct {
	pairing
	lent bool
}

func liveCases() []liveCase {
	var out []liveCase
	for _, p := range pairings() {
		out = append(out, liveCase{p, true}, liveCase{p, false})
	}
	return out
}

// name is the subtest, the cassette, and half of the sandbox's name.
func (c liveCase) name() string {
	if c.lent {
		return c.agent + "-lent"
	}
	return c.agent + "-shared"
}

// sandbox is the name this case's sandbox takes, and it is FIXED rather than
// derived from the clock or the pid.
//
// A sandbox's name reaches its hostname and its instance directory, and an
// agent that mentions either puts a per-run value on the wire. Fixed, a
// recording and its replay mint the same one. The cost is that two runs of this
// tier cannot overlap, which serial tiers do not do anyway; the driver
// force-destroys whatever an interrupted run left behind.
func (c liveCase) sandbox() string { return "csvcr-" + c.agent + boolSuffix(c.lent) }

func boolSuffix(lent bool) string {
	if lent {
		return "-l"
	}
	return "-s"
}

// flag is what create is asked for: the credential, in this case's mode.
func (c liveCase) flags(t *testing.T) []string {
	t.Helper()
	s, ok := lend.SlotByID(c.slot)
	if !ok {
		t.Fatalf("%s names slot %q, which this build does not have", c.name(), c.slot)
	}
	verb, what := "inherit", "api-key"
	if c.lent {
		verb = "lend"
	}
	if s.Kind == lend.Login {
		what = "agent-login"
	}
	return []string{"--" + verb + "-" + what, c.slot}
}

// baseEnv is the variable this case's base URL travels in, which is the slot's
// own: nothing in a sandbox is taught a new name for this.
func (c liveCase) baseEnv(t *testing.T) string {
	t.Helper()
	s, ok := lend.SlotByID(c.slot)
	if !ok {
		t.Fatalf("%s names slot %q, which this build does not have", c.name(), c.slot)
	}
	return s.BaseEnv
}

// upstream is the cs-vcr endpoint this case's model calls are addressed to.
//
// vcrHost differs by mode because the DIALLER differs. A shared sandbox dials
// the recorder itself, so it needs the name a guest reaches this host under. A
// lent one is dialled for, by the lender, which runs on the host — so it needs
// a loopback address, and `host.containers.internal` would not resolve there at
// all.
//
// Both arrive at cs-vcr on the same path, which is what lets the two halves of
// a pairing be compared: /c/<provider>/<case>/v1/… either way, put together by
// the client in one mode and by the lender in the other.
func (c liveCase) upstream(vcrHost string) string {
	u := "http://" + vcrHost + "/c/" + c.provider + "/" + c.name()
	if !c.lent {
		u += c.suffix
	}
	return u
}

// proxyEnv is what create is told so this case's traffic reaches the recorder.
//
// The two modes reach it differently and set the same variable to do it. A lent
// sandbox's base URL is READ at create and becomes the loan's upstream (SPEC
// R147a); the sandbox is handed the lender in its place, and --block-side-calls
// points its proxy variables at the lender too. A shared sandbox holds the
// credential itself, so there is no lender in the picture: it is pointed at the
// recorder directly, and its proxy variables have to be set here.
func (c liveCase) proxyEnv(t *testing.T) []string {
	t.Helper()
	if c.lent {
		return []string{"--env", c.baseEnv(t) + "=" + c.upstream(vcrLoopback)}
	}
	guest := engine.HostReachableName + ":" + vcrPort
	env := []string{"--env", c.baseEnv(t) + "=" + c.upstream(guest)}
	// The half of an agent's traffic a base URL does not govern. Claude Code
	// checks its session against api.anthropic.com and Codex reaches
	// chatgpt.com whatever base URL they were given, and what those answer
	// changes the prompt: a real login makes them succeed and a fabricated one
	// makes them 401. cs-vcr answers CONNECT on the same port and refuses that
	// handful, tunnelling the rest, so the sandbox's tools keep their network.
	//
	// Set while recording as well as while replaying. Refused in both halves,
	// the two runs ask the same question, which is what lets a session recorded
	// under a real credential replay under a fabricated one.
	//
	// NO_PROXY carries the recorder's own host, so the model calls above go
	// straight to the base URL rather than through the tunnel.
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		env = append(env, "--env", k+"=http://"+guest)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		env = append(env, "--env", k+"="+engine.HostReachableName+",127.0.0.1,localhost")
	}
	return env
}

// available reports whether this host holds what the case needs, or says what
// is missing. A run without a credential reports what one more login would
// cover, which is the only way anybody learns.
func (c liveCase) available(env map[string]string) string {
	if c.key != "" && env[c.key] == "" {
		return ".env has no " + c.key
	}
	if c.login != "" && !hostLoginPresent(c.login) {
		return "no host " + c.login + " login to share or lend"
	}
	return ""
}

// liveEnv reads the repository's .env. The values never reach a log: this
// returns them, and every caller writes them to a file or hands them to create.
func liveEnv(t *testing.T) map[string]string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for range 6 {
		if p := filepath.Join(dir, ".env"); fileExists(p) {
			path = p
			break
		}
		dir = filepath.Dir(dir)
	}
	if path == "" {
		t.Skip("no .env at the repository root: this tier needs provider keys")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

// liveAgentHome builds the throwaway host profile the money-spending tiers lend
// and share from, and points cs-sandbox at it.
//
// Keys are written from .env. Logins are symlinked from the developer's real
// profiles, because a login is the one credential this suite cannot fabricate
// against a real provider; the cases that need one skip when it is absent.
func liveAgentHome(t *testing.T, env map[string]string) string {
	t.Helper()
	home := agentHomeShell(t)
	keys := lend.KeysDir(home)
	for provider, variable := range map[string]string{
		"anthropic": "ANTHROPIC_API_KEY",
		"openai":    "OPENAI_API_KEY",
		"fireworks": "FIREWORKS_API_KEY",
	} {
		if v := env[variable]; v != "" {
			writeSecret(t, filepath.Join(keys, provider), []byte(v))
		}
	}
	real, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex", "opencode"} {
		src := filepath.Join(real, ".cs-"+agent)
		if !dirExists(src) {
			continue
		}
		if err := os.Symlink(src, filepath.Join(home, ".cs-"+agent)); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// replayKey is what a replayed sandbox authenticates with. It says of itself
// that it is fake, which is what lets a leak scan over a cassette stay strict.
const replayKey = "not-a-real-key-replay-only"

// fabricatedAgentHome builds the host profile the replay tier lends and shares
// from, holding credentials that authenticate nothing.
//
// A replaying case still has to START. An API key is only a string, but both
// login agents read a credential FILE and will not run unattended without one:
// Claude Code puts up its sign-in screen and Codex takes a 401 from its own
// backend. So the file has to be there, in the shape its client insists on.
//
// The shapes come from lend.Slot.MintGuest, which is the product's own
// fabrication — the one a LENT sandbox is given. Reusing it keeps one
// implementation of "what does this client require", rather than a second copy
// in a test that can drift from the first. It already satisfies both readers:
// the expiry check the lender applies, and the inference scope Claude Code
// looks for before it will send anything.
//
// It works because nothing on a replay path can refuse it. cs-vcr serves the
// model calls from the cassette and refuses the hosts the agents contact on
// their own, so a fabricated credential is never presented to anyone able to
// say no. Given an open network the same credential fails, and that is not a
// contradiction: it is the whole reason the recording had to be made with a
// real one.
func fabricatedAgentHome(t *testing.T) string {
	t.Helper()
	home := agentHomeShell(t)
	for _, provider := range []string{"anthropic", "openai", "fireworks"} {
		writeSecret(t, filepath.Join(lend.KeysDir(home), provider), []byte(replayKey))
	}
	for _, id := range lend.SlotIDs(lend.Login) {
		s, ok := lend.SlotByID(id)
		if !ok {
			t.Fatalf("slot %q disappeared between listing and reading it", id)
		}
		g, err := s.MintGuest("replay")
		if err != nil {
			t.Fatalf("fabricate a %s login: %v", id, err)
		}
		if g.File == "" {
			t.Fatalf("slot %q lends a login with no credential file", id)
		}
		writeSecret(t, filepath.Join(home, ".cs-"+g.Agent, g.File), g.Doc)
	}
	return home
}

// agentHomeShell is the empty profile tree both homes are built in, with
// cs-sandbox pointed at it.
//
// CS_SANDBOX_AGENT_HOME moves only where a login is READ from (SPEC R92a).
// Pointing HOME at this tree would do it too, and would take the instance
// directory and every cache along with it.
func agentHomeShell(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(lend.KeysDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CS_SANDBOX_AGENT_HOME", home)
	return home
}

func writeSecret(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// hostLoginPresent reports whether the developer's real profile holds a login
// for this agent, which is what the shared and lent cases of that agent need on
// a tier that reaches a provider.
func hostLoginPresent(agent string) bool {
	real, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	file := map[string]string{"claude": ".credentials.json", "codex": "auth.json"}[agent]
	return fileExists(filepath.Join(real, ".cs-"+agent, file))
}

// startLiveLender runs a lender in this process, on an address a sandbox can
// reach, with its slots reading from home.
//
// In process rather than detached, because create starts a lender by re-execing
// the binary it is running, and under `go test` that binary is the test itself.
// create finds this one by probing the address, which is also what a host
// running a lender under a service manager gets.
func startLiveLender(t *testing.T, home string) {
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
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	t.Setenv("CS_SANDBOX_LEND_ADDR", "0.0.0.0:"+port)
}

// runAgentCase drives one cell end to end and returns what the agent said.
//
// The sandbox's name is fixed, so anything an interrupted run left behind is
// removed first. Destroying is also what revokes a loan, so the cleanup is the
// revocation and there is nothing else to unwind.
func runAgentCase(t *testing.T, r *run.Exec, host hostenv.Host, c liveCase, proxied bool) string {
	t.Helper()
	name := c.sandbox()
	_, _ = execRoot(t, "destroy", name, "-f")
	t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })

	flags := c.flags(t)
	if proxied {
		flags = append(flags, c.proxyEnv(t)...)
	}
	out := createBox(t, r, name, flags...)
	if !strings.Contains(out, "lent:") && !strings.Contains(out, "agent login:") &&
		!strings.Contains(out, "api key:") {
		t.Fatalf("create reported no credential:\n%s", out)
	}

	step(t, "asking the model, through %s…", strings.Join(flags, " "))
	got := stripANSI(inBox(context.Background(), r, host, name, c.run))
	if !strings.Contains(strings.ToLower(got), pongWord) {
		t.Fatalf("the model did not answer through this credential.\ncommand: %s\noutput:\n%s",
			c.run, tail(got, 900))
	}
	return got
}

// Where the recorder listens. Fixed rather than drawn from the ephemeral range:
// the port is written into the sandbox's environment and into the lender's
// upstream, and a value that moves between a recording and its replay is one
// more thing to have to prove does not reach the wire.
const (
	vcrPort     = "8080"
	vcrListen   = "0.0.0.0:" + vcrPort
	vcrAdmin    = "127.0.0.1:8081"
	vcrLoopback = "127.0.0.1:" + vcrPort
)

// syncBuffer collects cs-vcr's output so it can be read WHILE it runs.
//
// os/exec copies a child's output on a goroutine of its own, so a plain
// bytes.Buffer is only safe to read once Wait has returned. The recording tier
// has to look at the log after each case, while the recorder is still serving
// the next one.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// vcrProxy is one running cs-vcr, and the knowledge of how to stop it and read
// what it did.
type vcrProxy struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	mode string
	// diag outlives the run: the whole log, and in replay the requests that
	// could not be served. `cs-vcr calibrate` reads a directory of those and
	// proposes the rules that would have matched, which is the documented way
	// to make a real agent run replayable.
	diag string
	done bool
}

// startVCR runs cs-vcr on the host in record or replay mode, serving cassettes
// from store, and returns once it is answering.
//
// A host process rather than a container. A sandbox already reaches this host
// by name (SPEC R52a), which is the same route the lender takes, so there is no
// network to join, no image to stage and no alias to allocate. Everything a
// container would buy here — isolation between concurrent runs — a serial tier
// does not need.
func startVCR(t *testing.T, mode, store string) *vcrProxy {
	t.Helper()
	bin, err := exec.LookPath("cs-vcr")
	if err != nil {
		t.Skipf("cs-vcr is not on PATH: run `make tools`, which builds it at the go.mod pin (%v)", err)
	}
	if err := os.MkdirAll(store, 0o750); err != nil {
		t.Fatal(err)
	}
	diag, err := filepath.Abs(filepath.Join("..", "..", ".tmp", "agent-vcr", mode))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(diag); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(diag, 0o700); err != nil {
		t.Fatal(err)
	}

	args := []string{mode, "--config", writeVCRConfig(t), "--cassettes", store,
		"--listen", vcrListen, "--admin", vcrAdmin}
	if mode == "replay" {
		args = append(args, "--dump-misses", diag)
	}
	p := &vcrProxy{cmd: exec.Command(bin, args...), out: &syncBuffer{}, mode: mode, diag: diag}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start cs-vcr %s: %v", mode, err)
	}
	t.Cleanup(func() { p.stop(t) })
	waitForVCR(t, p)
	t.Logf("cs-vcr %s on %s, cassettes in %s", mode, vcrListen, store)
	return p
}

// waitForVCR blocks until the admin port answers, so no sandbox is created
// against a recorder that is not up yet.
func waitForVCR(t *testing.T, p *vcrProxy) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get("http://" + vcrAdmin + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		if p.cmd.ProcessState != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.stop(t)
	t.Fatalf("cs-vcr %s never answered on %s. Port %s in use?\n%s", p.mode, vcrAdmin, vcrPort, p.out.String())
}

// stop interrupts cs-vcr and returns everything it printed.
//
// Interrupted rather than killed, and that is load-bearing: cs-vcr writes its
// accounting — how many steps it served and how many upstream calls it made —
// on the way out. Killed, the replay tier's central assertion has nothing to
// read. Idempotent, so the assertion and the cleanup can both ask.
func (p *vcrProxy) stop(t *testing.T) string {
	t.Helper()
	if !p.done {
		p.done = true
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(os.Interrupt)
		}
		_ = p.cmd.Wait() // exit 4 on a miss, which the summary explains
		if err := os.WriteFile(filepath.Join(p.diag, "cs-vcr.log"), []byte(p.out.String()), 0o600); err == nil {
			t.Logf("cs-vcr %s log: %s", p.mode, filepath.Join(p.diag, "cs-vcr.log"))
		}
		if dumped, err := filepath.Glob(filepath.Join(p.diag, "[0-9]*.json")); err == nil && len(dumped) > 0 {
			t.Logf("cs-vcr dumped %d missed request(s) in %s — propose rules with: cs-vcr calibrate <cassette> %s",
				len(dumped), p.diag, p.diag)
		}
	}
	return p.out.String()
}

// recordedTruncated reports whether cs-vcr had to record a response the client
// cut off, and says so.
//
// These CLIs print their answer and exit, often while the response that carried
// it is still streaming, so cs-vcr stores what arrived and warns. It is normal
// rather than exceptional: measured across one full recording of this matrix,
// eight of fourteen cases truncated at least one response, including every
// Codex one.
//
// So this reports rather than fails, which it did not always do. Failing on the
// warning threw away eight legitimate recordings, and the ones it threw away
// were mostly ones that replay perfectly well: a truncated response only breaks
// a replay when the client, handed the same short stream back, asks again — and
// the cassette has no second copy of a question that was only ever asked once.
// Measured on claude-anthropic-lent, whose four-event recording replayed with a
// miss where its eight-event re-recording replayed clean.
//
// What decides it is therefore the replay, not the warning, and the replay is
// one command away. This exists to name the likely cause when that command
// fails, so nobody has to find this note on their own.
func recordedTruncated(t *testing.T, p *vcrProxy, cassette string) bool {
	t.Helper()
	const interrupted = "recording an interrupted response"
	for line := range strings.SplitSeq(p.out.String(), "\n") {
		if !strings.Contains(line, interrupted) || !strings.Contains(line, "cassette="+cassette) {
			continue
		}
		t.Logf("cs-vcr recorded a truncated response for %s:\n%s\n"+
			"  the agent exited while a response was still streaming. Usually harmless.\n"+
			"  If this cassette then misses on replay, record it again:\n"+
			"    make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/%s'",
			cassette, strings.TrimSpace(line), cassette)
		return true
	}
	return false
}

// reportMisses says how much of this run the cassettes could not answer.
//
// Reported rather than asserted, and the reason is what makes the tier's other
// assertion enough. A miss on the turn that carries the QUESTION cannot pass:
// the agent has no answer to print, and the case fails on its word before this
// is ever reached. What can pass is a miss on a bookkeeping call, and those are
// not reproducible even in principle — whether a client makes one, how many
// times, and on which model all vary with how fast the answer came back, and a
// replay answers in a fraction of the time the recording took.
//
// cs-vcr absorbs most of them: `auxiliary_turns` answers a bookkeeping call
// from any recorded one, and `lookahead` absorbs the order they arrive in. The
// gap this tier sits in is the one case that leaves: auxiliary_turns recognizes
// a recorded turn as auxiliary only when it is ALSO on a model the cassette
// does not otherwise use, and PINNING the model — which this matrix does, and
// which any reproducible harness does — puts Claude Code's title generation on
// the same model as the turn it is naming. Measured: one miss in twenty-nine
// requests across the whole matrix, on claude-anthropic-lent, stable across
// runs, with every case still answering and nothing spent.
//
// So the number is printed and watched rather than enforced. A jump in it is
// the signal; a floor of zero is not one this tier can hold honestly. The
// missed requests are dumped, and `cs-vcr calibrate` reads that directory.
func reportMisses(t *testing.T, p *vcrProxy) {
	t.Helper()
	summary := p.stop(t)
	misses, err := countedAs(summary, "misses")
	if err != nil {
		t.Errorf("cs-vcr printed no miss accounting: %v\n%s", err, tailLines(summary, 40))
		return
	}
	served, err := countedAs(summary, "replayed")
	if err != nil {
		t.Errorf("cs-vcr printed no replay accounting: %v\n%s", err, tailLines(summary, 40))
		return
	}
	t.Logf("cs-vcr served %d request(s) and missed %d", served, misses)
	if misses > served/4 {
		t.Errorf("replay missed %d of %d request(s), which is more than bookkeeping accounts for: "+
			"the cassettes and this run are asking different things\n%s", misses, served+misses, tailLines(summary, 40))
	}
}

// assertSpentNothing is the assertion that makes the replay tier worth running.
// A replay that quietly fell through to a provider would pass every other check
// here, and cost money on every push.
//
// Two independent statements, because either alone can be true by accident.
// cs-vcr says at startup that this session will contact no provider, which is a
// property of the mode it was started in; and it accounts for the upstream
// calls it made when it shuts down, which is what actually happened.
func assertSpentNothing(t *testing.T, p *vcrProxy) {
	t.Helper()
	summary := p.stop(t)
	const promise = "no provider will be contacted this session"
	if !strings.Contains(summary, promise) {
		t.Errorf("cs-vcr never said %q, so this was not a replay session:\n%s", promise, tailLines(summary, 40))
		return
	}
	calls, err := countedAs(summary, "upstream calls")
	if err != nil {
		t.Errorf("cs-vcr printed no upstream accounting, so the replay cannot be shown to have spent nothing: %v\n%s",
			err, tailLines(summary, 40))
		return
	}
	if calls != 0 {
		t.Errorf("replay made %d upstream call(s): a cassette miss fell through to a real provider\n%s",
			calls, tailLines(summary, 40))
	}
}

// countedAs reads one number out of cs-vcr's shutdown accounting, whose lines
// are a label and a count separated by whitespace.
func countedAs(summary, label string) (int, error) {
	for line := range strings.SplitSeq(summary, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), label)
		if !ok {
			continue
		}
		return strconv.Atoi(strings.TrimSpace(rest))
	}
	return 0, fmt.Errorf("no %q line in the log", label)
}

// cassetteStore is where the committed cassettes live: one directory per case,
// which is the shape cs-vcr reads.
func cassetteStore(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "test", "cassettes"))
	if err != nil {
		t.Fatalf("resolve the cassette store: %v", err)
	}
	return abs
}

// hasCassette reports whether this case has been recorded. One with no cassette
// is skipped rather than failed: a recording that was never made cannot be said
// to have a broken replay.
func hasCassette(t *testing.T, c liveCase) bool {
	t.Helper()
	return fileExists(filepath.Join(cassetteStore(t), c.name(), "index.jsonl"))
}

// writeVCRConfig writes the recorder's configuration into scratch, and returns
// the path.
//
// A file of this suite's own rather than the developer's
// ~/.config/cs-vcr/config.yaml, which cs-vcr would otherwise read: that one is
// theirs to arrange, and one arrangement seen in the wild points the `openai`
// entry at the ChatGPT backend — right for a Codex subscription and wrong for
// the API key this matrix records against it.
//
// It names no providers. The four cs-vcr ships are the four this matrix uses,
// so saying them again would only be a second place for them to be wrong.
func writeVCRConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cs-vcr.yaml")
	// The account this run has. A sandbox gives the guest the uid and the name
	// of whoever launched it, so a cassette recorded here says /home/<whoever
	// recorded it> in every request that mentions a path. Blanking it on both
	// sides is what makes a cassette committable at all, and what lets CI,
	// running as a different account, replay one.
	//
	// The literal name rather than a shape: this is the one account the run
	// actually has, and a pattern like /home/[a-z]+ would blank a path a prompt
	// legitimately talks about.
	me := os.Getenv("USER")
	if me == "" {
		u, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("resolve the account the guest will mirror: %v", err)
		}
		me = filepath.Base(u)
	}
	user := regexp.QuoteMeta(me)

	// The first two entries are cs-vcr's own shipped captures, restated.
	//
	// They have to be, and it is worth knowing why before editing this: a
	// `capture:` block in a config file REPLACES the shipped list rather than
	// adding to it, so declaring one rule silently drops the rest. Measured on
	// the pinned build — a file declaring one `replace` rule took the resolved
	// ruleset from ten to one. Dropping <SESSION> would leave Claude Code's
	// per-run scratchpad path in the key, and every request of every session
	// would miss.
	body := fmt.Sprintf(`# Written by internal/cli/agentmatrix_test.go. Not committed.
normalize:
  capture:
    - pattern: '(?:/tmp/claude-\d+/[^/"]+/)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})'
      as: '<SESSION>'
    - pattern: '(?:chunk_id\\?":\\?")([^"\\]+)'
      as: '<CHUNK>'
    - pattern: '(?:/home/|-home-)(%[1]s)'
      as: '<USER>'
    - pattern: '(%[1]s %[1]s)'
      as: '<USER_GROUP>'
`, user)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the cs-vcr config: %v", err)
	}
	return path
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stripANSI removes the escape sequences the agents' TUIs emit, so an assertion
// is about the model's answer rather than about how it was painted.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= '@' && s[j] <= '~') {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
