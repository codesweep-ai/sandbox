//go:build linux

package engine

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// hostIOUring reads what chooseDiskIOEngine decides from. The probe sets up a
// one-entry io_uring and closes it at once: that asks the kernel, its sysctl,
// and any seccomp profile or security module around this process together,
// which none of them answers alone.
func hostIOUring() (release, disabled string, probe error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		release = unix.ByteSliceToString(uts.Release[:])
	}
	var params [120]byte // struct io_uring_params, zeroed
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		probe = errno
	} else {
		_ = unix.Close(int(fd))
	}
	if b, err := os.ReadFile("/proc/sys/kernel/io_uring_disabled"); err == nil {
		disabled = strings.TrimSpace(string(b))
	}
	return release, disabled, probe
}
