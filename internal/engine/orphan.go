package engine

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// StatusRemoved is the `ls` status of data left behind by `rm`: the sandbox is
// gone, its data is not. Naming it in the STATUS column follows `multipass list`
// (which keeps deleted instances visible as DELETED until `purge`) and `kubectl
// get pv` (a volume whose claim is gone reads Released) — the alternative,
// docker's invisible dangling volumes, is the failure mode this avoids.
const StatusRemoved = "removed"

// homeVolumePrefix is the podman volume holding a sandbox's home; the
// nested-containers store is the other volume a purge has to take.
const homeVolumePrefix = "cs-sandbox-home-"

// Orphan is data whose sandbox no longer exists: `rm` deliberately keeps the
// home (a podman volume, or a microVM's rootfs.ext4) so `create <name>` reuses
// it, while dropping the state record that every instance command looks up.
// Without this the kept data is unreachable — invisible to `ls` and impossible
// to `destroy`.
type Orphan struct {
	Name   string
	Engine state.Engine
	Since  time.Time // when the data was last written; zero if unknown
}

// SinceRFC3339 renders Since for the AGE column, or "" when unknown.
func (o Orphan) SinceRFC3339() string {
	if o.Since.IsZero() {
		return ""
	}
	return o.Since.UTC().Format(time.RFC3339)
}

// Orphans lists leftover data with no sandbox, name-ordered, across both
// engines: a microVM leaves `instances/<name>/rootfs.ext4` behind, a container
// leaves the `cs-sandbox-home-<name>` volume. Costs one `podman volume ls`.
func (d Deps) Orphans(ctx context.Context) []Orphan {
	// Keyed by the qualified reference, because that is what podman object names
	// carry: a bare name would mark one group's sandbox as accounting for
	// another group's leftover data of the same name.
	live := map[string]bool{}
	insts, _ := state.List(d.InstDir)
	for _, in := range insts {
		live[Qualify(in)] = true
	}

	found := map[string]Orphan{}
	// Firecracker: an instance dir pruned down to the home disk. Instance dirs
	// live one level below the group, so this walks groups then members.
	if groups, err := os.ReadDir(d.InstDir); err == nil {
		for _, ge := range groups {
			if !ge.IsDir() {
				continue
			}
			gdir := filepath.Join(d.InstDir, ge.Name())
			entries, err := os.ReadDir(gdir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				ref := obj(ge.Name(), e.Name())
				if !e.IsDir() || live[ref] {
					continue
				}
				fi, err := os.Stat(filepath.Join(gdir, e.Name(), "rootfs.ext4"))
				if err != nil {
					continue
				}
				found[ref] = Orphan{Name: ref, Engine: state.Firecracker, Since: fi.ModTime()}
			}
		}
	}
	// Podman: the home volume outliving its container.
	res, err := d.Runner.Run(ctx, run.Opts{ReadOnly: true}, "podman", "volume", "ls",
		"--format", "{{.Name}}|{{.CreatedAt}}")
	if err == nil {
		for line := range strings.SplitSeq(res.Stdout, "\n") {
			vol, created, _ := strings.Cut(strings.TrimSpace(line), "|")
			name, ok := strings.CutPrefix(vol, homeVolumePrefix)
			if !ok || name == "" || live[name] {
				continue
			}
			o := Orphan{Name: name, Engine: state.Podman}
			// podman renders CreatedAt as a Go time; an unparseable one just
			// leaves AGE blank rather than failing the listing.
			if t, err := time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", created); err == nil {
				o.Since = t
			}
			found[name] = o
		}
	}

	out := make([]Orphan, 0, len(found))
	for _, o := range found {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Orphan returns the leftover data named name, if any.
func (d Deps) Orphan(ctx context.Context, name string) (Orphan, bool) {
	for _, o := range d.Orphans(ctx) {
		if o.Name == name {
			return o, true
		}
	}
	return Orphan{}, false
}

// PurgeOrphan deletes leftover data — what `destroy` does for a sandbox that
// still exists, for one that `rm` already removed.
func (d Deps) PurgeOrphan(ctx context.Context, o Orphan) error {
	// o.Name is the qualified reference, so the data dir comes from its parts
	// rather than from whichever group this Deps happens to be scoped to.
	g, n := splitObj(o.Name)
	if o.Engine == state.Firecracker {
		return os.RemoveAll(state.Dir(d.InstDir, g, n))
	}
	for _, v := range []string{homeVolumePrefix + o.Name, "cs-sandbox-containers-" + o.Name} {
		if _, err := d.Runner.Run(ctx, run.Opts{}, "podman", "volume", "rm", "-f", v); err != nil {
			return err
		}
	}
	// A container sandbox can also leave an (empty) instance dir behind.
	return os.RemoveAll(state.Dir(d.InstDir, g, n))
}
