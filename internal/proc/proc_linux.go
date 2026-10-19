package proc

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// errGone is what openFD returns for a pid that names no process.
const errGone = -2

// openFD opens a pidfd on pid, or returns -1 where the kernel or a seccomp
// filter refuses pidfds, so the caller signals by pid instead.
func openFD(pid int) int {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return errGone
	}
	if err != nil {
		return -1
	}
	return fd
}

func sendFD(fd int, sig syscall.Signal) error {
	return unix.PidfdSendSignal(fd, sig, nil, 0)
}

func closeFD(fd int) { _ = unix.Close(fd) }

func hasProc() bool { return true }

func statOf(pid int) (stat, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return stat{}, false
	}
	return parseStat(string(b))
}

// argvOf reads the process's argv. A process part-way through execve has none
// for a moment, which is what a launcher like systemd-run is while it execs
// what it wraps, so an empty one is read again before it counts.
func argvOf(pid int) ([]string, bool) {
	for range 50 {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil {
			return nil, false
		}
		if len(b) > 0 {
			return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), true
		}
		time.Sleep(time.Millisecond)
	}
	return nil, false
}
