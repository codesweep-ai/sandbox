package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The contract tests execute the real remote-tool shell scripts, which target
// the Linux guest/host environment (/proc-based PID verification, GNU
// coreutils). On other platforms they would fail for environmental reasons,
// not contract violations, so CI's macOS leg skips them.
func skipUnlessLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("remote-tool scripts target Linux (/proc, GNU coreutils); skipping on %s", runtime.GOOS)
	}
}

// A stale PID file whose PID now belongs to an unrelated process (OS PID
// reuse) must not be signaled; cancellation still cleans up bookkeeping and
// writes the interrupted footer.
func TestRemoteKillLeavesUnrelatedPIDAlone(t *testing.T) {
	skipUnlessLinux(t)
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-sessions", "-logs", "-pids", "-locks"} {
		if err := os.MkdirAll(filepath.Join(home, ".cs-codex-remote"+suffix), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	name := "stale-contract"
	if err := os.WriteFile(filepath.Join(home, ".cs-codex-remote-sessions", name+".token"), []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, ".cs-codex-remote-logs", name+".log")
	if err := os.WriteFile(log, []byte("--- 2026-01-01 00:00:00 --- prompt: wait\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bystander := exec.Command("sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bystander.Process.Kill(); _, _ = bystander.Process.Wait() }()
	pidFile := filepath.Join(home, ".cs-codex-remote-pids", name+".pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(bystander.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := filepath.Join("..", "..", "image", "rootfs", "home", ".local", "bin", "cs-codex-remote")
	cmd := exec.Command(tool, "--kill", name)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--kill: %v: %s", err, out)
	}
	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was signaled despite failing runner verification: %v", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale PID file not cleaned: %v", err)
	}
	if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "finished (exit 130)") {
		t.Fatalf("interrupted footer missing: %v: %s", err, b)
	}
}

func TestRemoteKillStopsBackgroundRunner(t *testing.T) {
	skipUnlessLinux(t)
	tests := []struct {
		name, tool, prefix, mappingSuffix, mappingValue string
	}{
		{"codex", "cs-codex-remote", ".cs-codex-remote", ".token", "deadbeef"},
		{"claude", "cs-claude-remote", ".cs-claude-remote", "", "00000000-0000-4000-8000-000000000001"},
		{"opencode", "cs-opencode-remote", ".cs-opencode-remote", ".token", "deadbeef"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-sessions", "-logs", "-pids", "-locks"} {
				if err := os.MkdirAll(filepath.Join(home, tc.prefix+suffix), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			name := "kill-contract"
			mapping := filepath.Join(home, tc.prefix+"-sessions", name+tc.mappingSuffix)
			if err := os.WriteFile(mapping, []byte(tc.mappingValue+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(home, tc.prefix+"-logs", name+".log")
			if err := os.WriteFile(log, []byte("--- 2026-01-01 00:00:00 --- prompt: wait\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The runner must look like OUR runner: cancellation verifies the
			// PID's cmdline references the per-session runner file before
			// signaling, so a bare sleep would (correctly) be left alone.
			runnerFile := filepath.Join(home, tc.prefix+"-pids", name+".runner")
			if err := os.WriteFile(runnerFile, []byte("#!/bin/bash\nsleep 30\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			runner := exec.Command("bash", runnerFile)
			if err := runner.Start(); err != nil {
				t.Fatal(err)
			}
			pidFile := filepath.Join(home, tc.prefix+"-pids", name+".pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(runner.Process.Pid)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			tool := filepath.Join("..", "..", "image", "rootfs", "home", ".local", "bin", tc.tool)
			cmd := exec.Command(tool, "--kill", name)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s --kill: %v: %s", tc.tool, err, out)
			}
			done := make(chan error, 1)
			go func() { done <- runner.Wait() }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = runner.Process.Kill()
				t.Fatal("background runner survived --kill")
			}
			if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
				t.Fatalf("PID file still exists: %v", err)
			}
			b, err := os.ReadFile(log)
			if err != nil || !strings.Contains(string(b), "finished (exit 130)") {
				t.Fatalf("interrupted footer missing: %v: %s", err, b)
			}
			outputTool := filepath.Join("..", "..", "image", "rootfs", "home", ".local", "bin", tc.tool+"-output")
			status := exec.Command(outputTool, name, "-s")
			status.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin")
			out, statusErr := status.CombinedOutput()
			exitErr, ok := statusErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 3 || strings.TrimSpace(string(out)) != "failed" {
				t.Fatalf("interrupted status = %q, %v; want failed/3", out, statusErr)
			}
		})
	}
}
