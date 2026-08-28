package doctor

import (
	"strings"
	"testing"
)

// An upstream that does not answer is the failure this group exists to name,
// and the remedy has to say who dials it. The LENDER does, from this host — so
// an address that answers in a terminal here is evidence, and a reader told
// instead to check what a sandbox can reach would go and change the wrong one.
func TestLendGroupNamesTheLenderAsTheDiallerOfADarkUpstream(t *testing.T) {
	g, ok := lendGroup(LendState{
		Sandboxes: 2,
		Addr:      "0.0.0.0:2500",
		Upstreams: []UpstreamCheck{
			{Sandbox: "recording", URL: "http://127.0.0.1:8080/c/anthropic/build",
				Slot: "claude", Err: "connection refused"},
			{Sandbox: "working", URL: "http://127.0.0.1:8080/c/openai/build", Slot: "openai"},
		},
	})
	if !ok {
		t.Fatal("a host lending to two sandboxes reported no lending group")
	}
	text := checksText(g.Checks)
	for _, want := range []string{
		"recording sends its claude traffic to http://127.0.0.1:8080/c/anthropic/build",
		"the lender dials it from this host",
		"working sends its openai traffic through http://127.0.0.1:8080/c/openai/build",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report is missing %q:\n%s", want, text)
		}
	}
}
