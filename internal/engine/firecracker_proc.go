package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/fcconfig"
	"github.com/codesweep-ai/sandbox/internal/state"
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
	argv := append(cgroupWrapper(idir, serial),
		"podman", "unshare", "--rootless-netns",
		fcBin, "--no-api", "--config-file", filepath.Join(idir, "run.json"))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = serial, serial
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return os.WriteFile(filepath.Join(idir, "fc.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// cgroupWrapper returns the argv prefix that puts a microVM in its own cgroup —
// `systemd-run --user --scope …` — or nil to launch unwrapped.
//
// Why bother, when a guest cannot allocate beyond its own `mem_size_mib`: with
// no cgroup the VMM inherits the *launching shell's* scope, so a sandbox has no
// memory accounting of its own and a runaway one is charged to whatever
// terminal started it. The host OOM killer then picks its victim by heuristic
// and may kill something else entirely. A per-instance scope makes the limit
// enforceable and the kill attributable.
//
// The default ceiling is deliberately above what the guest can reach
// (mem_size_mib + cgroupHeadroomMiB), so it is a backstop that never fires in
// normal operation. Tighten it via CS_SANDBOX_FC_MEMORY_MAX once sandboxes are
// packed past the sum of their ceilings — that is when it starts doing work.
//
// `memory.high` is deliberately NOT set. Measured on this workload it is not a
// throttle but a cliff: once the cgroup can no longer reclaim, the VM makes no
// progress at ~97% pressure stall while staying alive, with no OOM and no error
// for a supervisor to notice. A hard `MemoryMax` fails loudly instead.
func cgroupWrapper(idir string, warnTo io.Writer) []string {
	if os.Getenv("CS_SANDBOX_FC_NO_CGROUP") != "" {
		return nil
	}
	// A user scope needs both the binary and a live user session bus; without
	// XDG_RUNTIME_DIR systemd-run would fail and take the boot down with it.
	if _, err := exec.LookPath("systemd-run"); err != nil || os.Getenv("XDG_RUNTIME_DIR") == "" {
		fmt.Fprintf(warnTo, "cs-sandbox: no systemd user session; running without a memory cgroup\n")
		return nil
	}

	max := os.Getenv("CS_SANDBOX_FC_MEMORY_MAX")
	if max == "" {
		mem := cgroupDefaultMaxMiB(idir)
		if mem <= 0 {
			return nil // unknown sizing — better unwrapped than wrongly capped
		}
		max = strconv.Itoa(mem) + "M"
	}

	// The unit name must be unique per launch: a scope whose process is
	// OOM-killed stays in `failed` state and blocks reuse of its name.
	unit := fmt.Sprintf("cs-sandbox-fc-%s-%d", filepath.Base(idir), time.Now().UnixNano())
	argv := []string{
		"systemd-run", "--user", "--scope", "--quiet",
		"--unit=" + unit,
		"-p", "MemoryMax=" + max,
	}
	// Swap is charged on top of MemoryMax, and on a zram host it is RAM — so
	// budget MemoryMax+MemorySwapMax, and default to no swap allowance.
	swap := os.Getenv("CS_SANDBOX_FC_MEMORY_SWAP_MAX")
	if swap == "" {
		swap = "0"
	}
	argv = append(argv, "-p", "MemorySwapMax="+swap)
	return argv
}

// cgroupHeadroomMiB is what the default MemoryMax adds on top of the guest's
// configured RAM to cover the VMM itself (~5–20 MiB measured), its page tables
// and the virtio queues.
const cgroupHeadroomMiB = 256

// cgroupDefaultMaxMiB derives the default ceiling from what the instance is
// actually configured to boot with. Returns 0 when run.json cannot be read.
func cgroupDefaultMaxMiB(idir string) int {
	cfg, err := fcconfig.ReadFile(filepath.Join(idir, "run.json"))
	if err != nil || cfg.MachineConfig.MemSizeMiB <= 0 {
		return 0
	}
	return cfg.MachineConfig.MemSizeMiB + cgroupHeadroomMiB
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

// removeVsock removes stale vm.vsock* sockets before (re)boot. The glob also
// sweeps the _<port> form, which nothing opens today but which an older build
// or a future guest→host channel could leave behind.
func removeVsock(idir string) {
	matches, _ := filepath.Glob(filepath.Join(idir, state.SockVsock+"*"))
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
