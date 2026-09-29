package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/codesweep-ai/sandbox/internal/fcconfig"
)

// The Firecracker block engine a microVM's disks run on.
//
// Sync, Firecracker's default, does disk IO on the thread that also runs the
// guest's network. A write the host throttles then freezes the VM's network with
// it: on a slow disk a guest went dark for 32 seconds at a time, and new
// connections failed once ARP gave up (SBX-031). Async does the IO through
// io_uring, off that thread, and the same run lost no packet.
//
// Firecracker still calls Async a developer preview, and its open concerns are
// io_uring's worker threads, so it is used only where those are accounted for:
// a kernel from 5.12 on, where the workers count against the VM's cgroup. It
// also needs io_uring that can actually be set up, which a disabling sysctl, a
// container's seccomp profile or SELinux refuses. Everywhere else the disks stay
// on Sync.
const (
	IOEngineAsync = "Async"
	IOEngineSync  = "Sync"
)

// DiskIOEngine is the engine this host gives microVM disks, and, for Sync, why.
// CS_SANDBOX_FC_IO_ENGINE set to Sync or Async overrides the choice.
func DiskIOEngine() (engine, why string) {
	diskIOOnce.Do(func() {
		switch v := os.Getenv("CS_SANDBOX_FC_IO_ENGINE"); {
		case strings.EqualFold(v, IOEngineSync):
			diskIO, diskIOWhy = IOEngineSync, "CS_SANDBOX_FC_IO_ENGINE is Sync"
		case strings.EqualFold(v, IOEngineAsync):
			diskIO = IOEngineAsync
		default:
			diskIO, diskIOWhy = chooseDiskIOEngine(hostIOUring())
		}
	})
	return diskIO, diskIOWhy
}

var (
	diskIOOnce        sync.Once
	diskIO, diskIOWhy string
)

// chooseDiskIOEngine decides from what the host says: its kernel release,
// kernel.io_uring_disabled, empty where the kernel has no such setting, and what
// setting up an io_uring returned.
func chooseDiskIOEngine(release, disabled string, probe error) (engine, why string) {
	if !kernelAtLeast(release, 5, 12) {
		return IOEngineSync, fmt.Sprintf("the kernel is %s, and io_uring's workers count against a microVM's cgroup only from 5.12", release)
	}
	if probe == nil {
		return IOEngineAsync, ""
	}
	switch disabled {
	case "2":
		return IOEngineSync, "io_uring is disabled on this host (kernel.io_uring_disabled is 2)"
	case "1":
		return IOEngineSync, "io_uring is limited to one group on this host (kernel.io_uring_disabled is 1)"
	}
	return IOEngineSync, fmt.Sprintf("io_uring cannot be set up here (%v), which a container's seccomp profile or SELinux can refuse", probe)
}

// kernelAtLeast reports whether a kernel release such as 6.8.0-45-generic is
// at least major.minor. A release it cannot read is not.
func kernelAtLeast(release string, major, minor int) bool {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	mnr, err2 := strconv.Atoi(leadingDigits(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || maj == major && mnr >= minor
}

// leadingDigits is the run of digits s starts with, as the 12 of 12-rc1.
func leadingDigits(s string) string {
	if i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		return s[:i]
	}
	return s
}

// setDiskIOEngine puts every disk of the microVM in idir on this host's engine.
// It runs at every launch rather than once at create, so a sandbox made before
// the host changed, or before this choice existed, boots on what the host can
// do now.
func setDiskIOEngine(idir string) error {
	path := filepath.Join(idir, "run.json")
	cfg, err := fcconfig.ReadFile(path)
	if err != nil {
		return err
	}
	engine, _ := DiskIOEngine()
	cfg.SetIOEngine(engine)
	return cfg.WriteFile(path)
}
