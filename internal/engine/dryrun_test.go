package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// --dry-run prints the external commands instead of running them, so nothing is
// started. A record written anyway names a sandbox that does not exist: it holds
// the name and the port it allocated, and `ls` reports it as stopped.
func TestSaveSkipsUnderDryRun(t *testing.T) {
	in := &state.Instance{Name: "box", Group: state.DefaultGroup, Engine: state.Podman}

	dir := t.TempDir()
	if err := (Deps{InstDir: dir, DryRun: true}).save(in); err != nil {
		t.Fatalf("dry-run save: %v", err)
	}
	if _, err := state.Load(dir, state.DefaultGroup, "box"); err == nil {
		t.Error("a dry run persisted an instance record")
	}

	dir = t.TempDir()
	if err := (Deps{InstDir: dir, DryRun: false}).save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := state.Load(dir, state.DefaultGroup, "box"); err != nil {
		t.Errorf("a real create persisted nothing: %v", err)
	}
}

// The readiness wait polls a sandbox that a dry run never started, so it can
// only time out. It used to spend the whole budget doing it, and a `create`
// that blocks for two minutes and then fails is not a plan anybody can read.
func TestWaitReadyReturnsAtOnceUnderDryRun(t *testing.T) {
	probeFails := func() *run.Fake {
		return run.NewFake().On("cs-sandbox-ready", run.Result{}, errors.New("no such container"))
	}

	start := time.Now()
	d := Deps{Runner: probeFails(), StartTimeout: 120, DryRun: true}
	if err := d.waitReady(context.Background(), "box"); err != nil {
		t.Fatalf("dry-run waitReady: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("dry-run waitReady took %s; it polls something that was never started", elapsed)
	}

	// The negative half: a real create still waits, and still fails when the
	// sandbox never answers.
	d = Deps{Runner: probeFails(), StartTimeout: 1}
	if err := d.waitReady(context.Background(), "box"); err == nil {
		t.Error("waitReady passed a sandbox that never became ready")
	}
}
