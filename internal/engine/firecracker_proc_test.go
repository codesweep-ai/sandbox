package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startFakeVMM starts a stand-in for the microVM's firecracker process. What
// identifies it is the --config-file argument, exactly as for the real one:
// nothing else about the command matters to the reaper.
//
// It loops rather than sleeping once, because `sh -c "sleep 120"` is a shape a
// shell may satisfy by exec'ing sleep over itself — and that replaces the argv
// carrying --config-file, so the process the reaper is meant to find stops
// being findable and the test passes for the wrong reason. The loop keeps the
// shell alive under its original argv, and leaves behind at most a one-second
// sleep when it dies.
func startFakeVMM(t *testing.T, idir string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("pkill"); err != nil {
		t.Skip("pkill is how the reaper finds a survivor; nothing to test without it")
	}
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done",
		"--no-api", "--config-file", filepath.Join(idir, "run.json"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// waitGone fails unless the process is reaped within a few seconds. pkill
// delivers the signal synchronously, but the kernel still has to tear the
// process down.
func waitGone(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the microVM survived teardown: destroy reports success and leaves a " +
			"firecracker running whose instance dir is about to be deleted")
	}
}

// TestKillFirecrackerReapsWhenThePidIsStale pins the case that leaked twice:
// fc.pid records the `podman unshare` wrapper, the VMM is its grandchild, and
// the wrapper dies first when a run is killed. Every pid-based check then agrees
// the VM is gone while it is still running, so the config-file sweep is the only
// thing that can end it — and Remove deletes the config moments later, after
// which nothing can name the process again.
func TestKillFirecrackerReapsWhenThePidIsStale(t *testing.T) {
	idir := t.TempDir()
	cmd := startFakeVMM(t, idir)

	// A pid that owns nothing: what the wrapper's entry becomes once it exits.
	if err := os.WriteFile(filepath.Join(idir, "fc.pid"), []byte("2147483646\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	killFirecracker(idir)
	waitGone(t, cmd)

	if _, err := os.Stat(filepath.Join(idir, "fc.pid")); !os.IsNotExist(err) {
		t.Errorf("fc.pid outlived the teardown: %v", err)
	}
}

// TestKillFirecrackerReapsWithNoPidFile: `rm` leaves the instance dir without an
// fc.pid, and a re-created sandbox reuses that directory. A VMM still holding
// the old config has to go before the next one binds the same paths.
func TestKillFirecrackerReapsWithNoPidFile(t *testing.T) {
	idir := t.TempDir()
	cmd := startFakeVMM(t, idir)

	killFirecracker(idir)
	waitGone(t, cmd)
}
