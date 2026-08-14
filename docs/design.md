# sandbox - design

`cs-sandbox` provisions multiple named, SSH-reachable dev **sandboxes** on a Linux or macOS host
(and on Windows through WSL2, which is a Linux host as far as everything below is concerned). Each
sandbox runs as either a rootless **Firecracker microVM** (the default on an x86_64 Linux/KVM host)
or a rootless **Podman container** (the default everywhere else, and available on any
host). The two engines are interchangeable: they share one image, one SSH trust model, one
network fabric, the same directory-sharing flags, and the same agent tools.

This document describes the **cross-engine model** - what every sandbox shares regardless of engine,
in the order a sandbox comes to life: what it's built from and how it boots, how you reach and trust
it, what you share into it, nested-Podman image management and agents, then security.
Companion documents cover the engine- and feature-specific parts:

- [`podman.md`](podman.md) - the Podman container engine.
- [`firecracker.md`](firecracker.md) - the Firecracker microVM engine.
- [`repo-sharing.md`](repo-sharing.md) - the `--repo` checkout model.
- [`agent-login.md`](agent-login.md) - how a sandbox gets a logged-in agent, and what is never copied.
- [`opencode.md`](opencode.md) - the OpenCode adapter.

## Overview

- **Two sandbox types**, differing only in SSH direction: a **user** sandbox is your interactive
  workspace and reaches every sandbox; an **agent** sandbox (the default) reaches other agents but
  never a user sandbox. Neither holds any host SSH key.
- **Reach any sandbox by name**, never by port number — from the host (`ssh <name>`) and between
  sandboxes, across both engines.
- **Groups**, an opt-in boundary: `--group` gives a set of sandboxes its own network, SSH keys and
  gateway. Without it everything joins one group (`default`) and behaves as a single fabric.
- **True nested Podman** inside every sandbox.
- **One generic image** with no developer identity baked in; the matching user is created at
  first boot, so one build serves every developer and machine.
- **The same behavior on Linux, macOS and WSL2** (macOS runs the Podman engine in a podman-machine
  VM - see [`podman.md`](podman.md#macos)).

## Anatomy of a sandbox

How a sandbox is built from the generic image and comes to life at boot - the parts common to both
engines. The engine-specific boot, storage, and nested-Podman mechanics are in
[`podman.md`](podman.md) (the container engine) and [`firecracker.md`](firecracker.md) (the microVM
engine, which delivers the same pieces as block devices instead of mounts).

The image bakes in **no** developer identity - no user name, uid, gid, or per-user home - so one
build serves every developer and machine and you never rebuild to match your local environment.
Two pieces make this work:

- **Toolchains live under `/opt`** (shared, root-owned) rather than in a per-user `$HOME`:
  pyenv+Python, nvm+Node, Go, the Temurin JDK + Maven, the coding-agent binaries, Python CLI tools
  in a venv, and Neovim's Mason packages (`/opt/nvim/mason`). Each is pinned in the `Containerfile`
  and wired up by `~/.bashrc`, so versions are reproducible rather than whatever the distro last
  shipped, and all are on `PATH` for every shell — non-interactive `ssh <name> <cmd>` included.
  Being root-owned they are read-only for the dev user, so a new language version or a global
  package needs `sudo`; per-project virtualenvs and `node_modules` are unaffected. The two cases
  that would chafe have escape hatches: Go keeps upstream's `GOTOOLCHAIN=auto`, so a `go.mod` naming
  another `go 1.x` fetches it into `$GOPATH` without `sudo`, and Mason honours `$MASON_ROOT` for a
  private, writable package set.
- **The runtime user is created at first boot.** `cs-sandbox` passes your identity and the sandbox
  config in (`CS_SANDBOX_USER`/`UID`/`GID`, plus type, YOLO and the rest) — as container environment
  on the Podman engine, as the seed's `cs-sandbox.conf` on the microVM engine. The guest **init**
  (the container entrypoint, or the microVM's `/fc-init`) then creates the matching group and user
  with NOPASSWD sudo, seeds and chowns the home, installs the seed material, starts sshd, and drops
  to that user for the main process. How it is launched and how ownership stays correct differ by
  engine: the Podman path (`--userns=keep-id`, the entrypoint) is in
  [`podman.md`](podman.md#container-boot), the microVM path (`/fc-init`, real root) in
  [`firecracker.md`](firecracker.md#per-sandbox-anatomy).

### The per-sandbox seed

The home persists across stop/start; how it is stored differs by engine - a named Podman volume
(see [`podman.md`](podman.md#home-volume)) or the VM's `rootfs.ext4` disk (see
[`firecracker.md`](firecracker.md#disks)).

A per-sandbox **read-only seed dir** (`instances/<group>/<name>/seed`, mounted at
`/run/cs-sandbox-seed`) is the interface between `cs-sandbox` and the guest init. The seed builder
(`internal/seed`), shared by both engines, populates it with:

- `authorized_keys` and the tier private key (`id_cs-sandbox_{user,agent}`);
- `ssh_config` and the stable `host_keys/`;
- `host_hosts` - the host-by-name map (see [Networking](#reaching-the-host-by-name-from-inside-a-sandbox));
- `inject-env` - `--env` / `--env-file` vars (see [Injecting environment variables](#injecting-environment-variables));
- `git_identity` - the host's global git `user.name`/`email`, seeded into the sandbox's `~/.gitconfig`;
- `claude/`, `codex/` and `opencode/` - the subscription credential of each agent named by
  `--inherit-agent-login` (nothing by default; provider API keys are never carried).

On boot the guest init (the container entrypoint or the microVM's `/fc-init`) splits work by a sentinel:

- **first boot** (`~/.cs-sandbox-initialized` absent): seed the skeleton home from the image
  (`/sandbox/home` - dotfiles, `~/.local/bin` + the bundled agent tools, pre-built Neovim plugins and
  treesitter parsers; the Neovim language servers are shared under `/opt` instead) and
  chown it; install the agent credentials. `--repo` clones use a separate
  `~/.cs-sandbox-repos-done` guard.
- **every boot** (idempotent): refresh the *managed* ssh material - `authorized_keys`, the tier key,
  `ssh_config` → `~/.ssh/config.d/cs-sandbox`, and the persisted `ssh_host_*` keys - so key rotation
  just works; normalize perms; start sshd; signal readiness.

The in-sandbox `~/.ssh/config.d/cs-sandbox` scopes its rules to `Host * !*.*` — dotless names, i.e.
peer sandboxes — so dotted hosts (GitHub, FQDNs) keep ssh's defaults. Both types pin their tier key
there as `IdentityFile`, it being the only key each holds that peers authorize. **Agent** sandboxes
additionally set `PreferredAuthentications publickey`, so an unaccepted key (ssh to the host, say)
is denied at once instead of falling through to a password prompt and hanging on a TTY; **user**
sandboxes omit that, since they may legitimately password-auth to a dotless LAN host.

The pin alone is not enough once the host does `ssh -A` into a user sandbox: peers also authorize
`H`, and OpenSSH offers forwarded-agent keys *before* a file-only `IdentityFile`, so `-A` would
silently change the key the sandbox presents. The config therefore adds `IdentitiesOnly yes`, but
only for real peers — a `Match exec` resolves the target and checks it lands on the fabric subnet.
Anything that does not (external machines, dotless or not) is left alone. A user sandbox thus uses
`U` for every peer with or without `-A`, while `ssh -A` still reaches external hosts with your
forwarded keys, and no key is ever copied in.

### Injecting environment variables

`--env KEY=VALUE` / `-e` and `--env-file FILE` (both repeatable) inject variables into the **whole
sandbox**. `cs-sandbox` resolves them at create time into one `KEY=VALUE` block — `#` comments and
blank lines are ignored, and a bare `KEY` passes through the host's current value, as
`docker --env-file` does — and writes it to the seed's `inject-env` (mode 600).

Two namespaces then have to be covered, because sshd runs `UsePAM=no` and so ignores
`/etc/environment`. The guest init installs the block into `~/.ssh/environment`, which sshd reads
under `PermitUserEnvironment=yes`, so **every** session sees the vars — interactive and
`ssh <name> cmd` alike. The same set also goes into the guest's **PID 1 environment**, so the whole
process tree inherits it (`cs-sandbox exec`, services, the agent): Podman passes the file to
`podman run --env-file` — never as `-e KEY=VALUE`, whose values are world-readable in
`/proc/<pid>/cmdline` — and Firecracker's `fc-init` exports it. `.env` is never auto-loaded; it is
passed explicitly, as with `docker run`.

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

`U` and `G` are **per group**, generated on the first `cs-sandbox create` in that group, into its
key directory (`~/.local/share/cs-sandbox/keys/groups/<group>/`, mode 600) — so a member of one
group holds no credential any other group's sandbox would accept. Each sandbox's `authorized_keys`
is **always generated by `cs-sandbox`** (a copied host `authorized_keys` is never inherited):

- **user** sandbox authorizes `H + U`, and holds only the private key `U` (no host keys are copied
  in; if it needs your own keys, you lend a specific set with `ssh -A`).
- **agent** sandbox authorizes `H + U + G`, and holds only `G`.

The single rule that blocks "agent → user": `G` is never written into a user sandbox's
`authorized_keys`, and agent sandboxes never receive `H` or `U`.

### Solo sandboxes (`--solo`)

By default every agent sits in the mesh above — any agent can SSH into any other, on the shared `G`
key. `create <name> --solo` takes one agent out of it, in one direction only. It is agent-only,
rejected for `--type user`, which intentionally carries `H` and reaches peers:

- **Outbound (blocked).** A solo agent is seeded **no** tier private key — no `U`, no `G` — so it
  holds nothing any sandbox or the host authorizes and cannot authenticate outward at all. Its ssh
  config pins no `IdentityFile` and keeps `PreferredAuthentications publickey`, so the attempt
  fails fast rather than hanging.
- **Inbound (allowed).** Its `authorized_keys` is normal (`H + U + G`), so the host, user sandboxes
  and other agents still SSH *in* and drive it.

This is a credential boundary, not a network one: a solo agent still `ping`s and `curl`s peers, the
host, the LAN and the internet, and peers still reach its services. What it loses is any
*authenticated SSH foothold* — which is the mitigation for the shared-`G` property under
[Limitations](#limitations).

The seed builder withholds the tier key and leaves `authorized_keys` untouched. The state is
recorded as `"solo":true` in `instances/<group>/<name>/state.json` and a `cs-sandbox.solo` Podman
label, and shown in the `SOLO` column of `cs-sandbox ls`.

## Groups

Groups are **opt-in**: without `--group` every sandbox joins one called `default`, and the rest of
this document describes exactly that case. Reach for one when two unrelated efforts share a host and
must not interfere — two experiments comparing approaches, each needing its own copy of the same
fixture.

```bash
cs-sandbox create api --group cache-redis   # creates the group if it does not exist
cs-sandbox exec api.cache-redis ls          # identity is (group, name)
cs-sandbox group ls                         # also: group create, group rm [-f]
```

Identity is `(group, name)`, so the same name may exist in several groups and the canonical
reference is `<name>.<group>`. **A bare name always means the default group** — never "whichever
group happens to hold it", which would make a reference's meaning depend on the rest of the host:
`ssh api` would work until an unrelated experiment created its own `api`, and then either break or,
worse, keep working while denoting a different sandbox. A miss names the qualified references that
do exist; `ls -q` and `ls --json` emit the qualified form for the same reason. `CS_SANDBOX_GROUP`
sets the default for new sandboxes.

### What a group owns

| Artifact | Name | Purpose |
|---|---|---|
| Podman network | `cs-sandbox-<group>` | the isolation boundary (`isolate=true`) |
| SSH keys | `keys/groups/<group>/` | trust material, valid only inside the group |
| Gateway | `cs-sandbox-<group>-keepalive` | pins the bridge; published as the group's ssh jump host |
| Fabric dir | `net/<group>/` | per-group dnsmasq state and VM name records |
| Tap prefix | recorded in `group.json` | allocated, not hashed: interface names are host-global, and a collision would surface far from its cause |

The default group keeps the historical spelling of the first two — network `cs-sandbox-net` (so its
gateway is `cs-sandbox-net-keepalive`) and fabric dir `net/` — so a fabric that predates groups is
undisturbed. Otherwise it is an ordinary group, with no special case in the implementation.

Podman object names carry the group because they are host-global; the guest hostname and DNS alias
stay bare, so members keep reaching each other as plain `<name>`. A `--repo` branch carries it too
(see [Branches and groups](repo-sharing.md#branches-and-groups)). `group rm` refuses while members
exist (`-f` destroys them first) and reclaims the network, gateway, keys and host-route leg; the
default group's network is shared host-wide and never reclaimed.

### Two layers of isolation

**The network.** Every group network is created `--opt isolate=true`, the default group's included —
separate bridges are not enough on their own, because netavark otherwise forwards between bridges in
the same rootless namespace. Isolation is not one-sided (measured: `isolate=true` on one network
does not block a *non-isolated* peer), so a default network lacking it is recreated on first use,
but only when nothing except its own keepalive is attached. A named group's network is never
recreated, and no pre-existing network is adopted unless it inspects as ours and isolated — an
unlabelled network of that name may be the user's own.

**The keys.** Each group has its own `U` and `G`. This is what makes the boundary robust rather than
merely correct: if `isolate` ever regressed, a shared key would turn a reachability bug into a
breach. Measured on Podman 5.8.2 / netavark, two groups on one host:

| | Result |
|---|---|
| Cross-group by raw IP | blocked |
| Outbound internet from a member | works |
| Group A's key against a group B sandbox | `Permission denied (publickey)` |
| Group B's own key against a group B sandbox | succeeds |

### Reaching a group from the host

Host access does not use the sandbox network at all: each member's sshd is published on
`127.0.0.1:<port>` and `sync-ssh-config` gives it a `Host <name>.<group>` block pinned to that
group's user key (plus the bare alias, for default-group members only). This plane keeps working
when a group's fabric is broken, which is exactly when you need it.

**The gateway** is the second route, and the one that gives you names. Each group publishes one port
— drawn from 2400-2499, so it cannot collide with a member's — fronting its keepalive container,
which doubles as the group's ssh jump host:

```bash
ssh cache-redis-gw                    # a shell inside the group
ssh -L 8080:api:8000 cache-redis-gw   # reach a member's service BY NAME
```

Inside the group names resolve over the group's own DNS, so one published port reaches every member
on any port they bind. The gateway runs with `--dns <prefix>.53`, the fabric dnsmasq: aardvark knows
container names but not microVM ones, so a gateway on the default resolver would be nameless for
half its members, and `group create` replaces one that cannot resolve them. It authorizes only its
group's key, and its ssh config block offers only that key — otherwise sshd's `MaxAuthTries` would
be spent before the right one was tried. `ssh -J` to a member's *alias* does not work: that alias is
a host loopback port, which means nothing inside the group.

## Networking and name resolution

Sandboxes run on a rootless bridge network, created on demand - **not** host networking. A bridge
keeps each sandbox's network stack isolated, gives DNS for free (so name-based reach is automatic),
forwards cleanly on macOS, and keeps sandbox services off the host's loopback by default.

Each group owns one such network (`cs-sandbox-net` for `default`, `cs-sandbox-<group>` otherwise),
created `--opt isolate=true` so traffic cannot cross between them. Without `--group` every sandbox
lands on the default network, and the rest of this section describes that single fabric.

**Reach by name, between sandboxes.** Podman's aardvark-dns resolves container names on the
network, and the microVM engine adds a small forwarding dnsmasq for VM names - so any sandbox
reaches any other **in its group** as `ssh <name>` (internal port 22), container↔VM included.
Names are bare inside a group, so a group's members are unaffected by what other groups call theirs.

### Reaching a sandbox from the host

Every sandbox's sshd listens on **22** internally, and the host publishes it on
`127.0.0.1:<PORT>`. Containers draw that port from 2200-2299 and microVMs from 2300-2399; a group's
gateway draws from 2400-2499, so an ingress can never collide with a sandbox. "Free" means both
unrecorded *and* unanswered — allocation probes loopback, because a port may be held by a sandbox
under a different `CS_SANDBOX_INSTANCES_DIR`, or by an unrelated program.

`cs-sandbox` then maintains a fragment under `~/.ssh/config.d` (included from `~/.ssh/config` by one
globbed `Include`) with a `Host` block per sandbox, keyed on the canonical `<name>.<group>` plus the
bare `<name>` for default-group members, so `ssh <name>` works. The fragment is per instances root,
so two sandbox sets sharing one `~/.ssh` cannot overwrite each other. Each block:

- points at `127.0.0.1:<PORT>` and emits an `IdentityFile` line for **every** authorized host
  key plus `IdentitiesOnly yes` - otherwise ssh only tries the default key names and a sandbox
  whose key is non-standard-named (e.g. `id_ed25519_work`) fails with "Permission denied" even
  though its key *is* authorized;
- uses `HostKeyAlias <name>.<group>` against a dedicated `~/.ssh/known_hosts.cs-sandbox`, so the
  known-hosts entry is keyed by identity rather than `127.0.0.1:<port>` - otherwise recycling a freed
  port for a different sandbox would trip "host key changed". Each sandbox's `ssh_host_*` keys
  are generated once at create time and persisted, so its identity is stable across restarts.

`cs-sandbox sync-ssh-config` regenerates all of it, for both engines. `cs-sandbox ls` is the
inventory — `GROUP` first, then `NAME`, `STATUS` (`running`/`stopped`, or `unknown` when podman
itself cannot be reached) and `AGE`. A fourth status, **`removed`**, is data `rm` kept after its
sandbox was gone: a home volume or `rootfs.ext4` with no state record beside it, listed so it cannot
sit on disk unnoticed and accepted by `destroy <name> -f` so it can be reclaimed. There is no port
column, because you reach a sandbox by name; `cs-sandbox port <name>` prints it for tools that need
one, and `ls -q` / `ls --json` emit qualified refs for scripting.

**From another machine**, use `ssh -J <host> <name>` (ProxyJump, nothing exposed), or bind the
published ports on the network (`export CS_SANDBOX_SSH_BIND=0.0.0.0` before `create` - mind your
firewall).

### Port forwarding (host → sandbox service)

Tunnel a host port to a port inside a sandbox over SSH — the no-sudo, every-platform way to reach a
service a sandbox binds. The alternative,
[`host-route`](#optional-reach-sandboxes-directly-by-name-host-route), makes *every* port a sandbox
binds reachable by name, but is Linux-only and needs sudo once to set up.

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

Two things catch people out. The host service must listen on a **non-loopback** address:
`169.254.1.2` is not a mapping onto the host's loopback, so a guest reaching it arrives on the
host's ordinary side and a server bound to `127.0.0.1` refuses the connection where `0.0.0.0`
answers. It reads as a network fault because the address answers ICMP either way.

And the pinned mapping is IPv4-only while the host's resolver still hands back **AAAA** records for
that name. `/etc/hosts` only wins per address family, so the unreachable IPv6 answer would survive —
and `getaddrinfo` prefers IPv6, so naive clients would try it first and hang. The guest init
therefore writes `/etc/gai.conf` (`precedence ::ffff:0:0/96 100`) to prefer IPv4, the standard fix
for a v4-only host.

### Optional: reach sandboxes directly by name (`host-route`)

By default the host reaches a sandbox only over SSH or an explicit `forward`; it can't `ping
<name>` or `curl <name>:PORT` the way a *peer* sandbox can, because the fabric lives in podman's
rootless network namespace and a sandbox's address isn't in the host's root netns.

`cs-sandbox host-route up` opts in, under a **one-time `sudo`**: it wires the host onto the sandbox
subnet with a veth into the rootless netns, and points **systemd-resolved** at the fabric's own DNS
for the **`.cs.sandbox`** domain. After that `ping <name>.cs.sandbox`, `curl
http://<name>.cs.sandbox:8000` and any other protocol work, on both engines. Names are published
rootlessly, so create/destroy need no further sudo and `/etc/hosts` is never touched. It is **off by
default, Linux-only, needs systemd-resolved**, and is the **only** feature that uses `sudo` — for
`up`/`down` alone, never in the create/exec path.

Groups are separate bridges, so a veth reaches exactly one of them: host-route wires **one leg per
group**, names them the usual way (`api.cs.sandbox` for the default group,
`api.cache-redis.cs.sandbox` elsewhere), and keeps each group's names in its own resolver scope. A
group created after `up` needs one `host-route refresh`, since wiring a leg is the part that needs
root. Forwarding is disabled on every leg, so the host cannot become a router *between* groups, and
`status` reports **DEGRADED** rather than `UP` if that ever drifts. Mechanism and rationale — the
veth, the resolver scopes, the forwarding knob, and why a name suffix is required — are in
[`firecracker.md`](firecracker.md#optional-reach-sandboxes-directly-from-the-host-host-route).

## Directory sharing

A host directory comes into a sandbox only when you ask - there is **no implicit `$PWD`
mount** on either engine. Two modes, each landing the directory at `~/<name>` (default name =
basename; `:NAME` to override), each repeatable; a name clash is a hard error:

| flag | what you get | writable in guest? | engines |
|---|---|:---:|---|
| `--repo PATH[@REF][:NAME]` | per-sandbox **git checkout** on branch `cs-sandbox/<name>` (objects borrowed read-only) | yes (own branch) | both |
| `--snapshot PATH[:NAME]` | **read-only frozen copy** of any directory | no | both |

`--repo` is the engine-portable, git-aware mode (retrieve the sandbox's commits with `cs-sandbox
fetch`, send host commits in with `cs-sandbox push`); it requires a git repo. Outside the default
group the branch carries the group too — `cs-sandbox/<name>.<group>` — because the host source repo
two groups fetch into sits outside both (see [Branches and
groups](repo-sharing.md#branches-and-groups)). `--snapshot` takes any directory. Both work the same
on either engine: a sandbox does its work, then you fetch the results back to the host.
Full design in [`repo-sharing.md`](repo-sharing.md).

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
delivered as a read-only ext4 disk built from the volume — the same content-addressed, cached
mechanism as the base rootfs (see [`firecracker.md`](firecracker.md#disks)). A read-only shared base
with a per-sandbox writable primary is the supported way to share: independent engines writing one
store risk lock contention and corruption.

## Bundled agent tools and login

Every sandbox ships the `cs-claude`, `cs-codex`, and `cs-opencode` toolsets, so a coding agent comes
up configured and working rather than asking setup questions. Everything non-secret is baked into
the image skeleton from `image/rootfs/home/` in this repo; everything secret is carried per sandbox
through the seed.

**Baked in (non-secret):**

- **Launch wrappers** in `~/.local/bin`, each on a dedicated profile so a sandbox's config never
  touches a personal `~/.claude` / `~/.codex` / `~/.config/opencode`, and each pre-answering the "do
  you trust this folder?" gate (opencode has none):

  | Wrapper | Profile | Defaults it launches with |
  |---|---|---|
  | `cs-claude` | `CLAUDE_CONFIG_DIR=~/.cs-claude` | `--permission-mode auto` |
  | `cs-codex` | `CODEX_HOME=~/.cs-codex` | `approval_policy=on-request`, `sandbox_mode=workspace-write` |
  | `cs-opencode` | `OPENCODE_CONFIG_DIR=~/.cs-opencode` | pinned model + blanket-allow permissions, profile-scoped session db (`OPENCODE_DB`), inline auth (`OPENCODE_AUTH_CONTENT`) |

  The OpenCode adapter's internals have their own reference, [opencode.md](opencode.md).
- **Remote agent tools**, also in `~/.local/bin`: `cs-claude-remote`, `cs-codex-remote`, and
  `cs-opencode-remote`, each with `-status`/`-output`/`-sessions`/`-forget` and a `-turn` driver.
  They start or resume an agent session on another host over SSH, keeping it warm in tmux, so an
  agent in one sandbox can hand a task to an agent in another. The target host defaults to the
  sandbox itself, so reaching anywhere else needs SSH access the sandbox actually has — for a user
  sandbox, typically keys you forwarded with `ssh -A`.
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
  granting only the caps nested Podman and ordinary network tooling need
  ([`podman.md`](podman.md#nested-podman) lists them). There is no host-root path absent a kernel bug.
  `--privileged` is an opt-in fallback that trades that defense-in-depth for breadth. The microVM
  engine removes the shared-kernel attack surface entirely.
- **Passwordless sudo inside is safe**, and is the usual setup for agent sandboxes, because the
  trust boundary is the *engine*, not in-sandbox sudo: on the container engine "root" inside is your
  own unprivileged host uid through `--userns=keep-id`; on the microVM engine it is real root
  confined to the guest's kernel. Either way `sudo` grants an agent nothing it does not already
  control over its own disposable sandbox, while restricting it would add friction for no boundary.
  This holds **only** while that boundary is intact — running the image rootful, `--privileged`, or
  `--userns=host` turns the same passwordless sudo into genuine host-root.
- SSH ports bind `127.0.0.1` only; sandboxes are not exposed on the LAN by default.
- **No host private keys inside any sandbox.** Nothing at rest to leak from a sandbox's disk. Peers
  are reached with generated per-group tier keys, and an inherited agent credential lives only in the
  seed and the home volume, never in the image or git. When a sandbox does need your own keys,
  `ssh -A` lends a specific set for the life of one connection and they stay on the host — but
  anything running as you there can *use* the socket meanwhile, so scope what you load
  ([README](../README.md#lending-a-sandbox-specific-ssh-keys-with-ssh--a)).
- **Type governs reach, not privilege.** An agent sandbox can never `ssh` into a user sandbox — see
  [the trust model](#sandbox-types-and-the-ssh-trust-model) for how the H/U/G keys enforce it.

## Testing

Two tiers, split by whether they touch a real engine:

- **Unit tests** (`make test`) — pure and fast, no external processes. They cover the logic where a
  silent bug would be costly: the seed trust material and agent-login inheritance, spec parsing, instance
  state, the kernel rebuild/pin decision, port allocation, and the CLI itself, driven through the real
  cobra tree with a fake `Runner`.
- **Integration tests** (`make test-integration`, behind a `//go:build integration` tag) — live tests
  on a Linux/KVM host with podman. They create namespaced sandboxes in temp state dirs, tear them
  down, and **skip gracefully** when podman or the image is unavailable. The suite runs with `-p 1`,
  because packages share one rootless network namespace and host SSH port pool.

The **smoke profile** (`make test-smoke`, the `SMOKE_TESTS` list in the Makefile) is not a third
tier: it is the subset of the integration tier that CI runs on every host — Linux, macOS and
Windows/WSL2 — against a slimmed image (`make build-ci-image`). Keep it short; see
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Limitations

- **No per-agent isolation _by default_.** Agent sandboxes in a group share that group's agent-tier
  SSH key (the `G` key from the [trust model](#sandbox-types-and-the-ssh-trust-model) above), so any
  agent sandbox can SSH into any other **in the same group**. Agents are walled off from you and from
  user sandboxes, but not from each other. Two ways to narrow it.
  [`--solo`](#solo-sandboxes---solo) denies one sandbox any outbound SSH, leaving its network reach
  intact and keeping it reachable from peers. [`--group`](#groups) separates whole fleets, which
  removes network reach too and shares no key across the boundary.
- **Not bit-for-bit reproducible.** The image runs a package update at build time, so rebuilds can
  pick up newer upstream packages.
