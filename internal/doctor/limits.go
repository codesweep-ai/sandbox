package doctor

import (
	"context"
	"os"

	eng "github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/hostenv"
)

// hostCgroups is what this host lets a sandbox be limited by, and
// podmanCgroupManager the manager podman runs containers with. Variables, so a
// test can stand in for the host.
var (
	hostCgroups         = eng.HostCgroups
	podmanCgroupManager = eng.PodmanCgroupManager
)

// limitsGroup reports whether sandboxes run under the shared slice and which
// limits the host can apply (R171). All advisory: a host with neither still
// runs every sandbox, only without the caps that keep the desktop responsive.
func limitsGroup(ctx context.Context, engine string, d Deps) Group {
	g := Group{Title: "resource limits (both engines)"}
	c := hostCgroups(hostenv.Host{UID: os.Getuid(), IsMacOS: d.IsMacOS, IsWSL: d.IsWSL})
	// Only the systemd manager takes a slice as a container's cgroup parent. A
	// manager podman cannot name is not reported here: doctor's podman checks
	// already say when podman does not answer.
	if c.Slice && engine == "podman" && d.Runner != nil {
		if m := podmanCgroupManager(ctx, d.Runner); m != "" && m != "systemd" {
			g.add(HM, "podman uses the "+m+" cgroup manager, so podman sandboxes run outside "+eng.SliceName+
				" with only their own limits; set cgroup_manager = \"systemd\" in containers.conf to put them in it")
			c.Slice = false
		}
	}
	if c.Slice {
		g.add(OK, "sandboxes run under "+eng.SliceName+", which caps their total")
	}
	if c.Why != "" {
		g.add(HM, c.Why)
	}
	return g
}
