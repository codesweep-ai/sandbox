package lend

import (
	"fmt"
	"net"
	"strconv"
)

// DefaultPort is where a lender run BY HAND on the host listens. It sits above
// the range R42 allocates from (2200-2399, sandboxes that asked to be
// published), so a lender and a sandbox can never claim the same port.
//
// The lender `create` starts binds no host port at all: it runs on the group's
// own network (SPEC R52b), where this number is namespace-local and collides
// with nothing. What used to live here was the supervision for a detached host
// lender — a pid file, a recorded address, a probe to tell a live one from a
// stale record. A container is supervised by the engine that runs it, so none
// of that survived the move.
const DefaultPort = 2500

// DefaultBind is the listen address.
//
// Not loopback, and it cannot be: a caller reaches the lender on the ordinary
// side of wherever it runs, and a server bound to 127.0.0.1 refuses that
// connection (R52, R152).
var DefaultBind = fmt.Sprintf("0.0.0.0:%d", DefaultPort)

// ProbeAddr turns a bind address into one that can be dialled. A wildcard bind
// is not a destination, so a health check goes to loopback on the same port —
// which for a lender on the fabric means from inside its own container, the
// only place its loopback exists.
func ProbeAddr(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return bind
}

// GuestURL is the base URL a sandbox is given: where the lender is as seen from
// inside, and the port it listens on. guestHost is whatever a guest reaches it
// by, which is GuestName on the group's own network and, for a lender run on
// the host, whatever name that host answers to from a sandbox.
func GuestURL(guestHost, bind string) string {
	_, port, err := net.SplitHostPort(bind)
	if err != nil {
		port = strconv.Itoa(DefaultPort)
	}
	return "http://" + net.JoinHostPort(guestHost, port)
}
