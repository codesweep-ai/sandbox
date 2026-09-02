package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/ports"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// nowUTC renders the current time in the `created` field's format.
func nowUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// allocPort wraps ports.Alloc with the live loopback probe.
func allocPort(lo, hi int, reserved map[int]bool) (int, error) {
	return ports.Alloc(lo, hi, reserved, ports.LoopbackBusy)
}

// reservedPorts collects ports already claimed by any instance (podman labels
// span running/stopped containers; state files cover removed ones + VMs),
// spanning both engines so numbers never collide. It sees only this instances
// dir; allocPort's live probe covers ports held from outside it.
func (d Deps) reservedPorts(ctx context.Context) map[int]bool {
	m := map[int]bool{}
	res, _ := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "ps", "-a",
		"--filter", "label=cs-sandbox.managed=1",
		"--format", `{{index .Labels "cs-sandbox.ssh_port"}}`)
	out := res.Stdout
	for line := range strings.SplitSeq(out, "\n") {
		if p, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			m[p] = true
		}
	}
	insts, _ := state.List(d.InstDir)
	for _, in := range insts {
		if in.Port > 0 {
			m[in.Port] = true
		}
	}
	return m
}

// copyTree performs a CoW-where-possible recursive copy, yielding
// frozen-snapshot semantics.
func (d Deps) copyTree(ctx context.Context, src, dst string) error {
	_, err := d.Runner.Run(ctx, run.Opts{}, copyTreeArgv(d.Host.IsMacOS, src, dst)...)
	return err
}

// ownTree hands a tree to the host user, so a share the sandbox mounts belongs
// to whoever works in it (SPEC R163).
//
// cp -a keeps what of the source's ownership it can, and as an unprivileged
// caller that is the group: a uid it may not set the copy simply inherits from
// the caller, but a gid the caller belongs to it does set. That gid is outside
// the container's user namespace, which maps the caller's own ids and nothing
// else, so such a file arrives in the sandbox owned by "nobody".
func (d Deps) ownTree(ctx context.Context, dir string) error {
	owner := fmt.Sprintf("%d:%d", d.Host.UID, d.Host.GID)
	_, err := d.Runner.Run(ctx, run.Opts{}, "chown", "-Rh", owner, dir)
	return err
}

// copyTreeArgv spells "recursive, attribute-preserving, copy-on-write if the
// filesystem can" for the host's cp. --reflink is GNU-only: BSD/macOS cp rejects
// it outright ("illegal option -- -"), and spells the same thing -c (clonefile,
// documented to fall back to a full copy across filesystems or where cloning is
// unsupported — exactly --reflink=auto's contract).
func copyTreeArgv(isMacOS bool, src, dst string) []string {
	if isMacOS {
		return []string{"cp", "-a", "-c", src, dst}
	}
	return []string{"cp", "-a", "--reflink=auto", src, dst}
}

// Sandbox lifecycle states reported by `ls`. The vocabulary follows the stop/start
// commands rather than each engine's own word (podman says "exited"), so what you
// read is what you would type.
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusUnknown = "unknown"
)

// Statuses reports each instance's lifecycle state. Container states come from a
// single `podman ps -a`; a microVM's comes from its pid file — so listing costs at
// most one subprocess regardless of how many sandboxes exist.
// Statuses is keyed by the QUALIFIED reference (<name>.<group>), because a bare
// name is not unique: the same sandbox name may exist in several groups, and a
// map keyed by it would report one group's state for another's.
func (d Deps) Statuses(ctx context.Context, insts []*state.Instance) map[string]string {
	out := make(map[string]string, len(insts))
	anyPodman := false
	for _, in := range insts {
		if in.Engine == state.Firecracker {
			if fcRunning(state.Dir(d.InstDir, InstGroup(in), in.Name)) {
				out[Qualify(in)] = StatusRunning
			} else {
				out[Qualify(in)] = StatusStopped
			}
			continue
		}
		anyPodman = true
	}
	if !anyPodman {
		return out
	}
	res, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "ps", "-a",
		"--filter", "label=cs-sandbox.managed=1", "--format", "{{.Names}} {{.State}}")
	seen := map[string]string{}
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		if name, st, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			seen[name] = st
		}
	}
	for _, in := range insts {
		if in.Engine == state.Firecracker {
			continue
		}
		switch {
		case err != nil:
			out[Qualify(in)] = StatusUnknown // podman unreachable: say so rather than guess
		case seen[Qualify(in)] == "running":
			out[Qualify(in)] = StatusRunning
		default:
			out[Qualify(in)] = StatusStopped
		}
	}
	return out
}

// InstGroup is an instance's group, defaulting so zero-valued records in tests
// behave like default-group members.
func InstGroup(in *state.Instance) string {
	if in.Group == "" {
		return state.DefaultGroup
	}
	return in.Group
}

// Qualify is an instance's host-global identity: <name>.<group>. It is both the
// podman object name and the key every name-indexed map uses.
func Qualify(in *state.Instance) string { return obj(InstGroup(in), in.Name) }
