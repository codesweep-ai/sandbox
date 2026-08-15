# sandbox - installation

`cs-sandbox` is a single, self-contained Go binary. Download a release or build it from a clone, put
it on your PATH, then set the host up once with the steps below. After that, head for the
[README](README.md) walkthroughs.

> **`cs-sandbox doctor` checks every prerequisite below and prints the exact fix for anything
> missing.** Run it once it's on your PATH; it's the fastest path through this page.

## 1. Install the binary

`cs-sandbox` is a **single self-contained binary** — the image build assets are embedded, so it can
build the sandbox image and boot microVMs with no source checkout. Two ways to get it:

### Download a release

From the releases page grab the archive for your OS/arch
(`cs-sandbox_<version>_<os>_<arch>.tar.gz`) and `checksums.txt`, verify, then install:

```bash
sha256sum -c --ignore-missing checksums.txt          # releases are checksummed + cosign-signed
tar xzf cs-sandbox_*.tar.gz cs-sandbox               # unpack the binary from your archive
install -m755 cs-sandbox ~/.local/bin/cs-sandbox     # anywhere on your PATH
```

To verify the cosign signature as well:

```bash
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/codesweep-ai/sandbox/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

### Or build from source

Needs **Go 1.26+**, **git**, and **goreleaser** (which produces the version-stamped static binary):

```bash
# --- Go (1.26 or newer) ---
brew install go                     # macOS
sudo dnf install golang             # Fedora
# Debian/Ubuntu package Go well behind 1.26 — install the current release instead
# (or pick your arch's tarball by hand from https://go.dev/dl/):
ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
curl -LO "https://go.dev/dl/${ver}.linux-amd64.tar.gz"
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf "${ver}.linux-amd64.tar.gz"
export PATH="/usr/local/go/bin:$PATH"          # add to ~/.bashrc to persist

# --- goreleaser (any OS; uses the Go above) ---
go install github.com/goreleaser/goreleaser/v2@latest
export PATH="$(go env GOPATH)/bin:$PATH"       # add to ~/.bashrc to persist

go version && goreleaser --version             # sanity-check both are on PATH
```

Then build and install:

```bash
git clone https://github.com/codesweep-ai/sandbox.git && cd sandbox
make build            # -> bin/cs-sandbox  (static CGO_ENABLED=0 binary, version-stamped)
make install          # installs bin/cs-sandbox into ~/.local/bin, plus CS_SANDBOX.md
                      # beside it (PREFIX=… to override)
```

## 2. Check the host

### Base packages

You need three, on every host. **Podman** builds the image and provides the rootless network fabric that both
engines use. The **OpenSSH client** reaches every sandbox by name, and **git** shares your repos in.
The last two ship by default on macOS and most Linux desktops.

```bash
sudo dnf install podman openssh-clients git                          # Fedora
sudo apt install podman openssh-client git                           # Ubuntu / Debian
brew install podman                                                  # macOS (ssh + git built in)
podman machine init --cpus 4 --memory 8192 --disk-size 60 --now      # macOS (--now also starts it)

# Windows (WSL2) — run this INSIDE the distro. The extras are podman's rootless
# network stack; apt pulls in none of them, and a missing iptables shows up only
# as an unnamed netavark error once a container starts, never at install time.
sudo apt install podman openssh-client git \
  catatonit uidmap netavark aardvark-dns iptables passt slirp4netns fuse-overlayfs
```

On macOS everything runs inside that one podman-machine VM, so every sandbox and every nested
container shares its CPUs, memory and disk. The default machine gets 2 GiB, which one sandbox
running a toolchain plus nested containers will exhaust. Size it at init as above, and give it more
if you want several sandboxes at once. To resize it later, stop it first:

```bash
podman machine stop && podman machine set --cpus 4 --memory 8192 --disk-size 60   # disk can only grow
```

The machine shares your home directory into the VM, and only that — so a `--repo` or `--snapshot`
source has to live under `$HOME`. `cs-sandbox create` rejects one that doesn't, rather than handing
the sandbox a path the VM can't see (macOS temp dirs like `/var/folders/…` are outside the share).

### Windows (WSL2)

Windows is supported **through WSL2**: `cs-sandbox` is a Linux binary that runs inside the distro,
so install the `linux_amd64` release there — not on Windows. There is no Windows build, and nothing
runs on the Windows side. CI validates this on Ubuntu 24.04; another distro needs the equivalent
packages.

Work as a normal user. Rootless podman is the supported configuration, and running as root is
neither tested nor supported. Enable systemd in `/etc/wsl.conf`, then run `wsl --shutdown` from
Windows to restart the distro:

```ini
[boot]
systemd=true
```

Without it nothing creates `/run/user/$(id -u)`, so podman warns and falls back to `/tmp`, and
`host-route` has no systemd-resolved to work with.

Two notes in the next section apply here even though Firecracker does not: the subuid ranges and the
AppArmor userns sysctl are rootless-podman requirements rather than Firecracker ones. `cs-sandbox
doctor` detects WSL and checks the rest.

Keep `--repo` sources on the distro's own filesystem. `/mnt/c` is DrvFs, root-owned and without real
Unix modes, so a sandbox would get a tree whose permissions do not mean what they say. Podman is the
engine here, and Firecracker is untested on WSL2.

### Firecracker packages (Linux + KVM, x86_64)

`cs-sandbox build` (step 3) downloads the Firecracker binary and builds the guest kernel and base
rootfs. The binary is pinned to a release, `v1.16.0` by default and overridable with
`CS_SANDBOX_FC_VERSION`, and is verified against a SHA256 committed in the source. You provide a few
host packages and `/dev/kvm` access first.

```bash
# Fedora  (the base packages above are required too)
sudo dnf install passt dnsmasq fakeroot e2fsprogs socat python3 shadow-utils iproute curl
# Ubuntu / Debian  (uidmap not shadow-utils, dnsmasq-base not dnsmasq, iproute2 not iproute)
sudo apt install passt dnsmasq-base fakeroot e2fsprogs socat python3 uidmap iproute2 curl

sudo usermod -aG kvm "$USER"            # /dev/kvm access (log out / back in afterward)
grep "^$USER:" /etc/subuid /etc/subgid  # must return a line in each file (rootless userns)
# if either is empty:  sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 "$USER"

# Ubuntu 24.04+ only: if Podman's rootless network namespace fails to start, allow unprivileged userns:
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
```

A Firecracker sandbox boots its own guest kernel, built from the sandbox image, so it is pinned
and reproducible with no dependency on the host's `/boot`. The reasoning is in
[SPEC.md](SPEC.md#122-the-guest-kernel). On macOS, and on any host without x86_64 KVM, this step is
skipped and sandboxes use Podman.

## 3. Build the sandbox artifacts

Every sandbox runs from one generic image (no user identity baked in - your user is created at first
boot). `cs-sandbox build` sets up the reusable, host-wide artifacts once so later `create`s are fast:

```bash
cs-sandbox build                       # image + (on x86_64 Linux/KVM) the Firecracker artifacts
```

With no flags it prepares **every engine the host supports**. That means the podman image always,
plus the Firecracker binary, kernel and base rootfs on a Firecracker-capable host. It **fails** when
the Firecracker packages above are missing. Restrict it when you want to:

```bash
cs-sandbox build --engine podman       # image only (skip Firecracker)
cs-sandbox build --engine firecracker  # force the Firecracker set (implies the image)
```

`cs-sandbox create` assumes `build` has run: it **does not** build under the covers, and fails with a
"run: cs-sandbox build" message if the image or Firecracker artifacts are missing.

The image bundles a broad toolchain: podman, skopeo and buildah, tmux, chromium, java and maven, Go,
pyenv with Python, nvm with Node, and Neovim. It adds the usual CLI helpers, and the Claude Code,
Codex and OpenCode agents with their launch wrappers and remote tools. See
[what's in a sandbox](README.md#whats-in-a-sandbox) for the tour, and
[`image/Containerfile`](image/Containerfile) for the full package list.

## 4. Host agent tools and login (recommended)

**A sandbox does not get your agent login by default.** Logging in once here, on the host, is what
lets a sandbox inherit it at create time with `--inherit-agent-login claude` (or `codex`/`opencode`).

Every sandbox already carries the agent tools at `~/.local/bin`, so this step is about your
**host**. It puts the same tools on your PATH, so you can log in with them and drive agents from
here.

```bash
cs-sandbox install-agent-tools    # -> ~/.local/bin  (pass a directory to install elsewhere)
```

`cs-claude`, `cs-codex` and `cs-opencode` invoke the `claude`, `codex` and `opencode` CLIs, so those
must be installed on the host too. `install-agent-tools` tells you which are missing.

Then log in once with each one you use. The credential a sandbox inherits is copied into its
per-sandbox seed at create time and installed on first boot, and is **never baked into the image**:

```bash
cs-claude          # launch Claude Code - log in with /login, then exit
cs-codex           # launch Codex - choose "Sign in with ChatGPT", then exit
cs-opencode providers login   # OpenCode - pick a provider (it is usually driven by an API key)
```

You can skip all of this and log in inside each sandbox instead, with `cs-sandbox agent-login claude
<name>`. That is also how you give a sandbox its **own account** rather than sharing yours. See
[SPEC.md](SPEC.md#101-login).

**Using an API key or a cloud provider** (a direct Anthropic/OpenAI key, Amazon Bedrock, Google
Vertex, …) instead of a subscription? Keys are never copied from your host — pass the ones a sandbox
needs explicitly at create time:

```bash
cs-sandbox create dev --env ANTHROPIC_API_KEY          # passes the value through from your shell
```

See [SPEC.md](SPEC.md#101-login) for what is and is not carried.

## 5. Verify the installation

```bash
cs-sandbox doctor                       # re-run: everything should be green
cs-sandbox create smoke --repo .        # create a throwaway sandbox with this repo
ssh smoke                               # shell in by name
cs-sandbox destroy smoke -f             # tear it down
```

You're set - head to the [README](README.md) walkthroughs.

## Optional: shell completion

`cs-sandbox` generates completion scripts for bash / zsh / fish /
PowerShell, with **live** completion of sandbox names, store names, and flag values (`ssh <TAB>`,
`destroy <TAB>`, `--engine <TAB>`, …):

```bash
# bash (per-user, no sudo — needs the bash-completion package, which lazy-loads this on demand):
mkdir -p ~/.local/share/bash-completion/completions
cs-sandbox completion bash > ~/.local/share/bash-completion/completions/cs-sandbox
cs-sandbox completion zsh  > "${fpath[1]}/_cs-sandbox"                     # zsh
cs-sandbox completion fish > ~/.config/fish/completions/cs-sandbox.fish   # fish
# or, ad hoc for the current shell (any of them):  source <(cs-sandbox completion bash)
```

To install the bash completion system-wide instead of per-user, pipe it into
`/etc/bash_completion.d/cs-sandbox` with `sudo tee`. Run `cs-sandbox completion --help` for
per-shell setup details.

## State and cache locations

**State lives in your home, not next to the binary**, so you can move or replace `cs-sandbox`
without losing your sandboxes. [MANUAL.md](MANUAL.md#files) lists every path it reads and writes,
and [its environment section](MANUAL.md#environment) the variables that relocate them.

Pointing `CS_SANDBOX_HOME` at another directory gives you a second set of sandboxes that runs
alongside the first, because the network underneath stays shared.
[SPEC.md](SPEC.md#127-the-fabric-is-host-global-an-instances-root-is-not) covers what keeps two
sets from colliding.
