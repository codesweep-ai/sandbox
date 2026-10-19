package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/codesweep-ai/sandbox/internal/fcconfig"
	"github.com/codesweep-ai/sandbox/internal/proc"
	"github.com/codesweep-ai/sandbox/internal/state"
)

var errKVM = errors.New("fc: /dev/kvm not available/writable — the firecracker engine needs a Linux host with KVM")

func errMissingTool(tool string) error {
	return fmt.Errorf("fc: missing host prerequisite %q — install it (e.g. dnf install / apt install)", tool)
}

// launchFirecracker boots firecracker for an instance inside podman's rootless
// netns as a detached, pid-tracked child (NOT via the capturing Runner — this is
// a long-lived process).
func launchFirecracker(idir, fcBin string, cg Cgroups) error {
	serial, err := os.Create(filepath.Join(idir, "serial.log"))
	if err != nil {
		return err
	}
	defer serial.Close()
	argv := append(cgroupWrapper(idir, cg, serial),
		"podman", "unshare", "--rootless-netns",
		fcBin, "--no-api", "--config-file", filepath.Join(idir, "run.json"))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = serial, serial
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Volunteer the guest RAM to KSM. The flag is inherited across the fork, so
	// it has to be set around Start rather than on the child.
	if err := withMemoryMerge(ksmEnabled(), cmd.Start); err != nil {
		return err
	}
	// Read the start time while the child is still unreaped, so it cannot be
	// another process's.
	id := proc.Of(cmd.Process.Pid)
	_ = cmd.Process.Release()
	return proc.WriteFile(filepath.Join(idir, proc.VMMPidFile), id)
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
//
// The scope goes under SliceName where the host has one (R171), and gets a CPU
// and a task cap beside the memory one (scopeCaps).
func cgroupWrapper(idir string, cg Cgroups, warnTo io.Writer) []string {
	if os.Getenv("CS_SANDBOX_FC_NO_CGROUP") != "" {
		return nil
	}
	// A user scope needs both the binary and a live user session bus; without
	// XDG_RUNTIME_DIR systemd-run would fail and take the boot down with it.
	if _, err := exec.LookPath("systemd-run"); err != nil || os.Getenv("XDG_RUNTIME_DIR") == "" {
		fmt.Fprintf(warnTo, "cs-sandbox: no systemd user session; running without a memory cgroup\n")
		return nil
	}

	cfg, err := fcconfig.ReadFile(filepath.Join(idir, "run.json"))
	if err != nil || cfg.MachineConfig.MemSizeMiB <= 0 {
		return nil // unknown sizing — better unwrapped than wrongly capped
	}
	max := os.Getenv("CS_SANDBOX_FC_MEMORY_MAX")
	if max == "" {
		max = strconv.Itoa(cfg.MachineConfig.MemSizeMiB+cgroupHeadroomMiB) + "M"
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
	argv = append(argv, scopeCaps(cfg.MachineConfig.VCPUCount)...)
	if cg.Slice {
		argv = append(argv, "--slice="+SliceName)
	}
	return argv
}

// vmmTasksMax caps the tasks of a microVM's scope. The guest's processes live
// in its own kernel and are not host tasks; what the scope holds is the VMM's
// threads, its io_uring workers and the podman unshare around it: fifteen at
// peak under eight parallel disk writers. The cap is a backstop against a VMM
// that leaks threads.
const vmmTasksMax = 1024

// scopeCaps are the CPU and task caps of a microVM's scope. The CPU cap is
// one core per vCPU plus one for the VMM's own device emulation, so it bounds
// what a guest takes without starving its disk and network.
func scopeCaps(vcpus int) []string {
	var a []string
	if vcpus > 0 {
		a = append(a, "-p", "CPUQuota="+strconv.Itoa((vcpus+1)*100)+"%")
	}
	return append(a, "-p", "TasksMax="+strconv.Itoa(vmmTasksMax))
}

// cgroupHeadroomMiB is what the default MemoryMax adds on top of the guest's
// configured RAM to cover the VMM itself (~5–20 MiB measured), its page tables
// and the virtio queues.
const cgroupHeadroomMiB = 256

// fcRunning reports whether the instance's firecracker process is alive: fc.pid
// names a running process that is the one recorded, by start time and by an
// argv naming this instance's config (R170).
func fcRunning(idir string) bool { return proc.VMMRunning(idir) }

// killFirecracker kills the instance's firecracker process (TERM, then KILL) and
// reaps any survivor by config-file match.
//
// The config-file sweep runs on every path out, not only after a SIGKILL.
// fc.pid holds the `podman unshare` wrapper and the VMM is its grandchild, so
// the wrapper can die while the microVM keeps running — which is the state a
// killed test or a timed-out run leaves behind. On that path the pid reads as
// gone, and every check here agrees the work is done while the guest is still
// up. Only the config file names the process that actually has to die, and
// Remove deletes the directory holding it moments later, so a sweep skipped
// here is a microVM nothing can address again.
//
// A pid that is no longer the recorded process is never signalled. One whose
// record predates start times is matched on its argv alone, and gets SIGTERM
// but never SIGKILL.
func killFirecracker(idir string) {
	defer func() {
		_ = exec.Command("pkill", "-9", "-f", "config-file "+filepath.Join(idir, proc.VMMConfig)).Run()
		_ = os.Remove(filepath.Join(idir, proc.VMMPidFile))
	}()
	h := proc.FindVMM(idir)
	if h == nil {
		return
	}
	defer h.Close()
	_ = h.Signal(syscall.SIGTERM)
	for range 16 {
		if !h.Running() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if h.Verified() {
		_ = h.Signal(syscall.SIGKILL)
	}
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
	for _, tool := range []string{"dnsmasq", "socat", "fakeroot", "mke2fs", "debugfs", "pasta", "newuidmap", "python3", "ip", "ssh", "curl", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			return errMissingTool(tool)
		}
	}
	return nil
}
