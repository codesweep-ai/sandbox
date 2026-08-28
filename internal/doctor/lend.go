package doctor

import (
	"fmt"
	"net"
	"strings"
)

// Credential lending checks.
//
// The failure this exists for is a quiet one. Every way of getting the wiring
// wrong — a lender that is not running, one bound to loopback, a host login
// that expired, a cs-vcr pointed at the wrong place — reaches the person as the
// same thing: an agent inside a sandbox announcing that it is not signed in.
// So the chain is walked here, where the answer can name the hop that is dark.

// LendState is what the CLI knows about lending on this host. doctor renders it
// rather than discovering it, because the paths and the loan records belong to
// the caller.
type LendState struct {
	// Sandboxes is how many sandboxes currently borrow a credential.
	Sandboxes int
	// Addr is where a lender is listening, or "" when none is.
	Addr string
	// Recorded is an address a stale record claims, when nothing answers there.
	Recorded string
	// Credentials are the slots the live loans name, with the reason each one
	// cannot be read right now (empty reason: it can).
	Credentials []CredentialCheck
	// Upstreams are the endpoints a sandbox's model calls pass through on the
	// way to a provider, and whether each answers.
	Upstreams []UpstreamCheck
}

// CredentialCheck is one lendable credential and whether the host can supply it.
type CredentialCheck struct {
	Slot   string
	Source string
	Err    string
}

// UpstreamCheck is one endpoint the lender forwards a slot's traffic to on the
// way to a provider: a recorder, or a gateway.
//
// It is dialled by the lender rather than by the sandbox, so it only has to
// answer on this host — which is what the failure line below has to say, since
// an address that works from a terminal here is not evidence either way.
type UpstreamCheck struct {
	Sandbox string
	URL     string
	// Slot is the credential whose traffic goes there. An upstream steers one
	// slot, because it is a property of that slot's loan.
	Slot string
	Err  string
}

// lendGroup renders the lending chain. Nothing is lent on most hosts, so the
// group appears only when there is something to say.
func lendGroup(s LendState) (Group, bool) {
	if s.Sandboxes == 0 && s.Addr == "" && s.Recorded == "" {
		return Group{}, false
	}
	g := Group{Title: "credential lending (sandboxes borrowing your logins and keys)"}

	switch {
	case s.Addr != "":
		g.add(OK, fmt.Sprintf("lender listening on %s, lending to %s", s.Addr, sandboxCount(s.Sandboxes)))
		if isLoopback(s.Addr) {
			// The whole trap in one line: the address answers on the host, so
			// this looks like a working lender right up to the first model call.
			g.add(NO, "that address is loopback, and no sandbox can reach it — a sandbox arrives on this host's "+
				"ordinary side. Restart it with:  cs-sandbox lender --addr 0.0.0.0:2500")
		}
	case s.Recorded != "":
		g.add(NO, fmt.Sprintf("a lender is recorded at %s but nothing answers there — the next create starts a new one, "+
			"or start it now:  cs-sandbox lender", s.Recorded))
	default:
		g.add(NO, sandboxesAre(s.Sandboxes)+" borrowing a credential and no lender is running — start it:  cs-sandbox lender")
	}

	for _, c := range s.Credentials {
		if c.Err == "" {
			g.add(OK, fmt.Sprintf("%s: lendable, from %s", c.Slot, c.Source))
			continue
		}
		g.add(NO, fmt.Sprintf("%s: %s", c.Slot, c.Err))
	}
	for _, c := range s.Upstreams {
		if c.Err == "" {
			g.add(OK, fmt.Sprintf("%s sends its %s traffic through %s", c.Sandbox, c.Slot, c.URL))
			continue
		}
		g.add(NO, fmt.Sprintf("%s sends its %s traffic to %s, which does not answer: %s\n"+
			"      the lender dials it from this host, so it needs to be listening here",
			c.Sandbox, c.Slot, c.URL, c.Err))
	}
	return g, true
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() || strings.EqualFold(host, "localhost")
}

// sandboxCount says "1 sandbox" / "3 sandboxes" — the package's own plural
// helper appends an s, which is wrong for this word.
func sandboxCount(n int) string {
	if n == 1 {
		return "1 sandbox"
	}
	return fmt.Sprintf("%d sandboxes", n)
}

// sandboxesAre carries the verb too, so a line reading "1 sandbox borrow" never
// reaches anybody.
func sandboxesAre(n int) string {
	if n == 1 {
		return "1 sandbox is"
	}
	return fmt.Sprintf("%d sandboxes are", n)
}
