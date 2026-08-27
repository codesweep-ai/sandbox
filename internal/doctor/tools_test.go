package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// installOnPath writes each name->content into a temp dir and points lookPath
// at it, so the hash comparison has real files to read. Anything not named here
// is absent, exactly as an uninstalled tool would be.
func installOnPath(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	orig := lookPath
	lookPath = func(bin string) (string, error) {
		p := filepath.Join(dir, bin)
		if _, err := os.Stat(p); err != nil {
			return "", os.ErrNotExist
		}
		return p, nil
	}
	t.Cleanup(func() { lookPath = orig })
}

func bundled(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func checksText(checks []Check) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(c.Message + "\n")
	}
	return b.String()
}

// The whole point of the identity check: a host with the tools installed, but
// installed from some OTHER build, must not read as healthy. Presence alone
// said "ok" here, which is the state that let a fleet run a harness nobody
// pinned.
func TestBundledToolsFlagsAToolThatDiffers(t *testing.T) {
	ship := map[string]string{"cs-claude": "v2", "cs-claude-turn": "same"}
	installOnPath(t, map[string]string{"cs-claude": "v1", "cs-claude-turn": "same"})

	checks, ok := bundledToolsGroup(bundled(ship))
	if !ok {
		t.Fatal("a host with tools installed must be compared, not skipped")
	}
	text := checksText(checks)
	if !strings.Contains(text, "NOT the ones this build ships") {
		t.Errorf("a differing tool must be named as such:\n%s", text)
	}
	if !strings.Contains(text, "cs-claude differs") {
		t.Errorf("the report must name the tool that differs:\n%s", text)
	}
	if strings.Contains(text, "cs-claude-turn differs") {
		t.Errorf("a tool that matches must not be reported:\n%s", text)
	}
	if statusOf(checks) != NO {
		t.Errorf("a drifted harness is a problem, not advice: %v", statusOf(checks))
	}
}

func TestBundledToolsAcceptsAnIdenticalInstall(t *testing.T) {
	same := map[string]string{"cs-claude": "a", "cs-codex": "b"}
	installOnPath(t, same)
	checks, ok := bundledToolsGroup(bundled(same))
	if !ok {
		t.Fatal("an installed surface must be compared")
	}
	if text := checksText(checks); !strings.Contains(text, "are the 2 this build ships") {
		t.Errorf("an identical install should say so:\n%s", text)
	}
	if statusOf(checks) != OK {
		t.Errorf("an identical install is not a problem: %v", statusOf(checks))
	}
}

// Docs ship in the same directory and install 0644. They are not executed, so a
// stale one is not a harness that behaves differently — and counting them would
// make the tally disagree with what `install-agent-tools` calls a tool.
func TestBundledToolsIgnoresDocs(t *testing.T) {
	installOnPath(t, map[string]string{"cs-claude": "a"})
	checks, ok := bundledToolsGroup(bundled(map[string]string{"cs-claude": "a", "CS_RC.md": "prose"}))
	if !ok {
		t.Fatal("an installed surface must be compared")
	}
	if text := checksText(checks); !strings.Contains(text, "are the 1 this build ships") {
		t.Errorf("docs must not be counted as tools:\n%s", text)
	}
}

// A host that has installed nothing is not a host running the wrong harness.
// Reporting every tool as missing there buries the one line it needs, which is
// "install them".
func TestBundledToolsSkipsAHostWithNothingInstalled(t *testing.T) {
	installOnPath(t, nil)
	if _, ok := bundledToolsGroup(bundled(map[string]string{"cs-claude": "a"})); ok {
		t.Error("a host with no tools installed must fall through to the presence advice")
	}
}

// Half an install is its own failure: the tools that are there can be perfectly
// current while the ones that are not make a dispatch die on a missing verb.
func TestBundledToolsNamesAMissingTool(t *testing.T) {
	installOnPath(t, map[string]string{"cs-claude": "a"})
	checks, ok := bundledToolsGroup(bundled(map[string]string{"cs-claude": "a", "cs-codex": "b"}))
	if !ok {
		t.Fatal("a partially installed surface must be compared")
	}
	text := checksText(checks)
	if !strings.Contains(text, "missing from PATH: cs-codex") {
		t.Errorf("the missing tool must be named:\n%s", text)
	}
	if statusOf(checks) != NO {
		t.Errorf("a half-installed harness is a problem: %v", statusOf(checks))
	}
}

func TestSiblingToolsComparesAgainstThePin(t *testing.T) {
	pins := map[string]string{
		"github.com/codesweep-ai/vcr":    "v0.0.0-aaa",
		"github.com/codesweep-ai/lint":   "v0.0.0-bbb",
		"github.com/codesweep-ai/ledger": "v0.0.0-ccc",
		"github.com/codesweep-ai/tracer": "v0.0.0-ddd",
	}
	installOnPath(t, map[string]string{"cs-vcr": "x", "cs-lint": "x"})
	fake := run.NewFake().
		OnStdout("cs-vcr version", "cs-vcr v0.0.0-aaa (linux/amd64, go1.27.0)").
		OnStdout("cs-lint version", "cs-lint v0.0.0-WRONG (linux/amd64, go1.27.0)")

	g := siblingToolsGroup(context.Background(), fake, pins)
	text := checksText(g.Checks)
	if !strings.Contains(text, "cs-vcr on PATH matches the pin (v0.0.0-aaa)") {
		t.Errorf("a matching sibling should say so:\n%s", text)
	}
	if !strings.Contains(text, "cs-lint on PATH is v0.0.0-WRONG, this build pins v0.0.0-bbb") {
		t.Errorf("a mismatched sibling must name both versions:\n%s", text)
	}
	if !strings.Contains(text, "go install github.com/codesweep-ai/lint/cmd/cs-lint@v0.0.0-bbb") {
		t.Errorf("a mismatch must carry the command that fixes it:\n%s", text)
	}
	// Absent is fine, and has to read that way: an operator who only boots
	// sandboxes has none of these and is not broken.
	if !strings.Contains(text, "not on PATH (fine") || !strings.Contains(text, "cs-ledger cs-tracer") {
		t.Errorf("absent siblings must be reported as fine, together:\n%s", text)
	}
	for _, c := range g.Checks {
		if strings.Contains(c.Message, "not on PATH") && c.Status != HM {
			t.Errorf("an absent sibling must not count as an issue: %v", c.Status)
		}
	}
}

// A tool on PATH that this build's go.mod does not name cannot be compared to
// anything, and a build that installs it into the image without pinning it
// cannot be reproduced. Silence there would read as "checked, fine".
func TestSiblingToolsFlagsAnUnpinnedToolOnPath(t *testing.T) {
	installOnPath(t, map[string]string{"cs-vcr": "x"})
	g := siblingToolsGroup(context.Background(), run.NewFake(), map[string]string{})
	text := checksText(g.Checks)
	if !strings.Contains(text, "go.mod pins no version for it") {
		t.Errorf("an unpinned tool on PATH must be named:\n%s", text)
	}
	if !strings.Contains(text, "go get -tool github.com/codesweep-ai/vcr/cmd/cs-vcr@main") {
		t.Errorf("the remedy must be the command that records the pin:\n%s", text)
	}
}

// "It is there but will not say what it is" is the same unknown the check
// exists to remove, so it is a problem rather than a silent pass.
func TestSiblingToolsFlagsAToolThatWillNotIdentifyItself(t *testing.T) {
	installOnPath(t, map[string]string{"cs-vcr": "x"})
	g := siblingToolsGroup(context.Background(), run.NewFake(),
		map[string]string{"github.com/codesweep-ai/vcr": "v0.0.0-aaa"})
	if text := checksText(g.Checks); !strings.Contains(text, "did not answer `cs-vcr version`") {
		t.Errorf("an unidentifiable tool must be reported:\n%s", text)
	}
}

// Go stamps +dirty on a binary built from a modified tree. A module version
// cannot carry that, so it can never equal a pin — and it must not be trimmed
// into agreement, because such a build is not the revision it names.
func TestToolVersionKeepsTheDirtyStamp(t *testing.T) {
	got := toolVersion("cs-vcr v0.0.0-20260826160252-bd9e6f2b8ab6+dirty (linux/amd64, go1.27.0)")
	if got != "v0.0.0-20260826160252-bd9e6f2b8ab6+dirty" {
		t.Errorf("toolVersion = %q, want the +dirty stamp kept", got)
	}
	if toolVersion("") != "" {
		t.Error("no output must read as no version, not as a match")
	}
}

// statusOf is the worst status in the set: one bad check makes the group bad.
func statusOf(checks []Check) Status {
	worst := OK
	for _, c := range checks {
		if c.Status == NO {
			return NO
		}
		if c.Status == HM {
			worst = HM
		}
	}
	return worst
}
