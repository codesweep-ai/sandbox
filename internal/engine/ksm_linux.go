//go:build linux

package engine

import (
	"os"

	"golang.org/x/sys/unix"
)

// withMemoryMerge runs launch with PR_SET_MEMORY_MERGE set on this process, so
// the microVM it starts inherits it.
//
// KSM only ever scans memory a process has volunteered — a VMA marked
// MADV_MERGEABLE, or the whole process marked with this prctl. Firecracker does
// neither, so its guest RAM is not a candidate and `ksm/run=1` dedupes nothing
// at all: the knob appears to work and silently buys zero. Setting it here is
// what makes a fleet of same-image sandboxes actually collapse onto shared
// pages.
//
// It goes on the parent rather than the child because there is no hook between
// fork and exec — but the flag survives both, and podman unshare in between, so
// the VMM inherits it. It is restored immediately after the fork to keep the
// launcher's own (small, and unrelated) heap out of ksmd's way.
//
// Requires kernel >= 6.4 and, separately, ksmd to be running
// (/sys/kernel/mm/ksm/run=1) — a host decision this cannot make. An unsupported
// kernel returns EINVAL, which is not an error worth failing a boot over:
// without it you get today's behaviour, no dedup.
func withMemoryMerge(enabled bool, launch func() error) error {
	if !enabled {
		return launch()
	}
	if err := unix.Prctl(unix.PR_SET_MEMORY_MERGE, 1, 0, 0, 0); err != nil {
		return launch() // pre-6.4 kernel: boot normally, just without dedup
	}
	defer func() { _ = unix.Prctl(unix.PR_SET_MEMORY_MERGE, 0, 0, 0, 0) }()
	return launch()
}

// ksmEnabled reports whether the launcher should volunteer guest memory to KSM.
//
// Default on: the sandboxes on one host are the same image driven by one user,
// which is exactly the case KSM pays for. Set CS_SANDBOX_NO_KSM=1 to opt out —
// worth doing when sandboxes belong to different trust domains, since page
// dedup is a documented side channel and the measured benefit across unrelated
// guests is only the shared base image anyway.
func ksmEnabled() bool { return os.Getenv("CS_SANDBOX_NO_KSM") == "" }
