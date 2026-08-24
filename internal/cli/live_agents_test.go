//go:build live_agents

// The credential matrix, against real providers.
//
// Every supported agent and credential combination, shared and lent, each one
// driving a real agent inside a real sandbox and asking a real model to say one
// word. It is the only tier that proves a credential this tool fabricated is one
// the provider accepts, and the only one that would catch the day a client
// changes which code path a credential puts it on.
//
//	make test-live-agents
//
// Deliberately outside CI, outside `make check` and outside test-integration: it
// spends money and quota on every run, and a provider being slow or rate-limiting
// is not a defect in this repository.
//
// Credentials come from .env at the repository root, which is git-ignored, and
// never from the developer's own profiles. The suite builds a throwaway agent
// home and points CS_SANDBOX_AGENT_HOME at it, so a lent or shared key is one
// this file put there. Host LOGINS are the exception: a login cannot be
// fabricated, so those members read the real profile through a symlink and skip
// themselves when it is absent.
package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/paths"
)

// pong is what every member asks the model for. One word, so the assertion is
// about the credential rather than about the model.
const (
	pongPrompt = "Reply with the single word: pong"
	pongWord   = "pong"
)

// Model ids are pinned rather than defaulted, so a member that fails says
// something about the credential path instead of about a model that moved. Each
// is the cheapest one its provider offers that these clients can address.
const (
	anthropicModel = "anthropic/claude-fable-5"
	openaiModel    = "openai/gpt-5-nano"
	codexAPIModel  = "gpt-5-nano"
	// The image already pins a Fireworks model, and the Fireworks members use it
	// rather than overriding: that is the path a lent Fireworks key travels, via
	// OPENCODE_BASE_URL and the pinned model's own provider.
)

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

// liveAgentHome builds the throwaway host profile this tier lends and shares
// from, and points cs-sandbox at it.
//
// Keys are written from .env. Logins are symlinked from the developer's real
// profiles, because a login is the one credential this suite cannot fabricate;
// the members that need one skip when it is absent.
func liveAgentHome(t *testing.T, env map[string]string) string {
	t.Helper()
	home := t.TempDir()
	keys := lend.KeysDir(home)
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	for provider, variable := range map[string]string{
		"anthropic": "ANTHROPIC_API_KEY",
		"openai":    "OPENAI_API_KEY",
		"fireworks": "FIREWORKS_API_KEY",
	} {
		v := env[variable]
		if v == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(keys, provider), []byte(v), 0o600); err != nil {
			t.Fatal(err)
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
	t.Setenv("CS_SANDBOX_AGENT_HOME", home)
	return home
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// hostLogin reports whether the developer's real profile holds a login for this
// agent, which is what the inherit and lend members of that agent need.
func hostLogin(t *testing.T, agent string) bool {
	t.Helper()
	real, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	file := map[string]string{"claude": ".credentials.json", "codex": "auth.json"}[agent]
	return fileExists(filepath.Join(real, ".cs-"+agent, file))
}

// startLiveLender runs a lender in this process, on an address a sandbox can
// reach, with its slots pointed at the real providers.
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

// liveCase is one cell of the matrix.
type liveCase struct {
	name  string   // and the sandbox's name suffix
	flags []string // what create is asked for
	run   string   // what to run inside the sandbox
	// key is the .env variable this member needs, or "" when it needs a login.
	key string
	// login is the host agent whose real profile this member needs, or "".
	login string
}

// The matrix. Every supported pairing of an agent with a credential, in both
// modes: SHARED (--inherit-…, the real credential is copied in) and LENT
// (--lend-…, the sandbox holds a fabrication and the host holds the credential).
func liveCases() []liveCase {
	claude := `cd ~ && cs-claude -p ` + shellQuote(pongPrompt)
	codexSub := `cd ~ && cs-codex exec --skip-git-repo-check ` + shellQuote(pongPrompt)
	codexKey := `cd ~ && cs-codex exec --skip-git-repo-check -m ` + codexAPIModel + ` ` + shellQuote(pongPrompt)
	opencode := func(model string) string {
		m := ""
		if model != "" {
			m = "-m " + model + " "
		}
		return `cd ~ && cs-opencode run ` + m + shellQuote(pongPrompt)
	}
	return []liveCase{
		// An agent login, which only its own agent can spend.
		{"claude-login-lent", []string{"--lend-agent-login", "claude"}, claude, "", "claude"},
		{"claude-login-shared", []string{"--inherit-agent-login", "claude"}, claude, "", "claude"},
		{"codex-login-lent", []string{"--lend-agent-login", "codex"}, codexSub, "", "codex"},
		{"codex-login-shared", []string{"--inherit-agent-login", "codex"}, codexSub, "", "codex"},

		// An Anthropic key, which Claude Code and OpenCode can both spend.
		{"claude-anthropic-lent", []string{"--lend-api-key", "anthropic"}, claude, "ANTHROPIC_API_KEY", ""},
		{"claude-anthropic-shared", []string{"--inherit-api-key", "anthropic"}, claude, "ANTHROPIC_API_KEY", ""},
		{"opencode-anthropic-lent", []string{"--lend-api-key", "anthropic"}, opencode(anthropicModel), "ANTHROPIC_API_KEY", ""},
		{"opencode-anthropic-shared", []string{"--inherit-api-key", "anthropic"}, opencode(anthropicModel), "ANTHROPIC_API_KEY", ""},

		// An OpenAI key, which Codex and OpenCode can both spend.
		{"codex-openai-lent", []string{"--lend-api-key", "openai"}, codexKey, "OPENAI_API_KEY", ""},
		{"codex-openai-shared", []string{"--inherit-api-key", "openai"}, codexKey, "OPENAI_API_KEY", ""},
		{"opencode-openai-lent", []string{"--lend-api-key", "openai"}, opencode(openaiModel), "OPENAI_API_KEY", ""},
		{"opencode-openai-shared", []string{"--inherit-api-key", "openai"}, opencode(openaiModel), "OPENAI_API_KEY", ""},

		// A Fireworks key, which only OpenCode reaches, and only through the
		// pinned model's own provider. No -m: this is the path OPENCODE_BASE_URL
		// governs, and the image already pins a Fireworks model.
		{"opencode-fireworks-lent", []string{"--lend-api-key", "fireworks"}, opencode(""), "FIREWORKS_API_KEY", ""},
		{"opencode-fireworks-shared", []string{"--inherit-api-key", "fireworks"}, opencode(""), "FIREWORKS_API_KEY", ""},
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestLiveAgentCredentialMatrix drives every combination end to end.
func TestLiveAgentCredentialMatrix(t *testing.T) {
	env := liveEnv(t)
	r, host := liveSetup(t)
	home := liveAgentHome(t, env)
	startLiveLender(t, home)

	for _, c := range liveCases() {
		t.Run(c.name, func(t *testing.T) {
			if c.key != "" && env[c.key] == "" {
				t.Skipf(".env has no %s", c.key)
			}
			if c.login != "" && !hostLogin(t, c.login) {
				t.Skipf("no host %s login to share or lend", c.login)
			}
			name := boxName(strings.ReplaceAll(c.name, "-", ""))
			t.Cleanup(func() { _, _ = execRoot(t, "destroy", name, "-f") })
			out := createBox(t, r, name, c.flags...)
			if !strings.Contains(out, "lent:") && !strings.Contains(out, "agent login:") &&
				!strings.Contains(out, "api key:") {
				t.Fatalf("create reported no credential:\n%s", out)
			}

			step(t, "asking the model, through %s…", strings.Join(c.flags, " "))
			got := stripANSI(inBox(context.Background(), r, host, name, c.run))
			if !strings.Contains(strings.ToLower(got), pongWord) {
				t.Fatalf("the model did not answer through this credential.\ncommand: %s\noutput:\n%s", c.run, tail(got, 900))
			}
		})
	}
}

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
