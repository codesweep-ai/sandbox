// Package engine is the ports-and-adapters boundary between the CLI and the two
// sandbox backends. Shared, engine-agnostic logic (the seed, shares, ports,
// state) lives above the Engine interface; Podman and Firecracker are the
// adapters that implement it.
package engine

import (
	"context"
	"fmt"

	"github.com/codesweep-ai/sandbox/internal/hostenv"
	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/spec"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// CreateSpec is the engine-agnostic description of a sandbox to create.
type CreateSpec struct {
	Name              string
	Group             string // isolation boundary: network, SSH keys and gateway
	Type              string // user | agent
	Yolo              bool
	Solo              bool
	Privileged        bool // podman: use --privileged instead of the scaled-down cap set
	CPUs              int  // firecracker
	MemMiB            int  // firecracker
	DiskGB            int  // firecracker: grow the instance disk to this size (0 = the base rootfs size)
	Snapshots         []spec.Snapshot
	RepoClones        []spec.RepoClone
	ImageStores       []string
	InjectedEnv       string   // resolved KEY=VALUE block
	InheritAgentLogin []string // --inherit-agent-login: agents whose host login to carry in
}

// ExecIO configures an interactive/exec session.
type ExecIO struct {
	Interactive bool
	Argv        []string // command to run; empty = login shell
}

// Engine is the per-engine adapter contract.
type Engine interface {
	Name() state.Engine
	// Prepare builds/warms this engine's reusable, host-wide artifacts (assuming
	// the shared image already exists). Podman needs none — the image is the
	// artifact. Firecracker builds its binary + guest kernel + base rootfs.
	// Invoked by `cs-sandbox build`; may be slow and may hard-fail on missing
	// host packages.
	Prepare(ctx context.Context) error
	// Verify checks that everything Create needs is already present, returning an
	// actionable error (pointing at `cs-sandbox build`) when it is not. Create
	// calls this instead of building anything implicitly.
	Verify(ctx context.Context) error
	Create(ctx context.Context, s CreateSpec) (*state.Instance, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Remove(ctx context.Context, name string, purge bool) error
	Exec(ctx context.Context, name string, io ExecIO) error
	Port(ctx context.Context, name string) (int, error)
}

// Deps are the shared services every engine adapter needs.
type Deps struct {
	Runner  run.Runner
	Host    hostenv.Host
	InstDir string
	TierDir string
	Image   string
	// Group is the isolation boundary this operation acts within: it selects the
	// instance directory, the Podman network, the SSH keys and the gateway.
	Group string
	// TapPrefix is the group's allocated VM tap prefix (host-global namespace).
	TapPrefix string
	Network   string
	SSHBind   string // host bind address for published SSH ports (127.0.0.1 default)
	TZ        string
	FCCache   string // firecracker artifact cache dir (XDG cache)
	AssetDir  string // checkout root holding build assets (Containerfile, guest init); "" -> embedded
	// StartTimeout is the readiness wait budget in seconds.
	StartTimeout int
	// Progress is an optional sink for human-facing progress lines emitted during
	// slow operations (create/build). nil = silent (the default in tests).
	Progress func(string)
	// Note is an optional sink for always-shown advisories (e.g. "no host Claude
	// auth — not seeding"). Unlike Progress it isn't verbosity-gated. nil = silent.
	Note func(string)
}

// say emits a progress line, or nothing when no reporter is wired (e.g. tests).
func (d Deps) say(format string, a ...any) {
	if d.Progress != nil {
		d.Progress(fmt.Sprintf(format, a...))
	}
}

// note emits an always-shown advisory, or nothing when no sink is wired.
func (d Deps) note(msg string) {
	if d.Note != nil {
		d.Note(msg)
	}
}

// group returns the group this operation acts within, defaulting to the
// default group so zero-valued Deps in tests stay usable.
func (d Deps) group() string {
	if d.Group == "" {
		return state.DefaultGroup
	}
	return d.Group
}

// InstanceDir returns <InstDir>/<group>/<name>.
func (d Deps) InstanceDir(name string) string { return state.Dir(d.InstDir, d.group(), name) }
