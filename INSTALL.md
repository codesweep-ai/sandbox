# sandbox - installation

`cs-sandbox` is a single script: clone the repo and run `./cs-sandbox` from it (the CLI itself needs
no install). Set up a host once with the steps below - then see the [README](README.md) walkthroughs.

> **`./cs-sandbox doctor` checks every prerequisite below and prints the exact fix for anything
> missing.** Run it first; it's the fastest path through this page.

## 1. Podman (required on every host)

Podman builds the image and provides the shared network fabric - both engines need it.

```bash
sudo dnf install podman                                              # Fedora
sudo apt install podman                                              # Ubuntu / Debian
brew install podman && podman machine init && podman machine start   # macOS
```

On macOS everything runs through the podman-machine VM (shared paths must be under `$HOME`); the
Firecracker engine is unavailable there, so sandboxes use Podman automatically.

## 2. Firecracker engine (default on Linux + KVM, x86_64) - extra host packages

`cs-sandbox` auto-downloads the SHA256-verified Firecracker binary on first `create`; you provide a
few host packages and `/dev/kvm` access.

```bash
# Fedora
sudo dnf install passt dnsmasq fakeroot e2fsprogs socat python3 shadow-utils curl git
# Ubuntu / Debian  (uidmap not shadow-utils, dnsmasq-base not dnsmasq)
sudo apt install passt dnsmasq-base fakeroot e2fsprogs socat python3 uidmap curl git

sudo usermod -aG kvm "$USER"            # /dev/kvm access (log out / back in afterward)
grep "^$USER:" /etc/subuid /etc/subgid  # must return a line in each file (rootless userns)

# Ubuntu 24.04+ only: if Podman's rootless network namespace fails to start, allow unprivileged userns:
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
```

A Firecracker sandbox boots its own guest kernel, built from the sandbox image - pinned and
reproducible, with no dependency on the host's `/boot` kernel. Full detail:
[`docs/firecracker.md`](docs/firecracker.md#prerequisites). On macOS / non-KVM hosts this step is
skipped and sandboxes default to Podman.

## 3. Build the image

Every sandbox runs from one generic image (no user identity baked in - your user is created at first
boot). `cs-sandbox` builds it once and reuses it (and auto-builds it on first `create`):

```bash
./cs-sandbox build
```

It bundles a broad toolchain (podman/skopeo/buildah, tmux, chromium, java/maven, go), CLI helpers
(ripgrep, fd, fzf, bat, git-delta, jq/yq, gh, uv), Neovim, pyenv/Python + nvm/Node, and the Claude
Code & Codex agents. See the [`Containerfile`](Containerfile) for the full list.

## 4. Install the host helper tools

Sandboxes already carry the agent toolset (`cs-claude` / `cs-codex` and the remote-delegation
families). This step puts the **host-relevant** ones on your PATH so you can sign in (next) and
delegate from the host:

```bash
./cs-sandbox install        # -> ~/bin  (pass a directory to install elsewhere)
```

`cs-claude` / `cs-codex` invoke the `claude` / `codex` CLIs, so those must be installed on the host
too; `install` tells you if either is missing.

## 5. Sign in to the agents (sandboxes inherit it)

Sign in to **Claude Code & Codex** once on the host. Every sandbox you create afterward inherits the
credentials - they're snapshotted into the sandbox on first boot, **never baked into the image**.

```bash
cs-claude          # launch Claude Code - sign in with /login, then exit
cs-codex           # launch Codex - choose "Sign in with ChatGPT", then exit
```

On macOS (where Claude's credentials live in the Keychain with no file to copy), or to give a
sandbox its own independent session, sign in *inside* a sandbox instead:

```bash
./cs-sandbox claude-login <name>     # or: ./cs-sandbox codex-login <name>
```

**Using an API key or a cloud provider** (a direct Anthropic/OpenAI key, Amazon Bedrock, Google
Vertex, …) instead of a subscription? Put the provider's environment in `~/.cs-claude/env` /
`~/.cs-codex/env` (credential files under `~/.cs-<agent>/creds/`); it's carried into sandboxes like a
login. Full provider matrix and the SSO/ADC caveat:
[`docs/design.md`](docs/design.md#bundled-agent-tooling-and-auth).

## Verify

```bash
./cs-sandbox doctor                       # re-run: everything should be green
./cs-sandbox create smoke --repo .        # create a throwaway sandbox with this repo
ssh smoke                                 # shell in by name
./cs-sandbox destroy smoke                # tear it down
```

You're set - head to the [README](README.md) walkthroughs.
