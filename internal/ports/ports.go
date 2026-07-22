// Package ports allocates host SSH ports for instances. The pool is split so
// podman-published ports never collide with the per-VM host ports the
// firecracker engine binds. Allocation scans a reserved set
// (spanning both engines) then, for containers, probes the port live.
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
// for tests; the default probes loopback.
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

// Alloc returns the first port in [lo,hi] not in reserved and (unless vmMode)
// not currently listening. A VM's host port is bound later (a per-VM socat), so
// vmMode skips the live probe.
func Alloc(lo, hi int, vmMode bool, reserved map[int]bool, busy Dialer) (int, error) {
	if busy == nil {
		busy = LoopbackBusy
	}
	for p := lo; p <= hi; p++ {
		if reserved[p] {
			continue
		}
		if !vmMode && busy(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("no free SSH port in range %d-%d", lo, hi)
}
