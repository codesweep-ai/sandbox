package store

import (
	"context"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"base", "b1", "a.b-c_d", "X9"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "-lead", ".dot", "has space", "a/b", strings.Repeat("a", 201)} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) should fail", bad)
		}
	}
}

func TestHelperRunOrder(t *testing.T) {
	m := Manager{Image: "img:1"}
	argv := m.helperRun("base", "--entrypoint", "/bin/bash")
	// The image must be the LAST element (post-image command is appended by callers),
	// and --entrypoint must precede it.
	if argv[len(argv)-1] != "img:1" {
		t.Fatalf("image must be last pre-command arg: %v", argv)
	}
	joined := ""
	for _, a := range argv {
		joined += a + " "
	}
	if idxOf(argv, "--entrypoint") > idxOf(argv, "img:1") {
		t.Errorf("--entrypoint must come before the image: %v", argv)
	}
	if idxOf(argv, "cs-sandbox-shared-base:/seed") < 0 {
		t.Errorf("store must be mounted at /seed: %v", argv)
	}
	// The seeding engine must be set up the way a sandbox's nested engine is, so the
	// ids it writes into the store are the ids every reader resolves (see helperRun).
	for _, want := range []string{"--userns=keep-id", "--cap-add=SYS_ADMIN", "--cap-add=SETFCAP", "unmask=ALL"} {
		if !contains(joined, want) {
			t.Errorf("store helper must match a sandbox's nested-rootless setup, missing %s: %v", want, argv)
		}
	}
}

// TestList filters `podman volume ls` output to the shared-store prefix and
// strips it, backing the `stores` command. It also pins the query argv.
func TestList(t *testing.T) {
	f := run.NewFake()
	f.OnStdout("volume ls", "cs-sandbox-shared-base\ncs-sandbox-shared-tools\n\n")
	m := Manager{Runner: f}

	got := m.List(context.Background())
	want := []string{"base", "tools"}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// The filter must be scoped to the shared-store prefix, not all volumes.
	if !f.Contains("--filter name=cs-sandbox-shared-") {
		t.Errorf("List must filter by the shared-store prefix: %v", f.Rendered())
	}
}

// TestListEmpty: no matching volumes -> empty slice (the `stores` command then
// prints its "no shared stores" hint).
func TestListEmpty(t *testing.T) {
	m := Manager{Runner: run.NewFake()}
	if got := m.List(context.Background()); len(got) != 0 {
		t.Errorf("List(empty) = %v, want none", got)
	}
}

func idxOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
