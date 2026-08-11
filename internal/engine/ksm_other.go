//go:build !linux

package engine

// KSM is Linux-only, and so is the firecracker engine — this file exists so the
// package still builds on the hosts that only ever use Podman.
func withMemoryMerge(_ bool, launch func() error) error { return launch() }

func ksmEnabled() bool { return false }
