package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// Start on a microVM that is already running must be a no-op. It used to launch
// a second VMM over the first one's disks and tap. That one died on the busy
// tap, start waited out the whole readiness budget and failed, and the pid it
// recorded left `ls` calling a running sandbox stopped.
func TestFirecrackerStartLeavesARunningVMAlone(t *testing.T) {
	dir := t.TempDir()
	f := run.NewFake()
	fe := NewFirecracker(Deps{Runner: f, InstDir: dir, FCCache: t.TempDir(), Network: "cs-sandbox-net", StartTimeout: 2})
	if err := state.Save(dir, &state.Instance{Name: "box", Type: "agent", Engine: state.Firecracker, FCIP: "10.89.0.200"}); err != nil {
		t.Fatal(err)
	}
	idir := state.Dir(dir, state.DefaultGroup, "box")
	recordVMM(t, idir, fakeVMM(t, idir))
	pidFile := filepath.Join(idir, "fc.pid")
	live, _ := os.ReadFile(pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fe.Start(ctx, "box"); err != nil {
		t.Errorf("start on a running microVM = %v, want nil", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("start on a running microVM ran %d commands, want none: %s", len(f.Calls), f)
	}
	if got, _ := os.ReadFile(pidFile); string(got) != string(live) {
		t.Errorf("start replaced the running VM's pid record: %q, want %q", got, live)
	}
}
