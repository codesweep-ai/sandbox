package cli

import (
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func faultApp(t *testing.T, loans ...lend.Loan) *App {
	t.Helper()
	app := lendApp(t, t.TempDir())
	// The root command takes the instances root from the environment, whatever
	// the App was built with.
	t.Setenv("CS_SANDBOX_INSTANCES_DIR", app.InstDir)
	if err := state.Save(app.InstDir, &state.Instance{
		Name: "box", Type: "agent", Engine: state.Podman, Port: 2200, Created: "2026-07-27T10:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if len(loans) > 0 {
		if err := lend.WriteLoans(state.Dir(app.InstDir, state.DefaultGroup, "box"), loans); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

// TestArmingAFaultWritesWhatTheLenderReads: the command and the lender meet at one file, so
// the contract is that file: where it is, and that a second arming adds to the first. The
// command talks to no lender and runs nothing, which is what keeps a fault out of reach of
// anything but the host.
func TestArmingAFaultWritesWhatTheLenderReads(t *testing.T) {
	app := faultApp(t, lend.Loan{Token: "loan_box_openai_x", Slot: "openai", Kind: lend.Key})
	f, err := runRoot(t, app, "lender", "fault", "box", "--status", "429", "--count", "3", "--retry-after", "12")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, app, "lender", "fault", "box", "--hang", "90s", "--slot", "openai"); err != nil {
		t.Fatal(err)
	}
	got, err := lend.ReadFaults(state.Dir(app.InstDir, state.DefaultGroup, "box"))
	if err != nil || len(got) != 2 {
		t.Fatalf("armed faults = %+v, %v; want two", got, err)
	}
	if got[0].Status != 429 || got[0].Count != 3 || got[0].RetryAfter != 12 || got[0].ID == "" || got[0].ID == got[1].ID {
		t.Errorf("first fault = %+v", got[0])
	}
	if got[1].Hang != "90s" || got[1].Slot != "openai" || got[1].Status != 0 {
		t.Errorf("second fault = %+v", got[1])
	}
	if calls := f.Rendered(); len(calls) != 0 {
		t.Errorf("arming a fault ran something:\n%s", strings.Join(calls, "\n"))
	}

	if _, err := runRoot(t, app, "lender", "fault", "box", "--clear"); err != nil {
		t.Fatal(err)
	}
	if got, _ := lend.ReadFaults(state.Dir(app.InstDir, state.DefaultGroup, "box")); len(got) != 0 {
		t.Errorf("--clear left %+v", got)
	}
}

// TestAFaultIsRefusedWhereNoLenderWouldServeIt: a fault on a sandbox that borrows nothing is
// never answered by anyone. The test it was armed for then runs against the real provider
// and passes, which is the worst outcome a fault can have.
func TestAFaultIsRefusedWhereNoLenderWouldServeIt(t *testing.T) {
	_, err := runRoot(t, faultApp(t), "lender", "fault", "box", "--status", "429")
	if err == nil || !strings.Contains(err.Error(), "borrows no credential") {
		t.Fatalf("err = %v; want a refusal that names the missing loan", err)
	}
	app := faultApp(t, lend.Loan{Token: "loan_box_openai_x", Slot: "openai", Kind: lend.Key})
	_, err = runRoot(t, app, "lender", "fault", "box", "--status", "429", "--slot", "anthropic")
	if err == nil || !strings.Contains(err.Error(), "does not borrow anthropic") {
		t.Fatalf("err = %v; want a refusal that names the slot", err)
	}
}
