package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	// EnsureNetwork now verifies the network really is ours and isolated, so the
	// fake has to answer the inspect the way a managed network would.
	f.On("network inspect", run.Result{Stdout: "true|1\n"}, nil)
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
	ctx := context.Background()
	dir := t.TempDir()
	fe := NewFirecracker(Deps{InstDir: dir, Runner: run.NewFake()})
	fab := fe.fabric()

	ip, err := fe.allocIP(ctx, fab, "10.89.0.1")
	if err != nil || ip != "10.89.0.200" {
		t.Fatalf("allocIP(empty) = %q, %v; want 10.89.0.200", ip, err)
	}

	if err := state.Save(dir, &state.Instance{Name: "vm1", Type: "agent", Engine: state.Firecracker, FCIP: "10.89.0.200"}); err != nil {
		t.Fatal(err)
	}
	ip, err = fe.allocIP(ctx, fab, "10.89.0.1")
	if err != nil || ip != "10.89.0.201" {
		t.Fatalf("allocIP(.200 used) = %q, %v; want 10.89.0.201", ip, err)
	}
}

// TestFirecrackerAllocIPSkipsForeignTaps: an address whose tap is already on the
// fabric is taken, even though no instance in THIS root records it — another
// root's VM owns it. Handing it out again would put two VMs on one address and
// let either one's teardown delete the other's tap.
func TestFirecrackerAllocIPSkipsForeignTaps(t *testing.T) {
	ctx := context.Background()
	f := run.NewFake()
	f.OnStdout("ip -br link show", "lo UNKNOWN 00:00\npodman1 UP aa:bb\nfdt200 UP 56:ac\nfdt201 UP 56:ad\n")
	fe := NewFirecracker(Deps{InstDir: t.TempDir(), Runner: f})

	ip, err := fe.allocIP(ctx, fe.fabric(), "10.89.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.89.0.202" {
		t.Errorf("allocIP = %q, want 10.89.0.202 (.200/.201 hold foreign taps)", ip)
	}
}

// TestAnyVMRunningSeesForeignTaps: fabric GC must not tear the network down while
// a VM from another root is still attached to it.
func TestAnyVMRunningSeesForeignTaps(t *testing.T) {
	f := run.NewFake()
	f.OnStdout("ip -br link show", "podman1 UP aa:bb\nfdt200 UP 56:ac\n")
	fe := NewFirecracker(Deps{InstDir: t.TempDir(), Runner: f})
	if !fe.anyVMRunning(context.Background()) {
		t.Error("a tap on the fabric means a VM still needs it")
	}

	f2 := run.NewFake()
	f2.OnStdout("ip -br link show", "podman1 UP aa:bb\n")
	fe2 := NewFirecracker(Deps{InstDir: t.TempDir(), Runner: f2})
	if fe2.anyVMRunning(context.Background()) {
		t.Error("no taps and no local instances means the fabric is idle")
	}
}

// TestStatuses: `ls` reports running/stopped per engine, and says "unknown"
// rather than guessing when podman can't be reached.
func TestStatuses(t *testing.T) {
	dir := t.TempDir()
	insts := []*state.Instance{
		{Name: "up", Group: state.DefaultGroup, Engine: state.Podman},
		{Name: "down", Group: state.DefaultGroup, Engine: state.Podman},
		{Name: "gone", Group: state.DefaultGroup, Engine: state.Podman}, // not in podman ps at all
		{Name: "vmup", Group: state.DefaultGroup, Engine: state.Firecracker},
		{Name: "vmdown", Group: state.DefaultGroup, Engine: state.Firecracker},
	}
	// A microVM is "running" when its pid file names a live process.
	if err := os.MkdirAll(state.Dir(dir, state.DefaultGroup, "vmup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.Dir(dir, state.DefaultGroup, "vmup"), "fc.pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := run.NewFake()
	f.On("podman ps", run.Result{Stdout: "up.default running\ndown.default exited\n"}, nil)
	d := Deps{Runner: f, InstDir: dir}

	got := d.Statuses(context.Background(), insts)
	// Keys are the qualified reference, not the bare name.
	want := map[string]string{
		"up.default": StatusRunning, "down.default": StatusStopped, "gone.default": StatusStopped,
		"vmup.default": StatusRunning, "vmdown.default": StatusStopped,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("status[%s] = %q, want %q", name, got[name], w)
		}
	}

	// podman unreachable: report unknown rather than claiming everything is stopped.
	f2 := run.NewFake()
	f2.On("podman ps", run.Result{}, errors.New("podman unavailable"))
	d2 := Deps{Runner: f2, InstDir: dir}
	if got := d2.Statuses(context.Background(), insts); got["up"+".default"] != StatusUnknown {
		t.Errorf("status with podman down = %q, want %q", got["up"+".default"], StatusUnknown)
	}
	// A microVM's state needs no podman, so it is still accurate.
	if got := d2.Statuses(context.Background(), insts); got["vmup"+".default"] != StatusRunning {
		t.Errorf("microVM status should not depend on podman: %q", got["vmup"+".default"])
	}
}
