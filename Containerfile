FROM registry.fedoraproject.org/fedora:44

# This image is GENERIC: it bakes in NO developer-specific user name / uid / gid and
# no per-user home. The toolchains live under /opt (shared, root-owned), and the
# container user (matching the host's name/uid/gid/home) is created at first boot by
# the entrypoint. So one image serves every developer and machine — no rebuild to
# match your local identity. See docs/design.md.

# Make dnf resilient to slow/flaky mirrors: drop any mirror that can't sustain ~100 KiB/s
# for 20s and fail over to the next, with parallel downloads + extra retries. First step so
# it governs every later dnf invocation.
RUN printf '%s\n' \
      'max_parallel_downloads=10' \
      'minrate=102400' \
      'timeout=20' \
      'retries=15' \
    >> /etc/dnf/dnf.conf

# Install Fedora packages
RUN dnf update -y && dnf install -y \
  vim findutils fd procps-ng tar zip unzip patch jq yq which lsof \
  iputils dnsutils iproute tcpdump wireshark-cli hostname netstat socat \
  sudo openssl openssh-clients openssh-server nmap telnet curl \
  git gcc gcc-c++ automake make cmake patch \
  zlib-devel bzip2 bzip2-devel \
  readline-devel sqlite sqlite-devel \
  openssl-devel tk-devel libffi-devel xz xz-devel \
  libuuid-devel gdbm-libs libnsl2 lzma lzma-sdk-devel \
  ncurses ncurses-devel ncurses-term tmux \
  g++ python-devel \
  graphviz graphviz-devel \
  podman skopeo buildah rsync shadow-utils \
  fuse-overlayfs crun slirp4netns passt containers-common \
  dnf-utils rclone bash-completion \
  ripgrep fzf bat git-delta tree htop less wget nmap-ncat gh uv \
  ninja-build gettext glibc-gconv-extra \
  java-latest-openjdk java-latest-openjdk-devel maven \
  golang-bin \
  pandoc && \
  dnf clean all

# Install Chromium
RUN dnf -y install \
    chromium \
    nss atk cups-libs pango alsa-lib \
    libXcomposite libXcursor libXdamage libXext libXi libXrandr libXScrnSaver libXtst \
    xorg-x11-fonts-Type1 xorg-x11-fonts-misc liberation-fonts \
    mesa-libEGL mesa-libGL && \
    dnf clean all

ENV CHROME_BIN=/usr/bin/chromium-browser

# NOTE: `COPY . /sandbox` is intentionally deferred to just before it's first needed
# (the Neovim plugin pre-build / entrypoint), AFTER the expensive toolchain compiles, so
# editing the entrypoint / dotfiles / vendored scripts doesn't invalidate those layers.

# Disable PAM for sudo.
#
# SAFETY ASSUMPTION: this unconditionally-succeeding sudo (passwordless NOPASSWD:ALL) is
# safe ONLY because the security boundary is the engine, not in-sandbox sudo — rootless
# `--userns=keep-id` (container "root" == your unprivileged host uid) or a microVM's own
# kernel. The agent already owns its disposable sandbox, so sudo grants nothing outside it.
# Running this image rootful, `--privileged`, or `--userns=host` would break that boundary
# and turn this into real host-root. See docs/design.md "Security model".
#
# The container user is created at first boot (see context/entrypoint) with a LOCKED
# password (useradd -p '*') and is granted NOPASSWD:ALL via /etc/sudoers.d. There is no
# password to authenticate against, and the image is a minimal rootless container with no
# running systemd/logind — so stock PAM modules for sudo would either reject the locked
# account (pam_unix auth/account) or fail/hang trying to open a login session
# (pam_systemd registering with a logind that isn't there). Replacing every PAM stack with
# pam_permit.so makes the sudo PAM transaction unconditionally succeed, so passwordless
# sudo is reliable and non-interactive. That matters because the rootful nested-podman
# wrapper shells out to `sudo` on every podman call (see below); a PAM stall there would
# wedge nested podman. sshd is likewise run with UsePAM=no for the same reason.
RUN echo -e "auth       sufficient   pam_permit.so\n\
account    sufficient   pam_permit.so\n\
password   sufficient   pam_permit.so\n\
session    sufficient   pam_permit.so" > /etc/pam.d/sudo

# Register a private podman registry so nested podman can pull from a local/private registry.
# Build-args (cs-sandbox forwards the matching CS_SANDBOX_PRIVATE_REGISTRY[_INSECURE] env vars;
# see docs/design.md "Private registry"):
#   CS_SANDBOX_PRIVATE_REGISTRY=host:port    registry to trust, as a BARE host:port (podman's
#                                            registries.conf `location` form — no scheme); empty
#                                            -> register none.
#   CS_SANDBOX_PRIVATE_REGISTRY_INSECURE=1   insecure: `insecure = true` lets podman use plain-HTTP
#                                            and skip TLS verification; 0 (default) -> secure (HTTPS,
#                                            TLS-verified).
# Per docker/podman convention the protocol is IMPLICIT in this flag — a registry is named by its
# bare host:port, secure means HTTPS, insecure permits HTTP / untrusted certs; there is no
# http://-vs-https:// scheme. Secure is the default; insecure is opt-in (e.g. a plain-HTTP registry:
# --build-arg CS_SANDBOX_PRIVATE_REGISTRY=registry.internal:5000 --build-arg ..._INSECURE=1).
ARG CS_SANDBOX_PRIVATE_REGISTRY=
ARG CS_SANDBOX_PRIVATE_REGISTRY_INSECURE=0
RUN if [ -n "$CS_SANDBOX_PRIVATE_REGISTRY" ]; then \
      { printf '[[registry]]\nlocation = "%s"\n' "$CS_SANDBOX_PRIVATE_REGISTRY"; \
        case "$CS_SANDBOX_PRIVATE_REGISTRY_INSECURE" in \
          1|true|TRUE|yes|YES|on|ON) printf 'insecure = true\n' ;; \
        esac; \
      } >> /etc/containers/registries.conf; \
    fi

# Configure nested podman. The container runs with a scaled-down cap set by default
# (SYS_ADMIN/NET_ADMIN/MKNOD/SYS_PTRACE + /dev/net/tun + unmask=/proc/sys, seccomp on;
# --privileged is an opt-in fallback) — and it's rootless, so caps are bounded by the
# unprivileged host user, while nested podman runs ROOTFUL as container-root (which
# under keep-id is your unprivileged host user). Default to the
# kernel's NATIVE overlay driver (fast path): the per-instance cs-sandbox-containers volume
# mounted at /var/lib/containers gives the store a non-overlay backing fs, so on
# kernel >= 5.11 native overlay mounts directly — no fuse-overlayfs userspace layer
# and its perf cost. The entrypoint auto-falls-back to fuse-overlayfs (still
# installed) if a native overlay mount isn't possible here.
RUN mkdir -p /etc/containers && \
    printf '[storage]\ndriver = "overlay"\n' > /etc/containers/storage.conf && \
    printf '[containers]\ncgroups = "disabled"\n\n[engine]\ncgroup_manager = "cgroupfs"\nevents_logger = "file"\n' > /etc/containers/containers.conf

# (Nested podman runs rootful via a /usr/local/bin/podman -> sudo wrapper, installed below.)

# Default internal sshd port (overridable at runtime via -e CS_SANDBOX_SSH_PORT)
ENV CS_SANDBOX_SSH_PORT=2222

# Configure sshd
RUN mkdir -p /var/run/sshd \
  && sed -i 's/^#PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config \
  && sed -i 's/^#PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config \
  && sed -i 's/^#PubkeyAuthentication.*/PubkeyAuthentication yes/' /etc/ssh/sshd_config \
  && sed -i 's/^#\?Port .*/Port 2222/' /etc/ssh/sshd_config \
  && ssh-keygen -A

# Build Neovim v0.11.4
RUN mkdir -p /usr/src \
  && cd /usr/src \
  && git clone --branch v0.11.4 --depth=1 https://github.com/neovim/neovim.git neovim-v0.11.4 \
  && cd neovim-v0.11.4 \
  && make CMAKE_BUILD_TYPE=Release \
  && make install

# --- Toolchains under /opt (shared, root-owned, path-independent) ---------------
# No developer user is created at build time (the entrypoint does that on first boot),
# so the heavy toolchains can't live in a per-user $HOME. They go under /opt instead,
# installed as root and shared by every runtime user. Read-only at runtime: to add
# more Python/Node versions or global npm/pip packages, use `sudo`. Per-project work
# (virtualenvs, node_modules) is unaffected — it lives in the user's repos.

# Pyenv + Python 3.12.10 (PYENV_ROOT=/opt/pyenv)
ENV PYENV_ROOT=/opt/pyenv
RUN curl -fsSL https://pyenv.run | PYENV_GIT_TAG=v2.6.5 bash \
  && PATH="$PYENV_ROOT/bin:$PATH" pyenv install -s 3.12.10 \
  && PATH="$PYENV_ROOT/bin:$PATH" pyenv global 3.12.10

# NVM + Node 24.4.1 (NVM_DIR=/opt/nvm): a general JS/TS runtime; npm -g lands in shared node.
# (The Claude Code & Codex agents are self-contained binaries under /opt, not npm packages.)
ENV NVM_DIR=/opt/nvm
RUN mkdir -p "$NVM_DIR" \
  && curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh | bash \
  && bash -c '. "$NVM_DIR/nvm.sh" && nvm install v24.4.1 && nvm alias default v24.4.1'

# Claude Code — native single-binary install (no Node/npm runtime dependency). Anthropic's
# install.sh normally drops a self-updating launcher in ~/.local/bin; instead we fetch the same
# verified binary straight into root-owned /opt. /opt is read-only at runtime, so the version is
# pinned and reproducible (no background auto-update fighting the read-only tree). The binary IS
# the full CLI — the launcher is only convenience + auto-update, neither of which we want here.
# Bump CLAUDE_CODE_VERSION (or pass --build-arg) to upgrade; checksum is verified from the manifest.
ARG CLAUDE_CODE_VERSION=2.1.178
RUN base=https://downloads.claude.ai/claude-code-releases \
  && case "$(uname -m)" in x86_64|amd64) arch=x64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; esac \
  && plat="linux-${arch}" \
  && mkdir -p /opt/claude/bin \
  && curl -fsSL -o /opt/claude/bin/claude "$base/$CLAUDE_CODE_VERSION/$plat/claude" \
  && want=$(curl -fsSL "$base/$CLAUDE_CODE_VERSION/manifest.json" | jq -r ".platforms[\"$plat\"].checksum") \
  && got=$(sha256sum /opt/claude/bin/claude | cut -d' ' -f1) \
  && { [ "$want" = "$got" ] || { echo "claude checksum mismatch: want=$want got=$got" >&2; exit 1; }; } \
  && chmod +x /opt/claude/bin/claude \
  && /opt/claude/bin/claude --version

# Claude Code's install-health check expects a "native"/npm install at ~/.local/bin or in
# node_modules; ours is a pinned single binary in read-only /opt (externally managed), which
# matches none of those, so it nags ("claude command at ~/.local/bin/claude missing or broken
# · run claude install to repair") and `claude install` would shadow-install a second copy.
# Disable the install checks and the auto-updater (it can't write /opt anyway). This covers
# the container engine for any direct `claude`; the cs-claude wrapper exports the same
# for the microVM engine (whose guest init doesn't inherit image ENV) and for either engine.
ENV DISABLE_INSTALLATION_CHECKS=1 \
    DISABLE_AUTOUPDATER=1

# OpenAI Codex — native static (musl) binary from the GitHub release, into /opt (same rationale:
# pinned, read-only-friendly, no Node). The musl build is fully self-contained so it runs on the
# Fedora glibc base. Bump CODEX_VERSION (or pass --build-arg) to upgrade.
ARG CODEX_VERSION=0.140.0
RUN case "$(uname -m)" in x86_64|amd64) tgt=x86_64-unknown-linux-musl ;; arm64|aarch64) tgt=aarch64-unknown-linux-musl ;; *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; esac \
  && mkdir -p /opt/codex/bin /tmp/codex-dl \
  && curl -fsSL -o /tmp/codex-dl/codex.tar.gz "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/codex-${tgt}.tar.gz" \
  && tar -xzf /tmp/codex-dl/codex.tar.gz -C /tmp/codex-dl \
  && mv "/tmp/codex-dl/codex-${tgt}" /opt/codex/bin/codex \
  && chmod +x /opt/codex/bin/codex \
  && rm -rf /tmp/codex-dl \
  && /opt/codex/bin/codex --version

# Python CLI tools in an isolated, root-owned venv
RUN python3 -m venv /opt/py-tools \
  && /opt/py-tools/bin/pip install --no-cache-dir claude-code-transcripts

# Everything under /opt readable/traversable by every runtime user
RUN chmod -R a+rX /opt

# Toolchains on PATH for ALL shells (incl. non-interactive `ssh cs-sandbox-x <cmd>`), so node,
# the agents (claude, codex), and the py-tools are found without sourcing ~/.bashrc. (~/.bashrc
# additionally runs pyenv/nvm init for interactive version switching.)
ENV PATH=/opt/nvm/versions/node/v24.4.1/bin:/opt/py-tools/bin:/opt/claude/bin:/opt/codex/bin:/opt/pyenv/bin:${PATH}

# Make `podman` run ROOTFUL by default — the nested engine must (rootless-inner fails
# under keep-id). A thin wrapper ahead of /usr/bin in PATH routes podman through the
# NOPASSWD sudo the user already has, so plain `podman` (shells, scripts, tools, and
# `ssh cs-sandbox-x podman …`) drives the rootful nested engine. No setuid (podman keys
# rootless/rootful off the real uid, so a setuid bit just yields a broken half-state);
# this grants nothing beyond the existing sudo. The real rootless binary stays at
# /usr/bin/podman for anyone who wants rootless explicitly.
RUN printf '#!/bin/sh\nexec sudo /usr/bin/podman "$@"\n' > /usr/local/bin/podman && \
    chmod 0755 /usr/local/bin/podman

# Repo context (entrypoint, home skeleton, vendored scripts). Deferred to here — after
# the toolchain compiles — so edits to these files don't bust the expensive cache above.
COPY . /sandbox

# Pre-build the Neovim plugin set into the home SEED (/sandbox/home). The entrypoint
# copies that seed into the user's home on first boot, so nvim is ready immediately
# (config comes from the repo via the COPY above; plugins land alongside it here).
RUN HOME=/sandbox/home XDG_CONFIG_HOME=/sandbox/home/.config \
    XDG_DATA_HOME=/sandbox/home/.local/share XDG_STATE_HOME=/sandbox/home/.local/state \
    nvim --headless -c 'autocmd User LazySync quitall' -c 'Lazy! sync'

# Image metadata. Placed here — after the heavy toolchain/COPY layers — so editing a
# label only rebuilds this cheap layer, not the build above. The :44 tag is a rolling
# pointer to the latest build; these labels make the running image self-describing
# (which Fedora base, what this image is) without baking a version into the tag.
# The version/vendor/licenses/url here OVERRIDE values inherited from the Fedora base
# (which would otherwise read 44 / "Fedora Project" / MIT / fedoraproject.org and
# misdescribe this image). version mirrors the rolling :44 tag — there is no separate
# image version scheme.
LABEL org.opencontainers.image.title="cs-sandbox" \
      org.opencontainers.image.description="Generic sandbox image (Fedora 44 base, shared /opt toolchains; per-developer user created at first boot)." \
      org.opencontainers.image.base.name="registry.fedoraproject.org/fedora:44" \
      org.opencontainers.image.version="44" \
      org.opencontainers.image.vendor="CodeSweep" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.url="https://github.com/codesweep-ai/sandbox" \
      org.opencontainers.image.source="https://github.com/codesweep-ai/sandbox" \
      ai.codesweep.sandbox.os-version="44"

# Create the per-developer user + finish home setup on first boot, then drop to them.
ENTRYPOINT ["/sandbox/entrypoint"]

# Shell
CMD ["/bin/bash"]
