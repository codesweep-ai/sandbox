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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/paths"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
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
// diff explains.
const (
	// Claude Code names a model bare; OpenCode names the provider too.
	claudeModel    = "claude-opus-5"
	anthropicModel = "anthropic/" + claudeModel
	// One slug for every OpenAI-side pairing, subscription and key alike.
	// cs-campaign records both its codex scenarios on it — the ChatGPT backend
	// and the versioned API — which is the evidence that both surfaces address
	// it. OpenCode names the provider too.
	codexModel  = "gpt-5.6-sol"
	openaiModel = "openai/" + codexModel
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
	agent string
	// run builds the command for one case: the session names are per case,
	// so the command cannot be a constant string.
	run func(liveCase) string
	// cli is the agent BINARY this pairing drives, and the one whose version
	// its cassette is bound to.
	//
	// Distinct from agent, which names the pairing and carries the credential
	// too: claude-login and claude-anthropic are two credential paths through
	// one Claude Code. That is the distinction the replay gate needs — a Claude
	// Code bump invalidates the cassettes of both, and leaves codex's and
	// opencode's alone.
	cli string
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
	// memMiB is what this case's sandbox is given. Zero takes matrixMemMiB.
	// OpenCode is the heaviest client here and does not get a turn started in
	// the default, so its pairings ask for more.
	memMiB int
}

// The tmux sessions the turn drivers work in, named per case.
//
// Derived rather than fixed, because these cells run in parallel and one token
// would have every one of them driving the same session. Derived rather than
// random, because a recording and its replay must reach the same session — and
// the name reaches nothing a provider sees, so a stable function of the case
// name satisfies both.
func (c liveCase) turnToken() string {
	return "csvcr" + strings.Map(func(r rune) rune {
		if r == '-' {
			return -1
		}
		return r
	}, c.name())
}

// turnUUID is the same name in the shape cs-claude-turn wants: it takes a UUID
// rather than a token, so the case name is hashed into one. Version 4 in shape
// only; nothing here checks it.
func (c liveCase) turnUUID() string {
	x := sha256.Sum256([]byte(c.name()))
	h := hex.EncodeToString(x[:])
	return fmt.Sprintf("%s-%s-4%s-8%s-%s", h[0:8], h[8:12], h[12:15], h[15:18], h[18:30])
}

// What a case's microVM gets. Small on purpose: these cases ask one question,
// and the budget is about proving the clients work in the room a caller
// actually gives them. cs-campaign runs its members at exactly these sizes.
//
// OpenCode is the outlier and gets twice as much. It is the heaviest client
// here, and at 1 GB it does not get a turn started at all — measured, in a
// campaign whose orchestrator ran fine beside it in the same budget.
//
// Only firecracker enforces either number: --mem is a microVM's RAM, and a
// container is left to the host's. That is one of the reasons the matrix runs
// on both engines rather than on the fast one — a client that only works with
// the whole machine behind it passes every podman cell.
const (
	matrixMemMiB   = 1024
	opencodeMemMiB = 2048
)

// matrixEngine is the engine this run boots its cells on.
//
// Both are worth running and neither subsumes the other. Podman is fast and is
// what a developer reaches for; firecracker is what a caller actually ships —
// cs-campaign's members are microVMs — and it is the only engine that honours
// the memory budget above, so a client that only works with the host's whole
// RAM behind it fails there and nowhere else.
//
// One cassette serves both. What a client sends is its own business and should
// not depend on what it is boxed in, so a miss on one engine and not the other
// is a finding rather than a fixture problem.
func matrixEngine() string {
	if e := os.Getenv("CS_SANDBOX_AGENTS_ENGINE"); e != "" {
		return e
	}
	return "podman"
}

// matrixSetup is liveSetup plus whatever the engine above needs before a cell
// can boot on it.
//
// The firecracker half is not optional decoration. A microVM copies the base
// rootfs per cell, and liveSetup's instance root is a t.TempDir() — a tmpfs on
// most hosts, where that copy dies with "Disk quota exceeded" before anything
// under test runs. It is also asked for BEFORE the caller starts a lender: the
// lender reads its loan records out of the instance root, so a root that moves
// afterwards leaves it reading an empty one.
//
// A host that cannot boot a microVM skips rather than fails, which is what
// every other live tier here does: the artifacts are a limitation of the
// machine, not a fault in the change.
func matrixSetup(t *testing.T) (*run.Exec, hostenv.Host) {
	t.Helper()
	r, host := liveSetup(t)
	matrixGroup(t)
	if matrixEngine() != "firecracker" {
		return r, host
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("CS_SANDBOX_AGENTS_ENGINE=firecracker, and /dev/kvm is unavailable: %v", err)
	}
	if !fileExists(filepath.Join(paths.FCCache(), "vmlinux.elf")) {
		t.Skip("CS_SANDBOX_AGENTS_ENGINE=firecracker, and the artifacts are not built " +
			"(run: cs-sandbox build --engine firecracker)")
	}
	fcInstancesDir(t, host)
	return r, host
}

// matrixGroup puts this run in a group of its own, and takes it away after.
//
// The group is the isolation, and everything else in this tier follows from it:
// a group is one podman network, the recorder and the lender are containers ON
// that network, and the cells are the only sandboxes attached to it. Two runs
// on one machine — two CI jobs, two developers, a developer beside a runner —
// therefore cannot see each other's recorder, cannot collide on its name, and
// cannot answer each other's health checks. None of that was true while the
// recorder was a process on the host's port 8080.
//
// A caller may name the group instead — CI does, so that several jobs on one
// machine each get their own, and a person debugging one cell can point a run at
// a group they can inspect afterwards. A named group is still CREATED here and
// is never removed here: taking away something this run did not make is how a
// debugging session loses its evidence, or one CI job takes another's network.
func matrixGroup(t *testing.T) {
	t.Helper()
	g, named := os.LookupEnv("CS_SANDBOX_GROUP")
	if !named || g == "" {
		g = "vcr" + runID
		t.Setenv("CS_SANDBOX_GROUP", g)
		// Registered before the recorder's own cleanup, so it runs after it:
		// the network cannot go while a container is still on it.
		t.Cleanup(func() {
			if out, err := execRoot(t, "group", "rm", g, "-f"); err != nil {
				t.Logf("could not remove the run's group %s: %v (%s)", g, err, strings.TrimSpace(out))
			}
		})
	}
	// The network has to exist before anything can join it, and the recorder
	// joins it before the first cell is created. `create` would have made it —
	// but the recorder comes first, which is the whole point of it: a cell is
	// pointed at the recorder as it is built.
	if out, err := execRoot(t, "group", "create", g); err != nil {
		t.Fatalf("create the run's group %s: %v (%s)", g, err, strings.TrimSpace(out))
	}
}

// runInBox runs one command inside a cell's sandbox, by the route its engine
// answers on.
//
// A container is reached with `podman exec`, which is both faster and the way
// every other podman member here reaches one. A microVM has no container to
// exec into: it answers ssh over its vsock bridge, under the name create wrote
// into ~/.ssh/config.d, and that is the only way in.
//
// Both arrive as the dev user, in their home, holding the environment create
// gave the sandbox — which is everything an agent reads. What differs between
// them is the shell's own initialisation, and nothing asked here comes from it:
// the turn drivers are on the image's PATH and the credentials arrive as
// environment. So what the agent sends is the same either way, which is what
// lets one cassette serve both engines.
// boxOutput is what one command inside a cell's sandbox produced.
//
// Split into the answer and the diagnosis, and that split is load-bearing.
// Joining them is how fourteen microVM cells came to report PASS on a run where
// the recorder served nothing at all: the diagnosis quotes the command, the
// command carries the prompt, and the prompt contains the single word the
// assertion looks for. Only Answer may ever be asserted on.
type boxOutput struct {
	Answer string // stdout: what the agent produced, and the only thing to assert on
	Diag   string // stderr and exit status, for a failure message and nothing else
}

func runInBox(ctx context.Context, t *testing.T, r *run.Exec, host hostenv.Host, name, sh string) boxOutput {
	t.Helper()
	var argv []string
	if matrixEngine() == "firecracker" {
		argv = append([]string{"ssh"}, sshArgv(t, host, name)...)
	} else {
		argv = []string{"podman", "exec", "--user", host.User,
			"--workdir", "/home/" + host.User, objName(name), "bash", "-lc"}
	}
	res, err := r.Run(ctx, run.Opts{ReadOnly: true}, append(argv, sh)...)
	// Everything the driver said, INCLUDING when it failed, and including
	// stderr. run.Output would have been the obvious call and is the wrong one
	// here: it returns "" on any non-zero exit, and these drivers exit 2 when a
	// turn stalls and 3 when the client will not start. Both are exactly the
	// cases worth reading, and both are where its diagnosis goes -- to stderr,
	// which run.Output drops even on success.
	//
	// Measured, on a CI leg where every firecracker cell failed: the harness
	// could report only "the model did not answer", with an empty output block
	// under it, because the one sentence naming the cause had been thrown away
	// twice over.
	out := boxOutput{Answer: res.Stdout}
	if e := strings.TrimSpace(res.Stderr); e != "" {
		out.Diag = "stderr:\n" + e
	}
	if err != nil {
		out.Diag += fmt.Sprintf("\n(the command exited %d: %v)", res.ExitCode, err)
	}
	return out
}

// pairings is the matrix. Every combination is listed whether or not this host
// can sign in for it: a case that skips says which credential is missing, and
// that is the only way a contributor learns what one more login would cover.
func pairings() []pairing {
	// --model, and not for the cost. Claude Code takes its own default
	// otherwise, measured recording as claude-opus-5: a cassette keyed on
	// whatever that default happens to be stops matching on the day it moves,
	// for a reason no diff explains.
	// Driven through cs-claude-turn rather than `cs-claude -p`, because the
	// two are different clients of the same credential.
	//
	// -p is headless: one process, one request, no terminal. cs-claude-turn
	// drives the INTERACTIVE Claude Code inside a long-running tmux session —
	// it pastes the prompt and waits for the turn_duration marker in the
	// session JSONL. That is the path a campaign uses for every turn, and it
	// is the one that has been stalling on hosted runners while this matrix,
	// on -p, stays green. Running the matrix on the driver a caller actually
	// uses is what makes this tier evidence for that caller.
	//
	// --timeout is the turn bound; the tier's own timeout is well above it, so
	// a stall reports as a stalled turn rather than as a killed test.
	// --wrapper carries the model pin, which cs-claude-turn has no flag of its
	// own for: it launches `$WRAPPER --session-id <uuid>`, so the model belongs
	// on the wrapper. Pinned for the reason above and not for the cost — the
	// TUI takes Claude Code's own default otherwise, and a cassette keyed on
	// whatever that happens to be stops matching the day it moves.
	claude := func(c liveCase) string {
		return `cd ~ && printf %s ` + shellQuote(pongPrompt) +
			` | cs-claude-turn --uuid ` + c.turnUUID() +
			` --wrapper ` + shellQuote("cs-claude --model "+claudeModel) +
			` --workdir "$HOME" --timeout 300`
	}
	// Codex and OpenCode go through their own turn drivers for the same reason
	// claude does: that is the client a caller drives, and a matrix that proves
	// the credential paths against a different one proves them for nobody.
	//
	// Neither driver takes a model flag, so the pin goes where each CLI reads
	// it from — the channel cs-campaign uses when it configures a member.
	// Without it a session takes the client's default, and a cassette keyed on
	// a default that moves stops matching for a reason no diff explains.
	codex := func(model string) func(liveCase) string {
		pin := ""
		if model != "" {
			// Prepended, not appended: a bare key after a [table] header
			// belongs to that table, so appending would land the model in
			// whatever section codex wrote last.
			pin = `mkdir -p ~/.cs-codex && touch ~/.cs-codex/config.toml && ` +
				`{ printf 'model = "%s"\n' ` + shellQuote(model) + `; ` +
				`grep -v '^model = ' ~/.cs-codex/config.toml; } > ~/.cs-codex/config.toml.new && ` +
				`mv ~/.cs-codex/config.toml.new ~/.cs-codex/config.toml && `
		}
		return func(c liveCase) string {
			return `cd ~ && ` + pin + `printf %s ` + shellQuote(pongPrompt) +
				` | cs-codex-turn --tmux ` + c.turnToken() + ` --workdir "$HOME" --timeout 300`
		}
	}
	opencode := func(model string) func(liveCase) string {
		pin := ""
		if model != "" {
			// opencode resolves its model from opencode.json. The image already
			// pins a Fireworks one, which is why that pairing passes no model
			// and overrides nothing.
			pin = `mkdir -p ~/.cs-opencode && python3 -c '` +
				`import json,pathlib,sys; p=pathlib.Path.home()/".cs-opencode/opencode.json"; ` +
				`c=json.loads(p.read_text()) if p.exists() else {}; c["model"]=sys.argv[1]; ` +
				`p.write_text(json.dumps(c,indent=2)+"\n")' ` + shellQuote(model) + ` && `
		}
		return func(c liveCase) string {
			return `cd ~ && ` + pin + `printf %s ` + shellQuote(pongPrompt) +
				` | cs-opencode-turn --tmux ` + c.turnToken() + ` --workdir "$HOME" --timeout 300`
		}
	}
	return []pairing{
		// An agent login, which only its own agent can spend.
		{agent: "claude-login", cli: "claude", run: claude, slot: "claude", provider: "anthropic", login: "claude"},
		{
			agent: "codex-login", cli: "codex", slot: "codex", provider: "chatgpt", login: "codex",
			run: codex(codexModel),
			// No suffix: the subscription transport has no version segment.
		},

		// An Anthropic key, which Claude Code and OpenCode can both spend.
		{agent: "claude-anthropic", cli: "claude", run: claude, slot: "anthropic", provider: "anthropic", key: "ANTHROPIC_API_KEY"},
		{
			agent: "opencode-anthropic", cli: "opencode", run: opencode(anthropicModel), slot: "anthropic",
			provider: "anthropic", suffix: "/v1", key: "ANTHROPIC_API_KEY", memMiB: opencodeMemMiB,
		},

		// An OpenAI key, which Codex and OpenCode can both spend.
		{
			agent: "codex-openai", cli: "codex", slot: "openai", provider: "openai", suffix: "/v1", key: "OPENAI_API_KEY",
			run: codex(codexModel),
		},
		{
			agent: "opencode-openai", cli: "opencode", run: opencode(openaiModel), slot: "openai",
			provider: "openai", suffix: "/v1", key: "OPENAI_API_KEY", memMiB: opencodeMemMiB,
		},

		// A Fireworks key, which only OpenCode reaches, and only through the
		// pinned model's own provider. No -m: this is the path OPENCODE_BASE_URL
		// governs, and the image already pins a Fireworks model.
		{
			agent: "opencode-fireworks", cli: "opencode", run: opencode(""), slot: "fireworks",
			provider: "fireworks", suffix: "/v1", key: "FIREWORKS_API_KEY", memMiB: opencodeMemMiB,
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
// vcrHost is the same for both modes now, and that is the change: the recorder
// is on the run's network, the lender is a container on the same network, and
// neither route goes through the host. It used to differ because the lender ran
// on the host, where the recorder was a loopback address.
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
		return []string{"--env", c.baseEnv(t) + "=" + c.upstream(vcrGuest)}
	}
	guest := vcrGuest
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
		env = append(env, "--env", k+"="+vcrName+",127.0.0.1,localhost")
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
		g, err := s.MintGuest("replay", "")
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
		Home:    home,
		KeysDir: lend.KeysDir(home),
		Loans:   lend.NewFileLoans(paths.Instances()),
		Callers: lend.CallersHost,
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
	_, _ = destroyBox(t, name)
	t.Cleanup(func() { _, _ = destroyBox(t, name) })

	// --yolo, because every case here drives the agent's INTERACTIVE client and
	// an interactive agent that has to ask permission never finishes a turn.
	//
	// It did not matter while this matrix ran headless one-shots: `cs-codex
	// exec` asks nobody. The TUI is governed by the image's own defaults
	// instead — approval_policy = "on-request" — and cs-codex only passes
	// --dangerously-bypass-approvals-and-sandbox when the instance is yolo.
	// Without it codex sat on an approval prompt and the driver tore the
	// session down mid-stream, which the recorder saw as "the client closed
	// the connection before the response ended".
	//
	// Uniform across the matrix rather than added to the case that exposed it:
	// a caller driving these clients unattended runs them all this way, and
	// cs-campaign creates every member yolo for exactly this reason.
	mem := c.memMiB
	if mem == 0 {
		mem = matrixMemMiB
	}
	flags := append([]string{"--yolo", "--mem", strconv.Itoa(mem)}, c.flags(t)...)
	if proxied {
		flags = append(flags, c.proxyEnv(t)...)
	}
	out := createBoxOn(t, r, name, matrixEngine(), flags...)
	if !strings.Contains(out, "lent:") && !strings.Contains(out, "agent login:") &&
		!strings.Contains(out, "api key:") {
		t.Fatalf("create reported no credential:\n%s", out)
	}

	step(t, "asking the model, through %s…", strings.Join(flags, " "))
	said := runInBox(context.Background(), t, r, host, name, c.run(c))
	// stripANSI over the ANSWER alone. The diagnosis is printed beside it and
	// never searched: see boxOutput.
	got := stripANSI(said.Answer)
	if !strings.Contains(strings.ToLower(got), pongWord) {
		t.Fatalf("the model did not answer through this credential.\ncommand: %s\noutput:\n%s\n%s\n%s",
			c.run(c), tail(got, 900), said.Diag, reachedRecorder(t, r, host, c, name))
	}
	return got
}

// reachedRecorder asks the guest what it can see of the recorder, for a turn
// that produced no answer.
//
// Only on the failure path, so it costs nothing on a green run. It exists
// because "the model did not answer" has several causes that look identical
// from the host and are told apart in one line from inside the sandbox: a name
// that does not resolve, a route that is not there, a port that refuses, and a
// port that hangs. A CI leg where every microVM cell failed could distinguish
// none of them, and the whole tier reported one sentence and an empty block.
//
// Best-effort by construction. The sandbox is in a bad state by the time this
// runs, so it asks for everything at once, bounds the curl, and reports whatever
// comes back -- including nothing, which is itself an answer about the guest.
func reachedRecorder(t *testing.T, r *run.Exec, host hostenv.Host, c liveCase, name string) string {
	t.Helper()
	base := "$" + c.baseEnv(t)
	probe := `echo "base: ` + base + `"; ` +
		`getent ahosts ` + vcrName + ` || echo "(does not resolve)"; ` +
		`curl -s -o /dev/null -w 'healthz: HTTP %{http_code} in %{time_total}s\n' --max-time 10 ` +
		`"` + base + `/healthz"; echo "curl exit $?"; ` +
		`ip route 2>&1 | head -5`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	seen := runInBox(ctx, t, r, host, name, probe)
	return "what the guest sees of the recorder:\n" + seen.Answer + seen.Diag +
		"\nwhat the recorder offers it:\n" + recorderSideOfTheHop(ctx, r, vcrBoxName())
}

// recorderSideOfTheHop is the other end of the probe above: what the recorder
// was actually offering while the guest could not reach it.
//
// Both halves are needed and neither substitutes for the other. "connection
// refused" from inside the guest means one thing if the recorder is listening
// and quite another if it is not listening at all, and a guest that times out
// looks the same whether the packets were dropped on the way out or on the way
// in.
//
// One hop to describe, where there used to be two. While the recorder was a
// host process the guest reached it through the rootless namespace's NAT, and a
// microVM took a different pasta to get there than a container did — which is
// how a CI leg came to pass every container cell and time out every microVM
// one. Recorder and sandbox are on the same bridge now, so the question is
// simply whether the recorder is up and whether its network answers.
func recorderSideOfTheHop(ctx context.Context, r *run.Exec, box string) string {
	var b strings.Builder
	for _, probe := range [][]string{
		{"podman", "exec", box, "ss", "-lntp"},
		{"podman", "exec", box, "ip", "-brief", "address"},
		{"podman", "inspect", box, "--format", "{{.State.Status}} on {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}}{{end}}"},
	} {
		res, err := r.Run(ctx, run.Opts{ReadOnly: true}, probe...)
		out := strings.TrimSpace(res.Stdout + res.Stderr)
		if err != nil && out == "" {
			out = err.Error()
		}
		if out == "" {
			out = "(no answer)"
		}
		fmt.Fprintf(&b, "  $ %s\n    %s\n", strings.Join(probe[2:], " "), strings.ReplaceAll(out, "\n", "\n    "))
	}
	return b.String()
}

// Where the recorder listens, and what reaches it.
//
// The port is fixed and can stay fixed, because it is not a host port any more:
// the recorder is a container on the run's own network, so 8080 there collides
// with nothing on the machine — not another run's recorder, not another user's,
// not whatever the developer already has on 8080. What a sandbox is given is
// the NAME, which resolves on that network and nowhere else.
//
// The admin port stays on the container's own loopback, deliberately. It is the
// control plane, and putting it on the network would let a sandbox drive the
// recorder. Nothing but `podman exec` can reach it.
const (
	vcrName     = "cs-vcr"
	vcrPort     = "8080"
	vcrListen   = "0.0.0.0:" + vcrPort
	vcrAdmin    = "127.0.0.1:8081"
	vcrGuest    = vcrName + ":" + vcrPort
	vcrInternal = "127.0.0.1:" + vcrPort
)

// vcrProxy is one running cs-vcr, and the knowledge of how to stop it and read
// what it did.
type vcrProxy struct {
	r    *run.Exec
	name string
	mode string
	// diag outlives the run: the whole log, and in replay the requests that
	// could not be served. `cs-vcr calibrate` reads a directory of those and
	// proposes the rules that would have matched, which is the documented way
	// to make a real agent run replayable.
	diag  string
	final string // the log, kept at stop, because the container is removed then
	done  bool
}

// vcrBoxName is the recorder container for this run's network. Named after the
// network the way the keepalive and the lender are, because that is its
// lifetime and its scope.
func vcrBoxName() string { return state.NetworkName(testGroup()) + "-vcr" }

// startVCR runs cs-vcr in record or replay mode, serving cassettes from store,
// and returns once it is answering.
//
// A CONTAINER on this run's network, where it used to be a host process on a
// fixed 8080. The port was the problem: one machine has one 8080, so a second
// run — another user's, another CI job's, or a developer's own — could not
// start a recorder at all. Worse than the collision was what followed it: the
// harness health-checked the PORT rather than its own child, so the second run
// adopted the first one's recorder and replayed its cassettes, or recorded its
// prompts into the other's store.
//
// On the network, none of that can happen. The port is namespace-local, the
// name resolves on this bridge and nowhere else, and a run in another group has
// no interface to reach it on. What the guest is given is a name rather than a
// host address, which also makes the two engines one case: a microVM and a
// container reach a bridge neighbour the same way, where they reached the host
// by different routes.
func startVCR(t *testing.T, mode, store string) *vcrProxy {
	t.Helper()
	bin := os.Getenv("CS_VCR_BIN")
	if bin == "" {
		t.Skip("set CS_VCR_BIN to a Linux cs-vcr for the image's architecture: " +
			"`make container-bins` builds one, and the tiers that need it set the variable")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("CS_VCR_BIN=%s: %v", bin, err)
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

	p := &vcrProxy{r: &run.Exec{}, name: vcrBoxName(), mode: mode, diag: diag}
	ctx := context.Background()
	_, _ = p.r.Run(ctx, run.Opts{}, "podman", "rm", "-f", p.name)

	// The cassettes are read-only in replay and written in record, and that is
	// the one difference between the two invocations here.
	mount := ":ro"
	if mode == "record" {
		mount = ""
	}
	argv := []string{"podman", "run", "-d",
		"--name", p.name,
		"--hostname", vcrName,
		"--network", state.NetworkName(testGroup()),
		"--network-alias", vcrName,
		"--label", "cs-sandbox.managed=1",
		"--label", "cs-sandbox.vcr=1",
		// The same reason the gateway and the lender use it: these are host
		// directories owned by the invoking user, and SELinux denies the read
		// without either this or a relabel of the repository.
		"--security-opt", "label=disable",
		"-v", bin + ":/usr/local/bin/cs-vcr-host:ro",
		"-v", store + ":" + store + mount,
		"-v", diag + ":" + diag,
	}
	cfg := writeVCRConfig(t, diag)
	argv = append(argv, "-v", cfg+":"+cfg+":ro", "--entrypoint", "/usr/local/bin/cs-vcr-host", image(t),
		mode, "--config", cfg, "--cassettes", store, "--listen", vcrListen, "--admin", vcrAdmin)
	if mode == "replay" {
		argv = append(argv, "--dump-misses", diag)
	}
	if res, err := p.r.Run(ctx, run.Opts{}, argv...); err != nil {
		t.Fatalf("start cs-vcr %s: %v\n%s", mode, err, strings.TrimSpace(res.Stderr))
	}
	t.Cleanup(func() { p.stop(t) })
	waitForVCR(t, p)
	t.Logf("cs-vcr %s as %s on %s, cassettes in %s", mode, vcrName, state.NetworkName(testGroup()), store)
	return p
}

// log is everything the recorder has printed so far.
//
// Read from the container rather than from a pipe, which is what lets the
// recording tier look at it after each case while the recorder is still serving
// the next one. Once stopped the container is gone, so the last read is kept.
func (p *vcrProxy) log() string {
	if p.final != "" {
		return p.final
	}
	return p.readLog()
}

// readLog takes BOTH of the container's streams. `podman logs` keeps them
// apart, and cs-vcr uses both: the session banner — including the promise the
// replay tier asserts on — is logging output on stderr, while the shutdown
// accounting is printed on stdout. Reading one of them finds half a session and
// fails an assertion about the other half.
func (p *vcrProxy) readLog() string {
	res, _ := p.r.Run(context.Background(), run.Opts{ReadOnly: true}, "podman", "logs", p.name)
	return res.Stderr + res.Stdout
}

// waitForVCR blocks until the recorder answers its own admin endpoint, so no
// sandbox is created against a recorder that is not up yet.
//
// Asked from INSIDE the container, because the admin plane is on the
// container's loopback and nothing else can reach it. That is also what makes
// this answer about THIS recorder: the old check dialled a host port and would
// happily accept somebody else's recorder answering on it.
func waitForVCR(t *testing.T, p *vcrProxy) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := p.r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "exec", p.name,
			"curl", "-fsS", "-o", "/dev/null", "-m", "2", "http://"+vcrAdmin+"/healthz"); err == nil {
			return
		}
		out := run.Output(ctx, p.r, "podman", "container", "inspect", p.name, "--format", "{{.State.Running}}")
		if strings.TrimSpace(out) != "true" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	log := p.log()
	p.stop(t)
	t.Fatalf("cs-vcr %s never answered on %s inside %s\n%s", p.mode, vcrAdmin, p.name, log)
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
		ctx := context.Background()
		// SIGINT, then wait for the process to write its summary before the
		// log is read and the container goes.
		//
		// The wait is BOUNDED. `podman wait` has no deadline of its own, so a
		// recorder that did not take the signal would hold the tier until go
		// test's own timeout — a wedge whose message is about the whole package
		// rather than about the recorder. Past the bound the log is read anyway
		// and `rm -f` ends it, which costs the accounting and keeps everything
		// else.
		_, _ = p.r.Run(ctx, run.Opts{}, "podman", "kill", "--signal", "INT", p.name)
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := p.r.Run(wctx, run.Opts{}, "podman", "wait", "--condition", "exited", p.name); err != nil {
			t.Logf("cs-vcr %s did not exit within 30s of SIGINT; reading what it wrote and removing it", p.mode)
		}
		cancel()
		p.final = p.readLog()
		_, _ = p.r.Run(ctx, run.Opts{}, "podman", "rm", "-f", p.name)
		if err := os.WriteFile(filepath.Join(p.diag, "cs-vcr.log"), []byte(p.final), 0o600); err == nil {
			t.Logf("cs-vcr %s log: %s", p.mode, filepath.Join(p.diag, "cs-vcr.log"))
		}
		if dumped, err := filepath.Glob(filepath.Join(p.diag, "[0-9]*.json")); err == nil && len(dumped) > 0 {
			t.Logf("cs-vcr dumped %d missed request(s) in %s — propose rules with: cs-vcr calibrate <cassette> %s",
				len(dumped), p.diag, p.diag)
		}
	}
	return p.final
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
	for line := range strings.SplitSeq(p.log(), "\n") {
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
// Reported with a ceiling rather than asserted at zero, and the difference is
// three hosts of evidence. A miss on the turn carrying the QUESTION cannot pass
// here: the agent has no answer to print, and the case fails on its word before
// this is reached. What can pass is a miss on a bookkeeping call, and whether a
// client makes one, how many times, and on which model are not properties of
// the code under test.
//
// Zero held on Linux and on arm64 macOS, at 29 of 29. It did not hold on Intel
// macOS, which asked 31 and missed 2, with all fourteen cases passing and
// nothing spent. Asserting zero on the first two turned the third red for a
// difference between one host and another, which is the failure this tier is
// least entitled to report.
//
// So the number is watched rather than enforced. A quarter of the session is a
// ceiling nothing legitimate approaches: bookkeeping is one or two calls beside
// a turn, and a cassette that has genuinely stopped matching misses nearly
// everything.
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
	// What each miss was, in the test's own output. The proxy's whole log goes
	// to a file under .tmp, which no CI job keeps, so without this a failure
	// here arrives as a number and nothing else. Measured: an Intel macOS run
	// reported two misses and gave a reader no way to learn what they were.
	for _, line := range missLines(summary) {
		t.Log(line)
	}
	if misses > served/4 {
		t.Errorf("replay missed %d of %d request(s), which is more than bookkeeping accounts for: "+
			"the cassettes and this run are asking different things", misses, served+misses)
	}
}

// assertTheCassettesWereUsed fails a run whose cells reported success without
// the recorder answering a single request.
//
// The one statement that catches a cell passing for a reason that has nothing
// to do with what it tests. A replayed turn IS a sequence of requests to this
// recorder; if none arrived, the guest never reached it, and whatever made the
// assertion pass came from somewhere else. That is not a hypothetical: fourteen
// microVM cells reported PASS on a run where every one of them served zero,
// because the failure text they were searched for quoted the prompt back.
//
// Keyed on cells that actually RAN rather than on cassettes present, because CI
// gives each cell its own job and narrows with -run: thirteen of the fourteen
// are filtered out there, and counting cassettes would fail every job.
func assertTheCassettesWereUsed(t *testing.T, p *vcrProxy, cells int64) {
	t.Helper()
	if cells == 0 {
		return // nothing ran: -run selected none, and the tier says so elsewhere
	}
	summary := p.stop(t)
	served, err := countedAs(summary, "replayed")
	if err != nil {
		return // reportMisses owns that complaint
	}
	if served == 0 {
		t.Errorf("%d cell(s) ran and the recorder answered nothing at all.\n"+
			"A replayed turn is requests to this recorder, so zero means the guest never "+
			"reached it and the cells passed for some other reason.", cells)
	}
}

// missLines are cs-vcr's own miss reports, one per line, with the escaped
// newlines it logs them with turned back into breaks so a reader can read them.
func missLines(summary string) []string {
	var out []string
	for line := range strings.SplitSeq(summary, "\n") {
		if !strings.Contains(line, "cassette miss") {
			continue
		}
		out = append(out, strings.ReplaceAll(strings.TrimSpace(line), `\n`, "\n        "))
	}
	return out
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

// recordingClaimName is what a cassette was recorded FROM, written at the
// cassette's own root beside cassette.yaml.
//
// Inside a directory cs-vcr owns, which was checked rather than assumed:
// `cassette ls` stats cassette.yaml to decide what is a cassette, `prune` walks
// only req/ and resp/, and `verify` reads the index. None of them enumerates
// the root, so a file cs-vcr did not write is one it does not see.
//
// The alternative was a sibling file next to the directory, and the record path
// decides against it: re-recording does RemoveAll on the cassette directory, so
// a claim inside it is destroyed and rewritten with the cassette it describes
// and cannot be left behind describing one that is gone.
const recordingClaimName = "recorded.json"

// The two outcomes a claim carries. Written started before the first request
// and settled once the run has come back, so a recording that died on the way
// leaves a claim that says so — absence proves nothing, because a cassette
// recorded before this file existed has no claim either.
const (
	recordingStarted = "in-progress"
	recordingSettled = "recorded"
)

// recordingClaim is which agent CLI recorded this cassette, at which version,
// and whether the recording finished.
//
// The version is the half that decides whether a replay can still pass. An
// agent CLI carries its own system prompt and tool list, so a bump rewrites
// every request the agent sends and the cassette misses on all of them at once.
// That failure reads as a dozen unrelated prompt diffs rather than as the one
// version that moved, and it costs a full tier of booted sandboxes to reach.
// Recorded here, the replay tier says it in one line before it boots anything.
//
// Shaped as campaign's test/cassettes/<scenario>/recorded.json, which came
// first and answers the same question about the same agents.
type recordingClaim struct {
	Case       string `json:"case"`
	CLI        string `json:"cli"`
	CLIVersion string `json:"cli_version"`
	Outcome    string `json:"outcome"`
	At         string `json:"at"`
}

func claimRecording(t *testing.T, store string, c liveCase, version string) {
	t.Helper()
	writeRecordingClaim(t, store, recordingClaim{
		Case: c.name(), CLI: c.cli, CLIVersion: version,
		Outcome: recordingStarted, At: time.Now().UTC().Format(time.RFC3339),
	})
}

func settleRecording(t *testing.T, store string, c liveCase, version string) {
	t.Helper()
	writeRecordingClaim(t, store, recordingClaim{
		Case: c.name(), CLI: c.cli, CLIVersion: version,
		Outcome: recordingSettled, At: time.Now().UTC().Format(time.RFC3339),
	})
}

func writeRecordingClaim(t *testing.T, store string, claim recordingClaim) {
	t.Helper()
	encoded, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		t.Fatalf("encode the recording claim: %v", err)
	}
	path := filepath.Join(store, claim.Case, recordingClaimName)
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write the recording claim: %v", err)
	}
}

// readRecordingClaim reads a cassette's claim, and reports false for one that
// has none. Those predate the file, and refusing them would fail on fixtures
// that replay perfectly well.
func readRecordingClaim(t *testing.T, store string, c liveCase) (recordingClaim, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store, c.name(), recordingClaimName))
	if err != nil {
		return recordingClaim{}, false
	}
	var claim recordingClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatalf("unreadable %s for %s: %v", recordingClaimName, c.name(), err)
	}
	return claim, true
}

// agentCLIVersions asks the IMAGE which version of each agent CLI it carries.
//
// The image rather than the pins in image/Containerfile.agents, and the
// difference between the two is the whole reason this exists: the pins move in
// one commit and the tier that carries them is published by another, so a
// checkout routinely names versions its image does not hold yet. What a
// cassette was recorded against is what booted, never what was pinned.
//
// One container for all three, because it costs a second and the alternative
// costs three. A CLI the image does not carry gets an empty string rather than
// an error: each case compares only the one it runs.
func agentCLIVersions(t *testing.T, r *run.Exec, img string) map[string]string {
	t.Helper()
	const probe = `for a in claude codex opencode; do ` +
		`v=$("$a" --version 2>/dev/null | head -1); printf '%s %s\n' "$a" "$v"; done`
	res, err := r.Run(context.Background(), run.Opts{ReadOnly: true},
		"podman", "run", "--rm", "--pull=never", "--entrypoint", "sh", img, "-c", probe)
	if err != nil {
		t.Fatalf("ask %s which agent CLIs it carries: %v\n%s", img, err, tail(res.Stderr, 400))
	}
	// The first dotted triple on the line. The three say it differently —
	// `2.1.258 (Claude Code)`, `codex-cli 0.152.1`, a bare `1.18.22` — and the
	// number is the shape they agree on.
	semver := regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)
	out := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(res.Stdout), "\n") {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		out[name] = semver.FindString(rest)
	}
	return out
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
// dir rather than t.TempDir(): the recorder container mounts this file, and on
// macOS a container can only mount what is under $HOME. The diagnostics
// directory is inside the repository, which is somewhere a container can always
// reach.
func writeVCRConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "cs-vcr.yaml")
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

	// The account's NUMBER, which travels separately from its name and is not
	// covered by blanking the name.
	//
	// Claude Code puts its scratchpad at /tmp/claude-<uid>/…, and a sandbox
	// mirrors the launching account's uid as well as its name. So a cassette
	// recorded by uid 1000 says claude-1000 and one replayed by uid 1001 says
	// claude-1001, in one character, 5780 bytes into a 10321-byte system prompt
	// that is otherwise identical. Everything else about the path was already
	// blanked -- the home directory, the session -- which is what made this the
	// last thing left and the hardest to see.
	//
	// Found the only way it could be: a GitHub runner replayed as uid 1001
	// where this machine recorded as 1000, both claude-login cells missed, and
	// the request cs-vcr dumped differed from the recording by that digit.
	uid := regexp.QuoteMeta(strconv.Itoa(os.Getuid()))

	// Under `extend`, which appends to the ruleset cs-vcr ships. The same names
	// directly under `normalize` would stand in for it, and the shipped rules
	// are what blank the date, the working directory, the platform and the
	// per-run scratchpad path. Losing any of them makes every request of every
	// session miss.
	body := fmt.Sprintf(`# Written by internal/cli/agentmatrix_test.go. Not committed.
normalize:
  extend:
    volatile:
      # What the account says a tool does, which is the world's answer and not
      # the agent's decision.
      #
      # Claude Code asks its account for managed settings and takes the tool
      # descriptions it is given. A recording made under a real login gets the
      # account's; a replay presents a fabricated one, the settings call is
      # refused, and the client falls back to its built-in text. The request
      # then differs in prose nobody chose, on a path the model never acts on.
      #
      # Only the description. The tool NAMES and their schemas stay exact,
      # because which tools an agent is offered is a decision and a difference
      # there is a real one.
      #
      # It shows up in claude-login-shared alone: that is the one case whose
      # guest presents the login itself, real when recording and fabricated
      # when replaying. A lent guest holds a loan token in both halves and
      # behaves identically.
      - 'tools[].description'
    capture:
      - pattern: '(?:/home/|-home-)(%[1]s)'
        as: '<USER>'
      - pattern: '(%[1]s %[1]s)'
        as: '<USER_GROUP>'
      - pattern: 'claude-(%[2]s)'
        as: '<USER_UID>'
`, user, uid)
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
