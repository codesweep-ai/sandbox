package doctor

import (
	"fmt"
	"strings"
	"time"
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
	// Renewer is the host-side process that keeps lent logins from expiring.
	Renewer RenewerCheck
}

// RenewerCheck is the credential renewer: whether the thing that stops a lent
// login going stale is actually running and actually working.
//
// It gets its own lines because a renewer fails quietly by nature. It spends a
// real turn through the owning client, and a turn that succeeds while renewing
// nothing looks identical to one that worked — so "running" is never the whole
// answer, and neither is the absence of an error.
type RenewerCheck struct {
	// Wanted is true when a live loan names a credential that expires, which is
	// the only case where a renewer should be running at all.
	Wanted  bool
	Running bool
	// Reported and LastReport are how long ago the renewer published its status.
	// A renewer that is running and no longer reporting is wedged, and that is the
	// one state it cannot report for itself.
	Reported   bool
	LastReport time.Duration
	Err        string
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

	// Expires is when the credential goes stale, zero for one that carries no
	// expiry. Reported even when nothing is wrong: "lendable" and "lendable for
	// another nine minutes" are different answers to the question somebody
	// starting a long unattended run is actually asking.
	Expires time.Time
	// RefreshDeadline is when renewing stops being able to help, and only an
	// interactive sign-in can. A second clock, and it does not move when the
	// credential is refreshed — so a host can be renewing happily every eight
	// hours and still be days from needing a person.
	RefreshDeadline time.Time
	// RenewErr is why the renewer last failed to renew this one, including the
	// case where its command succeeded and the expiry did not move.
	RenewErr string
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

	// The renewer, before the credentials it keeps: when it is missing, every
	// expiry line below it is a countdown rather than a status.
	switch {
	case s.Renewer.Err != "":
		g.add(NO, "the credential renewer's status cannot be read — "+s.Renewer.Err)
	case s.Renewer.Wanted && !s.Renewer.Running:
		g.add(NO, "a lent login expires and no renewer is running to renew it — "+
			"a run longer than that login's life will lose its credential partway through\n"+
			"      the next create starts one, or run 'cs-sandbox renewer' yourself")
	case s.Renewer.Running && s.Renewer.Reported && s.Renewer.LastReport > renewerStale:
		// Running but silent. Worse than not running, because the pidfile makes
		// every other check look covered.
		g.add(NO, fmt.Sprintf("the credential renewer is running but has not reported for %s — "+
			"treat it as not covering anything\n      its log is the place to look", ShortDur(s.Renewer.LastReport)))
	case s.Renewer.Running:
		g.add(OK, "credential renewer running, renewing lent logins before they expire")
	}

	for _, c := range s.Credentials {
		if c.Err != "" {
			g.add(NO, fmt.Sprintf("%s: %s", c.Slot, c.Err))
			continue
		}
		g.add(OK, fmt.Sprintf("%s: lendable, from %s%s", c.Slot, c.Source, expiresIn(c.Expires)))
		if c.RenewErr != "" {
			g.add(NO, fmt.Sprintf("%s: the renewer could not renew this login — %s", c.Slot, indented(c.RenewErr)))
		}
		// The outer clock. Said separately and only when it is close, because it
		// is a different remedy from everything else here: no amount of renewing
		// reaches past it, and the fix is a person signing in.
		if !c.RefreshDeadline.IsZero() {
			left := time.Until(c.RefreshDeadline)
			when := c.RefreshDeadline.Local().Format("Mon 2 Jan 15:04")
			switch {
			case left < loginSoon:
				g.add(NO, fmt.Sprintf("%s: renewing stops working in %s (%s) — refreshing does not extend that\n"+
					"      sign in again on the host before then, or an unattended run will stop and nothing can renew it",
					c.Slot, ShortDur(left), when))
			case left < loginEventually:
				// Graded rather than silent until it is urgent: this is the
				// number that bounds how long an unattended run can be, so
				// somebody planning one wants it before it becomes a problem.
				g.add(HM, fmt.Sprintf("%s: renewing stops working in %s (%s); a run cannot outlast that without somebody signing in",
					c.Slot, ShortDur(left), when))
			}
		}
	}
	for _, c := range s.Upstreams {
		if c.Err == "" {
			g.add(OK, fmt.Sprintf("%s sends its %s traffic through %s", c.Sandbox, c.Slot, c.URL))
			continue
		}
		g.add(NO, fmt.Sprintf("%s sends its %s traffic to %s, which the lender cannot connect to: %s\n"+
			"      the lender dials it from its container on the group's network, so a service on this "+
			"host has to be named host.containers.internal rather than 127.0.0.1",
			c.Sandbox, c.Slot, c.URL, c.Err))
	}
	return g, true
}

// indented re-indents the continuation lines of a message that came from
// somewhere else, so a multi-line reason from the renewer lines up with the ones
// written here rather than falling back to the left margin.
func indented(s string) string {
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "      " + strings.TrimLeft(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// renewerStale is how long a renewer may go without publishing before its silence
// is reported. Its own idle interval is five minutes, so three of them is quiet
// enough to mean something is wrong rather than nothing was due.
const renewerStale = 15 * time.Minute

// loginSoon is how close the end of the refresh chain has to be before it is
// worth interrupting somebody about. Two days is enough warning to sign in at a
// convenient moment rather than in the middle of a run.
const loginSoon = 48 * time.Hour

// loginEventually is when it becomes worth mentioning at all. It matches the
// window create reports, so the two commands do not disagree about when this
// stops being somebody's business.
const loginEventually = 7 * 24 * time.Hour

// expiresIn is the ", expires in 3h12m" tail on a credential line, or nothing at
// all for a credential that does not expire.
func expiresIn(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	left := time.Until(t)
	if left <= 0 {
		return ", expired"
	}
	return ", expires in " + ShortDur(left)
}

// ShortDur is a duration a person reads at a glance: 3h12m, 42m, 20s.
//
// Exported because create reports these same two clocks while somebody is still
// standing there, and one formatter with one rounding rule is the point — the
// first version of this was duplicated and the two copies disagreed.
//
// Rounded to the nearest minute rather than truncated. Truncating is defensible
// and reads as a bug: a credential with twenty minutes left renders as "19m" the
// moment a microsecond has passed, and the person checking it sees an
// off-by-one. Rounding after the split also carries correctly, so 1h59m40s is
// 2h00m and not 1h60m.
func ShortDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	// Days past two of them. The clocks this reports run to ten days and twenty,
	// and "239h41m" is a number a reader has to divide before it means anything.
	if d >= 48*time.Hour {
		h := int(d.Round(time.Hour).Hours())
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	}
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
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
