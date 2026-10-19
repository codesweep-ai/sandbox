package doctor

import (
	"os"

	eng "github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
)

// hostCgroups is what this host lets a sandbox be limited by. A variable, so a
// test can stand in for the host.
var hostCgroups = eng.HostCgroups

// limitsGroup reports whether sandboxes run under the shared slice and which
// limits the host can apply (R171). All advisory: a host with neither still
// runs every sandbox, only without the caps that keep the desktop responsive.
func limitsGroup(d Deps) Group {
	g := Group{Title: "resource limits (both engines)"}
	c := hostCgroups(hostenv.Host{UID: os.Getuid(), IsMacOS: d.IsMacOS, IsWSL: d.IsWSL})
	if c.Slice {
		g.add(OK, "sandboxes run under "+eng.SliceName+", which caps their total")
	}
	if c.Why != "" {
		g.add(HM, c.Why)
	}
	return g
}
