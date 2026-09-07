package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestPastaIsSetUpIsAnswerFromTheHostItself: the check is a request that either
// arrives or does not. Nothing here is inferred from a version, a setting or an
// interface name, each of which was wrong on a real host: podman reports the
// stack it is configured for rather than the one it runs, and a podman below
// 5.0 has no such field at all.
func TestPastaIsSetUpIsAnswerFromTheHostItself(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		want bool
	}{
		{"the host answers", "200", true},
		{"nothing answers", "000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run.NewFake().OnStdout("unshare", tc.code)
			if got := PastaIsSetUp(context.Background(), r); got != tc.want {
				t.Errorf("PastaIsSetUp = %v, want %v when the probe returns %s", got, tc.want, tc.code)
			}
		})
	}
}

// TestPastaIsSetUpProbesThroughTheRootlessNetns: the namespace a microVM's tap
// lives in is the one that has to reach the host, and it is not this process's.
// A probe that asked from here would pass on a host where every guest times out.
func TestPastaIsSetUpProbesThroughTheRootlessNetns(t *testing.T) {
	r := run.NewFake().OnStdout("unshare", "200")
	PastaIsSetUp(context.Background(), r)
	for _, argv := range r.Calls {
		if strings.Contains(strings.Join(argv, " "), "--rootless-netns") {
			return
		}
	}
	t.Errorf("the probe did not go through podman's rootless netns: %v", r.Calls)
}

// TestNoPastaSaysWhatIsRequired: one sentence, and it has to carry the address
// that failed and the requirement that explains it. What it replaces is a guest
// that boots, reaches nothing, and times out ten seconds later inside an agent.
func TestNoPastaSaysWhatIsRequired(t *testing.T) {
	msg := ErrNoPasta.Error()
	for _, want := range []string{HostReachableIP, "podman 5.0", "pasta", "passt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrNoPasta does not mention %q:\n%s", want, msg)
		}
	}
}
