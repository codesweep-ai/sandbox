package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/state"
)

// Identity is (group, name), so the same sandbox name may exist in several
// groups. Addressing follows one rule, with no exceptions:
//
//	worker-01              the worker-01 in the DEFAULT group, and nothing else
//	api.cache-redis        the api in the cache-redis group
//
// A bare name never falls back to "whichever group happens to hold it
// uniquely". That fallback made a reference's meaning depend on what else
// existed on the host: `ssh worker-01` worked until some unrelated group
// created its own worker-01, and then the same command either broke or — far
// worse — kept working while denoting a different sandbox. A reference that
// always means the same thing is worth more than one that saves typing.

// SplitRef splits "<name>.<group>" into its parts. A reference without a dot
// carries no group and must be resolved against what exists.
func SplitRef(ref string) (name, group string) {
	if i := strings.LastIndex(ref, "."); i > 0 && i < len(ref)-1 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// Ref renders an instance's canonical reference.
func Ref(in *state.Instance) string { return in.Name + "." + in.Group }

// resolve turns a user reference into exactly one instance record. An
// unqualified reference is a default-group reference; it is never searched for
// elsewhere.
func (a *App) resolve(ref string) (*state.Instance, error) {
	name, group := SplitRef(ref)
	if group == "" {
		group = state.DefaultGroup
	}
	if in, err := state.Load(a.InstDir, group, name); err == nil {
		return in, nil
	}
	return nil, a.notFound(ref, name, group)
}

// notFound explains a miss. When the name exists in some other group, saying so
// is the whole point: the user named something real and only got the group
// wrong, and the fix is a qualified reference rather than a different name.
func (a *App) notFound(ref, name, group string) error {
	insts, _ := state.List(a.InstDir)
	var elsewhere []string
	for _, in := range insts {
		if in.Name == name && in.Group != group {
			elsewhere = append(elsewhere, Ref(in))
		}
	}
	if len(elsewhere) == 0 {
		return fmt.Errorf("no such sandbox %q", ref)
	}
	sort.Strings(elsewhere)
	return fmt.Errorf("no such sandbox %q in group %q; it exists as %s — name the group",
		name, group, strings.Join(elsewhere, ", "))
}

// exists reports whether a reference already names a sandbox, for create's
// duplicate check.
func (a *App) exists(group, name string) bool {
	_, err := state.Load(a.InstDir, group, name)
	return err == nil
}
