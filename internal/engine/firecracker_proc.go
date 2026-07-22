package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var errKVM = errors.New("fc: /dev/kvm not available/writable — the firecracker engine needs a Linux host with KVM")

func errMissingTool(tool string) error {
	return fmt.Errorf("fc: missing host prerequisite %q — install it (e.g. dnf install / apt install)", tool)
}

// launchFirecracker boots firecracker for an instance inside podman's rootless
// netns as a detached, pid-tracked child (NOT via the capturing Runner — this is
// a long-lived process).
func launchFirecracker(idir, fcBin string) error {
	serial, err := os.Create(filepath.Join(idir, "serial.log"))
	if err != nil {
		return err
	}
	defer serial.Close()
	cmd := exec.Command("podman", "unshare", "--rootless-netns",
		fcBin, "--no-api", "--config-file", filepath.Join(idir, "run.json"))
	cmd.Stdout, cmd.Stderr = serial, serial
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return os.WriteFile(filepath.Join(idir, "fc.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// fcRunning reports whether the instance's firecracker process is alive.
func fcRunning(idir string) bool {
	data, err := os.ReadFile(filepath.Join(idir, "fc.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// killFirecracker kills the instance's firecracker process (TERM, then KILL) and
// reaps any survivor by config-file match.
func killFirecracker(idir string) {
	pidFile := filepath.Join(idir, "fc.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidFile)
		return
	}
	if syscall.Kill(pid, 0) != nil {
		_ = os.Remove(pidFile)
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 16; i++ {
		if syscall.Kill(pid, 0) != nil {
			_ = os.Remove(pidFile)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	// belt-and-suspenders: reap any firecracker for this instance's config.
	_ = exec.Command("pkill", "-9", "-f", "config-file "+filepath.Join(idir, "run.json")).Run()
	_ = os.Remove(pidFile)
}

// removeVsock removes stale vm.vsock* sockets before (re)boot.
func removeVsock(idir string) {
	matches, _ := filepath.Glob(filepath.Join(idir, "vm.vsock*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// preflight fails early if the host is missing a firecracker prerequisite
// (KVM + the host tools the engine shells out to).
func (fe *Firecracker) preflight() error {
	if fi, err := os.Stat("/dev/kvm"); err != nil || fi.Mode()&os.ModeDevice == 0 {
		return errKVM
	}
	// Writable check: open O_RDWR.
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		return errKVM
	} else {
		_ = f.Close()
	}
	// `ip` brings up the tap/bridge fabric; `ssh` is how you reach the booted VM.
	for _, tool := range []string{"dnsmasq", "socat", "fakeroot", "mke2fs", "pasta", "newuidmap", "python3", "ip", "ssh", "curl", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			return errMissingTool(tool)
		}
	}
	return nil
}
