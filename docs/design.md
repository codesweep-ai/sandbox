# sandbox - design

`cs-sandbox` provisions multiple named, SSH-reachable dev **sandboxes** on a Linux or macOS
host. Each sandbox runs as either a rootless **Firecracker microVM** (the default on a
Linux/KVM host) or a rootless **Podman container** (the default on macOS, and available on any
host). The two engines are interchangeable: they share one image, one SSH trust model, one
network fabric, the same directory-sharing flags, and the same agent tools.

This document describes the **cross-engine model** - what every sandbox shares regardless of engine,
in the order a sandbox comes to life: what it's built from and how it boots, how you reach and trust
it, what you share into it, nested-Podman image management and agents, then security.
Three companion documents cover the engine- and feature-specific parts:

- [`podman.md`](podman.md) - the Podman container engine.
- [`firecracker.md`](firecracker.md) - the Firecracker microVM engine.
- [`repo-sharing.md`](repo-sharing.md) - the `--repo` checkout model.
- [`agent-login.md`](agent-login.md) - how a sandbox gets a logged-in agent, and what is never copied.

## Overview

- **Two sandbox types**, differing only in SSH direction — otherwise the same image and capabilities:
  - **user** - your interactive workspace, the layer above: it can `ssh` into any sandbox (other user
    sandboxes and agent sandboxes), but no agent sandbox can `ssh` back into it. Receives **no** host
    SSH keys of its own; if it needs your keys you can lend a specific set for a session (`ssh -A`),
    so they are never copied in.
  - **agent** (default) - a sandbox for running a coding agent. Its own persistent home, **no** host
    SSH credentials of any kind, and it can `ssh` into other agent sandboxes but never a user sandbox.
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
  nvm+Node, the Go toolchain, the Temurin JDK + Maven, the native coding-agent binaries (Claude
  Code, Codex, OpenCode), Python CLI tools in a venv, and Neovim's Mason packages (the language servers and
  formatters, `/opt/nvim/mason`). Each is pinned in the `Containerfile` and wired up by
  `~/.bashrc`, so the versions are reproducible rather than whatever the distro last shipped
  (which also means one JDK on `PATH`, not whichever the distro's alternatives point at). Go
  additionally keeps
  upstream's `GOTOOLCHAIN=auto`, so a repo whose `go.mod` names another `go 1.x` fetches that
  toolchain into `$GOPATH` on demand — no `sudo`, despite `/opt` being read-only.
  All are on `PATH` for every shell (so non-interactive `ssh <name> <cmd>` finds them too). Being
  root-owned, they are effectively read-only for the dev user - adding language versions or global
  packages needs `sudo`; per-project virtualenvs and `node_modules` in your repos are unaffected.
  Mason additionally honours `$MASON_ROOT` if you want a private, writable package set.
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
the interface between `cs-sandbox` and the entrypoint. The seed builder (`internal/seed`) populates it
(shared with the Firecracker engine) with:

- `authorized_keys` and the tier private key (`id_cs-sandbox_{user,agent}`);
- `ssh_config` and the stable `host_keys/`;
- `host_hosts` - the host-by-name map (see [Networking](#reaching-the-host-by-name-from-inside-a-sandbox));
- `inject-env` - `--env` / `--env-file` vars (see [Injecting environment variables](#injecting-environment-variables));
- `git_identity` - the host's global git `user.name`/`email`, seeded into the sandbox's `~/.gitconfig`;
- `claude/` + `codex/` - the host login of each agent named by `--inherit-agent-login` (nothing by
  default).

On boot the guest init (the container entrypoint or the microVM's `/fc-init`) splits work by a sentinel:

- **first boot** (`~/.cs-sandbox-initialized` absent): seed the skeleton home from the image
  (`/sandbox/home` - dotfiles, `~/.local/bin` + the bundled agent tools, pre-built Neovim plugins and
  treesitter parsers; the Neovim language servers are shared under `/opt` instead) and
  chown it; install the agent credentials. `--repo` clones use a separate
  `~/.cs-sandbox-repos-done` guard.
- **every boot** (idempotent): refresh the *managed* ssh material - `authorized_keys`, the tier key,
  `ssh_config` → `~/.ssh/config.d/cs-sandbox`, and the persisted `ssh_host_*` keys - so key rotation
  just works; normalize perms; start sshd; signal readiness.

The in-sandbox `~/.ssh/config.d/cs-sandbox` scopes its rules to `Host * !*.*` (dotless names = peer
sandboxes) with `StrictHostKeyChecking accept-new`. **Both** types pin their tier key there
(`IdentityFile`) - it is the only key each holds that peers authorize (users hold `U`, agents hold
`G`); the pin is scoped to dotless peer names, so dotted hosts (GitHub, FQDNs) are untouched. **Agent**
sandboxes additionally set `PreferredAuthentications publickey` - fabric/host access is always
key-based, so when an agent's key isn't accepted (e.g. ssh to the host) it is denied immediately
instead of falling through to the host's `password` prompt and hanging on a TTY. **User** sandboxes
omit that (they may legitimately password-auth to a dotless LAN host) and can reach dotted external
hosts through an agent forwarded by `ssh -A` (if you forward one) - with no keys copied in.

Pinning the tier key is not enough on its own once the host does `ssh -A` into a user sandbox: the
peer also authorizes `H`, and OpenSSH offers forwarded-agent keys *before* a file-only `IdentityFile`,
so a forwarded `H` would be accepted first and `-A` would silently change the key the sandbox presents
to peers. To keep sandbox→peer auth on the tier key **with or without** `-A`, the config adds
`IdentitiesOnly yes` - but only for real peers, identified by a `Match exec` that resolves the target
and checks it lands on the **fabric subnet** (the podman network gateway's `/24`). Hosts that don't
resolve onto the fabric (external machines, dotless or not) are left alone, so a forwarded agent still
reaches them. Result: a user sandbox uses `U` for every peer regardless of `-A`, while `ssh -A` still
lets it reach external hosts with your forwarded keys.

### Injecting environment variables

`--env KEY=VALUE` / `-e` and `--env-file FILE` (both repeatable) inject variables into the **whole
sandbox**. `cs-sandbox` resolves them at create time into one `KEY=VALUE` block - `#` comments and
blank lines in a file are ignored, and a bare `KEY` (no `=`) passes through the host's current value
(like `docker --env-file`) - and writes it to the per-sandbox seed `inject-env` (mode 600). The guest
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

In words: the host and user sandboxes reach everything; agents reach other agents but **not** user
sandboxes — so an agent can never reach a forwarded `ssh -A` socket in a user sandbox.

Three key identities produce this matrix:

| Symbol | What | Lives in | Grants |
|---|---|---|---|
| **H** | the host user's existing `~/.ssh/*.pub` | **public** keys authorized in every sandbox; the private keys never leave the host | host → sandboxes (and, when *forwarded* via `ssh -A`, reaching external hosts from a sandbox) |
| **U** | a generated **user-tier** key | user sandboxes only | user sandbox → any sandbox |
| **G** | a generated **agent-tier** key | agent sandboxes only | agent sandbox → agent sandboxes |

`U` and `G` are generated once, on the first `cs-sandbox create`, into the tier-key store
(`~/.local/share/cs-sandbox/keys`, mode 600). Each sandbox's `authorized_keys` is **always generated by `cs-sandbox`** (a copied
host `authorized_keys` is never inherited):

- **user** sandbox authorizes `H + U`, and holds only the private key `U` (no host keys are copied
  in; if it needs your own keys, you lend a specific set with `ssh -A`).
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

Implemented in the seed builder (`internal/seed`): the solo flag withholds the tier private key
while leaving `authorized_keys` untouched. Solo state is recorded as
`"solo":true` in the sandbox's typed state (`internal/state`, persisted to `instances/<name>/state.json`)
and a `cs-sandbox.solo` Podman label, and surfaced in the `SOLO` column of `cs-sandbox ls`.

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
containers draw from 2200-2299, microVMs from 2300-2399 (recorded in `instances/<name>/state.json`).
"Free" means both unrecorded *and* unanswered - allocation probes loopback, because a port can be
held by a sandbox under a different `CS_SANDBOX_INSTANCES_DIR`, or by an unrelated program.
`cs-sandbox` maintains a fragment under `~/.ssh/config.d` (included from `~/.ssh/config` by a single
globbed `Include`) with one `Host <name>` block per sandbox, so `ssh <name>` works from the host too.
The fragment is per instances root — `cs-sandbox` for the default one, a distinct name for any other
— so that two sandbox sets sharing one `~/.ssh` cannot overwrite each other. Each block:

- points at `127.0.0.1:<PORT>` and emits an `IdentityFile` line for **every** authorized host
  key plus `IdentitiesOnly yes` - otherwise ssh only tries the default key names and a sandbox
  whose key is non-standard-named (e.g. `id_ed25519_work`) fails with "Permission denied" even
  though its key *is* authorized;
- uses `HostKeyAlias <name>` against a dedicated `~/.ssh/known_hosts.cs-sandbox`, so the
  known-hosts entry is keyed by name rather than `127.0.0.1:<port>` - otherwise recycling a freed
  port for a different sandbox would trip "host key changed". Each sandbox's `ssh_host_*` keys
  are generated once at create time and persisted, so its identity is stable across restarts.

`cs-sandbox sync-ssh-config` regenerates the host config (both engines). Discover sandboxes with
`cs-sandbox ls`, `cs-sandbox port <name>`. `ls` also reports each sandbox's lifecycle state in a
`STATUS` column — `running` or `stopped`, the same words `start`/`stop` use — read from `podman ps`
for a container and from the microVM's pid file for a VM, plus an `AGE` column. A third value,
**`removed`**, covers data `rm` kept after its sandbox was gone (`internal/engine/orphan.go`): the
home volume or `rootfs.ext4` with no state record beside it. Listing it keeps that data visible
(rather than dangling silently, as removed containers' volumes do), and `destroy <name> -f` works on
such a name — otherwise nothing could delete what `rm` deliberately kept. The SSH port is not
a column — you reach a sandbox by name — so `cs-sandbox port <name>` prints it for the tools that
need one. `ls -q` prints bare names for piping into other commands.

**Reach a service from another machine** via `ssh -J <host> <name>` (ProxyJump, nothing
exposed), or by binding the published ports on the network (`export CS_SANDBOX_SSH_BIND=0.0.0.0`
before `create` - mind your firewall).

**Port forwarding (host → sandbox service).** Tunnel a host port to a port inside a sandbox over
SSH - the no-sudo, every-platform way to reach a service a sandbox binds (the alternative to
[`host-route`](#optional-reach-sandboxes-directly-by-name-host-route), which makes every port a
sandbox binds reachable by name, but is Linux-only and needs sudo once, to set the route up):

```bash
cs-sandbox forward <name> [HOSTPORT:]VMPORT...   # e.g. forward web 9000:8000  -> host :9000 → sandbox :8000
cs-sandbox forward <name> --socks [PORT]         # open a SOCKS proxy into the sandbox instead
cs-sandbox forwards [<name>]                      # list active forwards (all, or for one sandbox)
cs-sandbox unforward <name> [HOSTPORT|all]        # tear down one forward, or all of them
```

### Reaching the host by name (from inside a sandbox)

From inside a sandbox the host's own LAN/Tailscale name isn't routable (it hairpins through the
rootless NAT), but the host *is* reachable at the pasta host address `169.254.1.2` - the same address
Podman exposes as `host.containers.internal`, and reachable from Firecracker VMs too since their taps
share the one rootless network namespace. At create time `cs-sandbox` maps the host's own name(s)
(`hostname` and its short form) to that address in the seed's `host_hosts`; the entrypoint (and
`fc-init`) append it to the guest's `/etc/hosts`, so `ssh <hostname>` / `curl <hostname>:PORT` from a
sandbox reach the host - NSS checks `files` before DNS, beating the unroutable name.

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

## Bundled agent tools and login

Every sandbox ships the `cs-claude`, `cs-codex`, and `cs-opencode` toolsets, so the coding agents work without
re-authenticating per sandbox. Everything non-secret is baked into the image skeleton from
`image/rootfs/home/` in this repo; everything secret is carried per sandbox through the seed.

**Baked in (non-secret):**

- **Launch wrappers** in `~/.local/bin` (the OpenCode adapter's internals — driver architecture,
  verified upstream behaviors, version-bump procedure — have their own reference,
  [opencode.md](opencode.md)). `cs-claude` runs `claude` under `CLAUDE_CONFIG_DIR=~/.cs-claude`
  in `--permission-mode auto`; `cs-codex` runs `codex` under `CODEX_HOME=~/.cs-codex` with
  `approval_policy=on-request` + `sandbox_mode=workspace-write`; `cs-opencode` runs `opencode` under
  `OPENCODE_CONFIG_DIR=~/.cs-opencode` with a profile-scoped session db (`OPENCODE_DB`) and inline
  auth (`OPENCODE_AUTH_CONTENT`), plus a pinned model and blanket-allow permissions in its
  `opencode.json`. Each uses a dedicated profile, so the sandbox's config never touches a personal
  `~/.claude`/`~/.codex`/`~/.config/opencode`, and each pre-trusts the launch directory (or, for
  opencode, has no trust gate) so the agent never stops at a "do you trust this folder?" gate.
- **Remote agent tools**, also in `~/.local/bin`: `cs-claude-remote`, `cs-codex-remote`, and
  `cs-opencode-remote`, each with `-status`/`-output`/`-sessions`/`-forget` and a `-turn` driver.
  They start or resume an agent
  session on another host over SSH, keeping it warm in tmux, so an agent in one sandbox can hand a
  task to an agent in another. The target host resolves per session and defaults to the sandbox
  itself, so reaching anywhere else needs SSH access the sandbox actually has — for a user sandbox,
  typically keys you forwarded with `ssh -A`.
- **Settings and instruction hubs**: `~/.cs-claude` (a `settings.json`, a `CLAUDE.md` hub, and a
  `CLAUDE_PERMISSIONS.md` reference), `~/.cs-codex` (a `config.toml` and an `AGENTS.md` hub), and
  `~/.cs-opencode` (an `opencode.json` and an `AGENTS.md` hub). Every hub describes **all three**
  toolsets and points at the per-tool docs in `~/.local/bin`, so an in-sandbox Claude can drive
  Codex or OpenCode remote sessions and vice versa.

**YOLO mode.** `cs-sandbox create --yolo` writes a `.yolo` marker; the wrappers then skip all
permission prompts. That is safe because the sandbox is the isolation boundary — it is disposable and
cannot reach your host.

**Carried per sandbox (secret, never baked).** Inheriting a host login is opt-in — but it is the
common case, since the alternative is logging in inside every sandbox: `create
--inherit-agent-login claude|codex|opencode` snapshots that agent's credential into the seed, and the guest
installs it into the home volume (mode 600) on first boot only. A sandbox created without the flag
has no agent login — log in inside it, or with `cs-sandbox agent-login <agent> <name>`. Provider API
keys are never carried; pass them with `--env` if a sandbox needs them. The single-seat and macOS
caveats are in [`agent-login.md`](agent-login.md).

## Security model

- Sandboxes run **rootless** with a **scaled-down cap set** (engine and container bounded by your
  unprivileged host user via keep-id), seccomp on, and `/proc/kcore` + host devices masked -
  granting only the caps nested Podman needs. There is no host-root path absent a kernel bug.
  `--privileged` is an opt-in fallback that trades that defense-in-depth for breadth. The microVM
  engine removes the shared-kernel attack surface entirely.
- **Passwordless sudo inside is safe**, and is the usual setup for agent sandboxes. The runtime user
  gets `NOPASSWD:ALL`, because the trust boundary is the *engine*, not in-sandbox sudo: on the
  container engine "root" inside is just your unprivileged host uid through `--userns=keep-id`, and
  on the microVM engine root is real but confined to the guest's own kernel. Either way `sudo` grants
  an agent nothing it does not already control over its own disposable sandbox, while restricting it
  would add friction (the nested-Podman wrapper shells out to `sudo` on every call) for no boundary.
  This holds **only** while that boundary is intact — running the image rootful, `--privileged`, or
  `--userns=host` turns the same passwordless sudo into genuine host-root.
- SSH ports bind `127.0.0.1` only; sandboxes are not exposed on the LAN by default.
- **No host private keys inside any sandbox.** Nothing at rest to leak from a sandbox's disk/volume;
  sandboxes reach each other with generated tier keys. If a sandbox needs your own keys (e.g. to
  `ssh` on to another machine), you *lend* a specific set with `ssh -A` - the keys stay on the host,
  present only for the life of that session, never copied in. The agent credential snapshot lives
  only in the per-sandbox seed and the home volume - never in the image or git.
- **Type governs reach, not privilege.** An agent sandbox can never `ssh` into a user sandbox — see
  [the trust model](#sandbox-types-and-the-ssh-trust-model) above for how the H/U/G keys enforce it.
- **`ssh -A` is a technique, not a property of a type.** Forwarding an agent lends a sandbox specific
  host keys for the life of a connection without copying any key in; anything running as you inside
  that sandbox can *use* the socket while you are connected. Scope what you load and forward only
  into a sandbox whose operator you trust — the README covers the judgment in
  [Lending a sandbox specific SSH keys](../README.md#lending-a-sandbox-specific-ssh-keys-with-ssh--a).

## Testing

Two tiers, split by whether they touch a real engine:

- **Unit tests** (`make test`) — pure and fast, no external processes. They cover the logic where a
  silent bug would be costly: the seed trust material and agent-login inheritance, spec parsing, instance
  state, the kernel rebuild/pin decision, port allocation, and the CLI itself, driven through the real
  cobra tree with a fake `Runner`.
- **Integration tests** (`make test-integration`, behind a `//go:build integration` tag) — live tests
  on a Linux/KVM host with podman. They create namespaced sandboxes in temp state dirs, tear them
  down, and **skip gracefully** when podman or the image is unavailable. The suite runs with `-p 1`,
  because packages share one network fabric and host SSH port pool.

## Limitations

- **No per-agent isolation _by default_.** All agent sandboxes share one agent-tier SSH key (the `G`
  key from the [trust model](#sandbox-types-and-the-ssh-trust-model) above), so any agent sandbox can
  SSH into any other. Agents are walled off from you and from user sandboxes, but not from each other,
  unless you create one with [`--solo`](#solo-sandboxes---solo), which denies it any outbound SSH
  (it can't SSH into peers or the host, though they can still SSH into it; network reach is unchanged).
- **Not bit-for-bit reproducible.** The image runs a package update at build time, so rebuilds can
  pick up newer upstream packages.
