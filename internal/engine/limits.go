package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// SliceName is the systemd user slice every sandbox runs under, so the whole
// fleet shares one cap however many sandboxes there are (R171). systemd reads
// the dash as nesting, so its cgroup is cs.slice/cs-sandbox.slice.
const SliceName = "cs-sandbox.slice"

// DefaultPids is a podman sandbox's process limit when create names none. It
// is four times podman's own default of 2048, which a parallel build with a
// nested engine inside comes close to, and small enough that ten sandboxes
// cannot spend the user's task budget between them.
const DefaultPids = 8192

// Cgroups is what this host lets cs-sandbox limit a sandbox by.
type Cgroups struct {
	// Slice is set when a systemd user manager is there to hold SliceName.
	Slice bool
	// Controllers are the cgroup controllers delegated to the user. A rootless
	// container can be limited only by these, and podman fails a run asked for
	// any other.
	Controllers []string
	// Why says what is missing, when something is.
	Why string
}

// Has reports whether a controller is delegated.
func (c Cgroups) Has(controller string) bool { return slices.Contains(c.Controllers, controller) }

// userManagerCgroup is the user manager's cgroup, %d being the uid. A package
// var so a test can point it at a directory of its own.
var userManagerCgroup = "/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service"

// HostCgroups reads what this host delegates. It reads one file and asks for
// nothing, so it is cheap enough to run on every command.
func HostCgroups(h hostenv.Host) Cgroups {
	if h.IsMacOS {
		// The podman machine is a VM of its own size, which already bounds the
		// fleet, and its user manager delegates these three.
		return Cgroups{Controllers: []string{"cpu", "memory", "pids"},
			Why: "on macOS the podman machine's own size bounds every sandbox"}
	}
	path := fmt.Sprintf(userManagerCgroup, h.UID, h.UID)
	data, err := os.ReadFile(path + "/cgroup.controllers")
	if err != nil {
		why := "no systemd user manager holds your cgroups, so sandboxes run without resource limits"
		if h.IsWSL {
			why += "; enable systemd in /etc/wsl.conf"
		}
		return Cgroups{Why: why}
	}
	c := Cgroups{Controllers: strings.Fields(string(data))}
	var missing []string
	for _, want := range []string{"cpu", "memory", "pids"} {
		if !c.Has(want) {
			missing = append(missing, want)
		}
	}
	_, lookErr := exec.LookPath("systemctl")
	switch {
	case os.Getenv("CS_SANDBOX_NO_SLICE") != "":
		c.Why = "CS_SANDBOX_NO_SLICE is set"
	case os.Getenv("XDG_RUNTIME_DIR") == "" || lookErr != nil:
		c.Why = "no systemd user session to hold " + SliceName
	default:
		c.Slice = true
	}
	if len(missing) > 0 {
		if c.Why != "" {
			c.Why += "; "
		}
		c.Why += "the " + strings.Join(missing, ", ") + " controller is not delegated to your user"
	}
	return c
}

// SliceProperties are the limits set on SliceName: what the whole fleet may
// take, leaving the rest of the host to the desktop.
//
// Memory is capped at three quarters of the host and swap at none, because on
// a zram host swap is RAM the cap does not count. CPU leaves an eighth of the
// cores, and at least one, outside the slice. The task cap is a quarter of the
// kernel's, which is most of the share systemd gives a user. override is
// CS_SANDBOX_SLICE_LIMITS: systemd assignments that replace a default of the
// same name or add one.
func SliceProperties(ncpu int, override string) ([]string, error) {
	props := []string{"MemoryMax=75%", "MemorySwapMax=0"}
	if q := ncpu - max(1, ncpu/8); q >= 1 {
		props = append(props, "CPUQuota="+strconv.Itoa(q*100)+"%")
	}
	props = append(props, "TasksMax=25%")
	for f := range strings.FieldsSeq(override) {
		key, _, ok := strings.Cut(f, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("CS_SANDBOX_SLICE_LIMITS: %q is not a systemd assignment such as MemoryMax=32G", f)
		}
		i := slices.IndexFunc(props, func(p string) bool { return strings.HasPrefix(p, key+"=") })
		if i >= 0 {
			props[i] = f
		} else {
			props = append(props, f)
		}
	}
	return props, nil
}

// ensureSlice sets SliceName's limits before a sandbox starts under it.
//
// They are runtime properties, so nothing is written under your home and they
// are gone at logout. Every create and start sets them again, which is what
// brings them back after a reboot. A host that cannot hold the slice starts the
// sandbox without one, and one that refuses the properties starts it in an
// uncapped slice, with a note, rather than failing.
func (d Deps) ensureSlice(ctx context.Context) {
	if !d.Cgroups.Slice {
		return
	}
	props, err := SliceProperties(runtime.NumCPU(), os.Getenv("CS_SANDBOX_SLICE_LIMITS"))
	if err != nil {
		d.note(err.Error() + "; using the default limits")
		props, _ = SliceProperties(runtime.NumCPU(), "")
	}
	argv := append([]string{"systemctl", "--user", "set-property", "--runtime", SliceName}, props...)
	if _, err := d.Runner.Run(ctx, run.Opts{}, argv...); err != nil {
		d.note(fmt.Sprintf("could not cap %s, so sandboxes share no total limit: %v", SliceName, err))
	}
}

// containerLimits are the cgroup limits a podman sandbox is run with. A zero
// field passes nothing, and Pids of -1 lifts podman's own default.
type containerLimits struct {
	Parent string // the cgroup parent: SliceName, or "" for podman's own
	Pids   int
	CPUs   int
	MemMiB int
}

// PodmanCgroupManager is the cgroup manager podman runs containers with,
// systemd or cgroupfs, or "" when podman cannot say. Only the systemd manager
// takes a slice as a cgroup parent; under cgroupfs podman refuses the run.
func PodmanCgroupManager(ctx context.Context, r run.Runner) string {
	return run.Output(ctx, r, "podman", "info", "--format", "{{.Host.CgroupManager}}")
}

// podmanLimits decides which of a create's limits this host can apply. A
// limit whose controller is not delegated is dropped, since podman fails a run
// asked for it, and one the caller named is said to be dropped. The slice is
// the cgroup parent only where podman's manager is systemd.
func podmanLimits(s CreateSpec, cg Cgroups, manager string) (l containerLimits, notes []string) {
	if cg.Slice && manager == "systemd" {
		l.Parent = SliceName
	}
	if cg.Has("pids") {
		l.Pids = s.Pids
		if s.Pids <= 0 {
			l.Pids = -1
		}
	}
	if s.CPUs > 0 {
		if cg.Has("cpu") {
			l.CPUs = s.CPUs
		} else {
			notes = append(notes, "--cpus not applied: the cpu controller is not delegated to your user")
		}
	}
	if s.MemMiB > 0 {
		if cg.Has("memory") {
			l.MemMiB = s.MemMiB
		} else {
			notes = append(notes, "--mem not applied: the memory controller is not delegated to your user")
		}
	}
	return l, notes
}

// args renders the limits as podman run flags. Swap is given the same ceiling
// as memory, which allows none, for the reason the slice allows none.
func (l containerLimits) args() []string {
	var a []string
	if l.Parent != "" {
		a = append(a, "--cgroup-parent="+l.Parent)
	}
	if l.Pids != 0 {
		a = append(a, "--pids-limit="+strconv.Itoa(l.Pids))
	}
	if l.CPUs > 0 {
		a = append(a, "--cpus="+strconv.Itoa(l.CPUs))
	}
	if l.MemMiB > 0 {
		m := strconv.Itoa(l.MemMiB) + "m"
		a = append(a, "--memory="+m, "--memory-swap="+m)
	}
	return a
}
