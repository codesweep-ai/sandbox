package doctor

import (
	"context"
	"strings"
	"testing"

	eng "github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestDiskIOGroupWarnsOnSync: Async is reported as fine, and Sync as a warning
// that says why and what it costs, never as a failure, since Sync still boots
// every sandbox (SBX-031).
func TestDiskIOGroupWarnsOnSync(t *testing.T) {
	was := diskIOEngine
	t.Cleanup(func() { diskIOEngine = was })

	diskIOEngine = func() (string, string) { return eng.IOEngineAsync, "" }
	if c := diskIOGroup().Checks; len(c) != 1 || c[0].Status != OK || !strings.Contains(c[0].Message, "Async") {
		t.Errorf("with io_uring: %+v", c)
	}

	diskIOEngine = func() (string, string) {
		return eng.IOEngineSync, "io_uring is disabled on this host (kernel.io_uring_disabled is 2)"
	}
	c := diskIOGroup().Checks
	if len(c) != 1 || c[0].Status != HM {
		t.Fatalf("without io_uring: %+v, want one warning", c)
	}
	for _, want := range []string{"kernel.io_uring_disabled is 2", "Sync engine", "freeze a VM's network"} {
		if !strings.Contains(c[0].Message, want) {
			t.Errorf("warning lacks %q: %s", want, c[0].Message)
		}
	}
}

// TestDiskIOGroupIsFirecrackerOnly: podman has no block engine of its own, and
// the section reads as part of the firecracker engine's, directly after it.
func TestDiskIOGroupIsFirecrackerOnly(t *testing.T) {
	const title = "firecracker disk IO"
	for _, g := range Diagnose(context.Background(), "podman", Deps{Runner: run.NewFake()}).Groups {
		if g.Title == title {
			t.Error("the podman engine must not report the firecracker disk IO engine")
		}
	}
	fc := Diagnose(context.Background(), "firecracker", Deps{Runner: run.NewFake()})
	for i, g := range fc.Groups {
		if g.Title != title {
			continue
		}
		if i < 1 || !strings.HasPrefix(fc.Groups[i-1].Title, "firecracker microVM engine") {
			t.Errorf("disk IO section should follow the engine's own section")
		}
		return
	}
	t.Fatalf("firecracker report is missing %q", title)
}
