package doctor

import (
	"strings"
	"testing"
	"time"
)

// An upstream that does not answer is the failure this group exists to name,
// and the remedy has to say who dials it. The LENDER does, from its container
// on the group's network — so an address that answers in a terminal here is NOT
// evidence, and a reader who fixes what the host can reach fixes the wrong hop.
func TestLendGroupNamesTheLenderAsTheDiallerOfADarkUpstream(t *testing.T) {
	g, ok := lendGroup(LendState{
		Sandboxes: 2,
		Lenders:   []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Upstreams: []UpstreamCheck{
			{Sandbox: "recording", URL: "http://host.containers.internal:8080/c/anthropic/build",
				Slot: "claude", Err: "connection refused"},
			{Sandbox: "working", URL: "http://host.containers.internal:8080/c/openai/build", Slot: "openai"},
		},
	})
	if !ok {
		t.Fatal("a host lending to two sandboxes reported no lending group")
	}
	text := checksText(g.Checks)
	for _, want := range []string{
		"group default: lender answering at http://cs-lender:2500, lending to 2 sandboxes",
		"recording sends its claude traffic to http://host.containers.internal:8080/c/anthropic/build",
		"the lender dials it from its container on the group's network",
		"working sends its openai traffic through http://host.containers.internal:8080/c/openai/build",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report is missing %q:\n%s", want, text)
		}
	}
}

// A lender is per GROUP now, so the group has to be named. On a host running
// several, "the lender is down" without saying whose sends the reader to the
// wrong network.
func TestLendGroupNamesWhichGroupsLenderIsDark(t *testing.T) {
	g, ok := lendGroup(LendState{
		Sandboxes: 3,
		Lenders: []LenderCheck{
			{Group: "default", Where: "http://cs-lender:2500"},
			{Group: "run-b", Where: "http://cs-lender:2500", Err: "the group's lender container is not running"},
		},
	})
	if !ok {
		t.Fatal("a host with lenders reported no lending group")
	}
	text := checksText(g.Checks)
	if !strings.Contains(text, "group run-b: nothing is answering") {
		t.Errorf("the dark group is not named:\n%s", text)
	}
	if !strings.Contains(text, "group default: lender answering") {
		t.Errorf("the working group is not reported:\n%s", text)
	}
}

// Nothing borrowed and no lender: the group says nothing at all, which is the
// state on almost every host.
func TestLendGroupIsSilentWhenNothingIsLent(t *testing.T) {
	if _, ok := lendGroup(LendState{}); ok {
		t.Error("a host lending nothing should report no lending group")
	}
}

// A lent login that expires with nothing renewing it is the failure the renewer
// exists to prevent, so its absence has to be a reported problem rather than a
// missing line.
func TestLendGroupSaysWhenNothingIsKeepingALentLoginAlive(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes:   1,
		Lenders:     []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Renewer:     RenewerCheck{Wanted: true, Running: false},
		Credentials: []CredentialCheck{{Slot: "claude", Source: "/h/.cs-claude/.credentials.json", Expires: time.Now().Add(20 * time.Minute)}},
	})
	text := checksText(g.Checks)
	if !strings.Contains(text, "no renewer is running") {
		t.Errorf("a missing renewer is not reported:\n%s", text)
	}
	if !strings.Contains(text, "expires in 20m") {
		t.Errorf("the remaining life is not reported:\n%s", text)
	}
	if issues(g.Checks) == 0 {
		t.Error("a lent login with nothing renewing it did not count as an issue")
	}
}

// Running but silent is worse than not running at all: the pidfile makes every
// other line in the report look covered.
func TestLendGroupTreatsASilentRenewerAsNotCovering(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes: 1,
		Lenders:   []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Renewer:   RenewerCheck{Wanted: true, Running: true, Reported: true, LastReport: 90 * time.Minute},
	})
	text := checksText(g.Checks)
	if !strings.Contains(text, "has not reported for 1h30m") {
		t.Errorf("a wedged renewer is not reported:\n%s", text)
	}
	if !strings.Contains(text, "not covering") {
		t.Errorf("the report does not say what the silence means:\n%s", text)
	}
}

// The outer clock. A host can be renewing happily every eight hours and still be
// days away from needing a person, and the remedy shares no word with anything
// else in this group.
func TestLendGroupWarnsBeforeRenewingStopsWorking(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes: 1,
		Lenders:   []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Renewer:   RenewerCheck{Wanted: true, Running: true, Reported: true},
		Credentials: []CredentialCheck{{
			Slot: "claude", Source: "/h/.cs-claude/.credentials.json",
			Expires:         time.Now().Add(7 * time.Hour),
			RefreshDeadline: time.Now().Add(30 * time.Hour),
		}},
	})
	text := checksText(g.Checks)
	if !strings.Contains(text, "renewing stops working in") {
		t.Errorf("the end of the refresh chain is not reported:\n%s", text)
	}
	if !strings.Contains(text, "sign in again on the host") {
		t.Errorf("the report does not name the only remedy:\n%s", text)
	}
	if issues(g.Checks) == 0 {
		t.Error("an imminent re-login did not count as an issue")
	}
}

// A deadline comfortably far off is noise, not news.
func TestLendGroupIsQuietAboutADistantRefreshDeadline(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes: 1,
		Lenders:   []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Renewer:   RenewerCheck{Wanted: true, Running: true, Reported: true},
		Credentials: []CredentialCheck{{
			Slot: "claude", Source: "/h/.cs-claude/.credentials.json",
			Expires:         time.Now().Add(7 * time.Hour),
			RefreshDeadline: time.Now().Add(15 * 24 * time.Hour),
		}},
	})
	if text := checksText(g.Checks); strings.Contains(text, "renewing stops working") {
		t.Errorf("warned about a deadline two weeks out:\n%s", text)
	}
}

// Between "not worth saying" and "act now" there is the case that actually
// bounds a plan: how long an unattended run can be at all.
func TestLendGroupMentionsAMiddlingRefreshDeadlineWithoutRaisingIt(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes: 1,
		Lenders:   []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Renewer:   RenewerCheck{Wanted: true, Running: true, Reported: true},
		Credentials: []CredentialCheck{{
			Slot: "claude", Source: "/h/.cs-claude/.credentials.json",
			Expires:         time.Now().Add(7 * time.Hour),
			RefreshDeadline: time.Now().Add(5 * 24 * time.Hour),
		}},
	})
	text := checksText(g.Checks)
	if !strings.Contains(text, "cannot outlast that") {
		t.Errorf("a five-day deadline is not mentioned at all:\n%s", text)
	}
	if issues(g.Checks) != 0 {
		t.Errorf("a five-day deadline was raised as a problem:\n%s", text)
	}
}

// A renewer that spent a turn and renewed nothing has to surface, because every
// other signal about it reads as healthy.
func TestLendGroupSurfacesARenewerThatRenewedNothing(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes: 1,
		Lenders:   []LenderCheck{{Group: "default", Where: "http://cs-lender:2500"}},
		Renewer:   RenewerCheck{Wanted: true, Running: true, Reported: true},
		Credentials: []CredentialCheck{{
			Slot: "claude", Source: "/h/.cs-claude/.credentials.json",
			Expires:  time.Now().Add(3 * time.Minute),
			RenewErr: "claude ran and exited cleanly, but the claude login's expiry did not move",
		}},
	})
	text := checksText(g.Checks)
	if !strings.Contains(text, "the renewer could not renew this login") {
		t.Errorf("a failed renewal is not reported:\n%s", text)
	}
	if issues(g.Checks) == 0 {
		t.Error("a failed renewal did not count as an issue")
	}
}

func TestShortDurReadsAtAGlance(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{3*time.Hour + 12*time.Minute, "3h12m"},
		{time.Hour + 5*time.Minute, "1h05m"},
		{42 * time.Minute, "42m"},
		{20 * time.Second, "20s"},
		// Rounded, not truncated, at every scale: a fresh 240-hour token that
		// read "9d23h" a minute after it was issued would look like a bug the
		// same way "19m" did.
		{239*time.Hour + 41*time.Minute, "10d0h"},
		{240 * time.Hour, "10d0h"},
		{192 * time.Hour, "8d0h"},
		{50*time.Hour + 30*time.Minute, "2d3h"},
	} {
		if got := ShortDur(tc.in); got != tc.want {
			t.Errorf("ShortDur(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// issues counts the lines doctor would raise as problems.
func issues(cs []Check) int {
	n := 0
	for _, c := range cs {
		if c.Status == NO {
			n++
		}
	}
	return n
}
