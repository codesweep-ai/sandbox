package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// startedEngine is an engine whose Start always works, and says it was asked.
type startedEngine struct {
	engine.Engine
	started []string
}

func (e *startedEngine) Start(_ context.Context, name string) error {
	e.started = append(e.started, name)
	return nil
}

// A sandbox that borrows a credential sends every HTTPS request through its
// group's lender, so a sandbox started without one has no git, no curl and no
// package manager, and nothing inside it says why. Only create used to bring a
// lender up. A host reboot, or a lender somebody stopped, then left a started
// sandbox pointing at a name that resolved to nothing.
func TestStartBringsBackTheLenderALoanNeeds(t *testing.T) {
	fake := run.NewFake() // the lender's container exists and is not running
	app := &App{InstDir: t.TempDir(), TierDir: t.TempDir(), Runner: fake}
	in := &state.Instance{Name: "box", Group: "team", Type: "agent", Engine: state.Podman}
	if err := state.Save(app.InstDir, in); err != nil {
		t.Fatal(err)
	}
	loans := []lend.Loan{{Token: "t", Label: "box/fireworks", Slot: "fireworks", Kind: lend.Key}}
	if err := lend.WriteLoans(state.Dir(app.InstDir, in.Group, in.Name), loans); err != nil {
		t.Fatal(err)
	}

	eng := &startedEngine{}
	if err := app.startInstance(context.Background(), eng, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	if want := "podman start " + lend.BoxName(state.NetworkName("team")); !fake.Contains(want) {
		t.Errorf("start left the group's stopped lender down, want %q among:\n%s",
			want, strings.Join(fake.Rendered(), "\n"))
	}
	if len(eng.started) != 1 {
		t.Errorf("the sandbox was started %d times, want once", len(eng.started))
	}
}

// And a sandbox that borrows nothing pays nothing: no lender is looked for, and
// none is started on a network that never needed one.
func TestStartLeavesALenderAloneWhenNothingIsLent(t *testing.T) {
	fake := run.NewFake()
	app := &App{InstDir: t.TempDir(), TierDir: t.TempDir(), Runner: fake}
	in := &state.Instance{Name: "box", Group: "team", Type: "agent", Engine: state.Podman}
	if err := state.Save(app.InstDir, in); err != nil {
		t.Fatal(err)
	}
	eng := &startedEngine{}
	if err := app.startInstance(context.Background(), eng, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	if fake.Contains("-lender") {
		t.Errorf("start touched a lender for a sandbox with no loan:\n%s", strings.Join(fake.Rendered(), "\n"))
	}
	if len(eng.started) != 1 {
		t.Errorf("the sandbox was started %d times, want once", len(eng.started))
	}
}

// lentBox is a sandbox in group team that borrows one key.
func lentBox(t *testing.T, fake *run.Fake) *App {
	t.Helper()
	app := &App{InstDir: t.TempDir(), TierDir: t.TempDir(), Runner: fake}
	in := &state.Instance{Name: "box", Group: "team", Type: "agent", Engine: state.Podman, Created: "2026-07-27T10:00:00Z"}
	if err := state.Save(app.InstDir, in); err != nil {
		t.Fatal(err)
	}
	loans := []lend.Loan{{Token: "t", Label: "box/fireworks", Slot: "fireworks", Kind: lend.Key}}
	if err := lend.WriteLoans(state.Dir(app.InstDir, in.Group, in.Name), loans); err != nil {
		t.Fatal(err)
	}
	return app
}

const lenderListing = "label=cs-sandbox.lender=1"

// A sandbox whose lender is down fails from the inside as a proxy that does not
// resolve, which names nothing. The listing is where somebody looks first, so
// it says which sandboxes are cut off, and how to bring the lender back.
func TestLsSaysWhenALentSandboxHasNoLender(t *testing.T) {
	app := lentBox(t, run.NewFake().OnStdout(lenderListing, "cs-sandbox-other-lender\n"))
	var buf bytes.Buffer
	if err := runLs(context.Background(), app, &buf, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lent (lender down)", "cs-sandbox start"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("ls does not say %q for a sandbox whose lender is not running:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	if err := runLsJSON(context.Background(), app, &buf); err != nil {
		t.Fatal(err)
	}
	var items []lsItem
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !items[0].LenderDown {
		t.Errorf("ls --json does not mark the sandbox whose lender is down:\n%s", buf.String())
	}
}

// A running lender is the ordinary case and reads as it always has. A listing
// that could not be had is not evidence of anything, so it condemns no lender.
func TestLsSaysNothingOfALenderThatRunsOrCannotBeSeen(t *testing.T) {
	for name, fake := range map[string]*run.Fake{
		"running": run.NewFake().OnStdout(lenderListing, lend.BoxName(state.NetworkName("team"))+"\n"),
		"unknown": run.NewFake().On(lenderListing, run.Result{}, errors.New("podman is unwell")),
	} {
		var buf bytes.Buffer
		if err := runLs(context.Background(), lentBox(t, fake), &buf, false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "lent") || strings.Contains(buf.String(), "lender down") {
			t.Errorf("%s: ls should read plain `lent`:\n%s", name, buf.String())
		}
	}
}
