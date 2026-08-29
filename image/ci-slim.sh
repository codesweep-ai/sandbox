#!/bin/sh
# Emit ONE slimmed tier Containerfile on stdout, for CI.
#
#   ./image/ci-slim.sh base   > /tmp/Containerfile.base.ci
#   ./image/ci-slim.sh agents > /tmp/Containerfile.agents.ci
#   ./image/ci-slim.sh leaf   > /tmp/Containerfile.ci
#
# The live tests need a sandbox that BOOTS: sshd, the dev user the entrypoint
# creates at first boot, sudo, git, python3, nested podman, and the /sandbox
# rootfs. They need none of the toolchains a developer's sandbox exists to
# provide — and those are ~5 GB of the shipped image and nearly all of its build
# time (pyenv alone compiles CPython from source; the Neovim layer pre-installs
# ~900 MB of language servers). Slimmed, the image is 474 MB and builds in about
# three minutes, which is what makes running the live tests in CI affordable.
#
# This DERIVES those images from the real Containerfiles rather than duplicating
# them. There is one source of truth per tier, and the failure mode of a stale
# DROP list is a slower CI image — never a wrong one, and never a divergence
# that lets CI pass on something the shipped image would fail.
#
# THE TIERS MIRROR THE REAL ONES, one for one:
#
#   base    Containerfile.base    -> sandbox-slim-base    (OS packages only)
#   agents  Containerfile.agents  -> sandbox-slim-agents  (+ the three CLIs)
#   leaf    Containerfile         -> sandbox-slim         (+ this repo)
#
# so a slim build gets the same per-commit economics as the shipped one: the
# leaf is `COPY . /sandbox` and the env file, and nothing else moves.
#
# THERE IS NO AGENT-FREE SLIM VARIANT. There used to be, saving ~325 MB on the
# CI image tar, and it cost a seventh published package plus a name
# (sandbox-slim-agents) that meant a product in one family and a tier in the
# other. The agents ride in tier 2 of both families now.
#
# If a new toolchain stanza is added and not listed here, CI keeps working and
# just carries the extra weight. Add its marker below when you notice.
set -eu

tier="${1:-}"
case "$tier" in
  base|agents|leaf) ;;
  *) echo "ci-slim: usage: $0 <base|agents|leaf> [containerfile]" >&2; exit 2 ;;
esac

here="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
case "$tier" in
  base)   default="$here/Containerfile.base" ;;
  agents) default="$here/Containerfile.agents" ;;
  leaf)   default="$here/Containerfile" ;;
esac
src="${2:-$default}"
test -f "$src" || { echo "ci-slim: no Containerfile at $src" >&2; exit 1; }

# What the slim BASE keeps. Each DROP entry below is a string unique to the one
# stanza it removes. A stanza is a blank-line-separated block, which is how the
# Containerfiles are already written, so ARG lines stay attached to the RUN they
# configure and comments stay with what they describe.
#
# Single-quoted deliberately: one marker contains ${GO_VERSION}, which the shell
# would otherwise expand to nothing and turn into a prefix that matches the Go
# stanza and anything else mentioning go.dev/dl.
#
# The base package layer is not dropped but REPLACED. What goes is the developer
# toolchain — compilers, headers, graphviz, the packet-capture and search tools —
# which no live test touches. What stays is what makes this a SANDBOX rather than
# a container that happens to boot, and two of those earned their place the hard
# way, by being cut and breaking something:
#
#   jq       the entrypoint pre-completes Claude's first-run wizard through it,
#            behind a `command -v jq` guard. Without jq the guard is simply
#            false, so a sandbox that inherited a login comes up showing "API
#            Usage Billing" instead of signed in — with no error anywhere.
#   podman   nested podman is a shipped feature and `internal/store` shells
#            /usr/bin/podman inside this image for every store operation.
#            Without the binary the entrypoint's nested-engine setup (guarded
#            the same silent way) never runs. An image like that is not the
#            thing under test. It costs ~260 MB of the ~690 MB total, and it is
#            worth it.
#            fuse-overlayfs/slirp4netns/passt/containers-common come with it
#            because the entrypoint's storage and network fallbacks name them.
#   curl     the Firecracker guest-kernel build runs inside this image and
#            fetches upstream's extract-vmlinux with it.
#   socat    a microVM's PID 1 is `exec socat VSOCK-LISTEN:22 ... TCP4:...:22`,
#            the bridge the host reaches sshd through. Without it PID 1 exits
#            127 and the guest kernel panics with "Attempted to kill init!" —
#            AFTER fc-init has already echoed FC-VM-READY, so the VM reports
#            ready, dies, and the failure surfaces as ssh never answering.
#   tmux     the agent tools are shell wrappers and every one of them drives its
#            agent inside a tmux session — 63 references across the family.
#            Without tmux a member comes up healthy, passes its readback,
#            accepts a dispatch, and then every turn dies at `tmux: command not
#            found` with exit 3 and nothing else to go on. It is unconditional
#            now that every slim image carries the agents; ncurses-term comes
#            along for tmux's own terminfo entry.
#
# A live test that needs more than this list belongs in `make test-integration`
# against the real image, or the package belongs here. Adding one is cheap
# relative to the toolchain layers, which are what the size is really about.
BASE_PACKAGES='openssh-server openssh-clients sudo shadow-utils git jq curl socat python3 procps-ng hostname iproute iputils findutils tar podman fuse-overlayfs slirp4netns passt containers-common tmux ncurses-term'

out=$(awk -v tier="$tier" -v base="$BASE_PACKAGES" '
BEGIN {
  RS = ""; ORS = ""
  want_replaced = 0; want_path = 0
  if (tier == "base") {
    DROP["chromium"]                        # browser + its X/mesa font stack
    DROP["neovim/releases/download"]        # editor tarball
    DROP["tree-sitter-cli"]                 # required by the nvim config only
    DROP["pyenv.run"]                       # compiles CPython from source
    DROP["nvm-sh/nvm"]                      # node
    DROP["temurin25-binaries"]              # jdk
    DROP["archive.apache.org/dist/maven"]   # maven
    DROP["go.dev/dl/go${GO_VERSION}"]       # go toolchain
    DROP["golangci-lint/releases/download"] # the dev toolchain for this repo
    DROP["cmd/deadcode@"]                   # deadcode, the one built from source
    DROP["python3 -m venv /opt/py-tools"]   # python CLI tools venv
    DROP["+Lazy! install"]                  # the nvim plugin/LSP pre-build
    DROP["COPY home/.config/nvim"]          # the config that pre-build reads
    DROP["/opt/nvim/.config-hash"]          # the guard over a pre-build that is gone
    want_dropped = 14
    want_replaced = 1                       # the base package install
    want_path = 1                           # the toolchain PATH
  } else if (tier == "leaf") {
    DROP["cmd/cs-sandbox@"]                 # the codesweep tools: need the go the base drops
    DROP["/opt/nvim/.config-hash"]          # the other half of the guard above
    want_dropped = 2
  } else {
    want_dropped = 0                        # agents: the three CLIs are the point
  }
}
{
  for (m in DROP)
    if (index($0, m)) { dropped++; next }

  # The base install layer: replaced rather than dropped — see BASE_PACKAGES.
  if (tier == "base" && index($0, "dnf update -y && dnf install -y")) {
    replaced++
    printf "# CI: reduced to what a sandbox must BOOT with. See image/ci-slim.sh.\n"
    printf "RUN dnf update -y && dnf install -y \\\n  %s && \\\n  dnf clean all\n\n", base
    next
  }

  # The shipped PATH names eight toolchain directories that are no longer in the
  # image. Harmless but misleading, so it is replaced by the three that are —
  # the agents have to be found by bare name, and tier 2 fills these in.
  if (tier == "base" && index($0, "ENV JAVA_HOME")) {
    pathed++
    printf "# CI: PATH reduced to the agent CLIs. See image/ci-slim.sh.\n"
    printf "ENV PATH=/opt/claude/bin:/opt/codex/bin:/opt/opencode/bin:${PATH}\n\n"
    next
  }

  printf "%s\n\n", $0
}
END {
  if (dropped < want_dropped)   { print "ci-slim: " tier ": only " dropped " of " want_dropped " stanzas dropped — markers have gone stale" > "/dev/stderr"; exit 1 }
  if (replaced != want_replaced) { print "ci-slim: " tier ": matched " replaced " base install layers, want " want_replaced > "/dev/stderr"; exit 1 }
  if (pathed != want_path)       { print "ci-slim: " tier ": matched " pathed " PATH stanzas, want " want_path > "/dev/stderr"; exit 1 }
}
' "$src")

printf '%s' "$out"
