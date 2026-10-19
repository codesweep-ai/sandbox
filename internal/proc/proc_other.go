//go:build !linux

package proc

import "syscall"

// Without /proc there is no start time or argv to read, so a record is judged
// by its pid alone, and without pidfds a signal goes by pid.

const errGone = -2

func openFD(int) int { return -1 }

func sendFD(int, syscall.Signal) error { return syscall.ENOSYS }

func closeFD(int) {}

func hasProc() bool { return false }

func statOf(int) (stat, bool) { return stat{}, false }

func argvOf(int) ([]string, bool) { return nil, false }
