package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestCountSwaps: zram is RAM, so swapping to it compresses pages in place
// rather than returning them to the host — the two cases lead to opposite
// advice, and telling them apart is a string prefix on a device path.
func TestCountSwaps(t *testing.T) {
	const header = "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
	for _, tc := range []struct {
		name         string
		body         string
		wantZ, wantD int
	}{
		{"none", "", 0, 0},
		{"zram only", "/dev/zram0                              partition\t8388604\t44\t100\n", 1, 0},
		{"disk only", "/dev/nvme0n1p2                          partition\t8388604\t0\t-2\n", 0, 1},
		{"file-backed counts as disk", "/swapfile                               file\t2097148\t0\t-2\n", 0, 1},
		{"both", "/dev/zram0 partition 1 0 100\n/dev/sda2 partition 1 0 -2\n", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, d := countSwaps(header + tc.body)
			if z != tc.wantZ || d != tc.wantD {
				t.Errorf("countSwaps = (zram %d, disk %d), want (%d, %d)", z, d, tc.wantZ, tc.wantD)
			}
		})
	}
}

// TestMemoryGroupIsAdvisory: a host with none of this still runs sandboxes, so
// nothing here may report NO — that status is reserved for things that block.
func TestMemoryGroupIsAdvisory(t *testing.T) {
	for _, c := range memoryGroup().Checks {
		if c.Status == NO {
			t.Errorf("memory checks must not block a host from running sandboxes: %q", c.Message)
		}
	}
}

// TestMemoryGroupIsFirecrackerOnly: none of this applies to the podman engine,
// which reclaims on its own — and where it does apply it has to read as part of
// the engine's own section rather than as advice about the host in general, so
// it follows immediately after it.
func TestMemoryGroupIsFirecrackerOnly(t *testing.T) {
	const title = "firecracker memory management (optional)"

	podman := Diagnose(context.Background(), "podman", Deps{Runner: run.NewFake()})
	for _, g := range podman.Groups {
		if g.Title == title {
			t.Error("the podman engine must not report firecracker memory settings")
		}
	}

	fc := Diagnose(context.Background(), "firecracker", Deps{Runner: run.NewFake()})
	at := -1
	for i, g := range fc.Groups {
		if g.Title == title {
			at = i
		}
	}
	if at < 1 {
		t.Fatalf("firecracker report is missing %q", title)
	}
	if prev := fc.Groups[at-1].Title; !strings.HasPrefix(prev, "firecracker microVM engine") {
		t.Errorf("memory section should follow the engine's own section, got %q before it", prev)
	}
}
