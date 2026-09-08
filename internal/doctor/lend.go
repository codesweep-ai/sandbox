package doctor

import "fmt"

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
	// Lenders is one entry per group that has something borrowed: a lender runs
	// on the group's own network, not on the host, so "is the lender up" is a
	// question per group rather than per machine.
	Lenders []LenderCheck
	// Credentials are the slots the live loans name, with the reason each one
	// cannot be read right now (empty reason: it can).
	Credentials []CredentialCheck
	// Upstreams are the endpoints a sandbox's model calls pass through on the
	// way to a provider, and whether each answers.
	Upstreams []UpstreamCheck
}

// LenderCheck is one group's lender: where a sandbox in that group addresses
// it, and why it is not serving when it is not.
type LenderCheck struct {
	Group string
	// Where is the base URL a sandbox uses, which is a name on the group's
	// network rather than an address on this host.
	Where string
	Err   string
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
	if s.Sandboxes == 0 && len(s.Lenders) == 0 {
		return Group{}, false
	}
	g := Group{Title: "credential lending (sandboxes borrowing your logins and keys)"}

	if len(s.Lenders) == 0 {
		g.add(NO, sandboxesAre(s.Sandboxes)+" borrowing a credential and no lender is running — "+
			"the next create starts one on the group's network")
	}
	for _, l := range s.Lenders {
		if l.Err == "" {
			g.add(OK, fmt.Sprintf("group %s: lender answering at %s, lending to %s",
				l.Group, l.Where, sandboxCount(s.Sandboxes)))
			continue
		}
		// A lender that is not answering fails the same way from inside a
		// sandbox as an expired login does, so the group is named: on a host
		// running several, the one that is dark is the thing to say.
		g.add(NO, fmt.Sprintf("group %s: nothing is answering at %s — %s\n"+
			"      the next create in that group starts one", l.Group, l.Where, l.Err))
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
			"      the lender dials it from its container on the group's network, so a service on this "+
			"host has to be named host.containers.internal rather than 127.0.0.1",
			c.Sandbox, c.Slot, c.URL, c.Err))
	}
	return g, true
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
