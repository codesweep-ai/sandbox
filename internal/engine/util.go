package engine

import (
	"context"
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
func allocPort(lo, hi int, vm bool, reserved map[int]bool) (int, error) {
	return ports.Alloc(lo, hi, vm, reserved, ports.LoopbackBusy)
}

// reservedPorts collects ports already claimed by any instance (podman labels
// span running/stopped containers; state files cover removed ones + VMs),
// spanning both engines so numbers never collide.
func (d Deps) reservedPorts(ctx context.Context) map[int]bool {
	m := map[int]bool{}
	res, _ := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "ps", "-a",
		"--filter", "label=cs-sandbox.managed=1",
		"--format", `{{index .Labels "cs-sandbox.ssh_port"}}`)
	out := res.Stdout
	for _, line := range strings.Split(out, "\n") {
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
