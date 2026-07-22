package repo

import (
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/state"
)

func TestSelect(t *testing.T) {
	in := &state.Instance{RepoClones: []state.RepoClone{
		{Source: "/a", Dir: "api", Branch: "cs-sandbox/x"},
		{Source: "/b", Dir: "web", Branch: "cs-sandbox/x"},
	}}
	if got := Select(in, ""); len(got) != 2 {
		t.Errorf("Select(all) = %d, want 2", len(got))
	}
	got := Select(in, "web")
	if len(got) != 1 || got[0].Dir != "web" {
		t.Errorf("Select(web) = %+v", got)
	}
	if got := Select(in, "nope"); len(got) != 0 {
		t.Errorf("Select(nonexistent) should be empty, got %+v", got)
	}
}

func TestTransportRemote(t *testing.T) {
	tr := Transport{Host: hostenv.Host{User: "dev"}, Name: "x", Port: 2201}
	if got := tr.remote("api"); got != "dev@127.0.0.1:api" {
		t.Errorf("remote = %q, want dev@127.0.0.1:api", got)
	}
}
