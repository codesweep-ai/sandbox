package doctor

import (
	"strings"
	"testing"
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
