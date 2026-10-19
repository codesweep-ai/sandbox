package doctor

import (
	"context"
	"strings"
	"testing"

	eng "github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// standIn replaces the host probes for one test.
func standIn(t *testing.T, cg eng.Cgroups, manager string) {
	t.Helper()
	wasCG, wasMgr := hostCgroups, podmanCgroupManager
	t.Cleanup(func() { hostCgroups, podmanCgroupManager = wasCG, wasMgr })
	hostCgroups = func(hostenv.Host) eng.Cgroups { return cg }
	podmanCgroupManager = func(context.Context, run.Runner) string { return manager }
}

// TestLimitsGroupSaysWhatIsMissing: a host without the slice or a controller
// is told which, as advice rather than a failure, since every sandbox still runs.
func TestLimitsGroupSaysWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	d := Deps{Runner: run.NewFake()}
	full := eng.Cgroups{Slice: true, Controllers: []string{"cpu", "memory", "pids"}}

	standIn(t, full, "systemd")
	if c := limitsGroup(ctx, "podman", d).Checks; len(c) != 1 || c[0].Status != OK || !strings.Contains(c[0].Message, "cs-sandbox.slice") {
		t.Errorf("full delegation: %+v", c)
	}

	standIn(t, eng.Cgroups{Why: "no systemd user manager holds your cgroups"}, "systemd")
	c := limitsGroup(ctx, "podman", d).Checks
	if len(c) != 1 || c[0].Status != HM || !strings.Contains(c[0].Message, "no systemd user manager") {
		t.Errorf("no user manager: %+v", c)
	}
}

// TestLimitsGroupNamesTheCgroupfsManager: under cgroupfs a podman sandbox runs
// outside the slice, which the podman report says along with the fix. A
// microVM's scope is systemd's own, so the firecracker report is unaffected.
func TestLimitsGroupNamesTheCgroupfsManager(t *testing.T) {
	ctx := context.Background()
	d := Deps{Runner: run.NewFake()}
	standIn(t, eng.Cgroups{Slice: true, Controllers: []string{"cpu", "memory", "pids"}}, "cgroupfs")

	c := limitsGroup(ctx, "podman", d).Checks
	if len(c) != 1 || c[0].Status != HM || !strings.Contains(c[0].Message, "cgroupfs cgroup manager") ||
		!strings.Contains(c[0].Message, `cgroup_manager = "systemd"`) {
		t.Errorf("podman under cgroupfs: %+v", c)
	}
	if c := limitsGroup(ctx, "firecracker", d).Checks; len(c) != 1 || c[0].Status != OK {
		t.Errorf("firecracker under cgroupfs podman: %+v", c)
	}
}
