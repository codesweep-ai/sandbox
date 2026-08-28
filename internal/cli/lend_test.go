package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// lendHome writes a host home holding the credentials the flags name.
func lendHome(t *testing.T) string {
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
	put(".cs-claude/.credentials.json",
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"real","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli()))
	put(".cs-codex/auth.json", `{"tokens":{"access_token":"real","account_id":"acct"}}`)
	put(".cs-keys/anthropic", "the-hosts-real-key\n")
	put(".cs-keys/openai", "the-hosts-real-openai-key\n")
	put(".cs-keys/fireworks", "the-hosts-real-fireworks-key\n")
	return home
}

func lendApp(t *testing.T, home string) *App {
	t.Helper()
	return &App{
		Host:    hostenv.Host{Home: home},
		InstDir: t.TempDir(),
		TierDir: t.TempDir(),
		Runner:  run.NewFake(),
		errW:    &bytes.Buffer{},
	}
}

// A flag that names something the host cannot supply fails at create, not at
// the first model call from inside a sandbox — where it surfaces as the agent
// claiming to be signed out.
func TestLendFlagsFailBeforeAnythingIsProvisioned(t *testing.T) {
	cases := []struct {
		name  string
		flags createFlags
		want  string
	}{
		{"unknown agent", createFlags{lendAgentLogin: []string{"emacs"}}, "unknown agent"},
		{"unknown provider", createFlags{lendAPIKey: []string{"acme"}}, "unknown provider"},
		{"a key slot is not a login", createFlags{lendAgentLogin: []string{"anthropic"}}, "unknown agent"},
		{"a login slot is not a key", createFlags{lendAPIKey: []string{"claude"}}, "unknown provider"},
		{
			"inherit and lend the same login",
			createFlags{inheritAgentLogin: []string{"claude"}, lendAgentLogin: []string{"claude"}},
			"opposite things",
		},
		{
			"two slots driving one agent",
			createFlags{lendAgentLogin: []string{"claude"}, lendAPIKey: []string{"anthropic"}},
			"both drive ANTHROPIC_BASE_URL",
		},
		{
			"a key copied in while the same agent is lent one",
			createFlags{lendAgentLogin: []string{"claude"}, inheritAPIKey: []string{"anthropic"}},
			"pick one",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := lendApp(t, lendHome(t))
			_, err := app.resolveLoans(&c.flags, "box", "")
			if err == nil {
				t.Fatalf("resolveLoans(%+v) succeeded, want an error mentioning %q", c.flags, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err, c.want)
			}
		})
	}
}

// The remedy is the useful half: a host with no key to lend is told the one
// command that gives it one.
func TestLendingAKeyTheHostDoesNotHaveNamesTheRemedy(t *testing.T) {
	app := lendApp(t, t.TempDir()) // an empty home
	_, err := app.resolveLoans(&createFlags{lendAPIKey: []string{"anthropic"}}, "box", "")
	if err == nil {
		t.Fatal("lending a key that does not exist should fail")
	}
	for _, want := range []string{".cs-keys/anthropic", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// A --env that shadowed a lent credential would leave the sandbox holding
// nothing while create reported a loan in place.
func TestEnvCannotShadowALentCredential(t *testing.T) {
	_, err := mergeLoanEnv("ANTHROPIC_AUTH_TOKEN=mine\n", []string{"ANTHROPIC_AUTH_TOKEN=loan_x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("merge = %v, want a collision error", err)
	}
	block, err := mergeLoanEnv("FOO=bar\n", []string{"ANTHROPIC_BASE_URL=http://lender", "ANTHROPIC_AUTH_TOKEN=loan_x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FOO=bar", "ANTHROPIC_BASE_URL=http://lender", "ANTHROPIC_AUTH_TOKEN=loan_x"} {
		if !strings.Contains(block, want) {
			t.Errorf("merged block missing %q:\n%s", want, block)
		}
	}
}

// A base URL a loan took over is dropped rather than refused, and the lender's
// own address takes the name. The sandbox must not keep the value: it is where
// the LENDER forwards, and a sandbox that reached it directly would be holding
// a token that host does not honour.
func TestAConsumedBaseURLLeavesTheSandbox(t *testing.T) {
	block, err := mergeLoanEnv(
		"FOO=bar\nANTHROPIC_BASE_URL=http://recorder:8080/c/anthropic/build\n",
		[]string{"ANTHROPIC_BASE_URL=http://lender:2500"},
		[]string{"ANTHROPIC_BASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(block, "recorder") {
		t.Errorf("the sandbox kept the upstream the caller named:\n%s", block)
	}
	for _, want := range []string{"FOO=bar", "ANTHROPIC_BASE_URL=http://lender:2500"} {
		if !strings.Contains(block, want) {
			t.Errorf("merged block missing %q:\n%s", want, block)
		}
	}
}

// A copied key is a credential the sandbox holds: it needs no lender, no token
// and no base URL, and the value must be the real one.
func TestInheritedKeyIsCopiedInWithoutALender(t *testing.T) {
	app := lendApp(t, lendHome(t))
	plan, err := app.resolveLoans(&createFlags{inheritAPIKey: []string{"anthropic"}}, "box", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.loans) != 0 {
		t.Errorf("copying a key minted %d loans, want 0", len(plan.loans))
	}
	if got := strings.Join(plan.env, " "); !strings.Contains(got, "ANTHROPIC_API_KEY=the-hosts-real-key") {
		t.Errorf("env = %q, want the real key", got)
	}
	if strings.Contains(strings.Join(plan.env, " "), "BASE_URL") {
		t.Errorf("a copied key should not redirect the agent: %q", plan.env)
	}
}

// The loans a sandbox holds are what `ls` and `inspect` read back, and the
// tokens must not be in either.
func TestLoansAreReportedWithoutTheirTokens(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, &state.Instance{
		Name: "box", Type: "agent", Engine: state.Podman, Port: 2200, Created: "2026-07-27T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	tok := "loan_box_claude_secretnonce"
	if err := lend.WriteLoans(state.Dir(dir, state.DefaultGroup, "box"), []lend.Loan{
		{Token: tok, Slot: "claude", Kind: lend.Login},
	}); err != nil {
		t.Fatal(err)
	}
	if got := loanSlots(dir, state.DefaultGroup, "box"); len(got) != 1 || got[0] != "claude" {
		t.Errorf("loanSlots = %v, want [claude]", got)
	}

	app := &App{InstDir: dir, TierDir: t.TempDir(), Runner: run.NewFake()}
	var buf bytes.Buffer
	if err := runLs(context.Background(), app, &buf, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "lent") {
		t.Errorf("ls should mark a borrowing sandbox:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), tok) {
		t.Errorf("ls printed a loan token:\n%s", buf.String())
	}

	buf.Reset()
	if err := runLsJSON(context.Background(), app, &buf); err != nil {
		t.Fatal(err)
	}
	var items []lsItem
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Loans) != 1 || items[0].Loans[0] != "claude" {
		t.Errorf("ls --json loans = %+v, want [claude]", items)
	}
	if strings.Contains(buf.String(), tok) {
		t.Errorf("ls --json printed a loan token:\n%s", buf.String())
	}
}

// The CREDS column is the difference between a sandbox that holds your
// credentials and one that only borrows them.
func TestCredsColumn(t *testing.T) {
	for _, c := range []struct {
		held, borrowed []string
		want           string
	}{
		{nil, nil, "-"},
		{[]string{"claude"}, nil, "held"},
		{nil, []string{"claude"}, "lent"},
		{[]string{"codex"}, []string{"anthropic"}, "held+lent"},
	} {
		if got := creds(c.held, c.borrowed); got != c.want {
			t.Errorf("creds(%v, %v) = %q, want %q", c.held, c.borrowed, got, c.want)
		}
	}
}

// Every hop of the lending chain fails the same way from inside a sandbox, so
// doctor has to name the one that is dark.
func TestDoctorReportsALoanWithNoLender(t *testing.T) {
	app := lendApp(t, lendHome(t))
	if err := state.Save(app.InstDir, &state.Instance{
		Name: "box", Type: "agent", Engine: state.Podman, Port: 2200, Created: "2026-07-27T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := lend.WriteLoans(state.Dir(app.InstDir, state.DefaultGroup, "box"), []lend.Loan{
		{Token: "loan_box_claude_1", Slot: "claude", Kind: lend.Login},
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing is listening on a port nothing was started on.
	t.Setenv("CS_SANDBOX_LEND_ADDR", "127.0.0.1:1")
	st := app.lendState()
	if st.Sandboxes != 1 {
		t.Errorf("doctor saw %d borrowing sandboxes, want 1", st.Sandboxes)
	}
	if st.Addr != "" {
		t.Errorf("doctor found a lender at %q, want none", st.Addr)
	}
	if len(st.Credentials) != 1 || st.Credentials[0].Slot != "claude" || st.Credentials[0].Err != "" {
		t.Errorf("doctor credentials = %+v, want claude readable", st.Credentials)
	}
}

// A dry run prints what it would do. Minting a credential and starting a daemon
// are both things it would do, so it must do neither.
func TestDryRunMintsNothingAndStartsNothing(t *testing.T) {
	app := lendApp(t, lendHome(t))
	app.Exec = &run.Exec{DryRun: true}
	t.Setenv("CS_SANDBOX_LEND_ADDR", "127.0.0.1:1") // nothing is listening there

	plan, err := app.resolveLoans(&createFlags{lendAPIKey: []string{"anthropic"}, blockSideCalls: true}, "box", "")
	if err != nil {
		t.Fatalf("a dry run should still resolve the plan: %v", err)
	}
	if len(plan.loans) != 1 {
		t.Fatalf("the plan should still describe the loan, got %d", len(plan.loans))
	}
	if _, _, alive := (lend.Daemon{Dir: app.InstDir}).Status(); alive {
		t.Error("a dry run started a lender")
	}
	if _, err := os.Stat(filepath.Join(app.InstDir, "lender")); err == nil {
		t.Error("a dry run recorded a lender it did not start")
	}
	// And the environment it reports is the environment a real run would seed.
	if got := strings.Join(plan.env, " "); !strings.Contains(got, "ANTHROPIC_BASE_URL=http://"+engine.HostReachableName+":") {
		t.Errorf("env = %q, want the address a real run would use", got)
	}
}

// A base URL the caller sets for a slot they are lending names where the lender
// forwards it, and the sandbox is handed the lender instead.
//
// This is the same trade the credential itself makes. The host's key is read at
// create and the sandbox is given a loan token in the variable that held it, so
// reading the base URL and giving back the lender's address is one shape rather
// than two. It is also what lets a recorder run somewhere only the host can
// reach, since nothing between the sandbox and the lender changes.
func TestALentSlotTakesTheBaseURLTheCallerNamed(t *testing.T) {
	app := lendApp(t, lendHome(t))
	app.Exec = &run.Exec{DryRun: true}
	t.Setenv("CS_SANDBOX_LEND_ADDR", "127.0.0.1:1")

	const upstream = "http://127.0.0.1:8080/c/anthropic/testcassette"
	plan, err := app.resolveLoans(&createFlags{lendAPIKey: []string{"anthropic"}}, "box",
		"ANTHROPIC_BASE_URL="+upstream+"\n")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.loans) != 1 || plan.loans[0].Origin != upstream {
		t.Fatalf("loan origin = %q, want %q", plan.loans[0].Origin, upstream)
	}
	// The sandbox is pointed at the lender, and the variable it was given is
	// the one it would have had without any of this.
	if !slices.Contains(plan.env, "ANTHROPIC_BASE_URL=http://"+engine.HostReachableName+":1") {
		t.Errorf("the sandbox was not pointed at the lender:\n%s", strings.Join(plan.env, "\n"))
	}
	if !slices.Contains(plan.consumed, "ANTHROPIC_BASE_URL") {
		t.Errorf("the variable was not taken over: %v", plan.consumed)
	}
	if notes := strings.Join(plan.notes, "\n"); !strings.Contains(notes, upstream) {
		t.Errorf("create does not report where the traffic goes:\n%s", notes)
	}
}

// An address the lender could not forward to fails at create, not as a 502 on
// the sandbox's first model call.
func TestALentSlotsBaseURLMustBeAnAddress(t *testing.T) {
	app := lendApp(t, lendHome(t))
	app.Exec = &run.Exec{DryRun: true}
	t.Setenv("CS_SANDBOX_LEND_ADDR", "127.0.0.1:1")

	for _, bad := range []string{"api.anthropic.com", "://nonsense", "https://", "file:///etc/passwd"} {
		_, err := app.resolveLoans(&createFlags{lendAPIKey: []string{"anthropic"}}, "box",
			"ANTHROPIC_BASE_URL="+bad+"\n")
		if err == nil {
			t.Errorf("--env ANTHROPIC_BASE_URL=%q was accepted", bad)
		}
	}
}

// A variable belonging to a slot this sandbox does not borrow is left alone. It
// is the caller's to set, and nothing about it is the lender's business.
func TestAnUnlentBaseURLIsNotTakenOver(t *testing.T) {
	app := lendApp(t, lendHome(t))
	app.Exec = &run.Exec{DryRun: true}
	t.Setenv("CS_SANDBOX_LEND_ADDR", "127.0.0.1:1")

	plan, err := app.resolveLoans(&createFlags{lendAPIKey: []string{"anthropic"}}, "box",
		"OPENAI_BASE_URL=http://somewhere.test\n")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.consumed) != 0 {
		t.Errorf("consumed %v, want nothing: openai is not lent here", plan.consumed)
	}
	if plan.loans[0].Origin != "" {
		t.Errorf("the anthropic loan took another slot's variable: %q", plan.loans[0].Origin)
	}
}

// A key reaches clients that disagree about what to read, and a provider that
// has no base-URL variable of its own borrows a client's. Both are cheap to get
// wrong and expensive to notice: the sandbox looks configured either way, and
// the provider answers 401.
func TestAKeySeedsEveryVariableItsClientsRead(t *testing.T) {
	home := lendHome(t)
	for _, c := range []struct {
		provider string
		want     []string
	}{
		{"anthropic", []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY="}},
		// Codex reads CODEX_API_KEY and attaches no header for OPENAI_API_KEY.
		{"openai", []string{"OPENAI_BASE_URL=", "OPENAI_API_KEY=", "CODEX_API_KEY="}},
		// Fireworks has no variable of its own; OpenCode's points it.
		{"fireworks", []string{"OPENCODE_BASE_URL=", "FIREWORKS_API_KEY="}},
	} {
		t.Run(c.provider, func(t *testing.T) {
			app := lendApp(t, home)
			app.Exec = &run.Exec{DryRun: true} // resolve the plan, start nothing
			t.Setenv("CS_SANDBOX_LEND_ADDR", "127.0.0.1:1")
			plan, err := app.resolveLoans(&createFlags{lendAPIKey: []string{c.provider}}, "box", "")
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(plan.env, "\n")
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("a lent %s key set no %s\n%s", c.provider, want, got)
				}
			}
		})
	}
}
