package doctor

// The tools this build ships and the tools it pins are two different questions,
// and doctor asks both here.
//
// The agent tools are FILES cs-sandbox carries: `install-agent-tools` copies
// image/rootfs/home/.local/bin onto the host PATH, and the same directory seeds
// every sandbox's ~/.local/bin. So the copy on PATH should be byte-identical to
// the copy this binary was built with, and when it is not the host is running a
// harness from some other build — which is exactly the state that reads as
// healthy right up until a dispatch behaves differently than the source says.
//
// The sibling cs- tools are not files this build carries but VERSIONS it names,
// in its own go.mod, embedded (assets.ToolPins). A host that has none of them is
// complete: they are developer gates and a lending proxy, and an operator who
// only boots sandboxes needs none. A host that HAS one at a version this build
// does not name is the case worth a line, because the two will disagree about
// something and nothing else would say so.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// siblingTools are the cs- tools this repository pins in go.mod and installs
// into the image, paired with the module path ToolPins keys by.
//
// cs-sandbox itself is deliberately absent: a binary comparing its own version
// to its own go.mod compares a number to itself. What answers for cs-sandbox is
// the caller that pinned it — `cs-campaign doctor` does exactly this check with
// cs-sandbox in the list.
var siblingTools = []struct{ bin, module string }{
	{"cs-vcr", "github.com/codesweep-ai/vcr"},
	{"cs-lint", "github.com/codesweep-ai/lint"},
	{"cs-ledger", "github.com/codesweep-ai/ledger"},
	{"cs-tracer", "github.com/codesweep-ai/tracer"},
}

// bundledToolsGroup compares every agent tool this build ships against the copy
// on PATH.
//
// Docs ship in the same directory and are skipped: `install-agent-tools` writes
// them 0644 precisely because they are not executed, and a stale CS_RC.md is
// not a harness that behaves differently.
//
// Returns ok=false when there is nothing to compare — no bundled tree, or not
// one tool installed — so the caller can keep saying "install them" rather than
// reporting 23 deviations at someone who has installed nothing.
func bundledToolsGroup(bundled fs.FS) (checks []Check, ok bool) {
	if bundled == nil {
		return nil, false
	}
	names, err := bundledToolNames(bundled)
	if err != nil || len(names) == 0 {
		return nil, false
	}
	var missing, deviating []string
	matched := 0
	for _, name := range names {
		want, err := hashFS(bundled, name)
		if err != nil {
			continue
		}
		path, err := lookPath(name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		got, err := hashFile(path)
		switch {
		case err != nil:
			deviating = append(deviating, fmt.Sprintf("%s unreadable at %s", name, path))
		case got != want:
			deviating = append(deviating, fmt.Sprintf("%s differs (ships %.12s…, on PATH %.12s…)", name, want, got))
		default:
			matched++
		}
	}
	if matched == 0 && len(deviating) == 0 {
		return nil, false // nothing installed; the presence advice covers it
	}
	if len(missing) == 0 && len(deviating) == 0 {
		return []Check{{OK, fmt.Sprintf("agent tools on PATH are the %d this build ships", matched)}}, true
	}
	if len(missing) > 0 {
		checks = append(checks, Check{NO, fmt.Sprintf("agent tools missing from PATH: %s — reinstall them:  cs-sandbox install-agent-tools",
			strings.Join(missing, " "))})
	}
	if len(deviating) > 0 {
		checks = append(checks, Check{NO, "agent tools on PATH are NOT the ones this build ships:\n      " +
			strings.Join(deviating, "\n      ") +
			"\n      the host is running a harness from another build — reinstall:  cs-sandbox install-agent-tools"})
	}
	return checks, true
}

// bundledToolNames lists the executables in the bundled tree, sorted so a
// report reads the same way twice.
func bundledToolNames(bundled fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(bundled, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// siblingToolsGroup compares each pinned sibling on PATH against the version
// this build names for it.
//
// Absent is not a finding. Present-and-unpinnable is: a go.mod that names no
// version for a tool the image installs is a build that cannot be reproduced,
// and it would otherwise pass silently as "nothing to compare".
func siblingToolsGroup(ctx context.Context, r run.Runner, pins map[string]string) Group {
	g := Group{Title: "codesweep tools (optional — checked against this build's go.mod)"}
	if pins == nil {
		return g
	}
	var absent []string
	for _, t := range siblingTools {
		if _, err := lookPath(t.bin); err != nil {
			absent = append(absent, t.bin)
			continue
		}
		want := pins[t.module]
		if want == "" {
			g.add(NO, t.bin+" is on PATH but this build's go.mod pins no version for it — run:  go get -tool "+t.module+"/cmd/"+t.bin+"@main")
			continue
		}
		got := toolVersion(run.Output(ctx, r, t.bin, "version"))
		switch {
		case got == "":
			g.add(NO, t.bin+" is on PATH but did not answer `"+t.bin+" version` — it cannot be identified")
		case got != want:
			g.add(NO, fmt.Sprintf("%s on PATH is %s, this build pins %s — install the pinned one:  go install %s/cmd/%s@%s",
				t.bin, got, want, t.module, t.bin, want))
		default:
			g.add(OK, t.bin+" on PATH matches the pin ("+want+")")
		}
	}
	if len(absent) > 0 {
		g.add(HM, "not on PATH (fine — nothing here needs them): "+strings.Join(absent, " "))
	}
	return g
}

// toolVersion picks the version out of `<tool> version`, which every tool in
// the family prints as `<name> <version> (<platform>, <toolchain>)`.
//
// The whole second field, +dirty included. Go's build info appends that to a
// binary built from a modified tree, a module version cannot carry it, so it
// can never equal a pin — which is the right answer rather than a formatting
// problem to trim away: a dirty build is not the revision it names.
func toolVersion(out string) string {
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return ""
	}
	// A version token starts with `v` or a digit: v0.0.0-<stamp> from a module
	// pseudo-version, 0.0.1-snapshot-<rev> from the older stamp, or the bare
	// short commit `git describe --always` falls back to. Anything else is not
	// a version line at all, and reading its second word as one would report
	// "on PATH is a" at an operator who printed prose.
	v := fields[1]
	if v == "" || !(v[0] == 'v' || (v[0] >= '0' && v[0] <= '9')) {
		return ""
	}
	return v
}

func hashFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(content)), nil
}

func hashFS(fsys fs.FS, name string) (string, error) {
	content, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(content)), nil
}
