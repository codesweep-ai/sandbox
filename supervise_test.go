package assets

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// supervise keeps sshd up on both engines. These run the real script.

// A command that keeps dying must be started again, but on a growing delay, so
// a crash loop cannot spin. Restarts at 0.1, 0.2, 0.4 and 0.8 s fit five starts
// in 1.7 s, where a loop with no backoff would make hundreds.
func TestSuperviseRestartsWithBackoff(t *testing.T) {
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	cmd := exec.Command("image/rootfs/supervise", "crashy", "sh", "-c", "echo x >> "+starts+"; exit 3")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1700 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Errorf("supervise exited %v on TERM, want 0", err)
	}
	data, _ := os.ReadFile(starts)
	n := strings.Count(string(data), "x")
	if n < 2 || n > 6 {
		t.Errorf("command started %d times in 1.7s, want 2 to 6 (restart, with backoff)", n)
	}
	if !strings.Contains(stderr.String(), "crashy exited with status 3; restarting in 0.200s") {
		t.Errorf("supervise did not log the doubled delay:\n%s", stderr.String())
	}
}

// TERM to supervise must take the command down with it, or stopping a
// container's supervisor would leave an orphaned sshd behind.
func TestSuperviseStopsItsCommandOnTerm(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("image/rootfs/supervise", "longrun", "sh", "-c", "echo $$ > "+pidFile+"; exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var child int
	for range 100 {
		if b, err := os.ReadFile(pidFile); err == nil {
			if child, _ = strconv.Atoi(strings.TrimSpace(string(b))); child > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("the supervised command never started")
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	for range 50 {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	t.Error("the supervised command outlived supervise")
}

// Both boot paths have to run sshd under it. One that starts sshd bare leaves a
// sandbox unreachable over ssh once sshd dies.
func TestBothBootPathsSuperviseSSHD(t *testing.T) {
	for _, script := range []string{"image/rootfs/entrypoint", "image/guest/init"} {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "/sandbox/supervise sshd /usr/sbin/sshd -D") {
			t.Errorf("%s does not start sshd under /sandbox/supervise", script)
		}
	}
}
