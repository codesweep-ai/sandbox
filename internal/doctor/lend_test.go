package doctor

import (
	"strings"
	"testing"
)

// An upstream that does not answer is the failure this group exists to name,
// and the remedy differs by which side of the lender it sits on. One in front
// is dialled by the sandbox, so it has to listen where a sandbox can reach it.
// One behind is dialled by the lender, so it only has to answer on this host.
// Printing the wrong remedy sends the reader to change the wrong address.
func TestLendGroupNamesWhoDialsADarkUpstream(t *testing.T) {
	g, ok := lendGroup(LendState{
		Sandboxes: 2,
		Addr:      "0.0.0.0:2500",
		Upstreams: []UpstreamCheck{
			{Sandbox: "infront", URL: "http://host.containers.internal:8080/c/anthropic/build",
				Err: "connection refused"},
			{Sandbox: "behind", URL: "http://127.0.0.1:8080/c/anthropic/build",
				Slot: "claude", ByLender: true, Err: "connection refused"},
		},
	})
	if !ok {
		t.Fatal("a host lending to two sandboxes reported no lending group")
	}
	text := checksText(g.Checks)
	for _, want := range []string{
		"a cs-vcr on this host needs --listen 0.0.0.0",
		"the lender dials it from this host",
		"behind sends its claude traffic to http://127.0.0.1:8080/c/anthropic/build",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report is missing %q:\n%s", want, text)
		}
	}
}

// A healthy upstream says which side it is on too, because "records through"
// and "sends its traffic through" are different topologies and a reader
// checking their wiring needs to see the one they built.
func TestLendGroupSaysWhichSideAHealthyUpstreamIsOn(t *testing.T) {
	g, _ := lendGroup(LendState{
		Sandboxes: 1,
		Addr:      "0.0.0.0:2500",
		Upstreams: []UpstreamCheck{
			{Sandbox: "infront", URL: "http://vcr:8080/c/anthropic/build"},
			{Sandbox: "behind", URL: "http://127.0.0.1:8080/c/anthropic/build", Slot: "claude", ByLender: true},
		},
	})
	text := checksText(g.Checks)
	for _, want := range []string{
		"infront records through http://vcr:8080/c/anthropic/build",
		"behind sends its claude traffic through http://127.0.0.1:8080/c/anthropic/build",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report is missing %q:\n%s", want, text)
		}
	}
}
