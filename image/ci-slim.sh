#!/bin/sh
# Emit a slimmed Containerfile on stdout, for CI.
#
# The live tests need a sandbox that BOOTS: sshd, the dev user the entrypoint
# creates at first boot, sudo, git, python3, nested podman, and the /sandbox
# rootfs. They need none of the toolchains a developer's sandbox exists to
# provide — and those are ~5 GB of the 6.04 GB image and nearly all of its build
# time (pyenv alone compiles CPython from source; the Neovim layer pre-installs
# ~900 MB of language servers). Slimmed, the image is 474 MB and builds in about
# three minutes, which is what makes running the live tests in CI affordable.
#
# This DERIVES that image from the real Containerfile rather than duplicating it
# as a second one. There is one source of truth, and the failure mode of a stale
# DROP list is a slower CI image — never a wrong one, and never a divergence
# that lets CI pass on something the shipped image would fail.
#
#   ./image/ci-slim.sh > /tmp/Containerfile.ci
#   podman build -f /tmp/Containerfile.ci -t localhost/cs-sandbox:ci image/rootfs
#
# If a new toolchain stanza is added to the Containerfile and not listed here,
# CI keeps working and just carries the extra weight. Add its marker below when
# you notice.
set -eu

src="${1:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/Containerfile}"
test -f "$src" || { echo "ci-slim: no Containerfile at $src" >&2; exit 1; }

# CI_SLIM_KEEP_AGENTS=1 keeps the three agent CLIs — claude, codex, opencode —
# that the default output drops with the rest of /opt.
#
# They are dropped by default because THIS repository's live tests only need a
# sandbox that boots; they never run an agent. A consumer whose tests drive one
# does need them: the campaign repository's smoke tier replays a whole campaign,
# and its readback asks `command -v <cli>` inside every member before anything
# else runs. Without the binaries that check fails and the run stops there.
#
# Measured: 474 MB without them, 1.38 GB with — the three trees under /opt are
# 858 MB of that, and nothing duplicates them any more now that each install
# stanza chmods in its own layer. Against 6.04 GB for the real image that is
# still small enough to build on a hosted runner, which is the point here.
keep_agents=0
case "${CI_SLIM_KEEP_AGENTS:-0}" in
  1|true|yes|on) keep_agents=1 ;;
esac

# Each DROP entry is a string unique to the one stanza it removes. A stanza is a
# blank-line-separated block, which is how the Containerfile is already written,
# so ARG lines stay attached to the RUN they configure and comments stay with
# what they describe.
#
# Single-quoted deliberately: one marker contains ${GO_VERSION}, which the shell
# would otherwise expand to nothing and turn into a prefix that matches the Go
# stanza and anything else mentioning go.dev/dl/go.
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
#
# Measured: every live test in the suite passes against this image.
#
# The codesweep tools and this repo's own dev toolchain both go, so the CI image
# is the one sandbox carrying neither. Nothing in the live tests reaches for them
# from inside: they drive sandboxes from the host. The codesweep tools and
# deadcode would need the Go this script drops anyway; the three prebuilt linters
# would not, and go because a CI image that boots sandboxes has no use for them.
#
# A live test that needs more than this list belongs in `make test-integration`
# against the real image, or the package belongs here. Adding one is cheap
# relative to the toolchain layers, which are what the size is really about.
#   curl     the Firecracker guest-kernel build runs inside this image and
#            fetches upstream's extract-vmlinux with it.
#   socat    a microVM's PID 1 is `exec socat VSOCK-LISTEN:22 ... TCP4:...:22`,
#            the bridge the host reaches sshd through. Without it PID 1 exits
#            127 and the guest kernel panics with "Attempted to kill init!" —
#            AFTER fc-init has already echoed FC-VM-READY, so the VM reports
#            ready, dies, and the failure surfaces as ssh never answering.
#
# Neither is needed by the podman image, so both gaps only appear once CI grows
# a Firecracker job. Every other command image/guest/init runs is present.
BASE_PACKAGES='openssh-server openssh-clients sudo shadow-utils git jq curl socat python3 procps-ng hostname iproute iputils findutils tar podman fuse-overlayfs slirp4netns passt containers-common'

# Keeping the agents means keeping what runs them. The agent tools are shell
# wrappers, and every one of them drives its agent inside a tmux session — 63
# references across the family. Without tmux a member comes up healthy, passes
# its readback, accepts a dispatch, and then every turn dies at
# `tmux: command not found` with exit 3 and nothing else to go on; the campaign
# re-dispatches on a timer and gets the same nothing until its ceiling.
# ncurses-term comes along for tmux's own terminfo entry, the way the shipped
# base layer installs the two together.
if [ "$keep_agents" = 1 ]; then
  BASE_PACKAGES="$BASE_PACKAGES tmux ncurses-term"
fi

out=$(awk -v base="$BASE_PACKAGES" -v keep_agents="$keep_agents" '
BEGIN {
  RS = ""; ORS = ""
  DROP["chromium"]                        # browser + its X/mesa font stack
  DROP["neovim/releases/download"]        # editor tarball
  DROP["tree-sitter-cli"]                 # required by the nvim config only
  DROP["pyenv.run"]                       # compiles CPython from source
  DROP["nvm-sh/nvm"]                      # node
  DROP["temurin25-binaries"]              # jdk
  DROP["archive.apache.org/dist/maven"]   # maven
  DROP["go.dev/dl/go${GO_VERSION}"]       # go toolchain
  DROP["cmd/cs-sandbox@"]                 # the codesweep tools: need the go above
  DROP["golangci-lint/releases/download"] # the dev toolchain for this repo
  DROP["cmd/deadcode@"]                   # deadcode, the one built from source
  DROP["python3 -m venv /opt/py-tools"]   # python CLI tools venv
  DROP["+Lazy! install"]                  # the nvim plugin/LSP pre-build
  DROP["COPY home/.config/nvim"]          # the config that pre-build reads, staged ahead of it
  want_dropped = 14
  # The agents, and the PATH stanza that is mostly about them. Kept together or
  # dropped together: keeping the binaries without putting them on PATH would
  # pass every check here and still fail `command -v claude` in the sandbox.
  if (keep_agents) {
    want_path = 1
  } else {
    DROP["downloads.claude.ai"]           # claude code
    DROP["openai/codex/releases"]         # codex
    DROP["anomalyco/opencode/releases"]   # opencode
    DROP["ENV JAVA_HOME"]                 # PATH additions for all of the above
    want_dropped = 18
    want_path = 0
  }
}
{
  for (m in DROP)
    if (index($0, m)) { dropped++; next }
  # The base install layer: replaced rather than dropped — see BASE_PACKAGES.
  if (index($0, "dnf update -y && dnf install -y")) {
    replaced++
    printf "# CI: reduced to what a sandbox must BOOT with. See image/ci-slim.sh.\n"
    printf "RUN dnf update -y && dnf install -y \\\n  %s && \\\n  dnf clean all\n\n", base
    next
  }
  # Keeping the agents: the shipped PATH names eight toolchain directories that
  # are no longer in the image. Harmless but misleading, so it is replaced by
  # the three that are — the agents have to be found by bare name.
  if (keep_agents && index($0, "ENV JAVA_HOME")) {
    pathed++
    printf "# CI: PATH reduced to the agent CLIs. See image/ci-slim.sh.\n"
    printf "ENV PATH=/opt/claude/bin:/opt/codex/bin:/opt/opencode/bin:${PATH}\n\n"
    next
  }
  printf "%s\n\n", $0
}
END {
  if (dropped < want_dropped) { print "ci-slim: only " dropped " of " want_dropped " stanzas dropped — markers have gone stale" > "/dev/stderr"; exit 1 }
  if (replaced != 1) { print "ci-slim: matched " replaced " base install layers, want exactly 1" > "/dev/stderr"; exit 1 }
  if (pathed != want_path) { print "ci-slim: matched " pathed " PATH stanzas, want " want_path > "/dev/stderr"; exit 1 }
}
' "$src")

printf '%s' "$out"
