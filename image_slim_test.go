package assets

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The image is built in three tiers, each its own Containerfile and its own
// published package, because they change at three different rates:
//
//	base    the OS and every toolchain   rebuilt weekly, on a schedule
//	agents  the three agent CLIs         7 bumps in this repo's first 59 days
//	leaf    this repository              every commit
//
// Two consecutive builds of the old single-file image shared NONE of their 26
// layer blobs, so a commit put 2167.8 MB on the wire to change 25 MB of content.
// The tests below guard the boundaries that make the split hold: what lives in
// which file, and what ci-slim.sh may drop from each.
const (
	baseFile   = "Containerfile.base"
	agentsFile = "Containerfile.agents"
	leafFile   = "Containerfile"
)

// slimmed runs image/ci-slim.sh over one tier and returns what it emits.
// Shelling out is the point: this is the script `cs-sandbox build --slim` runs,
// and the contract under test is its output, not a Go transcription of it.
func slimmed(t *testing.T, tier string) string {
	t.Helper()
	cmd := exec.Command("sh", "image/ci-slim.sh", tier)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ci-slim.sh %s: %v: %s", tier, err, errb.String())
	}
	return string(out)
}

// tierSource reads one tier's real Containerfile.
func tierSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("image", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// agentStanzas are the download markers for the three agent CLIs — one per
// stanza, and the whole of what tier 2 installs.
var agentStanzas = []string{
	"downloads.claude.ai",
	"openai/codex/releases",
	"anomalyco/opencode/releases",
}

// toolchainStanzas are what the slim base must drop: they are the size and
// nearly all of the build time, and no live test in either repository touches
// them.
var toolchainStanzas = []string{
	"chromium", "neovim/releases/download", "tree-sitter-cli", "pyenv.run",
	"nvm-sh/nvm", "temurin25-binaries", "archive.apache.org/dist/maven",
	"go.dev/dl/go", "python3 -m venv /opt/py-tools", "+Lazy! install",
}

// TestTierBoundaries: each Containerfile holds only what its tier is for.
//
// This is the invariant the whole split rests on. The base and the agents are
// rebuilt on a schedule and named by tag from the tier above, so anything in
// them that reads this repository would be frozen at whatever the last base
// build saw — an entrypoint edit that never reaches the image, tested green
// against the old one. The staged Neovim config is the single exception, and
// the config-hash guard below is the price of it.
func TestTierBoundaries(t *testing.T) {
	base, agents, leaf := tierSource(t, baseFile), tierSource(t, agentsFile), tierSource(t, leafFile)

	// Instruction lines only: all three files DISCUSS `COPY . /sandbox` in
	// comments, because where it sits is the point of the split.
	copies := func(src string) []string {
		var out []string
		for line := range strings.SplitSeq(src, "\n") {
			if strings.HasPrefix(line, "COPY ") || strings.HasPrefix(line, "ADD ") {
				out = append(out, line)
			}
		}
		return out
	}
	if !slices.ContainsFunc(copies(leaf), func(l string) bool { return strings.HasPrefix(l, "COPY . /sandbox") }) {
		t.Error("the leaf no longer copies the repository in; that is what it is for")
	}
	// The one COPY the base is allowed: the editor config the pre-build reads.
	for _, line := range copies(base) {
		if !strings.HasPrefix(line, "COPY home/.config/nvim") {
			t.Errorf("%s has an unexpected %q; the base may read only the Neovim config, or every "+
				"edit under image/rootfs needs a base rebuild before it reaches the image", baseFile, line)
		}
	}
	for _, line := range copies(agents) {
		t.Errorf("%s reads the build context (%q); tier 2 is three downloads and nothing else", agentsFile, line)
	}

	// The agents live in their own tier, not smuggled into the one below or the
	// one above: below, an agent bump would re-create the 956 MB Neovim
	// pre-build; above, it would ride on every commit.
	for _, m := range agentStanzas {
		if !strings.Contains(agents, m) {
			t.Errorf("%s no longer installs %q", agentsFile, m)
		}
		if strings.Contains(base, m) || strings.Contains(leaf, m) {
			t.Errorf("%q is installed outside %s", m, agentsFile)
		}
	}

	// Each tier names the one below it, and only through a build arg, so the
	// same files serve a local `--rebuild-base` and the published tags.
	if !strings.Contains(agents, "FROM ${BASE_REF}") {
		t.Errorf("%s must take its base through BASE_REF", agentsFile)
	}
	if !strings.Contains(leaf, "FROM ${AGENTS_REF}") {
		t.Errorf("%s must take its base through AGENTS_REF", leafFile)
	}
}

// TestConfigHashGuard: the base records the Neovim config it pre-built against,
// and the leaf refuses to ship a different one.
//
// Without it the failure is silent. The base carries .config/nvim AND the
// plugins built from it; the leaf's `COPY . /sandbox` then overwrites the config
// with whatever is in the checkout. Change the config and the image ships one
// naming plugins that were never installed — no build error, no failing test,
// visible only when somebody opens nvim inside a sandbox.
func TestConfigHashGuard(t *testing.T) {
	const marker = "/opt/nvim/.config-hash"
	base, leaf := tierSource(t, baseFile), tierSource(t, leafFile)
	if !strings.Contains(base, marker) {
		t.Errorf("%s does not record the config hash; the leaf has nothing to check against", baseFile)
	}
	if !strings.Contains(leaf, marker) {
		t.Errorf("%s does not check the config hash; a config edit would half-apply in silence", leafFile)
	}
	// The check has to run AFTER the copy that can invalidate it.
	if strings.Index(leaf, "COPY . /sandbox") > strings.Index(leaf, marker) {
		t.Errorf("%s checks the config hash before `COPY . /sandbox` replaces the config", leafFile)
	}
	// Both halves go from the slim images, where there is no pre-build to guard.
	for _, tier := range []string{"base", "leaf"} {
		if strings.Contains(slimmed(t, tier), marker) {
			t.Errorf("the slim %s keeps the config-hash guard, but drops the pre-build it guards", tier)
		}
	}
}

// TestCISlimKeepsTheAgents: every slim image carries the three CLIs.
//
// There is no agent-free variant. The one that existed saved ~325 MB on a CI
// artifact and cost a seventh published package plus a name that meant a
// product in one family and a tier in the other.
//
// The PATH half is not decoration. The campaign repository's smoke tier replays
// a whole campaign, and its readback runs `command -v <cli>` inside every member
// before anything else; a binary kept in /opt but off PATH fails that check in
// exactly the way an absent one does.
func TestCISlimKeepsTheAgents(t *testing.T) {
	agents := slimmed(t, "agents")
	for _, m := range agentStanzas {
		if !strings.Contains(agents, m) {
			t.Errorf("the slim agents tier dropped %q", m)
		}
	}

	base := slimmed(t, "base")
	const want = "ENV PATH=/opt/claude/bin:/opt/codex/bin:/opt/opencode/bin:${PATH}"
	if !strings.Contains(base, want) {
		t.Errorf("the slim base is missing the agent PATH stanza %q", want)
	}
	// The PATH names nothing that was dropped — a stale entry is a sandbox that
	// looks configured and resolves nothing.
	for _, gone := range []string{"/opt/go/bin", "/opt/java/bin", "/opt/maven/bin", "/opt/pyenv/bin", "/opt/py-tools/bin", "/opt/nvm"} {
		if strings.Contains(base, gone) {
			t.Errorf("the slim PATH still names dropped directory %q", gone)
		}
	}
	// Still slim: keeping the agents must not smuggle the toolchains back.
	for _, m := range toolchainStanzas {
		if strings.Contains(base, m) {
			t.Errorf("the slim base still has toolchain %q", m)
		}
	}
}

// TestCISlimShipsTheCLIOnlyInTheRealImage: the shipped leaf installs cs-sandbox,
// and the CI one does not.
//
// A guard rather than a preference. The CI image drops the Go toolchain that
// builds the CLI, so it is the one sandbox without it, which means no live test
// can notice the day the install stanza is deleted from the Containerfile.
func TestCISlimShipsTheCLIOnlyInTheRealImage(t *testing.T) {
	const marker = "cmd/cs-sandbox@"
	if !strings.Contains(tierSource(t, leafFile), marker) {
		t.Errorf("the shipped leaf no longer installs the CLI (%q)", marker)
	}
	if strings.Contains(slimmed(t, "leaf"), marker) {
		t.Error("the CI leaf installs the CLI, but has no Go to build it")
	}
}

// TestCISlimKeepsWhatASandboxBootsWith: the slim base keeps the reduced package
// layer, and it carries what the campaign mission actually shells out to — git
// to commit on a branch, python3 to run the unittest suite the mission asks for,
// tmux because every agent tool wrapper drives its agent inside a session.
func TestCISlimKeepsWhatASandboxBootsWith(t *testing.T) {
	out := slimmed(t, "base")
	for _, pkg := range []string{
		"openssh-server", "sudo", "git", "jq", "curl", "socat", "python3", "podman",
		"python-unversioned-command", "tmux", "ncurses-term",
	} {
		if !strings.Contains(out, pkg) {
			t.Errorf("the slim base layer lost %q", pkg)
		}
	}
	if !strings.Contains(out, "FROM ") {
		t.Error("no FROM line survived in the slim base")
	}
}

// optRoots are the shared toolchain trees under /opt, each installed by one
// stanza — the toolchains by the base, the three CLIs by the agents tier.
var optRoots = []string{
	"/opt/pyenv", "/opt/nvm", "/opt/java", "/opt/maven", "/opt/go",
	"/opt/claude", "/opt/codex", "/opt/opencode", "/opt/py-tools", "/opt/nvim",
}

// buildsSomething reports whether a stanza writes a layer at all.
//
// The PATH stanza names every tree under /opt in a single ENV line and creates
// none of them, and since the three agent CLIs moved BELOW the Neovim pre-build
// (so an agent bump stops re-creating its 956 MB) that ENV block now sits above
// their installs. Without this, "the first stanza naming this tree" picks the
// PATH block for /opt/claude, /opt/codex and /opt/opencode and reports a missing
// chmod that is really there. Matching on RUN/COPY rather than on the absence of
// ENV keeps the pyenv and nvm stanzas working: those name their root only in
// `ENV PYENV_ROOT=` / `ENV NVM_DIR=` and install through the variable.
func buildsSomething(stanza string) bool {
	for line := range strings.SplitSeq(stanza, "\n") {
		switch {
		case strings.HasPrefix(line, "RUN "), strings.HasPrefix(line, "COPY "), strings.HasPrefix(line, "ADD "):
			return true
		}
	}
	return false
}

// stanzas splits a Containerfile on blank lines and strips comment-only lines.
// Blank-line-separated blocks are how the Containerfiles are already written
// (and how ci-slim.sh reads them), so a stanza is one install and the ARG/ENV
// lines that configure it. Comments go because they mention paths their stanza
// never writes, which would make the "first stanza naming this tree" test below
// pick the wrong block.
func stanzas(src string) []string {
	var out []string
	for block := range strings.SplitSeq(src, "\n\n") {
		var kept []string
		for line := range strings.SplitSeq(block, "\n") {
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

// TestOptStanzasChmodInLayer: every tree under /opt is chmodded by the stanza
// that installs it, in the same layer.
//
// This is a size guard, not a correctness one — the permissions are identical
// either way. `RUN chmod -R a+rX /opt` as its own stanza cost 1.9 GB of the 9.2
// GB image: chmod writes every inode it touches, overlayfs copies up every file
// written in a layer, so a whole-tree chmod after the installs duplicates all of
// /opt into a layer that adds no content. Doing it inside each install layer is
// free, and it is the kind of thing that gets "helpfully" re-added.
//
// Base and agents together: the split moved three of the ten trees into their
// own file, and the rule applies to a tree wherever it is installed.
func TestOptStanzasChmodInLayer(t *testing.T) {
	blocks := append(
		stanzas(tierSource(t, baseFile)),
		stanzas(tierSource(t, agentsFile))...,
	)

	for _, b := range blocks {
		if strings.Contains(b, "chmod -R a+rX /opt\n") || strings.HasSuffix(b, "chmod -R a+rX /opt") {
			t.Error("a whole-tree `chmod -R a+rX /opt` stanza is back; it duplicates ~1.9 GB of /opt " +
				"into a layer that adds no content. chmod inside each install stanza instead")
		}
	}

	for _, root := range optRoots {
		var found bool
		for _, b := range blocks {
			if !strings.Contains(b, root) || !buildsSomething(b) {
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

// TestCSToolsStanzaIsLast: nothing that writes a layer comes after the codesweep
// tools install.
//
// This is a churn guard, and it exists because of how buildah keys its cache.
// CS_SANDBOX_VERSION is the version of the cs-sandbox running the build, so it
// moves on every commit — and buildah writes EVERY in-scope build arg into each
// later step's history entry, which is its cache key. A step that never reads
// the arg still misses when it changes. That makes the ARG line's position, not
// just the RUN's, decide how much a version bump rebuilds.
//
// It matters less than it did now that the leaf is 25 MB rather than 6 GB, but
// it is the same rule one tier down: last, two leaves that differ only by
// cs-sandbox version share every layer but this one.
//
// Metadata instructions (LABEL/ENTRYPOINT/CMD) are fine after it: they add no
// filesystem content and nothing follows them.
func TestCSToolsStanzaIsLast(t *testing.T) {
	blocks := stanzas(tierSource(t, leafFile))

	idx := -1
	for i, b := range blocks {
		if strings.Contains(b, "cmd/cs-sandbox@") {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("no stanza installs cs-sandbox — the churn guard below has nothing to anchor on")
	}

	// Continuation lines are indented; only a column-0 first token is an
	// instruction, which keeps shell inside a RUN from matching.
	for _, b := range blocks[idx+1:] {
		for line := range strings.SplitSeq(b, "\n") {
			if line == "" || line[0] == ' ' || line[0] == '\t' {
				continue
			}
			switch verb, _, _ := strings.Cut(line, " "); verb {
			case "RUN", "COPY", "ADD", "ARG", "ENV":
				t.Errorf("%s comes after the codesweep tools stanza; that stanza must stay last "+
					"so a cs-sandbox version bump rebuilds its 25 MB and nothing else. "+
					"New stanzas go above it", verb)
			}
		}
	}
}

// TestNvimConfigStagedAboveThePreBuild: within the base, the Neovim config is
// staged above the pre-build that reads it, and its hash is recorded below.
//
// The pre-build is the largest layer in the image at 956 MB, and everything
// copied above it is one more thing whose edit re-creates it. Staging only the
// editor config keeps that to a real edit of the config or lazy-lock.json — the
// whole repository used to be up there, so touching the entrypoint rebuilt 956
// MB of language servers that had not changed.
func TestNvimConfigStagedAboveThePreBuild(t *testing.T) {
	blocks := stanzas(tierSource(t, baseFile))
	idx := func(want string) int {
		for i, b := range blocks {
			if strings.Contains(b, want) {
				return i
			}
		}
		t.Fatalf("no stanza in %s contains %q", baseFile, want)
		return -1
	}
	if staged, prebuild := idx("COPY home/.config/nvim"), idx("+Lazy! install"); staged > prebuild {
		t.Error("the nvim config is copied after the pre-build that reads it; stage it above")
	}
	// And the hash of that config is recorded after the pre-build, so it
	// describes what was actually built.
	if prebuild, hash := idx("+Lazy! install"), idx("/opt/nvim/.config-hash"); prebuild > hash {
		t.Error("the config hash is recorded before the pre-build it describes")
	}
}
