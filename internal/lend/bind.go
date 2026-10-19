package lend

import (
	"context"
	"net"
	"slices"
	"sync"
	"time"
)

// Binding a loan to its sandbox (R152a).
//
// A loan token is a bearer value, and a sandbox leaks it the ordinary ways: a
// credential file a peer can read, an environment dumped into a log, a repo two
// members share. Without a binding, any member of the group holding it spends
// the loan, and the log names the wrong sandbox. So a lender on a group's
// network honours a loan only from its own sandbox's address, and finds that
// address the way the sandbox's peers do: by its name, on the group's own DNS
// (R40). That answer is live, so a container that came back on a new address
// after a restart is found there, and a sandbox recreated under the same name
// holds a new token anyway.
//
// It binds the token to an address, and an address on a bridge is not proof of
// identity. A member that can forge its source address or poison the lender's
// ARP, which NET_RAW in a container and root in a microVM both allow, can pass
// as another member. What it stops is a token copied out of one sandbox being
// spent from another without that.
//
// A lender run on the host binds nothing. Every sandbox reaches it at the host's
// own address, so it cannot tell two of them apart.

// bindFresh is how long an answer is trusted for a caller it matched. Short,
// because a stopped container's address can be handed to another one.
const bindFresh = 2 * time.Second

// bindRetry bounds how often a caller that did not match sends the lender back
// to the DNS. A restarted sandbox is found on its first call, and a member
// replaying a stolen token costs one lookup per interval rather than one per
// request.
const bindRetry = 250 * time.Millisecond

// bindLookup is how long one lookup may take before the caller is refused.
const bindLookup = 2 * time.Second

// whereabouts is where one member was last found.
type whereabouts struct {
	mu  sync.Mutex
	ips []net.IP
	at  time.Time
}

// fromItsSandbox reports whether a loan was presented from its own sandbox.
func (s *Server) fromItsSandbox(ctx context.Context, loan Loan, remote string) bool {
	if s.cfg.Callers != CallersNetwork {
		return true
	}
	// The table holds every group's loans, and a name resolves on this network
	// only. Another group's loan would resolve to whichever member here shares
	// its sandbox's name.
	if loan.Group == "" || loan.Group != s.cfg.Group || loan.Name == "" {
		return false
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	s.bindMu.Lock()
	if s.found == nil {
		s.found = map[string]*whereabouts{}
	}
	w := s.found[loan.Name]
	if w == nil {
		w = &whereabouts{}
		s.found[loan.Name] = w
	}
	s.bindMu.Unlock()

	// Per member, held across the lookup, so a burst from one sandbox waits for
	// one answer and the other members are not held up by it.
	w.mu.Lock()
	defer w.mu.Unlock()
	asked := !w.at.IsZero()
	age := s.cfg.Now().Sub(w.at)
	if asked && age < bindFresh && slices.ContainsFunc(w.ips, ip.Equal) {
		return true
	}
	if asked && age < bindRetry {
		return false
	}
	lookup := s.cfg.Addrs
	if lookup == nil {
		lookup = fabricAddrs
	}
	ctx, cancel := context.WithTimeout(ctx, bindLookup)
	defer cancel()
	w.ips, _ = lookup(ctx, loan.Name)
	w.at = s.cfg.Now()
	return slices.ContainsFunc(w.ips, ip.Equal)
}

// fabricAddrs finds a member of this lender's network by its bare name.
//
// Two resolvers hold the names, and both are asked at once. aardvark is this
// container's own and knows every container on the network. The fabric's
// dnsmasq knows the microVMs, and exists only once a group has had one, so
// waiting on it first would cost a podman-only group a timeout per lookup. Only
// an address on this lender's own networks counts: aardvark hands a name it does
// not hold to the host's resolvers, and their answer is not a member.
func fabricAddrs(ctx context.Context, name string) ([]net.IP, error) {
	nets := bridgeNets()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		ips []net.IP
		err error
	}
	answers := make(chan answer, 2)
	ask := func(r *net.Resolver, host string) {
		ips, err := r.LookupIP(ctx, "ip", host)
		answers <- answer{onNets(ips, nets), err}
	}
	asked := 1
	go ask(net.DefaultResolver, name)
	if dns := fabricDNS(nets); dns != "" {
		asked++
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(dns, "53"))
		}}
		// Absolute, so the search list does not send it round aardvark first.
		go ask(r, name+".")
	}
	var err error
	for range asked {
		a := <-answers
		if len(a.ips) > 0 {
			return a.ips, nil
		}
		if a.err != nil {
			err = a.err
		}
	}
	return nil, err
}

// bridgeNets are the IPv4 networks this lender has an interface on, leaving
// out loopback.
func bridgeNets() []*net.IPNet {
	var out []*net.IPNet
	for _, n := range ownNets() {
		if ip4 := n.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
			out = append(out, n)
		}
	}
	return out
}

func onNets(ips []net.IP, nets []*net.IPNet) []net.IP {
	var out []net.IP
	for _, ip := range ips {
		if slices.ContainsFunc(nets, func(n *net.IPNet) bool { return n.Contains(ip) }) {
			out = append(out, ip)
		}
	}
	return out
}

// fabricDNS is the fabric's dnsmasq on the first of these networks: <prefix>.53
// of its /24. The engine places it there (internal/fcnet, Fabric.DNSIP), and
// this package may not import that one.
func fabricDNS(nets []*net.IPNet) string {
	if len(nets) == 0 {
		return ""
	}
	ip := slices.Clone(nets[0].IP.To4())
	ip[3] = fabricDNSOctet
	return ip.String()
}

// fabricDNSOctet is the last octet of the fabric's dnsmasq address.
const fabricDNSOctet = 53
