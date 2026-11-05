package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// budgetLoans are what a lent sandbox created with token budgets holds: a loan
// that lends, and the meter loan beside it.
var budgetLoans = []lend.Loan{
	{Token: "loan_box_openai_x", Slot: "openai", Kind: lend.Key},
	{Token: "loan_box_meter_x", Kind: lend.MeterLoan},
}

// runOut runs the command tree against f and returns what it printed.
func runOut(t *testing.T, app *App, f *run.Fake, args ...string) (string, error) {
	t.Helper()
	app.Runner = f
	if app.errW == nil {
		app.errW = io.Discard
	}
	var out bytes.Buffer
	root := newRootCmd(app)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(io.Discard)
	err := root.Execute()
	return out.String(), err
}

// writeUsage writes the lender's record for box, as the lender would.
func writeUsage(t *testing.T, app *App, u lend.Usage) string {
	t.Helper()
	p := lend.UsagePath(app.InstDir, state.DefaultGroup, "box", lend.MeterID(budgetLoans))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(u)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func boxBudget(t *testing.T, app *App) lend.Budget {
	t.Helper()
	b, err := lend.ReadBudget(state.Dir(app.InstDir, state.DefaultGroup, "box"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A limit is per token type. A bare number would be one total, which on this
// host's sessions was 98.8% cached input and said little about the rest, so it
// is refused with the types named.
func TestATokenBudgetIsReadByType(t *testing.T) {
	got, err := parseTokenBudget("--token-budget", "input=20M, cached=1G,output=1.5k")
	if err != nil || got["input"] != 20_000_000 || got["cached"] != 1_000_000_000 || got["output"] != 1500 || len(got) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
	for in, want := range map[string]string{
		"1G":                     "name the token type",
		"tokens=5M":              "unknown token type",
		"cached=1G,cached=2G":    "given twice",
		"input=-1":               "a number of tokens",
		"output=lots":            "a number of tokens",
		"":                       "at least one",
		"input=20M,,output=5MM":  "a number of tokens",
		"cached=1000000000000GG": "a number of tokens",
	} {
		if _, err := parseTokenBudget("--token-budget", in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want one saying %q", in, err, want)
		}
	}
	for in, want := range map[string]float64{"90%": 0.9, "0.5": 0.5, "99.5%": 0.995} {
		if got, err := parseAskAt(in); err != nil || got != want {
			t.Errorf("--ask-at %s = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "100%", "1", "150%", "most"} {
		if _, err := parseAskAt(in); err == nil {
			t.Errorf("--ask-at %s was accepted", in)
		}
	}
}

// create refuses a budget it cannot hold before anything is provisioned: one on
// a sandbox that borrows nothing would look like a boundary it is not.
func TestCreateRefusesABudgetItCannotHold(t *testing.T) {
	for _, c := range []struct {
		name string
		f    createFlags
		want string
	}{
		{"no lend flag", createFlags{tokenBudget: "cached=1G"}, "borrows nothing"},
		{"--hard alone", createFlags{hardBudget: true, lendAPIKey: []string{"openai"}}, "none was given"},
		{"--ask-at on a soft budget", createFlags{tokenBudget: "cached=1G", askAt: "90%", lendAPIKey: []string{"openai"}}, "add --hard"},
	} {
		if _, err := createBudget(&c.f); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one saying %q", c.name, err, c.want)
		}
	}
	b, err := createBudget(&createFlags{tokenBudget: "cached=200M,output=2M", hardBudget: true, askAt: "90%",
		lendAgentLogin: []string{"codex"}})
	if err != nil || !b.Hard || b.AskAt != 0.9 || b.Limits["cached"] != 200_000_000 || len(b.History) != 1 {
		t.Fatalf("budget = %+v, %v", b, err)
	}
	if b, _ := createBudget(&createFlags{}); !b.Empty() || b.Hard {
		t.Errorf("no budget flags gave %+v", b)
	}
}

// `budget` writes the file the lender reads on the next call, under a lock of
// the sandbox's own, and keeps a history so a top-up can be accounted for.
// With no lender running it writes anyway: the budget applies when one starts.
func TestBudgetChangesTheFileTheLenderReads(t *testing.T) {
	app := faultApp(t, budgetLoans...)
	for _, step := range []struct {
		args  []string
		check func(lend.Budget) bool
	}{
		{[]string{"--set", "cached=1G,output=5M"}, func(b lend.Budget) bool {
			return b.Limits["cached"] == 1e9 && b.Limits["output"] == 5e6 && !b.Hard
		}},
		{[]string{"--add", "cached=250M"}, func(b lend.Budget) bool { return b.Limits["cached"] == 1.25e9 }},
		{[]string{"--clear=output"}, func(b lend.Budget) bool { _, ok := b.Limits["output"]; return !ok && len(b.Limits) == 1 }},
		{[]string{"--hard", "--ask-at", "90%"}, func(b lend.Budget) bool { return b.Hard && b.AskAt == 0.9 }},
		{[]string{"--soft"}, func(b lend.Budget) bool { return !b.Hard && b.AskAt == 0 }},
		{[]string{"--clear"}, func(b lend.Budget) bool { return b.Empty() && len(b.History) == 6 }},
		// Clearing a hard budget's every limit removes it, mode and all: measured live,
		// where it once refused to.
		{[]string{"--set", "output=2M", "--hard"}, func(b lend.Budget) bool { return b.Hard }},
		{[]string{"--clear"}, func(b lend.Budget) bool { return b.Empty() && !b.Hard && b.AskAt == 0 }},
	} {
		if _, err := runOut(t, app, run.NewFake(), append([]string{"budget", "box"}, step.args...)...); err != nil {
			t.Fatalf("budget %v: %v", step.args, err)
		}
		if b := boxBudget(t, app); !step.check(b) {
			t.Errorf("after budget %v the file holds %+v", step.args, b)
		}
	}
	for args, want := range map[string]string{
		"--add input=1M":       "no limit to raise",
		"--hard --soft":        "opposite things",
		"--ask-at 90%":         "add --hard",
		"--set tokens=1M":      "unknown token type",
		"--clear=input,tokens": "unknown token type",
	} {
		_, err := runOut(t, app, run.NewFake(), append([]string{"budget", "box"}, strings.Fields(args)...)...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("budget box %s: err = %v, want one saying %q", args, err, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(state.Dir(app.InstDir, state.DefaultGroup, "box"), lend.BudgetFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("budget.json: %v %v", fi, err)
	}
}

// A sandbox that borrows nothing has no lender to meter it, so `budget` and
// `usage` both refuse it rather than report a budget nothing applies.
func TestABudgetNeedsALentSandbox(t *testing.T) {
	app := faultApp(t)
	for _, args := range [][]string{{"budget", "box", "--set", "cached=1G"}, {"usage", "box"}} {
		if _, err := runOut(t, app, run.NewFake(), args...); err == nil || !strings.Contains(err.Error(), "borrows nothing") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

// A running lender older than token budgets would ignore the file, which reads as
// a test that passed. So `budget` asks it what it enforces first, and names the
// command that replaces it.
func TestABudgetIsRefusedByALenderThatPredatesIt(t *testing.T) {
	app := faultApp(t, budgetLoans...)
	lender := func(health string) *run.Fake {
		return run.NewFake().OnStdout("container inspect", "true\n").OnStdout("/healthz", health)
	}
	_, err := runOut(t, app, lender(`{"ok":true,"service":"cs-sandbox lend"}`), "budget", "box", "--set", "cached=1G")
	if err == nil || !strings.Contains(err.Error(), "predates token budgets") || !strings.Contains(err.Error(), "cs-sandbox start") {
		t.Fatalf("err = %v, want a refusal naming cs-sandbox start", err)
	}
	if !boxBudget(t, app).Empty() {
		t.Error("the budget was written anyway")
	}
	if _, err := runOut(t, app, lender(`{"ok":true,"features":["meter"]}`), "budget", "box", "--set", "cached=1G", "--hard"); err == nil {
		t.Error("a hard budget was set against a lender that cannot refuse")
	}
	if _, err := runOut(t, app, lender(`{"ok":true,"features":["meter","refuse"]}`), "budget", "box", "--set", "cached=1G", "--hard"); err != nil {
		t.Errorf("a current lender refused the budget: %v", err)
	}
}

// `usage` reads the budget and the lender's record, and asks the lender
// nothing, so it answers for a stopped group too. A spent type says how far
// past its limit it went, and a budget the lender has not read since it
// changed says so rather than showing a figure that looks fine.
func TestUsageReportsWhatTheLenderRecorded(t *testing.T) {
	app := faultApp(t, budgetLoans...)
	b := lend.Budget{Limits: map[string]int64{"cached": 1e9, "output": 5e6}}
	if err := lend.WriteBudget(state.Dir(app.InstDir, state.DefaultGroup, "box"), b); err != nil {
		t.Fatal(err)
	}
	applied := lend.Budget{Limits: b.Limits}
	writeUsage(t, app, lend.Usage{
		Sandbox: "box", Calls: 2318, Estimated: 3, InFlight: 1, Applied: &applied,
		EstimatedTokens: lend.Tokens{Input: 100_000, CacheRead: 400_000, Output: 24_000},
		Rows: []lend.UsageRow{
			{Slot: "openai", Surface: "openai.responses", Model: "gpt-5.6-sol", Calls: 2201,
				Input: 4_400_000, CacheRead: 1_004_100_000, Output: 1_600_000},
			{Slot: "openai", Surface: "openai.responses", Model: "gpt-5.6-nano", Calls: 117,
				Input: 400_000, CacheRead: 200_000, Output: 100_000, Reasoning: 50_000},
		},
	})
	out, err := runOut(t, app, run.NewFake(), "usage", "box")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"box: soft budget 2318 calls · 1 in flight · 3 estimated",
		"spent, 4.30M past the limit",
		"output  1.70M",
		"gpt-5.6-nano",
		"3 calls estimated (aborted or unread): 100.0K input, 400.0K cached, 24.0K output",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not yet applied") {
		t.Errorf("an applied budget reads as not applied:\n%s", out)
	}

	// Raised on the host and not yet read by the lender.
	if err := lend.WriteBudget(state.Dir(app.InstDir, state.DefaultGroup, "box"),
		lend.Budget{Limits: map[string]int64{"cached": 2e9, "output": 5e6}}); err != nil {
		t.Fatal(err)
	}
	if out, _ := runOut(t, app, run.NewFake(), "usage", "box"); !strings.Contains(out, "not yet applied") {
		t.Errorf("a budget the lender has not read is not reported as such:\n%s", out)
	}
	if out, _ := runOut(t, app, run.NewFake(), "usage", "box", "--detail"); !strings.Contains(out, "reasoning") || !strings.Contains(out, "50.0K") {
		t.Errorf("--detail does not show the finer buckets:\n%s", out)
	}
	js, err := runOut(t, app, run.NewFake(), "usage", "--json")
	var counts []sandboxCount
	if err != nil || json.Unmarshal([]byte(js), &counts) != nil || len(counts) != 1 || counts[0].Usage.Calls != 2318 || !counts[0].Readable {
		t.Errorf("usage --json = %s, %v", js, err)
	}
}

// ls says how full a budget is only when some sandbox has one, by the type that
// is fullest, which is the one that stops it first.
func TestLsShowsTheFullestTypeWhenABudgetIsSet(t *testing.T) {
	app := faultApp(t, budgetLoans...)
	var out bytes.Buffer
	if err := runLs(t.Context(), app, &out, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "BUDGET") {
		t.Errorf("ls shows a budget column with no budget set:\n%s", out.String())
	}
	if err := lend.WriteBudget(state.Dir(app.InstDir, state.DefaultGroup, "box"),
		lend.Budget{Limits: map[string]int64{"cached": 1e9, "output": 10e6}}); err != nil {
		t.Fatal(err)
	}
	writeUsage(t, app, lend.Usage{Rows: []lend.UsageRow{{CacheRead: 590_000_000, Output: 1_000_000}}})
	out.Reset()
	if err := runLs(t.Context(), app, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "BUDGET") || !strings.Contains(out.String(), "cached 59%") {
		t.Errorf("ls does not show the fullest type:\n%s", out.String())
	}
}

// A record no live sandbox owns is removed, once it has gone unwritten for
// longer than a call that was in flight at destroy could take to settle
// (R155d). A live sandbox's record is never touched.
func TestTheSweepRemovesOnlyRecordsNoSandboxOwns(t *testing.T) {
	app := faultApp(t, budgetLoans...)
	live := writeUsage(t, app, lend.Usage{Calls: 1})
	dir := filepath.Dir(live)
	old := filepath.Join(dir, "gone.0123456789abcdef.json")
	fresh := filepath.Join(dir, "gone.fedcba9876543210.json")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * usageSweepAfter)
	for _, p := range []string{old, live} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	app.sweepUsage(state.DefaultGroup)
	for p, want := range map[string]bool{live: true, fresh: true, old: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

// destroy and rm say what the sandbox spent before the record goes with it. A
// budget belongs to one creation, so the next one starts at zero (R155c).
func TestTheFinalCountIsReadBeforeTheSandboxGoes(t *testing.T) {
	app := faultApp(t, budgetLoans...)
	p := writeUsage(t, app, lend.Usage{Calls: 7, Estimated: 1, InFlight: 1,
		Rows: []lend.UsageRow{{Input: 1_200_000, CacheRead: 30_000_000, Output: 400_000}}})
	in, err := state.Load(app.InstDir, state.DefaultGroup, "box")
	if err != nil {
		t.Fatal(err)
	}
	line, record := app.finalCount(in)
	if record != p || !strings.Contains(line, "input 1.20M · cached 30.00M · output 400.0K, over 7 calls (1 estimated, 1 in flight)") {
		t.Errorf("finalCount = %q, %q", line, record)
	}
	app.dropCount(state.DefaultGroup, record)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the record survived its sandbox: %v", err)
	}
}
