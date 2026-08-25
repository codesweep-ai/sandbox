package assets

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// slimmed runs image/ci-slim.sh over the real Containerfile and returns what it
// emits. Shelling out is the point: this is the script `cs-sandbox build --slim`
// runs, and the contract under test is its output, not a Go transcription of it.
func slimmed(t *testing.T, keepAgents string) string {
	t.Helper()
	cmd := exec.Command("sh", "image/ci-slim.sh", "image/Containerfile")
	cmd.Env = append(os.Environ(), "CI_SLIM_KEEP_AGENTS="+keepAgents)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ci-slim.sh CI_SLIM_KEEP_AGENTS=%s: %v: %s", keepAgents, err, errb.String())
	}
	return string(out)
}

// agentStanzas are the download markers for the three agent CLIs — one per
// stanza, the same strings ci-slim.sh keys its DROP list on.
var agentStanzas = []string{
	"downloads.claude.ai",
	"openai/codex/releases",
	"anomalyco/opencode/releases",
}

// toolchainStanzas are what BOTH modes must drop: they are the size and nearly
// all of the build time, and no live test in either repository touches them.
var toolchainStanzas = []string{
	"chromium", "neovim/releases/download", "tree-sitter-cli", "pyenv.run",
	"nvm-sh/nvm", "temurin25-binaries", "archive.apache.org/dist/maven",
	"go.dev/dl/go", "python3 -m venv /opt/py-tools", "+Lazy! install",
}

// TestCISlimDropsAgentsByDefault: with no CI_SLIM_KEEP_AGENTS the three agent
// CLIs go with the toolchains. That is what this repository's own CI builds —
// its live tests need a sandbox that boots and never run an agent.
func TestCISlimDropsAgentsByDefault(t *testing.T) {
	out := slimmed(t, "0")
	// tmux rides with the agents; nothing that boots needs it.
	if strings.Contains(out, "tmux") {
		t.Error("the boot-only slim image installs tmux, which only the agent tools use")
	}
	for _, m := range append(append([]string{}, agentStanzas...), toolchainStanzas...) {
		if strings.Contains(out, m) {
			t.Errorf("slim Containerfile still has %q", m)
		}
	}
	// The PATH stanza goes too: every directory it named is gone.
	if strings.Contains(out, "ENV PATH=") {
		t.Error("slim Containerfile kept a PATH stanza naming directories it dropped")
	}
}

// TestCISlimKeepsAgentsWhenAsked: CI_SLIM_KEEP_AGENTS=1 keeps the three CLIs and
// puts them — and only them — on PATH.
//
// The PATH half is not decoration. The campaign repository's smoke tier replays a
// whole campaign, and its readback runs `command -v <cli>` inside every member
// before anything else; a binary kept in /opt but off PATH fails that check in
// exactly the way an absent one does, so the two have to move together.
func TestCISlimKeepsAgentsWhenAsked(t *testing.T) {
	out := slimmed(t, "1")
	for _, m := range agentStanzas {
		if !strings.Contains(out, m) {
			t.Errorf("--with-agents slim Containerfile dropped %q", m)
		}
	}
	const want = "ENV PATH=/opt/claude/bin:/opt/codex/bin:/opt/opencode/bin:${PATH}"
	if !strings.Contains(out, want) {
		t.Errorf("missing the agent PATH stanza %q", want)
	}
	// Still slim: keeping the agents must not smuggle the toolchains back.
	for _, m := range toolchainStanzas {
		if strings.Contains(out, m) {
			t.Errorf("--with-agents slim Containerfile still has toolchain %q", m)
		}
	}
	// tmux is not optional here: every agent tool wrapper drives its agent in a
	// tmux session, so without it a member boots, passes its readback, takes a
	// dispatch and then fails every turn with "tmux: command not found".
	for _, pkg := range []string{"tmux", "ncurses-term"} {
		if !strings.Contains(out, pkg) {
			t.Errorf("--with-agents base layer is missing %q, which the agent tools need to run", pkg)
		}
	}
	// And the PATH names nothing that was dropped — a stale entry is a sandbox
	// that looks configured and resolves nothing.
	for _, gone := range []string{"/opt/go/bin", "/opt/java/bin", "/opt/maven/bin", "/opt/pyenv/bin", "/opt/py-tools/bin", "/opt/nvm"} {
		if strings.Contains(out, gone) {
			t.Errorf("PATH still names dropped directory %q", gone)
		}
	}
}

// TestCISlimShipsTheCLIOnlyInTheRealImage: the shipped image installs cs-sandbox,
// and the CI image does not.
//
// A guard rather than a preference. The CI image drops the Go toolchain that
// builds the CLI, so it is the one sandbox without it, which means no live test
// can notice the day the install stanza is deleted from the Containerfile.
func TestCISlimShipsTheCLIOnlyInTheRealImage(t *testing.T) {
	const marker = "cmd/cs-sandbox@latest"
	real, err := os.ReadFile(filepath.Join("image", "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(real), marker) {
		t.Errorf("the shipped Containerfile no longer installs the CLI (%q)", marker)
	}
	for _, keep := range []string{"0", "1"} {
		if out := slimmed(t, keep); strings.Contains(out, marker) {
			t.Errorf("CI_SLIM_KEEP_AGENTS=%s: the CI image installs the CLI, but has no Go to build it", keep)
		}
	}
}

// TestCISlimKeepsWhatASandboxBootsWith: both modes keep the reduced base layer,
// and it carries what the campaign mission actually shells out to — git to commit
// on a branch, python3 to run the unittest suite the mission asks for.
func TestCISlimKeepsWhatASandboxBootsWith(t *testing.T) {
	for _, keep := range []string{"0", "1"} {
		out := slimmed(t, keep)
		for _, pkg := range []string{"openssh-server", "sudo", "git", "jq", "curl", "socat", "python3", "podman"} {
			if !strings.Contains(out, pkg) {
				t.Errorf("CI_SLIM_KEEP_AGENTS=%s: base layer lost %q", keep, pkg)
			}
		}
		if !strings.Contains(out, "FROM ") {
			t.Errorf("CI_SLIM_KEEP_AGENTS=%s: no FROM line survived", keep)
		}
	}
}

// optRoots are the shared toolchain trees under /opt, each installed by one
// stanza of the Containerfile.
var optRoots = []string{
	"/opt/pyenv", "/opt/nvm", "/opt/java", "/opt/maven", "/opt/go",
	"/opt/claude", "/opt/codex", "/opt/opencode", "/opt/py-tools", "/opt/nvim",
}

// stanzas splits a Containerfile on blank lines and strips comment-only lines.
// Blank-line-separated blocks are how the Containerfile is already written (and
// how ci-slim.sh reads it), so a stanza is one install and the ARG/ENV lines
// that configure it. Comments go because they mention paths their stanza never
// writes, which would make the "first stanza naming this tree" test below pick
// the wrong block.
func stanzas(src string) []string {
	var out []string
	for _, block := range strings.Split(src, "\n\n") {
		var kept []string
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				kept = append(kept, line)
			}
		}
		if s := strings.TrimSpace(strings.Join(kept, "\n")); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TestOptStanzasChmodInLayer: every /opt toolchain is made world-readable by the
// same stanza that installs it, and nothing chmods /opt as a whole afterwards.
//
// This is a size guard, not a correctness one — the permissions are identical
// either way. `RUN chmod -R a+rX /opt` as its own stanza cost 1.9 GB of the 9.2
// GB image: chmod writes every inode it touches, overlayfs copies up every file
// written in a layer, so a whole-tree chmod after the installs duplicates all of
// /opt into a layer that adds no content. Doing it inside each install layer is
// free, and it is the kind of thing that gets "helpfully" re-added.
func TestOptStanzasChmodInLayer(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("image", "Containerfile"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := stanzas(string(src))

	for _, b := range blocks {
		if strings.Contains(b, "chmod -R a+rX /opt\n") || strings.HasSuffix(b, "chmod -R a+rX /opt") {
			t.Error("a whole-tree `chmod -R a+rX /opt` stanza is back; it duplicates ~1.9 GB of /opt " +
				"into a layer that adds no content. chmod inside each install stanza instead")
		}
	}

	for _, root := range optRoots {
		var found bool
		for _, b := range blocks {
			if !strings.Contains(b, root) {
				continue
			}
			found = true
			// The first stanza that names the tree is the one that installs it.
			if !strings.Contains(b, "chmod -R a+rX") {
				t.Errorf("the stanza installing %s does not chmod it in the same layer; "+
					"without that it is unreadable to non-root users in the sandbox", root)
			}
			break
		}
		if !found {
			t.Errorf("no stanza installs %s — drop it from optRoots if the toolchain is gone", root)
		}
	}
}
