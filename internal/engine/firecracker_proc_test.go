package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	return fakeVMM(t, idir)
}

// fakeVMM is startFakeVMM for a test that does not need the reaper.
func fakeVMM(t *testing.T, idir string) *exec.Cmd {
	t.Helper()
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

// startBystander starts a process of this user's that has nothing to do with
// any sandbox, the kind a pid recorded before a reboot can name afterwards. The
// channel closes if it exits.
func startBystander(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gone := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(gone) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-gone })
	return cmd, gone
}

// stillRunning fails if the bystander was signalled.
func stillRunning(t *testing.T, gone <-chan struct{}) {
	t.Helper()
	select {
	case <-gone:
		t.Fatal("an unrelated process holding the recorded pid was killed")
	case <-time.After(300 * time.Millisecond):
	}
}

// writePid writes fc.pid as launchFirecracker does, or with the pid alone as
// a record written before start times were kept.
func writePid(t *testing.T, idir string, pid int, start string) {
	t.Helper()
	rec := strconv.Itoa(pid) + "\n"
	if start != "" {
		rec += start + "\n"
	}
	if err := os.WriteFile(filepath.Join(idir, "fc.pid"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
}

// needProc skips a test of telling two holders of one pid apart. Without /proc
// there is no start time or argv to tell them by, and R170 judges a record by
// its pid alone.
func needProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc here: a record is judged by its pid alone")
	}
}

// recordVMM writes fc.pid for cmd as launchFirecracker does.
func recordVMM(t *testing.T, idir string, cmd *exec.Cmd) {
	t.Helper()
	writePid(t, idir, cmd.Process.Pid, startTime(t, cmd.Process.Pid))
}

// startTime is field 22 of /proc/<pid>/stat.
func startTime(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])[19]
}

// SBX-002: fc.pid survives a reboot, and the number in it can then belong to
// any process of yours. That process is not the microVM: `ls` must not call
// the sandbox running, `start` must not refuse it, and `stop` must not kill it.
func TestAReusedVMMPidIsNotTheMicroVM(t *testing.T) {
	needProc(t)
	for _, tc := range []struct {
		name   string
		legacy bool
	}{
		{"record with start time", false},
		{"record from before start times", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idir := t.TempDir()
			cmd, gone := startBystander(t)
			start := "1" // the start time the recorded VMM had, which the bystander does not
			if tc.legacy {
				start = ""
			}
			writePid(t, idir, cmd.Process.Pid, start)

			if fcRunning(idir) {
				t.Error("fcRunning reports a microVM for a pid an unrelated process holds")
			}
			killFirecracker(idir)
			stillRunning(t, gone)
			if _, err := os.Stat(filepath.Join(idir, "fc.pid")); !os.IsNotExist(err) {
				t.Errorf("the stale fc.pid outlived the teardown: %v", err)
			}
		})
	}
}

// The recorded launcher is still running: it is the microVM, whether the
// record holds its start time or predates them, and stop ends it.
func TestTheRecordedVMMIsRunningAndStopEndsIt(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		idir := t.TempDir()
		cmd := startFakeVMM(t, idir)
		start := startTime(t, cmd.Process.Pid)
		if legacy {
			start = ""
		}
		writePid(t, idir, cmd.Process.Pid, start)

		if !fcRunning(idir) {
			t.Fatalf("legacy=%v: fcRunning reports the recorded VMM as stopped", legacy)
		}
		killFirecracker(idir)
		waitGone(t, cmd)
		if fcRunning(idir) {
			t.Errorf("legacy=%v: fcRunning still true after the kill", legacy)
		}
	}
}

// A process whose argv names this instance's config but whose start time is
// not the recorded one is a later launch, or a reused pid: not this record's.
func TestAVMMWithAnotherStartTimeIsNotTheRecordedOne(t *testing.T) {
	needProc(t)
	idir := t.TempDir()
	cmd := startFakeVMM(t, idir)
	writePid(t, idir, cmd.Process.Pid, "1")
	if fcRunning(idir) {
		t.Error("fcRunning matched on the argv while the start time differs")
	}
}
