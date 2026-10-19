package doctor

import (
	"strings"
	"testing"

	eng "github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
)

// TestLimitsGroupSaysWhatIsMissing: a host without the slice or a controller
// is told which, as advice rather than a failure, since every sandbox still runs.
func TestLimitsGroupSaysWhatIsMissing(t *testing.T) {
	was := hostCgroups
	t.Cleanup(func() { hostCgroups = was })

	hostCgroups = func(hostenv.Host) eng.Cgroups {
		return eng.Cgroups{Slice: true, Controllers: []string{"cpu", "memory", "pids"}}
	}
	if c := limitsGroup(Deps{}).Checks; len(c) != 1 || c[0].Status != OK || !strings.Contains(c[0].Message, "cs-sandbox.slice") {
		t.Errorf("full delegation: %+v", c)
	}

	hostCgroups = func(hostenv.Host) eng.Cgroups {
		return eng.Cgroups{Why: "no systemd user manager holds your cgroups"}
	}
	c := limitsGroup(Deps{}).Checks
	if len(c) != 1 || c[0].Status != HM || !strings.Contains(c[0].Message, "no systemd user manager") {
		t.Errorf("no user manager: %+v", c)
	}
}
