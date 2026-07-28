// Package ports allocates host SSH ports for instances. The pool is split so
// podman-published ports never collide with the per-VM host ports the
// firecracker engine binds. Allocation scans a reserved set (spanning both
// engines) and probes each candidate live.
package ports

import (
	"fmt"
	"net"
	"time"
)

// Default port range constants (overridable via env at the CLI layer).
const (
	Min   = 2200
	Split = 2300 // [Min,Split) = containers; [Split,Max] = VMs
	Max   = 2399
)

// Dialer reports whether something is listening on 127.0.0.1:port. Injectable
// for tests; callers pass LoopbackBusy.
type Dialer func(port int) bool

// LoopbackBusy is the production Dialer: a successful connect means "in use".
func LoopbackBusy(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Alloc returns the first port in [lo,hi] that is neither reserved nor
// currently listening.
//
// Both checks are load-bearing. The reserved set covers instances that are
// stopped (nothing is listening for them right now), while the live probe
// covers ports held by anything the reserved set cannot see — a forwarder from
// a sandbox under a different CS_SANDBOX_HOME, or an unrelated program that
// happens to sit in the range. Skipping the probe hands out a port someone else
// already answers on, which silently routes `ssh <name>` to the wrong host.
func Alloc(lo, hi int, reserved map[int]bool, busy Dialer) (int, error) {
	for p := lo; p <= hi; p++ {
		if reserved[p] || busy(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free SSH port in range %d-%d", lo, hi)
}
