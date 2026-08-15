# .bashrc

# Source global definitions
if [ -f /etc/bashrc ]; then
    . /etc/bashrc
fi

# User specific environment
if ! [[ "$PATH" =~ "$HOME/.local/bin:$HOME/bin:" ]]; then
    PATH="$HOME/.local/bin:$HOME/bin:$PATH"
fi
export PATH

# The guest environment declared in /etc/cs-sandbox/env. sshd resets the
# environment for every session, so a Containerfile ENV never reaches this shell;
# replaying the declaration here is what does. Adding a variable there needs no
# line here. Anything already set wins, so --env injection (applied by sshd from
# ~/.ssh/environment before this runs) and a user's own export are not clobbered.
if [[ -r /etc/cs-sandbox/env ]]; then
    while IFS= read -r _l; do
        [[ "$_l" == \#* || -z "${_l// }" ]] && continue
        [[ "$_l" == *=* ]] || continue
        _k="${_l%%=*}"
        [[ -z "${!_k+x}" ]] && export "$_k=${_l#*=}"
    done < /etc/cs-sandbox/env
    unset _l _k
fi

# Uncomment the following line if you don't like systemctl's auto-paging feature:
# export SYSTEMD_PAGER=

# User specific aliases and functions
if [ -d ~/.bashrc.d ]; then
    for rc in ~/.bashrc.d/*; do
        if [ -f "$rc" ]; then
            . "$rc"
        fi
    done
fi
unset rc

# Sandbox customizations

# Umask
umask 0022

# Command line editor
export EDITOR=vi
set -o vi

# Shared shell history
shopt -s histappend
PROMPT_COMMAND="history -a; history -n${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
HISTSIZE=10000
HISTFILESIZE=20000

# Prompt
export PROMPT_HIGHLIGHT=1
export TERM=screen-256color
export PS1="\[\e[1;32m\]\u@\h:\[\e[1;31m\]sandbox\[\e[1;32m\]:\w\\$\[\e[0m\] "

# Bash completion
if [ -f /etc/bash_completion ]; then
    . /etc/bash_completion
fi

# Neovim
alias vi=nvim
alias vimdiff='nvim -d'
export EDITOR=nvim

# ~/.local/bin (e.g. mdtohtml, mdview) is already on PATH via the block near the top of this file.

# Nested podman (sandbox): how it runs depends on the engine under you, and the
# /usr/local/bin/podman wrapper picks — just use `podman`.
#   podman container: ROOTFUL (wrapper routes through sudo). Podman-in-podman must be,
#     under --userns=keep-id. Still a rootless container, so caps are bounded by your
#     unprivileged host user, not host root. Images: /var/lib/containers (own volume).
#   firecracker microVM: ROOTLESS, i.e. podman as on any normal machine — a VM is real
#     root on a real kernel, so none of the above applies. Images: ~/.local/share/containers.
# For inner containers that run as YOU (uid:gid + names, files owned by you not subuid
# 524288) use:
#   user-podman run ...  — podman + auto --user/--passwd-entry/--group-entry
#     (a no-op passthrough in a microVM, where plain podman already gives you that)
# The real binary is always /usr/bin/podman.

# Pyenv (shared, under /opt — see SPEC.md §3.1; `sudo` to add Python versions)
export PYENV_ROOT="${PYENV_ROOT:-/opt/pyenv}"
export PYENV_VIRTUALENV_DISABLE_PROMPT=1
[[ -d $PYENV_ROOT/bin ]] && export PATH="$PYENV_ROOT/bin:$PATH"
if command -v pyenv >/dev/null 2>&1; then
  # --no-rehash: the shared /opt/pyenv/shims are pre-built and read-only, so skip the
  # startup rehash (which would otherwise warn "shims isn't writable" on every shell).
  eval "$(pyenv init --no-rehash - bash)"
  eval "$(pyenv virtualenv-init -)" 2>/dev/null || true
fi

# Node (nvm shared, under /opt; `sudo` to add Node versions / global npm packages)
export NVM_DIR="${NVM_DIR:-/opt/nvm}"
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
[ -s "$NVM_DIR/bash_completion" ] && \. "$NVM_DIR/bash_completion"

# Go (shared toolchain under /opt — see SPEC.md §3.1; `sudo` to change the toolchain
# itself). GOTOOLCHAIN is left at upstream's "auto", so a repo whose go.mod names a
# different `go 1.x` downloads that toolchain into GOPATH on demand — no sudo needed,
# which is Go's equivalent of switching versions with nvm/pyenv.
export GOROOT="${GOROOT:-/opt/go}"
export GOPATH="${GOPATH:-$HOME/go}"
export GOBIN="${GOBIN:-$GOPATH/bin}"
[[ -d $GOROOT/bin ]] && [[ ":$PATH:" != *":$GOROOT/bin:"* ]] && export PATH="$GOROOT/bin:$PATH"
# so `go install <pkg>@latest` yields a command you can actually run
[[ ":$PATH:" != *":$GOBIN:"* ]] && export PATH="$GOBIN:$PATH"

# Java + Maven (pinned Temurin JDK under /opt, like the other toolchains). JAVA_HOME is
# set outright rather than derived from `readlink -f $(command -v java)`: that used to
# resolve to whichever JDK the distro's alternatives happened to point at.
export JAVA_HOME="${JAVA_HOME:-/opt/java}"
for d in "$JAVA_HOME/bin" /opt/maven/bin; do
  [[ -d $d ]] && [[ ":$PATH:" != *":$d:"* ]] && export PATH="$d:$PATH"
done
unset d

# Native AI coding agents (shared, under /opt — pinned single binaries, no npm/Node).
# Added here too because sshd resets PATH and bash sources ~/.bashrc for `ssh <instance> <cmd>`,
# so the image's ENV PATH (which `podman exec` inherits) wouldn't otherwise cover SSH shells.
for d in /opt/claude/bin /opt/codex/bin /opt/opencode/bin; do
  [[ -d $d ]] && [[ ":$PATH:" != *":$d:"* ]] && export PATH="$d:$PATH"
done
unset d

