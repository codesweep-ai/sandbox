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

# ~/bin (e.g. mdtohtml, mdview) is already on PATH via the block near the top of this file.

# Nested podman (sandbox): a rootless container, so caps are bounded by your unprivileged
# host user, not host root. Podman-in-podman must
# run ROOTFUL, so plain `podman` is rootful by default (a /usr/local/bin/podman
# wrapper routes it through sudo) — just use `podman`. For inner containers that run
# as YOU (uid:gid + names, files owned by you not subuid 524288) use:
#   user-podman run ...  — podman + auto --user/--passwd-entry/--group-entry
# The real rootless binary is /usr/bin/podman; rootful images live in
# /var/lib/containers (a dedicated volume).

# Pyenv (shared, under /opt — see docs/design.md; `sudo` to add Python versions)
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

# Java
if command -v java >/dev/null 2>&1; then
  JAVA_BIN=$(readlink -f "$(command -v java)")
  JAVA_HOME=$(dirname "$(dirname "$JAVA_BIN")")
  export JAVA_HOME
  export PATH="$JAVA_HOME/bin:$PATH"
fi

# Native AI coding agents (shared, under /opt — pinned single binaries, no npm/Node).
# Added here too because sshd resets PATH and bash sources ~/.bashrc for `ssh <instance> <cmd>`,
# so the image's ENV PATH (which `podman exec` inherits) wouldn't otherwise cover SSH shells.
for d in /opt/claude/bin /opt/codex/bin; do
  [[ -d $d ]] && [[ ":$PATH:" != *":$d:"* ]] && export PATH="$d:$PATH"
done
unset d

