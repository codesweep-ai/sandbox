package engine

import (
	"context"
	"strings"
	"testing"
	"time"

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
			if got := PastaIsSetUp(context.Background(), r, 0); got != tc.want {
				t.Errorf("PastaIsSetUp = %v, want %v when the probe returns %s", got, tc.want, tc.code)
			}
		})
	}
}

// answersOnCall is a runner that starts refusing and then answers, which is
// what a namespace podman is still bringing pasta up in looks like.
type answersOnCall struct {
	calls int
	after int
}

func (a *answersOnCall) Run(context.Context, run.Opts, ...string) (run.Result, error) {
	a.calls++
	if a.calls < a.after {
		return run.Result{Stdout: "000"}, nil
	}
	return run.Result{Stdout: "200"}, nil
}

// A probe that has to create the namespace itself waits for it.
//
// This is the doctor's case, and it is why the budget belongs to the caller.
// Podman starts pasta in a namespace it has just made, and it does so
// asynchronously, so the first ask can arrive before anything is listening.
// Asked once, a slow host reports a fault it does not have — measured as six CI
// legs failing a check that every one of them then went on to satisfy.
func TestPastaIsSetUpWaitsOutASlowNamespace(t *testing.T) {
	slow := &answersOnCall{after: 3}
	if !PastaIsSetUp(context.Background(), slow, 5*time.Second) {
		t.Errorf("a namespace that answered on ask %d was reported broken", slow.after)
	}

	// And with no budget it is the single attempt create relies on: create asks
	// once the fabric is up, where waiting could not change the answer.
	never := &answersOnCall{after: 1 << 30}
	if PastaIsSetUp(context.Background(), never, 0) {
		t.Error("a host that never answers must not report pasta")
	}
	if never.calls != 1 {
		t.Errorf("asked %d times with no budget, want exactly 1", never.calls)
	}
}

// TestPastaIsSetUpProbesThroughTheRootlessNetns: the namespace a microVM's tap
// lives in is the one that has to reach the host, and it is not this process's.
// A probe that asked from here would pass on a host where every guest times out.
func TestPastaIsSetUpProbesThroughTheRootlessNetns(t *testing.T) {
	r := run.NewFake().OnStdout("unshare", "200")
	PastaIsSetUp(context.Background(), r, 0)
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
