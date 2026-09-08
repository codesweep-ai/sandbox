package lend

import (
	"net"
	"testing"
)

// The two homes a lender has, and the one sentence that differs between them.
//
// As a host process it is reached at the host's own address, so its callers are
// this machine and nothing else — a neighbour on the same LAN is exactly what
// R152 refuses. In a container on a group's network its callers ARE the
// neighbours: the sandboxes on that bridge, which is the only network it has an
// interface on.
func TestCallersPolicyDrawsTheBoundary(t *testing.T) {
	_, lan, _ := net.ParseCIDR("192.168.1.10/24")
	lan.IP = net.ParseIP("192.168.1.10")
	_, bridge, _ := net.ParseCIDR("10.89.1.54/24")
	bridge.IP = net.ParseIP("10.89.1.54")
	restore := ownNets
	ownNets = func() []*net.IPNet { return []*net.IPNet{lan, bridge} }
	t.Cleanup(func() { ownNets = restore })

	cases := []struct {
		name    string
		callers Callers
		remote  string
		want    bool
	}{
		{"a host lender answers itself", CallersHost, "192.168.1.10:5000", true},
		{"a host lender refuses the machine next to it", CallersHost, "192.168.1.11:5000", false},
		{"a host lender refuses a sandbox address it does not hold", CallersHost, "10.89.1.7:5000", false},
		{"a fabric lender answers a sandbox on its bridge", CallersNetwork, "10.89.1.7:5000", true},
		{"a fabric lender refuses another network", CallersNetwork, "10.89.2.7:5000", false},
		{"loopback is always in", CallersHost, "127.0.0.1:5000", true},
		{"loopback is in for a fabric lender too", CallersNetwork, "127.0.0.1:5000", true},
		{"any means any", CallersAny, "203.0.113.5:5000", true},
		{"a caller with no address at all is refused", CallersHost, "not-an-address", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{Callers: c.callers}}
			if got := s.peerAllowed(c.remote); got != c.want {
				t.Errorf("peerAllowed(%q) under %q = %v, want %v", c.remote, c.callers, got, c.want)
			}
		})
	}
}

// A policy typed wrong is refused where it is typed. Falling back to the strict
// one would be safe and silent, which is how a lender ends up refusing every
// sandbox for a reason nobody can see.
func TestAnUnknownCallersPolicyIsAnError(t *testing.T) {
	if _, err := ParseCallers("everyone"); err == nil {
		t.Error("--callers everyone should not be accepted")
	}
	for _, in := range []string{"", "host", "network", "any"} {
		if _, err := ParseCallers(in); err != nil {
			t.Errorf("--callers %q: %v", in, err)
		}
	}
}
