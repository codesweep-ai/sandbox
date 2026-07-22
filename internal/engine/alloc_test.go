package engine

import (
	"context"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// TestReservedPorts unions the ports from `podman ps` labels (fake) and the
// on-disk instance state, so numbers never collide across engines.
func TestReservedPorts(t *testing.T) {
	dir := t.TempDir()
	if err := state.Save(dir, &state.Instance{Name: "vm1", Type: "agent", Engine: state.Firecracker, Port: 2205}); err != nil {
		t.Fatal(err)
	}
	f := run.NewFake()
	f.On("podman ps", run.Result{Stdout: "2200\n2201\n\n"}, nil)
	d := Deps{Runner: f, InstDir: dir}

	got := d.reservedPorts(context.Background())
	for _, p := range []int{2200, 2201, 2205} { // 2200/2201 from podman, 2205 from state
		if !got[p] {
			t.Errorf("reservedPorts missing %d: %v", p, got)
		}
	}
	if got[9999] {
		t.Errorf("reservedPorts falsely contains 9999")
	}
}

// TestPodmanStartEnsuresNetwork: Start re-ensures the shared network before
// starting the container, so a resume after a host reboot brings it back fully.
func TestPodmanStartEnsuresNetwork(t *testing.T) {
	f := run.NewFake() // default: all calls succeed (network already exists)
	p := NewPodman(Deps{Runner: f, Network: "cs-sandbox-net"})
	if err := p.Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}
	if !f.Contains("network exists cs-sandbox-net") {
		t.Errorf("Start should check the shared network; calls: %s", f)
	}
	if !f.Contains("podman start box1") {
		t.Errorf("Start should start the container; calls: %s", f)
	}
}

// TestPodmanStartNetworkFailurePropagates: if the network can't be ensured, Start
// fails and does NOT start the container.
func TestPodmanStartNetworkFailurePropagates(t *testing.T) {
	f := run.NewFake()
	f.On("network exists", run.Result{}, &run.ExitError{ExitCode: 1}) // never resolvable
	p := NewPodman(Deps{Runner: f, Network: "cs-sandbox-net"})
	if err := p.Start(context.Background(), "box1"); err == nil {
		t.Error("Start should fail when the network can't be ensured")
	}
	if f.Contains("podman start box1") {
		t.Error("Start should not start the container when the network is unavailable")
	}
}

// TestFirecrackerAllocIP: the VM address pool starts at .200 and skips addresses
// already recorded in instance state.
func TestFirecrackerAllocIP(t *testing.T) {
	dir := t.TempDir()
	fe := NewFirecracker(Deps{InstDir: dir})

	ip, err := fe.allocIP("10.89.0.1")
	if err != nil || ip != "10.89.0.200" {
		t.Fatalf("allocIP(empty) = %q, %v; want 10.89.0.200", ip, err)
	}

	if err := state.Save(dir, &state.Instance{Name: "vm1", Type: "agent", Engine: state.Firecracker, FCIP: "10.89.0.200"}); err != nil {
		t.Fatal(err)
	}
	ip, err = fe.allocIP("10.89.0.1")
	if err != nil || ip != "10.89.0.201" {
		t.Fatalf("allocIP(.200 used) = %q, %v; want 10.89.0.201", ip, err)
	}
}
