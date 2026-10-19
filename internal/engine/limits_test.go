package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// fakeUserManager points HostCgroups at a user manager cgroup delegating the
// given controllers, or at none when controllers is nil.
func fakeUserManager(t *testing.T, controllers *string) {
	t.Helper()
	root := t.TempDir()
	was := userManagerCgroup
	t.Cleanup(func() { userManagerCgroup = was })
	userManagerCgroup = filepath.Join(root, "user-%d", "user@%d.service")
	if controllers == nil {
		return
	}
	dir := fmt.Sprintf(userManagerCgroup, 1000, 1000)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte(*controllers+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHostCgroupsReadsDelegation: the slice needs a user manager and a session
// to reach it, and each limit needs its controller. A host missing either still
// gets an answer, which says what is missing rather than failing.
func TestHostCgroupsReadsDelegation(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemctl on this host")
	}
	linux := hostenv.Host{UID: 1000}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CS_SANDBOX_NO_SLICE", "")

	fakeUserManager(t, new("cpu io memory pids"))
	if c := HostCgroups(linux); !c.Slice || !c.Has("pids") || c.Why != "" {
		t.Errorf("full delegation: %+v", c)
	}

	fakeUserManager(t, new("memory pids"))
	c := HostCgroups(linux)
	if !c.Slice || c.Has("cpu") || !strings.Contains(c.Why, "cpu controller is not delegated") {
		t.Errorf("no cpu delegation: %+v", c)
	}

	t.Setenv("CS_SANDBOX_NO_SLICE", "1")
	if c := HostCgroups(linux); c.Slice || !c.Has("memory") {
		t.Errorf("CS_SANDBOX_NO_SLICE keeps the limits and drops the slice: %+v", c)
	}
	t.Setenv("CS_SANDBOX_NO_SLICE", "")

	t.Setenv("XDG_RUNTIME_DIR", "")
	if c := HostCgroups(linux); c.Slice {
		t.Errorf("no session, no slice: %+v", c)
	}
}

// TestHostCgroupsWithoutAUserManager: WSL2 without systemd, or a shell inside a
// sandbox, has no user manager at all. Nothing is limited, and WSL is told how
// to get one.
func TestHostCgroupsWithoutAUserManager(t *testing.T) {
	fakeUserManager(t, nil)
	c := HostCgroups(hostenv.Host{UID: 1000, IsWSL: true})
	if c.Slice || len(c.Controllers) > 0 {
		t.Errorf("no user manager must limit nothing: %+v", c)
	}
	if !strings.Contains(c.Why, "/etc/wsl.conf") {
		t.Errorf("WSL should be told to enable systemd: %q", c.Why)
	}
	if c := HostCgroups(hostenv.Host{UID: 1000, IsMacOS: true}); c.Slice || !c.Has("pids") {
		t.Errorf("macOS limits containers in its machine and needs no slice: %+v", c)
	}
}

// TestSliceProperties: the defaults leave the desktop a share of every
// resource, and an override replaces one by name without losing the others.
func TestSliceProperties(t *testing.T) {
	got, err := SliceProperties(28, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"MemoryMax=75%", "MemorySwapMax=0", "CPUQuota=2500%", "TasksMax=25%"}
	if !slices.Equal(got, want) {
		t.Errorf("28 cores: %v, want %v", got, want)
	}
	if got, _ := SliceProperties(4, ""); !slices.Contains(got, "CPUQuota=300%") {
		t.Errorf("4 cores should keep one back: %v", got)
	}
	if got, _ := SliceProperties(1, ""); slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "CPUQuota=") }) {
		t.Errorf("one core has none to keep back, so no CPU cap: %v", got)
	}

	got, err = SliceProperties(8, "MemoryMax=32G  CPUWeight=50")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"MemoryMax=32G", "MemorySwapMax=0", "CPUQuota=700%", "TasksMax=25%", "CPUWeight=50"}
	if !slices.Equal(got, want) {
		t.Errorf("override: %v, want %v", got, want)
	}
	if _, err := SliceProperties(8, "MemoryMax"); err == nil {
		t.Error("a token with no = must be refused")
	}
}

// TestPodmanLimits: the slice and the pids limit are on by default, a CPU or
// memory cap only when asked for, and a limit the host cannot apply is dropped
// rather than failing the run, with a note when the caller asked for it.
func TestPodmanLimits(t *testing.T) {
	full := Cgroups{Slice: true, Controllers: []string{"cpu", "memory", "pids"}}

	l, notes := podmanLimits(CreateSpec{Pids: DefaultPids}, full, "systemd")
	if got := strings.Join(l.args(), " "); got != "--cgroup-parent=cs-sandbox.slice --pids-limit=8192" || len(notes) > 0 {
		t.Errorf("defaults: %q %v", got, notes)
	}

	l, _ = podmanLimits(CreateSpec{Pids: 0, CPUs: 2, MemMiB: 6144}, full, "systemd")
	if got := strings.Join(l.args(), " "); got != "--cgroup-parent=cs-sandbox.slice --pids-limit=-1 --cpus=2 --memory=6144m --memory-swap=6144m" {
		t.Errorf("caps given: %q", got)
	}

	l, notes = podmanLimits(CreateSpec{Pids: DefaultPids, CPUs: 2, MemMiB: 6144}, Cgroups{}, "")
	if got := l.args(); len(got) > 0 {
		t.Errorf("no delegation must pass no limit: %v", got)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "--cpus not applied") || !strings.Contains(notes[1], "--mem not applied") {
		t.Errorf("a cap the caller named and the host refused should be said: %v", notes)
	}
}

// TestPodmanLimitsFollowTheCgroupManager: podman refuses a slice as the cgroup
// parent under cgroupfs, so there the slice is skipped while the container's
// own limits, which cgroupfs applies, are kept. A manager podman could not name
// is treated the same way, since a failed run costs more than a missing slice.
func TestPodmanLimitsFollowTheCgroupManager(t *testing.T) {
	full := Cgroups{Slice: true, Controllers: []string{"cpu", "memory", "pids"}}
	spec := CreateSpec{Pids: DefaultPids, CPUs: 2, MemMiB: 1024}
	for _, c := range []struct{ manager, want string }{
		{"systemd", "--cgroup-parent=cs-sandbox.slice --pids-limit=8192 --cpus=2 --memory=1024m --memory-swap=1024m"},
		{"cgroupfs", "--pids-limit=8192 --cpus=2 --memory=1024m --memory-swap=1024m"},
		{"", "--pids-limit=8192 --cpus=2 --memory=1024m --memory-swap=1024m"},
	} {
		l, _ := podmanLimits(spec, full, c.manager)
		if got := strings.Join(l.args(), " "); got != c.want {
			t.Errorf("manager %q: %q, want %q", c.manager, got, c.want)
		}
	}
}

// TestPodmanCgroupManagerAsksPodman: the answer is podman's own, trimmed.
func TestPodmanCgroupManagerAsksPodman(t *testing.T) {
	f := run.NewFake().OnStdout("podman info --format {{.Host.CgroupManager}}", "cgroupfs\n")
	if got := PodmanCgroupManager(context.Background(), f); got != "cgroupfs" {
		t.Errorf("manager = %q, want cgroupfs", got)
	}
}

func TestBuildRunArgsCarriesLimits(t *testing.T) {
	p := baseParams()
	p.Limits = containerLimits{Parent: SliceName, Pids: 8192, CPUs: 2, MemMiB: 1024}
	got := strings.Join(buildRunArgs(p), " ")
	for _, want := range []string{"--cgroup-parent=cs-sandbox.slice", "--pids-limit=8192", "--cpus=2", "--memory=1024m", "--memory-swap=1024m"} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
	if got := strings.Join(buildRunArgs(baseParams()), " "); strings.Contains(got, "cgroup-parent") || strings.Contains(got, "pids-limit") {
		t.Errorf("no limits, no flags:\n%s", got)
	}
}

// TestPodmanStartSetsTheSlice: the slice's limits are runtime properties a
// reboot clears, so start sets them again before the container comes up.
func TestPodmanStartSetsTheSlice(t *testing.T) {
	f := run.NewFake()
	f.On("network inspect", run.Result{Stdout: "true|1\n"}, nil)
	p := NewPodman(Deps{Runner: f, Network: "cs-sandbox-net", Cgroups: Cgroups{Slice: true}})
	if err := p.Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}
	set := slices.IndexFunc(f.Rendered(), func(c string) bool {
		return strings.HasPrefix(c, "systemctl --user set-property --runtime cs-sandbox.slice MemoryMax=")
	})
	start := slices.IndexFunc(f.Rendered(), func(c string) bool { return strings.HasPrefix(c, "podman start") })
	if set < 0 || start < 0 || set > start {
		t.Errorf("start should set the slice's limits before the container; calls: %s", f)
	}

	f = run.NewFake()
	f.On("network inspect", run.Result{Stdout: "true|1\n"}, nil)
	if err := NewPodman(Deps{Runner: f, Network: "cs-sandbox-net"}).Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}
	if f.Contains("systemctl") {
		t.Errorf("a host with no slice should not be asked to set one: %s", f)
	}
}

// TestSliceFailureIsANote: a manager that refuses the properties leaves the
// sandbox uncapped and says so; it does not fail the start.
func TestSliceFailureIsANote(t *testing.T) {
	f := run.NewFake()
	f.On("set-property", run.Result{}, &run.ExitError{ExitCode: 1})
	var notes []string
	d := Deps{Runner: f, Cgroups: Cgroups{Slice: true}, Note: func(s string) { notes = append(notes, s) }}
	d.ensureSlice(context.Background())
	if len(notes) != 1 || !strings.Contains(notes[0], "could not cap cs-sandbox.slice") {
		t.Errorf("notes = %v", notes)
	}
}

// TestCgroupWrapperScopeCaps: a microVM's scope is capped at one core per vCPU
// plus one for the VMM, holds a bounded number of tasks, and goes under the
// slice only where there is one.
func TestCgroupWrapperScopeCaps(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, 4096) // 2 vCPUs
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "")
	t.Setenv("CS_SANDBOX_FC_MEMORY_MAX", "")

	argv := cgroupWrapper(dir, Cgroups{Slice: true}, &bytes.Buffer{})
	if len(argv) == 0 {
		t.Skip("systemd-run not present on this host")
	}
	for _, want := range []string{"CPUQuota=300%", "TasksMax=1024", "--slice=cs-sandbox.slice"} {
		if !argvHas(argv, want) {
			t.Errorf("argv %v does not contain %q", argv, want)
		}
	}
	if argv := cgroupWrapper(dir, Cgroups{}, &bytes.Buffer{}); slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "--slice") }) {
		t.Errorf("no slice on this host, so none named: %v", argv)
	}
}
