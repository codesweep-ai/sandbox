package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/proc"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// A boot takes a second or two, so a poll on whole seconds added up to a second
// to every create and start. The first answer has to be seen well inside one.
func TestPollUntilSeesReadinessPromptly(t *testing.T) {
	ready := time.Now().Add(100 * time.Millisecond)
	start := time.Now()
	err := pollUntil(context.Background(), 5*time.Second, 50*time.Millisecond, 250*time.Millisecond,
		func() (bool, error) { return time.Now().After(ready), nil })
	if err != nil {
		t.Fatalf("pollUntil = %v, want nil", err)
	}
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Errorf("pollUntil saw readiness after %s, want well under a second", took)
	}
}

// A long wait must not keep polling at the starting rate: at a fixed 10 ms this
// budget would make sixty calls.
func TestPollUntilBacksOff(t *testing.T) {
	var calls int
	err := pollUntil(context.Background(), 600*time.Millisecond, 10*time.Millisecond, 80*time.Millisecond,
		func() (bool, error) { calls++; return false, nil })
	if !errors.Is(err, errPollBudget) {
		t.Fatalf("pollUntil = %v, want errPollBudget", err)
	}
	if calls > 20 {
		t.Errorf("pollUntil made %d calls in 600ms, want the interval to grow to 80ms", calls)
	}
}

func TestPollUntilStopsOnErrorAndCancel(t *testing.T) {
	boom := errors.New("boom")
	if err := pollUntil(context.Background(), time.Minute, time.Millisecond, time.Millisecond,
		func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("pollUntil = %v, want the check's own error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pollUntil(ctx, time.Minute, time.Second, time.Second,
		func() (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("pollUntil = %v, want context.Canceled", err)
	}
}

// fcInstance makes an instance dir whose fc.pid records pid, with its start
// time, the way launchFirecracker does (R170).
func fcInstance(t *testing.T, pid func(idir string) int) (*Firecracker, string) {
	t.Helper()
	dir := t.TempDir()
	fe := NewFirecracker(Deps{Runner: run.NewFake(), InstDir: dir, FCCache: t.TempDir(), Network: "cs-sandbox-net", StartTimeout: 5})
	if err := state.Save(dir, &state.Instance{Name: "box", Type: "agent", Engine: state.Firecracker, FCIP: "10.89.0.200"}); err != nil {
		t.Fatal(err)
	}
	idir := fe.d.InstanceDir("box")
	if err := proc.WriteFile(filepath.Join(idir, "fc.pid"), proc.Of(pid(idir))); err != nil {
		t.Fatal(err)
	}
	return fe, idir
}

// liveVMM is a pid for fcInstance: a stand-in VMM whose argv names the instance.
func liveVMM(t *testing.T) func(string) int {
	return func(idir string) int { return fakeVMM(t, idir).Process.Pid }
}

func TestFirecrackerWaitReadySeesTheMarkerPromptly(t *testing.T) {
	fe, idir := fcInstance(t, liveVMM(t))
	serial := filepath.Join(idir, "serial.log")
	if err := os.WriteFile(serial, []byte("booting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(serial, []byte("booting\n##### FC-VM-READY #####\n"), 0o600)
	}()
	start := time.Now()
	if err := fe.waitReady(context.Background(), "box"); err != nil {
		t.Fatalf("waitReady = %v", err)
	}
	if took := time.Since(start); took > 800*time.Millisecond {
		t.Errorf("waitReady returned %s after start for a marker written at 150ms", took)
	}
}

func TestFirecrackerWaitReadyFailsWhenTheVMMDies(t *testing.T) {
	fe, _ := fcInstance(t, func(string) int { return deadPid(t) })
	if err := fe.waitReady(context.Background(), "box"); err == nil {
		t.Fatal("waitReady passed a microVM whose VMM is gone")
	}
}

// blockingRunner stands in for an ssh that never returns on its own, which is
// what one does when the guest it reached reboots without closing the
// connection.
type blockingRunner struct{ cancelled atomic.Bool }

func (b *blockingRunner) Run(ctx context.Context, _ run.Opts, _ ...string) (run.Result, error) {
	<-ctx.Done()
	b.cancelled.Store(true)
	return run.Result{}, ctx.Err()
}

// The ssh that asks for the reboot hangs once the guest is gone, so stop used to
// sit out its whole 8s timeout. It has to end as soon as the VMM exits.
func TestRebootGuestReturnsWhenTheVMMExits(t *testing.T) {
	var vmm *exec.Cmd
	fe, idir := fcInstance(t, func(idir string) int { vmm = fakeVMM(t, idir); return vmm.Process.Pid })
	r := &blockingRunner{}
	fe.d.Runner = r
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = vmm.Process.Kill()
		_ = vmm.Wait()
	}()
	start := time.Now()
	fe.rebootGuest(context.Background(), idir, []string{"ssh", "box", "reboot"})
	took := time.Since(start)
	if took > 2*time.Second {
		t.Errorf("rebootGuest returned %s after start for a VMM that exited at 200ms", took)
	}
	if took < 150*time.Millisecond {
		t.Errorf("rebootGuest returned %s after start, before the VMM exited at 200ms", took)
	}
	if !r.cancelled.Load() {
		t.Error("rebootGuest left the ssh running")
	}
}

// deadPid returns the pid of a process that has exited and been reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}
