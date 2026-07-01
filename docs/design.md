# sandbox - design

`cs-sandbox` provisions multiple named, SSH-reachable dev **sandboxes** on a Linux or macOS
host. Each sandbox runs as either a rootless **Firecracker microVM** (the default on a
Linux/KVM host) or a rootless **Podman container** (the default on macOS, and available on any
host). The two engines are interchangeable: they share one image, one SSH trust model, one
network fabric, the same directory-sharing flags, and the same agent tooling.

This document describes the **cross-engine model** - what every sandbox shares regardless of engine,
in the order a sandbox comes to life: what it's built from and how it boots, how you reach and trust
it, what you share into it, nested-Podman image management and agents, then security.
Three companion documents cover the engine- and feature-specific parts:

- [`podman.md`](podman.md) - the Podman container engine.
- [`firecracker.md`](firecracker.md) - the Firecracker microVM engine.
- [`repo-sharing.md`](repo-sharing.md) - the `--repo` checkout model.

## Overview

- **Two sandbox types**, by who runs them:
  - **user** - for the human host user. Gets a one-time **copy** of the
    host's `~/.ssh`, so in-sandbox git-over-SSH works as it does on the host.
  - **agent** (default) - for an autonomous coding agent. Its own persistent home, and **no**
    host SSH credentials of any kind.
- **Reach any sandbox by name**, never by port number - from the host (`ssh <name>`) and
  between sandboxes, across both engines.
- **True nested Podman** inside every sandbox.
- **One generic image** with no developer identity baked in; the matching user is created at
  first boot, so one build serves every developer and machine.
- **The same behavior on Linux and macOS** (which runs the Podman engine in a podman-machine VM -
  see [`podman.md`](podman.md#macos)).

## Anatomy of a sandbox

How a sandbox is built from the generic image and comes to life at boot - the parts common to both
engines. The engine-specific boot, storage, and nested-Podman mechanics are in
[`podman.md`](podman.md) (the container engine) and [`firecracker.md`](firecracker.md) (the microVM
engine, which delivers the same pieces as block devices instead of mounts).

The image bakes in **no** developer identity - no user name, uid, gid, or per-user home - so one
build serves every developer and machine and you never rebuild to match your local environment.
Two pieces make this work:

- **Toolchains live under `/opt`** (shared, root-owned), not in a per-user `$HOME`: pyenv+Python,
  nvm+Node, the native coding-agent binaries (Claude Code, Codex), and Python CLI tools in a venv.
  All are on `PATH` for every shell (so non-interactive `ssh <name> <cmd>` finds them too). Being
  root-owned, they are effectively read-only for the dev user - adding language versions or global
  packages needs `sudo`; per-project virtualenvs and `node_modules` in your repos are unaffected.
- **The runtime user is created at first boot.** `cs-sandbox` passes your identity and the sandbox
  config as environment (`CS_SANDBOX_USER`/`UID`/`GID`/`HOME`, plus `CS_SANDBOX_TYPE` / `YOLO` /
  `SSH_PORT` / `IMAGE_STORES`), and the guest **init** - the container entrypoint, or the microVM's
  `/fc-init` - creates the matching group + user with NOPASSWD sudo, seeds and chowns the home,
  installs the seed material, starts sshd, and drops to that user for the main process. How the
  guest is launched and how file ownership stays correct differ by engine: the Podman path
  (`--userns=keep-id`, the entrypoint) is in [`podman.md`](podman.md#container-boot); the microVM
  path (`/fc-init`, real root) in [`firecracker.md`](firecracker.md#per-sandbox-anatomy).

### The per-sandbox seed

The home persists across stop/start; how it is stored differs by engine - a named Podman volume
(see [`podman.md`](podman.md#home-volume)) or the VM's `rootfs.ext4` disk (see
[`firecracker.md`](firecracker.md#disks)).

A per-sandbox **read-only seed dir** (`instances/<name>/seed`, mounted at `/run/cs-sandbox-seed`) is
the interface between `cs-sandbox` and the entrypoint. `build_seed` populates it (shared with the
Firecracker engine) with:

- `authorized_keys` and the tier private key (`id_cs-sandbox_{user,agent}`);
- `ssh_config` and the stable `host_keys/`;
- `host_hosts` - the host-by-name map (see [Networking](#reaching-the-host-by-name-from-inside-a-sandbox));
- `inject-env` - `--env` / `--env-file` vars (see [Injecting environment variables](#injecting-environment-variables));
- `host_ssh/` - the host `~/.ssh` snapshot (user sandboxes only);
- `git_identity` - the host's global git `user.name`/`email`, seeded into the sandbox's `~/.gitconfig`;
- `claude/` + `codex/` - the agent credentials, plus the API-key/cloud `env` + `creds/` when present.

On boot the guest init (the container entrypoint or the microVM's `/fc-init`) splits work by a sentinel:

- **first boot** (`~/.cs-sandbox-initialized` absent): seed the skeleton home from the image
  (`/sandbox/home` - dotfiles, `~/bin` + the bundled agent tooling, pre-built Neovim plugins) and
  chown it; copy the host `~/.ssh` snapshot (user sandboxes); install the agent credentials. `--repo`
  clones use a separate `~/.cs-sandbox-repos-done` guard.
- **every boot** (idempotent): refresh the *managed* ssh material - `authorized_keys`, the tier key,
  `ssh_config` → `~/.ssh/config.d/cs-sandbox`, and the persisted `ssh_host_*` keys - so key rotation
  just works; normalize perms; start sshd; signal readiness.

The in-sandbox `~/.ssh/config.d/cs-sandbox` scopes its rules to `Host * !*.*` (dotless names = peer
sandboxes) with `StrictHostKeyChecking accept-new`. **Agent** sandboxes also pin the agent-tier key
there (`IdentityFile`, their only key) and set `PreferredAuthentications publickey` - fabric/host
access is always key-based, so when an agent's key isn't accepted (e.g. ssh to the host) it is
denied immediately instead of falling through to the host's `password` prompt and hanging on a TTY.
**User** sandboxes omit both so ssh still offers the copied host keys to real git hosts (and may
password-auth to a dotless LAN host). Dotted hosts (GitHub, FQDNs) are untouched, so git-over-SSH
keeps working.

### Injecting environment variables

`--env KEY=VALUE` / `-e` and `--env-file FILE` (both repeatable) inject variables into the **whole
sandbox**. `cs-sandbox` resolves them at create time into one `KEY=VALUE` block - `#` comments and
blank lines in a file are ignored, and a bare `KEY` (no `=`) passes through the host's current value
(like `docker --env-file`) - and writes it to the gitignored seed `inject-env` (mode 600). The guest
init installs it into the user's `~/.ssh/environment`, and sshd runs with `PermitUserEnvironment=yes`
(it already runs `UsePAM=no`, so `/etc/environment`/pam_env wouldn't apply) - so **every** ssh
session sees the vars, interactive *and* `ssh <name> cmd`. The same set is also placed in the guest's
**PID 1 environment** so the whole process tree inherits it (`cs-sandbox exec`, services, the agent):
Podman sets it as the container env (`-e`), and Firecracker's `fc-init` exports it. So both engines
cover the two namespaces ssh can't bridge under `UsePAM=no` - the ssh session env and the process
tree - from one resolved set. `.env` is never auto-loaded - it's passed explicitly, consistent with
`docker run` / `podman run`.

## Sandbox types and the SSH trust model

Access is governed by sandbox **type**, independent of engine. The matrix (client → server):

| client ↓ \ server → | user | agent |
|---|:---:|:---:|
| **host**            | ✓ | ✓ |
| **user** sandbox   | ✓ | ✓ |
| **agent** sandbox  | ✗ | ✓ |

In words: the host and user sandboxes reach everything; agents reach other agents but **not**
user sandboxes (so an agent can never touch a sandbox carrying your host SSH keys). The allowed
reaches - note that nothing points *into* a user sandbox from an agent:

```
                    ┌──────┐
                    │ host │
                    └──┬───┘
              ┌────────┴────────┐
              ▼                 ▼
        ┌───────────┐     ┌───────────┐
        │   user    │ ──▶ │   agent   │
        │ sandboxes │     │ sandboxes │
        └───────────┘     └───────────┘
          ↺ itself          ↺ itself

        ──▶ = "may SSH into".  An agent never ──▶ a user sandbox.
```

Three key identities produce this matrix:

| Symbol | What | Lives in | Grants |
|---|---|---|---|
| **H** | the host user's existing `~/.ssh/*.pub` | host `~/.ssh`, copied into user sandboxes | host → sandboxes; in-sandbox git-over-SSH |
| **U** | a generated **user-tier** key | user sandboxes only | user sandbox → any sandbox |
| **G** | a generated **agent-tier** key | agent sandboxes only | agent sandbox → agent sandboxes |

`U` and `G` are generated once, on the first `cs-sandbox create`, into `tier-keys/` (gitignored,
mode 600). Each sandbox's `authorized_keys` is **always generated by `cs-sandbox`** (a copied
host `authorized_keys` is never inherited):

- **user** sandbox authorizes `H + U`, and holds the private keys `H` (copied host `~/.ssh`) + `U`.
- **agent** sandbox authorizes `H + U + G`, and holds only `G`.

The single rule that blocks "agent → user": `G` is never written into a user sandbox's
`authorized_keys`, and agent sandboxes never receive `H` or `U`.

### Solo sandboxes (`--solo`)

By default every agent sits in the mesh of the matrix above - any agent can SSH into any other
agent (the shared `G` key). `cs-sandbox create <name> --type agent --solo` denies one agent any
**outbound SSH** into the fabric: it **can't SSH into any peer or the host**, but **peers and the
host can still SSH into it**. The restriction is one-directional. Agent-only (rejected for
`--type user`, which intentionally carries `H` and reaches peers).

This is an **SSH-credential boundary, not a network one**, and it is asymmetric:

- **Outbound (blocked).** A solo agent is seeded **no** tier private key - no `U` and no `G`. It
  therefore holds no key that any sandbox (or the host) authorizes, so it cannot authenticate
  outward to anything. (Its in-sandbox ssh config also pins no `IdentityFile` and keeps the
  agent's `PreferredAuthentications publickey`, so an outbound attempt fails fast.)
- **Inbound (allowed).** Its `authorized_keys` is **normal** (`H + U + G`, exactly like any agent),
  so the host, user sandboxes, and other agents can still SSH *in* and drive it.

It is otherwise a normal sandbox on the shared fabric: its network access is exactly like any
other sandbox's - it `ping`s / `curl`s peers, the host, the LAN, and the internet, and peers reach
*its* services. `--solo` is purely a credential restriction; what it removes is the solo agent's
ability to get an *authenticated SSH foothold* on anything else.

Implemented entirely in `build_seed` (the third `solo` argument withholds the tier private key
while leaving `authorized_keys` untouched). Solo state is recorded as
`solo=1` in `instances/<name>/meta` (and a `cs-sandbox.solo` Podman label), and surfaced in the
`SOLO` column of `cs-sandbox ls`.

This is the mitigation for the "any agent can SSH into any other agent" property noted under
[Limitations](#limitations): put a sandbox you don't fully trust on `--solo` and it can't SSH into
your other sandboxes (you keep full reach into it).

## Networking and name resolution

Sandboxes run on a rootless bridge network (`cs-sandbox-net`), created on demand - **not**
host networking. A bridge keeps each sandbox's network stack isolated, gives DNS for free
(so name-based reach is automatic), forwards cleanly on macOS, and keeps sandbox services off
the host's loopback by default.

**Reach by name, between sandboxes.** Podman's aardvark-dns resolves container names on the
network, and the microVM engine adds a small forwarding dnsmasq for VM names - so any sandbox
reaches any other as `ssh <name>` (internal port 22), container↔VM included.

**Reach by name, from the host.** Every sandbox's sshd listens on **22** internally; the host
publishes it on `127.0.0.1:<PORT>`, where `<PORT>` is the first free port in **2200-2399**:
containers draw from 2200-2299, microVMs from 2300-2399 (recorded in `instances/<name>/meta`).
`cs-sandbox` maintains `~/.ssh/config.d/cs-sandbox` (included from `~/.ssh/config`) with one
`Host <name>` block per sandbox, so `ssh <name>` works from the host too. Each block:

- points at `127.0.0.1:<PORT>` and emits an `IdentityFile` line for **every** authorized host
  key plus `IdentitiesOnly yes` - otherwise ssh only tries the default key names and a sandbox
  whose key is non-standard-named (e.g. `id_ed25519_work`) fails with "Permission denied" even
  though its key *is* authorized;
- uses `HostKeyAlias <name>` against a dedicated `~/.ssh/known_hosts.cs-sandbox`, so the
  known-hosts entry is keyed by name rather than `127.0.0.1:<port>` - otherwise recycling a freed
  port for a different sandbox would trip "host key changed". Each sandbox's `ssh_host_*` keys
  are generated once at create time and persisted, so its identity is stable across restarts.

`cs-sandbox sync-ssh-config` regenerates the host config (both engines). Discover sandboxes with
`cs-sandbox ls`, `cs-sandbox port <name>`.

**Reach a service from another machine** via `ssh -J <host> <name>` (ProxyJump, nothing
exposed), or by binding the published ports on the network (`export CS_SANDBOX_SSH_BIND=0.0.0.0`
before `create` - mind your firewall).

**Port forwarding (host → sandbox service).** Tunnel a host port to a port inside a sandbox over
SSH - the no-sudo, every-platform way to reach a service a sandbox binds (the alternative to
[`host-route`](#optional-reach-sandboxes-directly-by-name-host-route), which reaches ports by name
but is Linux-only and needs a one-time sudo):

```bash
cs-sandbox forward <name> [HOSTPORT:]VMPORT...   # e.g. forward web 9000:8000  -> host :9000 → sandbox :8000
cs-sandbox forward <name> --socks [PORT]         # open a SOCKS proxy into the sandbox instead
cs-sandbox forwards [<name>]                      # list active forwards (all, or for one sandbox)
cs-sandbox unforward <name> [HOSTPORT|all]        # tear down one forward, or all of them
```

### Reaching the host by name (from inside a sandbox)

From inside a sandbox the host's own LAN/Tailscale name isn't routable (it hairpins through the
rootless NAT), but the host *is* reachable at the pasta address `host.containers.internal`
(an IPv4 link-local, `169.254.1.2`). `build_seed` resolves that address and maps the host's
hostname(s) to it in `host_hosts`; the entrypoint (and `fc-init`) append that to the guest's
`/etc/hosts`, so `ssh <hostname>` / `curl <hostname>:PORT` from a sandbox reach the host - NSS
checks `files` before DNS, beating the unroutable name.

One catch: the pinned mapping is **IPv4-only**, but the sandbox network is also IPv4-only (the
guest has just a link-local IPv6, no v6 route), while the host's resolver / Tailscale MagicDNS still
hands back **AAAA** records for that name. `/etc/hosts` only wins *per address family*, so the
unreachable IPv6 answer would survive - and since `getaddrinfo` prefers IPv6 by default, naive
single-address clients (e.g. `bash`'s `/dev/tcp`) would try it first and hang, and every dual-stack
lookup would eat a v6 timeout. So the guest init also writes `/etc/gai.conf`
(`precedence ::ffff:0:0/96 100`) to **prefer IPv4** - the standard fix for a v4-only host - which
makes the pinned IPv4 (and v4 generally) win deterministically.

### Optional: reach sandboxes directly by name (`host-route`)

By default the host reaches a sandbox only over SSH or an explicit `forward`; it can't `ping
<name>` or `curl <name>:PORT` the way a *peer* sandbox can, because the fabric lives in podman's
rootless network namespace and a sandbox's address isn't in the host's root netns.

`cs-sandbox host-route up` opts in to direct reachability, under a **one-time `sudo`**: it wires
the host onto the sandbox subnet (a veth into the rootless netns) and points **systemd-resolved**
at the fabric's own DNS resolver for the **`.cs.sandbox`** domain. After that, `ping
<name>.cs.sandbox`, `curl http://<name>.cs.sandbox:8000`, and any other protocol work from the
host, for both engines. Names are published into the resolver **rootlessly**, so create/destroy
need no further sudo, and `/etc/hosts` is never touched. It is **off by default, Linux-only,
needs systemd-resolved**, and is the **only** feature that uses `sudo` (and only for `up`/`down`,
never in the create/exec path). Mechanism and rationale (including why a name suffix is required)
are in [`firecracker.md`](firecracker.md#optional-reach-sandboxes-directly-from-the-host-host-route).

## Directory sharing

A host directory comes into a sandbox only when you ask - there is **no implicit `$PWD`
mount** on either engine. Two modes, each landing the directory at `~/<name>` (default name =
basename; `:NAME` to override), each repeatable; a name clash is a hard error:

| flag | what you get | writable in guest? | engines |
|---|---|:---:|---|
| `--repo PATH[@REF][:NAME]` | per-sandbox **git checkout** on branch `cs-sandbox/<name>` (objects borrowed read-only) | yes (own branch) | both |
| `--snapshot PATH[:NAME]` | **read-only frozen copy** of any directory | no | both |

`--repo` is the engine-portable, git-aware mode (retrieve the sandbox's commits with `cs-sandbox
fetch`, send host commits in with `cs-sandbox push`); it requires a git repo. `--snapshot` takes
any directory. Both work identically on either engine — a sandbox does its work, then you fetch the
results back to the host. Full design in [`repo-sharing.md`](repo-sharing.md).

On **macOS**, each shared path must resolve under a podman-machine-shared root (by default, under
`$HOME`); `cs-sandbox` errors with remediation otherwise.

## Nested Podman & image management

Every sandbox runs **true nested Podman**. In a microVM you are real root on your own kernel, so it
just works; in a Podman container it needs a scaled-down capability set + a rootful inner engine -
those container-engine specifics (caps, the rootful `podman` wrapper, per-sandbox container storage)
are in [`podman.md`](podman.md#nested-podman). The image-management features below work on **both**
engines.

### Shared image stores

To reuse images across sandboxes instead of re-pulling per sandbox, a container references
**named shared stores** read-only via Podman's `additionalimagestores`. `cs-sandbox create …
--image-store <name>` (repeatable) mounts the store read-only and the entrypoint lists it under
`additionalimagestores`; new pulls still land in the sandbox's own writable store. Populate a
store with `cs-sandbox seed-store <name> <image>…` (pulls from a registry) or `--from-host` (copies
an image already in your local store, e.g. the sandbox image itself); manage with
`create-store` / `stores` / `rm-store`.

Because a store is written by the rootful nested engine, image-uid-0 is stored under the keep-id
root and every container's keep-id maps it back to uid 0 inside - so images run with correct
`root`/setuid ownership. `--image-store` works on the **microVM** engine too, where the store is
delivered as a read-only ext4 disk built from the volume (the same content-addressed, cached
mechanism as the base rootfs); see
[`firecracker.md`](firecracker.md#disks). A read-only shared base with a per-sandbox
writable primary is the supported way to share: independent engines writing one store risk lock
contention and corruption.

### Private registry

Nested Podman can pull from a **private registry** - a local/internal registry rather than a
public one. The registry to trust is baked into the image's `registries.conf` at build time and
controlled by two env vars (read by `cs-sandbox` and forwarded to the build as the matching
`--build-arg`s):

| Env var | Default | Meaning |
|---|---|---|
| `CS_SANDBOX_PRIVATE_REGISTRY` | _(none)_ | Registry to trust, as a bare `host:port` (no `http://`/`https://` scheme). Empty registers none. |
| `CS_SANDBOX_PRIVATE_REGISTRY_INSECURE` | `0` (secure) | `1`/`true`/`yes`/`on` → insecure: permit plain-HTTP and skip TLS verification. Anything else → secure: HTTPS with a verified cert. |

Following standard docker/podman convention, the **protocol is implicit in the security setting**,
not a scheme on the registry value: a registry is named by its bare `host:port`, a *secure* entry is
reached over HTTPS with a verified TLS cert, and an *insecure* entry permits plain-HTTP and untrusted
/ self-signed certs. (This mirrors Podman's `registries.conf` `location` + `insecure`, and Docker's
`insecure-registries`.) **Secure is the default**; an insecure registry is opt-in.

Both variables are read at **build** time (`cs-sandbox build`); rebuild the image after changing
them. Examples:

```bash
# Secure private registry (HTTPS, TLS-verified) - the default
CS_SANDBOX_PRIVATE_REGISTRY=registry.corp.example:5000 ./cs-sandbox build

# Insecure private registry (plain-HTTP or self-signed cert)
CS_SANDBOX_PRIVATE_REGISTRY=registry.internal:5000 \
CS_SANDBOX_PRIVATE_REGISTRY_INSECURE=1 ./cs-sandbox build
```

A secure registry writes only a `location` entry (TLS enforced); an insecure one adds
`insecure = true`, which lets Podman use plain-HTTP and accept untrusted/self-signed certs for
that host only. Other registries are unaffected.

## Bundled agent tooling and auth

Every sandbox ships the `cs-claude` and `cs-codex` toolsets so the coding agents work without
re-authenticating per sandbox. The launch wrappers, helper scripts, and companion docs are
maintained in this repo under `context/home/` - generic and free of host/personal specifics.

**Launch wrappers → `~/bin`** (non-secret, baked into the image skeleton). Each agent gets a
parallel wrapper that runs it under a dedicated profile (keeping the sandbox's config/auth isolated
from any personal `~/.claude`/`~/.codex`) and pre-trusts the launch directory, so the agent never
stops at a "do you trust this folder?" gate:

- **`cs-claude`** runs `claude` with `CLAUDE_CONFIG_DIR=~/.cs-claude` in `--permission-mode auto`,
  which honors the profile's allow/deny rules.
- **`cs-codex`** runs `codex` with `CODEX_HOME=~/.cs-codex`; its `config.toml` supplies the defaults
  `approval_policy=on-request` + `sandbox_mode=workspace-write` - the analogue of Claude's `auto` mode.

**Remote-delegation families → `~/bin`** (non-secret, baked). A parallel family for each agent -
`cs-claude-remote` / `cs-codex-remote`, each plus `-status`/`-output`/`-sessions`/`-forget` and a
`-turn` driver - delegates a task to an agent session on another host over SSH, keeping the session
warm in tmux and reading output from the session JSONL. The target host resolves per session
(`-H <host>` > a per-session stored host > `$CS_CLAUDE_REMOTE_HOST`/`$CS_CODEX_REMOTE_HOST` > this
machine's short hostname - inside a sandbox, that is its own name), so by default they target the
sandbox itself. Reaching an external host needs
SSH access to it - user sandboxes have it via the copied host keys; an agent can only reach hosts
that trust the agent tier.

**Settings + instruction hubs** (non-secret, baked into each profile dir):

- **`~/.cs-claude`** - a `settings.json` (the allow/deny rules plus editor defaults `editorMode:
  vim` and `remoteControlAtStartup: true`), a `CLAUDE.md` instruction hub, and a
  `CLAUDE_PERMISSIONS.md` reference.
- **`~/.cs-codex`** - a `config.toml` (the `approval_policy`/`sandbox_mode` defaults above) and an
  `AGENTS.md` instruction hub.

Both hubs (`CLAUDE.md` and `AGENTS.md`) describe **both** toolsets inline and point to the full
per-tool reference docs in `~/bin` (read on demand), so an in-sandbox Claude can drive Codex remote
sessions and an in-sandbox Codex can drive Claude remote sessions.

**YOLO mode (`--yolo`, agents only).** `cs-claude`/`cs-codex` skip all permission prompts
(`--dangerously-skip-permissions` / `--dangerously-bypass-approvals-and-sandbox`) when a
`.yolo` marker exists. `cs-sandbox create --yolo` is **rejected for `--type user`**; the marker is
written only when the type is `agent` (a defense-in-depth re-check). Skipping prompts is safe only
because the sandbox is an isolated sandbox - never for a user sandbox, which carries your host
keys.

**Subscription auth (secret, NEVER baked).** `cs-sandbox create` snapshots the host's agent
credentials (`~/.cs-claude/.credentials.json`, `~/.cs-codex/auth.json`) into the gitignored
per-sandbox seed; the entrypoint installs them into the home volume (mode 600) on **first boot
only** (so a token the sandbox later refreshes isn't clobbered). Seeded into both sandbox types.
Two caveats:

- **Single seat / concurrency.** One subscription shared across the host and many sandboxes shares
  a rate-limit pool, and independent OAuth refreshes can log each other out. Accepted trade-off.
- **macOS.** Agent credentials live in the Keychain (no file) on a Mac, so the copy-from-host path
  needs a Linux host. On macOS, run `cs-sandbox claude-login <name>` / `codex-login <name>` to
  authenticate once inside the sandbox - also the way to give any sandbox an independent session.

**API-key / cloud-provider auth (secret, NEVER baked).** A subscription is one self-contained
credential file; an API-key or cloud setup is a *bundle* - provider selection + a secret +
region/project, sometimes an external credential file - and both agents read it from the
**environment** (with provider config in `settings.json` / `config.toml`). So `cs-sandbox` carries,
per agent, through the same gitignored-seed → first-boot-install (mode 600) path:

- a **`~/.cs-<agent>/env`** file (`KEY=value`) the `cs-claude` / `cs-codex` wrapper **sources** at
  launch - e.g. `ANTHROPIC_API_KEY`; `CLAUDE_CODE_USE_BEDROCK`/`_VERTEX` + `AWS_*` / `CLOUD_ML_REGION`
  / `ANTHROPIC_VERTEX_PROJECT_ID`; `OPENAI_API_KEY`; or a custom Codex provider's `env_key` var;
- an optional **`~/.cs-<agent>/creds/`** dir for credential files. Because a sandbox reproduces the
  host username/home, a path like `~/.cs-claude/creds/sa.json` resolves identically host↔sandbox, so
  `GOOGLE_APPLICATION_CREDENTIALS` / `AWS_SHARED_CREDENTIALS_FILE` need no remapping.

On top of the declarative `env`, `create` **auto-captures** a known scalar provider-var allowlist
present in its environment (the declarative file wins). Path-valued vars
(`GOOGLE_APPLICATION_CREDENTIALS`, `AWS_*_FILE`) are *not* auto-captured - they point at host files,
so use `env` + `creds/` (a note fires if a cloud flag is set without them). More caveats:

- **Precedence.** An injected `ANTHROPIC_API_KEY` / cloud flag overrides a subscription OAuth login,
  so carrying is opt-in by virtue of creating the `env` file - subscription users (no `env`) are
  unaffected.
- **Scoping / blast radius.** Carried into both sandbox types by default; `create --no-keys` opts an
  sandbox out, and a warning fires when an **agent** receives credentials - a cloud key handed to an
  autonomous agent is a bigger blast radius than a model-only subscription token (prefer a
  least-privilege key for agents).
- **Refresh limitation.** Static keys / service-account JSON / a Bedrock API key work headless;
  **interactive AWS SSO / GCP user ADC** can't refresh in a sandbox - use static / service-account
  credentials (or Claude Code's non-interactive `awsCredentialExport` / `gcpAuthRefresh` settings).
- **Codex custom providers** (Azure/OpenRouter) also need a `config.toml` `[model_providers]` block
  (`wire_api = "responses"`); carrying that into the seeded `config.toml` is a planned follow-up -
  for now add it in-sandbox.

## Security model

- Sandboxes run **rootless** with a **scaled-down cap set** (engine and container bounded by your
  unprivileged host user via keep-id), seccomp on, and `/proc/kcore` + host devices masked -
  granting only the caps nested Podman needs. There is no host-root path absent a kernel bug.
  `--privileged` is an opt-in fallback that trades that defense-in-depth for breadth. The microVM
  engine removes the shared-kernel attack surface entirely.
- **Passwordless sudo inside is safe - and is the usual setup for agent sandboxes.** The runtime
  user gets `NOPASSWD:ALL`, but the trust boundary is the *engine*, not in-sandbox sudo. On the
  container engine, "root" inside is just your unprivileged host uid through `--userns=keep-id`, so
  `sudo` grants the agent nothing it doesn't already control over its own disposable sandbox - and
  cannot reach anything outside the namespace. On the microVM engine, root is real but confined to
  the guest's own kernel. Giving an autonomous coding agent full root *inside* a throwaway, isolated
  sandbox, while delegating all real isolation to the host boundary, is the standard pattern for agent
  sandboxes (rootless userns, gVisor/Kata, or a fresh microVM); restricting sudo inside would add
  friction (the nested-Podman wrapper shells out to `sudo` on every call) without adding a boundary.
  This holds **only** while that boundary is intact: running the image rootful, `--privileged`, or
  `--userns=host` would turn the same passwordless sudo into genuine host-root.
- SSH ports bind `127.0.0.1` only; sandboxes are not exposed on the LAN by default.
- User sandboxes hold a copy of your host private keys inside the home **volume** (mode 600,
  seeded once) - not in a repo-adjacent, git-trackable directory. The agent credential snapshot
  lives only in the gitignored per-sandbox seed and the home volume - never in the image or git.

## Limitations

- **No per-agent isolation _by default_.** All agent sandboxes share one agent-tier SSH key (the `G`
  key from the [trust model](#sandbox-types-and-the-ssh-trust-model) above), so any agent sandbox can
  SSH into any other. Agents are walled off from you and from user sandboxes, but not from each other,
  unless you create one with [`--solo`](#solo-sandboxes---solo), which denies it any outbound SSH
  (it can't SSH into peers or the host, though they can still SSH into it; network reach is unchanged).
- **Not bit-for-bit reproducible.** The image runs a package update at build time, so rebuilds can
  pick up newer upstream packages.
