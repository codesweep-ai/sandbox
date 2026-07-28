# sandbox - installation

`cs-sandbox` is a single, self-contained Go binary. Get it (download a release or build from a clone),
put it on your PATH, then set up the host once with the steps below - then see the
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

### Or build from source

Needs **Go 1.25+**, **git**, and **goreleaser** (which produces the version-stamped static binary):

```bash
# --- Go (1.25 or newer) ---
brew install go                     # macOS
sudo dnf install golang             # Fedora
# Debian/Ubuntu package Go well behind 1.25 — install the current release instead
# (pick your arch's tarball from https://go.dev/dl/):
curl -LO https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
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

Podman builds the image and provides the shared network fabric - both engines need it. The OpenSSH
client (`ssh` / `ssh-keygen`) reaches every sandbox by name, and `git` shares your repos into
sandboxes (`--repo`) - both required too (both ship by default on macOS and most Linux desktops).

```bash
sudo dnf install podman openssh-clients git                          # Fedora
sudo apt install podman openssh-client git                           # Ubuntu / Debian
brew install podman                                                  # macOS (ssh + git built in)
podman machine init --cpus 4 --memory 8192 --disk-size 60 --now      # macOS (--now also starts it)
```

On macOS everything runs inside that one podman-machine VM — every sandbox, and anything they run
in nested containers, shares its CPUs, memory and disk. Podman's default machine gets 2 GiB of RAM,
which a single sandbox running a toolchain plus nested containers will exhaust, so size it at init
as above; give it more if you plan to run several sandboxes at once. To resize later:

```bash
podman machine stop && podman machine set --cpus 4 --memory 8192 --disk-size 60   # disk can only grow
```

The machine shares your home directory into the VM, and only that — so a `--repo` or `--snapshot`
source has to live under `$HOME`. `cs-sandbox create` rejects one that doesn't, rather than handing
the sandbox a path the VM can't see (macOS temp dirs like `/var/folders/…` are outside the share).

### Firecracker packages (Linux + KVM, x86_64)

`cs-sandbox build` (step 3) downloads the SHA256-verified Firecracker binary and builds the
guest kernel + base rootfs; you provide a few host packages and `/dev/kvm` access first.

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

A Firecracker sandbox boots its own guest kernel, built from the sandbox image - pinned and
reproducible, with no dependency on the host's `/boot` kernel. Full detail:
[`docs/firecracker.md`](docs/firecracker.md#prerequisites). On macOS / non-KVM hosts this step is
skipped and sandboxes default to Podman.

## 3. Build the sandbox artifacts

Every sandbox runs from one generic image (no user identity baked in - your user is created at first
boot). `cs-sandbox build` sets up the reusable, host-wide artifacts once so later `create`s are fast:

```bash
cs-sandbox build                       # image + (on Linux/KVM) the Firecracker artifacts
```

With no flags it prepares **every engine the host supports**: the podman image always, plus the
Firecracker binary/kernel/base-rootfs on a Firecracker-capable host (**it fails** if the Firecracker
packages above are missing). Restrict it when you want to:

```bash
cs-sandbox build --engine podman       # image only (skip Firecracker)
cs-sandbox build --engine firecracker  # force the Firecracker set (implies the image)
```

`cs-sandbox create` assumes `build` has run: it **does not** build under the covers, and fails with a
"run: cs-sandbox build" message if the image or Firecracker artifacts are missing.

It bundles a broad toolchain (podman/skopeo/buildah, tmux, chromium, java/maven, go), CLI helpers
(ripgrep, fd, fzf, bat, git-delta, jq/yq, gh, uv), Neovim, pyenv/Python + nvm/Node, and the Claude
Code & Codex agents. See the [`image/Containerfile`](image/Containerfile) for the full list.

## 4. Verify the installation

```bash
cs-sandbox doctor                       # re-run: everything should be green
cs-sandbox create smoke --repo .        # create a throwaway sandbox with this repo
ssh smoke                               # shell in by name
cs-sandbox destroy smoke -f             # tear it down
```

You're set - head to the [README](README.md) walkthroughs.

## Optional: host agent tools and sign-in

Sign in on the host once and any sandbox can inherit that login on demand, with
`cs-sandbox create <name> --inherit-agent-login claude` — it is never carried unless you ask. Skip
this section entirely and sign in inside each sandbox instead (`cs-sandbox agent-login claude <name>`).

Sandboxes already carry the agent toolset (`cs-claude` / `cs-codex` and the remote-delegation
families) at `~/.local/bin` — nothing to install inside them. This step puts the same tools on your
**host** PATH (the same `~/.local/bin`) so you can sign in (next) and delegate from the host:

```bash
cs-sandbox install-agent-tools    # -> ~/.local/bin  (pass a directory to install elsewhere)
```

`cs-claude` / `cs-codex` invoke the `claude` / `codex` CLIs, so those must be installed on the host
too; `install-agent-tools` tells you if either is missing.

Sign in to **Claude Code & Codex** once on the host. A sandbox created with
`--inherit-agent-login claude` (or `codex`, or both) then starts already signed in — the credential
is snapshotted into that sandbox on first boot, **never baked into the image**.

```bash
cs-claude          # launch Claude Code - sign in with /login, then exit
cs-codex           # launch Codex - choose "Sign in with ChatGPT", then exit
```

On macOS (where Claude's credentials live in the Keychain with no file to copy), or to give a
sandbox its own independent session, sign in *inside* a sandbox instead:

```bash
cs-sandbox agent-login claude <name>     # or: agent-login codex <name>
```

**Using an API key or a cloud provider** (a direct Anthropic/OpenAI key, Amazon Bedrock, Google
Vertex, …) instead of a subscription? Keys are never copied from your host — pass the ones a sandbox
needs explicitly at create time:

```bash
cs-sandbox create dev --env ANTHROPIC_API_KEY          # passes the value through from your shell
```

See [`docs/agent-login.md`](docs/agent-login.md) for cloud targets and credential files.

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

For bash system-wide instead of per-user: `cs-sandbox completion bash | sudo tee
/etc/bash_completion.d/cs-sandbox`. Run `cs-sandbox completion --help` for per-shell setup details.

## State and cache locations

**State lives in your home, not next to the binary** — instances and
tier keys under `$XDG_DATA_HOME/cs-sandbox` (`~/.local/share/cs-sandbox`), the firecracker artifact
cache under `$XDG_CACHE_HOME/cs-sandbox` (`~/.cache/cs-sandbox`); override with
`CS_SANDBOX_INSTANCES_DIR` / `CS_SANDBOX_TIER_DIR` / `CS_SANDBOX_FC_CACHE`, or `CS_SANDBOX_HOME` for all of it.

Separate state directories give you separate sets of sandboxes that can run side by side, because
the network underneath is per-host and shared: addresses and SSH ports are checked against what is
actually live on the host rather than against one directory's records, each directory gets its own
`~/.ssh/config.d` fragment, and the network's own working dir (`~/.cache/cs-sandbox/net`, moved by
`CS_SANDBOX_FC_NET`) stays put when you relocate the rest.
