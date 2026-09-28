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

// TestStaleAddrComparesTheNamespaceWithTheHost: pasta copies the host's
// addresses into the namespace once, so after the host changes networks the
// namespace keeps the old one (SBX-088). Only an interface the host has is
// compared: the namespace's own bridges are not the host's.
func TestStaleAddrComparesTheNamespaceWithTheHost(t *testing.T) {
	ns := "2: wlp0s20f3    inet 192.168.40.105/24 brd 192.168.40.255 scope global dynamic wlp0s20f3\n" +
		"3: podman1    inet 10.89.4.1/24 brd 10.89.4.255 scope global podman1\n"
	for _, tc := range []struct {
		name string
		host map[string][]string
		want string
	}{
		{"the host moved to another network", map[string][]string{"wlp0s20f3": {"192.168.5.109"}},
			"192.168.40.105 on wlp0s20f3, where the host now has 192.168.5.109"},
		{"the host lost its address", map[string][]string{"wlp0s20f3": nil},
			"192.168.40.105 on wlp0s20f3, which has no address on the host now"},
		{"the host has the same address", map[string][]string{"wlp0s20f3": {"192.168.40.105"}}, ""},
		{"a bridge only the namespace has", map[string][]string{"lo": {"127.0.0.1"}}, ""},
	} {
		if got := staleAddr(ns, tc.host); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRootlessBusyLeavesOnlyIdleFabrics: a rebuild takes the namespace's network
// from everything in it, so it goes ahead only where nothing but the fabrics'
// keepalives runs there.
func TestRootlessBusyLeavesOnlyIdleFabrics(t *testing.T) {
	was := microVMRunning
	t.Cleanup(func() { microVMRunning = was })
	microVMRunning = func() bool { return false }
	for _, tc := range []struct {
		name, ps string
		vm       bool
		want     string
	}{
		{"idle keepalives", "cs-sandbox-net-keepalive 1\ncs-sandbox-g-keepalive 1\n", false, ""},
		{"a sandbox", "cs-sandbox-net-keepalive 1\napi.default <no value>\n", false, "the container api.default"},
		{"a microVM", "cs-sandbox-net-keepalive 1\n", true, "a microVM"},
	} {
		f := run.NewFake()
		f.OnStdout("podman ps", tc.ps)
		if got := rootlessBusy(context.Background(), f, tc.vm); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	microVMRunning = func() bool { return true }
	if got := rootlessBusy(context.Background(), run.NewFake(), false); got != "a microVM" {
		t.Errorf("a microVM of another root: got %q", got)
	}
}

// TestStaleErrorSaysWhatHoldsTheNetwork: the error names the address the host
// no longer has, and what has to stop before the network can be set up again,
// rather than blaming the podman version (SBX-088).
func TestStaleErrorSaysWhatHoldsTheNetwork(t *testing.T) {
	stale := "192.168.40.105 on wlp0s20f3, where the host now has 192.168.5.109"
	err := staleError(stale, "the container api.default").Error()
	for _, want := range []string{stale, "changed networks", HostReachableIP, "the container api.default still does"} {
		if !strings.Contains(err, want) {
			t.Errorf("error lacks %q: %s", want, err)
		}
	}
	if strings.Contains(err, "podman 5.0") {
		t.Errorf("error blames the podman version: %s", err)
	}
	if err := staleError(stale, "").Error(); !strings.Contains(err, "did not help") {
		t.Errorf("a rebuild that did not help says so: %s", err)
	}
}
