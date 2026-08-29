# The cs-sandbox specification

This document specifies what a `cs-sandbox` sandbox is, what it guarantees, and how it is built. It
is the contract between the CLI, the image, the guest init and the network fabric.

**Audience.** Anyone changing `cs-sandbox`, reviewing a change to it, or reasoning about what a
sandbox can and cannot reach. For the tour, read [README.md](README.md). For the command surface,
read [MANUAL.md](MANUAL.md).

**Normative language.** **MUST** and **MUST NOT** carry their RFC 2119 meanings, and are the only
normative keywords used here. The numbered requirements (**R1**, **R2**, …) are the testable
statements, and the prose between them explains why each one is there.

**How to read it.** Start with the vocabulary, then read sections 2 to 10. Those are the
cross-engine model: everything true of a sandbox whatever backs it. Sections 11 and 12 specify each
engine in turn. Sections 13 to 17 close with the security model, the boundaries and the testing
contract.

| | | | |
|---|---|---|---|
| [1. Vocabulary](#1-vocabulary) | [6. Networking and name resolution](#6-networking-and-name-resolution) | [11. The Podman container engine](#11-the-podman-container-engine) | [16. Conformance and testing](#16-conformance-and-testing) |
| [2. The model](#2-the-model) | [7. Sharing directories in](#7-sharing-directories-in) | [12. The Firecracker microVM engine](#12-the-firecracker-microvm-engine) | [17. Open questions](#17-open-questions) |
| [3. Anatomy of a sandbox](#3-anatomy-of-a-sandbox) | [8. Injecting environment variables](#8-injecting-environment-variables) | [13. Security model](#13-security-model) |  |
| [4. Types and the SSH trust model](#4-types-and-the-ssh-trust-model) | [9. Nested Podman and image stores](#9-nested-podman-and-image-stores) | [14. Limitations](#14-limitations) |  |
| [5. Groups](#5-groups) | [10. Agent tools and login](#10-agent-tools-and-login) | [15. Non-goals](#15-non-goals) |  |

## 1. Vocabulary

| Term | Meaning |
|---|---|
| **sandbox** | One named, SSH-reachable Linux environment: a rootless Podman container or a rootless Firecracker microVM. |
| **engine** | Which of the two backs a given sandbox. Chosen per sandbox with `--engine`. |
| **type** | `agent` or `user`. It decides what a sandbox may reach over SSH, and nothing else. |
| **group** | An isolation boundary owning a network, a key pair set and a gateway. Every sandbox is in exactly one. |
| **seed** | The per-sandbox read-only directory through which the host hands trust material and config to the guest init. |
| **guest init** | The first process in a sandbox: the container entrypoint, or the microVM's `/fc-init`. |
| **fabric** | The rootless bridge network a group's sandboxes share, plus its DNS. |
| **tier key** | A generated SSH key granting a whole tier of access. `U` for user sandboxes, `G` for agent sandboxes. |

## 2. The model

**R1.** A sandbox **MUST** be reachable by name, from the host and from its peers, and **MUST NOT** require a
caller to know a port number.

**R2.** The two engines **MUST** be interchangeable. They share one image, one trust model, one network
fabric, the same sharing flags and the same agent tools.

**R3.** Host data and host credentials **MUST NOT** reach a sandbox unless a flag names them. There
is no implicit `$PWD` mount and no implicit credential.

**R4.** A sandbox **MUST** behave the same on Linux, macOS and Windows under WSL2.

**R5.** Every sandbox **MUST** support nested Podman.

The engine is a choice about isolation and weight, not about capability. A Podman container shares
the host kernel and starts fast. A Firecracker microVM boots its own kernel under hardware
virtualization, which is the stronger boundary for untrusted or autonomous work. Everything else in
this document holds either way.

## 3. Anatomy of a sandbox

### 3.1 One generic image

**R6.** The image **MUST NOT** bake in a developer identity: no user name, uid, gid or per-user home.
One build serves every developer and every machine.

**R7.** Toolchains **MUST** live under `/opt`, root-owned and shared, rather than in a per-user `$HOME`.

**R8.** Every toolchain **MUST** be pinned in the `Containerfile` and **MUST** be on `PATH` for every shell,
including a non-interactive `ssh <name> <cmd>`. The sibling `cs-` tools the image carries **MUST** be
pinned by this module's `go.mod` rather than installed at `@latest`.

Pinning those in `go.mod` keeps one source of truth: `make versions` reports what an image will
ship, and `make repin` moves the pins in a diff somebody reviews. `cs-sandbox` itself is pinned to
the revision of the binary running the build, because a module cannot name its own version. That
revision has to be published for the install to resolve. Serving it from the checkout instead is
what `cs-sandbox build --local-sandbox` is for, and it is the deliberate exception.

**R160.** The image **MUST** be named after the version of `cs-sandbox` that built it, in the
package `ghcr.io/codesweep-ai/sandbox`, and a sandbox **MUST** run the image its own binary names.

**R161.** A binary that reports no version **MUST** refuse to name or build an image, rather than
installing an unnamed `cs-sandbox` into one.

**R162.** `build` **MUST** try the registry before building, and `create` **MUST** do neither: a
missing image is an error naming `build`.

The image carries the `cs-sandbox` that built it. The version is therefore the only thing that says
what is inside, which is why the tag is the version string rather than the revision. The same commit
yields a different image once it is tagged for release, because the `cs-sandbox` in it then reports
the release version. Go marks a binary from a modified tree `+dirty`, which no tag may contain. That
binary names a `-dirty` tag instead. No `-dirty` image is ever published, so it always builds its
own. `CS_SANDBOX_IMAGE` overrides the name, which is how a test run pins one.

The two CI images (§16) are separate packages, `-slim` and `-slim-agents`. Neither is a sandbox, and
a container that boots with no toolchains would be a confusing thing to find under the sandbox
package.

Toolchains under `/opt` are read-only for the dev user, so a new language version or a global
package needs `sudo`. Per-project virtualenvs and `node_modules` are unaffected. Two cases would
otherwise chafe, and both have an escape hatch. Go keeps upstream's `GOTOOLCHAIN=auto`, so a
`go.mod` naming another `go 1.x` fetches it into `$GOPATH` without `sudo`. Neovim's Mason honours
`$MASON_ROOT` for a private writable package set.

### 3.2 The user is created at first boot

**R9.** `cs-sandbox` **MUST** pass the caller's identity and the sandbox config to the guest, as container
environment on Podman and as the seed's `cs-sandbox.conf` on Firecracker.

**R10.** The guest init **MUST** create the matching group and user with NOPASSWD sudo, seed and chown the
home, install the seed material, start sshd, and drop to that user.

This is what R6 buys. Because identity arrives at boot rather than at build, you never rebuild the
image to match your local environment.

### 3.3 The seed

**R11.** Each sandbox **MUST** have a read-only seed directory at `instances/<group>/<name>/seed`, mounted
in the guest at `/run/cs-sandbox-seed`.

**R12.** The seed **MUST** be built by one shared builder for both engines, and **MUST** carry:

- `authorized_keys` and the tier private key;
- the `ssh_config` and the stable `host_keys/`;
- the `host_hosts` map, for reaching the host by name;
- the resolved `inject-env` block;
- the host's git identity;
- the host's Claude Code theme;
- the credential of each agent named by `--inherit-agent-login`, and the fabricated one of each
  agent named by `--lend-agent-login` (§10.2).

**R13.** The guest init **MUST** split its work by a sentinel. First boot, when `~/.cs-sandbox-initialized`
is absent, seeds the skeleton home from the image and installs agent credentials. Every boot
refreshes the managed SSH material, normalizes permissions, starts sshd and signals readiness.

**R14.** The every-boot half **MUST** be idempotent, so that rotating a key takes effect on restart with no
other action.

`--repo` clones use their own guard, `~/.cs-sandbox-repos-done`, because a repo may be added to a
sandbox that has already initialized.

### 3.4 The guest SSH config

**R15.** The guest's `~/.ssh/config.d/cs-sandbox` **MUST** scope its rules to dotless names (`Host * !*.*`),
leaving dotted hosts on ssh's defaults.

**R16.** Both types **MUST** pin their tier key there as `IdentityFile`.

**R17.** An agent sandbox **MUST** set `PreferredAuthentications publickey`. A user sandbox **MUST NOT**.

**R18.** The config **MUST** set `IdentitiesOnly yes` for fabric peers only, resolved by a `Match exec` that
checks the target lands on the fabric subnet.

**R18a.** The config **MUST** bound the connect to a peer. *A peer that does not answer leaves
`connect()` in the kernel's retry window, measured at 136 seconds. Every caller that reaches a peer
sits under something that gives up sooner. The failure then arrives as a kill with no output instead
of an unreachable machine, and a campaign judged a teammate's branch it had never fetched.*

R17 exists because an agent offering an unaccepted key would otherwise fall through to a password
prompt and hang on a TTY nobody is watching. A user sandbox omits it because it may legitimately
password-authenticate to a dotless machine on the LAN.

R18 is subtler. Peers also authorize `H`, and OpenSSH offers forwarded-agent keys before a
file-only `IdentityFile`. Without `IdentitiesOnly`, `ssh -A` into a user sandbox would silently
change which key that sandbox presents to its peers. Scoping the setting to the fabric means a user
sandbox always uses `U` for peers, with or without `-A`, while `ssh -A` still reaches external hosts
with your forwarded keys.

## 4. Types and the SSH trust model

**R19.** Access **MUST** be governed by type, independently of engine, according to this matrix:

| client ↓ \ server → | user | agent |
|---|:---:|:---:|
| **host** | ✓ | ✓ |
| **user** sandbox | ✓ | ✓ |
| **agent** sandbox | ✗ | ✓ |

Three key identities produce it:

| Symbol | What it is | Where it lives | What it grants |
|---|---|---|---|
| **H** | the host user's existing public keys | authorized in every sandbox; the private keys never leave the host | host to sandboxes |
| **U** | a generated user-tier key | user sandboxes only | user sandbox to any sandbox |
| **G** | a generated agent-tier key | agent sandboxes only | agent sandbox to agent sandboxes |

**R20.** `U` and `G` **MUST** be generated per group, on the first `create` in that group, into
`keys/groups/<group>/` at mode 600.

**R21.** Each sandbox's `authorized_keys` **MUST** be generated by `cs-sandbox`. A host `authorized_keys`
**MUST NOT** be inherited.

**R22.** A user sandbox **MUST** authorize `H + U` and **MUST** hold only `U`.

**R23.** An agent sandbox **MUST** authorize `H + U + G` and **MUST** hold only `G`.

**R24.** A host private key **MUST NOT** be written into any sandbox.

One rule blocks the agent-to-user direction: `G` is never written into a user sandbox's
`authorized_keys`, and an agent sandbox never receives `H` or `U`. That is what stops an agent
reaching a forwarded `ssh -A` socket in your workspace.

### 4.1 Solo sandboxes

**R25.** `--solo` **MUST** be rejected for `--type user`.

**R26.** A solo sandbox **MUST NOT** be seeded a tier private key, and its ssh config **MUST NOT**
pin an `IdentityFile`.

**R27.** A solo sandbox's `authorized_keys` **MUST** be the ordinary agent set, so the host, user sandboxes
and other agents can still reach it.

**R28.** Solo state **MUST** be recorded in the sandbox's state record and **MUST** be shown by `ls`.

This is a credential boundary rather than a network one. A solo agent still reaches peers, the
host, the LAN and the internet with `ping` and `curl`, and peers still reach its services. What it
loses is any authenticated SSH foothold, which is the mitigation for the shared-`G` property in §12.

## 5. Groups

Groups are opt-in. Without `--group` every sandbox joins one called `default`, and every other
section of this document describes exactly that case.

**R29.** Identity **MUST** be the pair (group, name), with `<name>.<group>` as the canonical reference.

**R30.** A bare name **MUST** mean the `default` group, and **MUST NOT** resolve to whichever group happens to
hold it.

**R31.** A reference that matches nothing **MUST** name the qualified references that do exist.

**R32.** `ls -q` and `ls --json` **MUST** emit qualified refs.

R30 is the load-bearing one. Resolving a bare name by search would make a reference's meaning depend
on the rest of the host. `ssh api` would work until an unrelated experiment created its own `api`,
and would then either break or, worse, keep working while denoting a different sandbox.

### 5.1 What a group owns

| Artifact | Name | Purpose |
|---|---|---|
| Podman network | `cs-sandbox-<group>` | the isolation boundary, created `isolate=true` |
| SSH keys | `keys/groups/<group>/` | trust material valid only inside the group |
| Gateway | `cs-sandbox-<group>-keepalive` | pins the bridge; published as the group's ssh jump host |
| Fabric dir | `net/<group>/` | per-group dnsmasq state and VM name records |
| Tap prefix | recorded in `group.json` | allocated rather than hashed |

**R33.** The default group's network and fabric directory **MUST** be named `cs-sandbox-net` and `net/`,
without the group suffix every other group carries. The default group **MUST** otherwise be an ordinary
group with no special case.

**R34.** Podman object names **MUST** carry the group, because they are host-global. The guest hostname and
DNS alias **MUST** stay bare, so members reach each other as plain `<name>`.

**R35.** `group rm` **MUST** refuse while members exist, unless `-f` is given, in which case it **MUST** destroy
them first.

The tap prefix is allocated rather than derived from a hash because interface names are host-global.
A hash collision would surface as a networking fault far from its cause.

### 5.2 Two layers of isolation

**R36.** Every group network **MUST** be created with `isolate=true`, the default group's included.

**R37.** Each group **MUST** have its own `U` and `G`, so a member of one group holds no credential any
other group's sandbox would accept.

**R38.** A pre-existing network **MUST NOT** be adopted unless it inspects as ours and isolated.

Separate bridges are not enough on their own, because netavark otherwise forwards between bridges in
the same rootless namespace. Isolation is also not one-sided: `isolate=true` on one network does not
block a non-isolated peer. So a default network lacking the option is recreated on first use, but
only when nothing except its own keepalive is attached. A named group's network is never recreated.

R37 is what makes the boundary robust rather than merely correct. The two layers fail
independently. A member of one group has no route to another group's subnet, and it holds no key
any other group's sshd would accept. A regression in either layer therefore leaves the other
standing. Isolation applies between bridges rather than to the upstream route, so a member still
reaches the internet.

## 6. Networking and name resolution

**R39.** Sandboxes **MUST** run on a rootless bridge network created on demand, and **MUST NOT** use host
networking.

**R40.** Any sandbox **MUST** reach any other in its group by bare name over SSH on port 22.

A bridge keeps each sandbox's network stack isolated, gives DNS for free, forwards cleanly on
macOS, and keeps sandbox services off the host's loopback by default. Podman's aardvark-dns resolves
container names, and the microVM engine adds a small forwarding dnsmasq for VM names. A container
and a microVM therefore reach each other exactly as two containers would.

### 6.1 Reaching a sandbox from the host

**R41.** Every sandbox's sshd **MUST** listen on 22 internally, and the host **MUST** publish it on
`127.0.0.1:<PORT>`.

**R42.** Ports **MUST** be drawn from distinct ranges, so an ingress cannot collide with a sandbox.
Containers take 2200 to 2299, microVMs 2300 to 2399, and group gateways 2400 to 2499. The credential
lender (§10.2) takes 2500, above them all.

**R43.** A port **MUST** be treated as free only when it is both unrecorded and unanswered. Allocation **MUST**
probe loopback.

**R44.** `cs-sandbox` **MUST** maintain a fragment under `~/.ssh/config.d`, included from `~/.ssh/config`,
with a `Host` block per sandbox keyed on `<name>.<group>` plus the bare `<name>` for default-group
members.

**R45.** Each block **MUST** emit an `IdentityFile` line for every authorized host key, plus
`IdentitiesOnly yes`.

**R46.** Each block **MUST** use `HostKeyAlias <name>.<group>` against a dedicated
`~/.ssh/known_hosts.cs-sandbox`.

**R47.** Each sandbox's host keys **MUST** be generated once at create time and persisted, so its identity
is stable across restarts.

**R48.** The fragment **MUST** be per instances root, so two sandbox sets sharing one `~/.ssh` cannot
overwrite each other.

R43 matters because a port may be held by a sandbox under a different `CS_SANDBOX_INSTANCES_DIR`, or
by an unrelated program. R45 exists because ssh otherwise tries only the default key names, so a
sandbox authorized under a key named `id_ed25519_work` would be refused despite that key being
authorized. R46 keys the known-hosts entry by identity rather than by `127.0.0.1:<port>`, so
recycling a freed port for a different sandbox does not trip "host key changed".

Host access does not use the sandbox network at all, which is deliberate. This plane keeps working
when a group's fabric is broken, and that is exactly when you need it.

### 6.2 The gateway

**R49.** Each group **MUST** publish one gateway port fronting its keepalive container, which doubles as the
group's ssh jump host.

**R50.** The gateway **MUST** run against the fabric's own DNS, and `group create` **MUST** replace one that
cannot resolve its members.

**R51.** The gateway **MUST** authorize only its own group's key, and its ssh config block **MUST** offer only
that key.

Inside a group, names resolve over the group's own DNS, so one published port reaches every member
on any port they bind:

```bash
ssh cache-redis-gw                    # a shell inside the group
ssh -L 8080:api:8000 cache-redis-gw   # reach a member's service by name
```

R50 exists because aardvark knows container names but not microVM ones, so a gateway on the default
resolver would be nameless for half its members. R51 exists because sshd's `MaxAuthTries` would
otherwise be spent before the right key was tried.

`ssh -J` to a member's bare alias does not work. That alias is a host loopback port, which means
nothing inside the group.

### 6.3 Reaching the host from inside a sandbox

**R52.** `cs-sandbox` **MUST** map the host's own names to the pasta host address `169.254.1.2` in the
seed's `host_hosts`, and the guest init **MUST** append them to `/etc/hosts`. Where the engine
published an address for the host itself, the guest init **MUST** use that one instead.

**R52a.** A sandbox **MUST** reach a service on the host by the name `host.containers.internal`.
Anything this tool points a sandbox at on the host **MUST** use that name rather than an address.
The seed **MUST** pin the name on an engine whose guest is not given it.

**R53.** The guest init **MUST** write `/etc/gai.conf` with `precedence ::ffff:0:0/96 100`.

The host's LAN or Tailscale name is not routable from inside a sandbox, because it hairpins through
the rootless NAT. The pasta address is the same one Podman exposes as `host.containers.internal`,
and Firecracker taps reach it too, because they share the one rootless network namespace. NSS
checks `files` before DNS, so the pinned mapping beats the unroutable name.

R52 and R52a are there because `169.254.1.2` is only the host on a Linux box running podman itself
with pasta. An older podman maps the host at slirp4netns' own address, and under a podman machine the
literal is the VM rather than the Mac the tool runs on. Podman publishes the name with the right
address in all three, so asking by name is asking podman where its host is. The microVM engine has
no podman inside the guest to publish anything, so the seed pins the name there, which holds because
that engine is Linux and KVM only.

Two things catch people out, and R52 and R53 address them. The host service must listen on a
non-loopback address, because `169.254.1.2` is not a mapping onto the host's loopback. A guest
reaching it arrives on the host's ordinary side, where a server bound to `127.0.0.1` refuses the
connection. It reads as a network fault because the address answers ICMP either way. Separately, the
pinned mapping is IPv4-only while the host's resolver still returns AAAA records for that name.
`/etc/hosts` wins only per address family, and `getaddrinfo` prefers IPv6, so without R53 a naive
client would try the unreachable address first and hang.

### 6.4 Port forwarding and host-route

**R54.** `forward` **MUST** tunnel over SSH, and **MUST NOT** require elevated privilege on any platform. A
local forward **MUST** carry one host port to one sandbox port. A dynamic forward **MUST** publish a SOCKS5
proxy on a host port. Either **MUST** bind `127.0.0.1`, and any other bind **MUST** be opt-in.

**R55.** `host-route` **MUST** be optional, Linux-only, and **MUST** be the only feature that uses `sudo`. It
**MUST** use it for `up`, `down` and `refresh` alone, never in the create or exec path.

**R56.** `host-route` **MUST** wire one leg per group, and **MUST** keep each group's names in its own resolver
scope.

**R57.** Forwarding **MUST** be disabled on every leg, and `status` **MUST** lead with `DEGRADED`, and name
the fix, if that drifts.

A local forward names its destination, so it reaches the one sandbox port you asked for. A dynamic
forward resolves and connects from inside the sandbox, so it reaches whatever the sandbox reaches.
That covers a group peer by bare name, the sandbox's own loopback, and the internet through its
NAT. Both are tracked the same way, so `forwards` lists either and `unforward` tears either down.

`--bind` is R54's opt-in, and `forward` warns on stderr when its value is not loopback. What it
costs is not what R142's costs. A published SSH port is still key-gated, while a forwarded port
carries whatever authentication the sandbox service behind it has, which is often none. A dynamic
forward is the sharpest case, because bound beyond loopback it is an open proxy onto the group's
fabric.

By default the host cannot `ping <name>` the way a peer sandbox can, because the fabric lives in
Podman's rootless network namespace. `host-route up` opts in: it wires the host onto the sandbox
subnet with a veth into that namespace, and points systemd-resolved at the fabric's DNS for the
`.cs.sandbox` domain. Names are published rootlessly, so create and destroy need no further `sudo`
and `/etc/hosts` is never touched.

R56 follows from groups being separate bridges, so one veth reaches exactly one of them. A group
created after `up` needs one `host-route refresh`, because wiring a leg is the part that needs root.
R57 stops the host becoming a router between groups, which would undo §5.2.

## 7. Sharing directories in

**R58.** A host directory **MUST** enter a sandbox only through an explicit flag. There **MUST** be no implicit
mount.

**R59.** Both modes **MUST** land the directory at `~/<name>`, defaulting the name to the basename of the
resolved path, overridable with `:NAME`. Both **MUST** be repeatable, and a name clash **MUST** be an error.

| Flag | What you get | Writable | Requires |
|---|---|:---:|---|
| `--repo PATH[@REF][:NAME]` | a per-sandbox git checkout on its own branch, borrowing the source's objects read-only | yes | a git repo |
| `--snapshot PATH[:NAME]` | a frozen read-only copy | no | any directory |

**R60.** On macOS every shared path **MUST** resolve under a podman-machine-shared root, by default under
`$HOME`, and `create` **MUST** reject anything else with a remedy.

### 7.1 The `--repo` checkout

**R61.** The checkout **MUST** be a `git clone --shared` off a read-only copy of the source, borrowing
existing history through `objects/info/alternates`.

**R62.** The clone **MUST NOT** write to the source.

**R63.** The checkout **MUST** live on branch `cs-sandbox/<name>`, or `cs-sandbox/<name>.<group>` outside
the default group.

**R64.** `@REF` **MUST** set the base commit, defaulting to the source's `HEAD`, and **MUST** fall back to
`HEAD` when it does not resolve.

**R65.** The clone **MUST** be configured `receive.denyCurrentBranch=updateInstead`, so a host `push` can
update its working tree.

**R66.** Cloning **MUST** happen on first boot only, and re-runs on later boots **MUST** be no-ops.

The source is read-only, so the mechanism has to write nothing back into it. That rules out a `git
worktree`, which records itself in the source's `.git`. Alternates are git's way to borrow objects
read-only and keep every new object local. The clone copies no history, so it is kilobytes.

**R67.** Each clone's local `user.name` and `user.email` **MUST** be set to whatever identity that repo uses
on the host, resolving a local override, an `includeIf`, or the global.

**R68.** The sandbox's global `~/.gitconfig` **MUST** be seeded from the host's global one, and **MUST** be set
only if unset, so a later in-sandbox change is never clobbered.

Delivering the objects is the one engine-specific part, because alternates stores an absolute path
that must be identical on every boot of that sandbox. Podman bind-mounts the host repo read-only at
a stable path, reading the host's live objects with no copy. Firecracker attaches a read-only ext4
disk holding a bare clone, which is one point-in-time copy, content-addressed and cached so that
microVMs off the same commit share one build.

### 7.2 Moving commits

**R69.** `fetch` and `push` **MUST** run from the host over the host-to-sandbox SSH alias, so they work for
an agent sandbox that cannot SSH back.

**R70.** Both **MUST** be fast-forward only, and **MUST** reject a diverged branch with a hint rather than
rewriting.

**R71.** Both **MUST** read the host source repo and the branch from the sandbox's state record, rather than
recomputing the branch name.

**R72.** `push` **MUST** be rejected unless the sandbox's tree is clean.

R71 is why `inspect --json` exposes the record. The naming rule in R63 is stable, but a caller that
reimplements it agrees only until it changes, and then computes a plausible wrong answer. That is
harder to notice than not being able to ask.

### 7.3 Branches and groups

The host source repository is not inside any group. It is the one place two groups meet, which is
why R63 puts the group in the branch name everywhere except the default group.

| Sandbox | Branch |
|---|---|
| `api` (default group) | `cs-sandbox/api` |
| `api.cache-redis` | `cs-sandbox/api.cache-redis` |

Without this, the case groups exist for breaks on the way home. Two copies of the same fixture both
target `refs/heads/cs-sandbox/api`, and the second `fetch` is rejected as a non-fast-forward.

The group is appended rather than nested, because nesting puts a directory where a ref may already
be. A default-group `api` owns `refs/heads/cs-sandbox/api`, so a group named `api` could not create
`refs/heads/cs-sandbox/api/<member>`: git rejects it with `cannot lock ref`. Appended, the two are
siblings, and the branch is spelled exactly like the reference you would pass to any other command.

### 7.4 Peer to peer

A user sandbox can fetch or push a peer agent sandbox's branch directly, with plain git and no host
round-trip. It works because a user sandbox reaches agent sandboxes by name, and both clones borrow
the same base objects, so only new commits transfer. It is user-to-agent only, because agents cannot
SSH back.

```bash
git fetch worker:api cs-sandbox/worker      # <agent>:<dir> is scp-style; <dir> is relative to ~
git push  worker:api HEAD:cs-sandbox/worker
```

### 7.5 Lifecycle

**R73.** `stop` and `start` **MUST** keep the sandbox and all its disks.

**R74.** `rm` **MUST** keep the data and remove only the instance, and `ls` **MUST** keep listing that data with
status `removed`.

**R75.** Recreating with the same name **MUST** reuse kept data.

**R76.** `destroy` **MUST** drop the home, and **MUST** accept a name whose sandbox `rm` already removed.

Fetch before you destroy. R76 is what makes `rm` safe to reach for, and R74 is what stops kept data
sitting on disk unnoticed.

Do not run `git gc --prune` on the source while a Podman sandbox has it borrowed, because the source
is bind-mounted live into the container. A Firecracker sandbox is immune, its disk being a copy.

## 8. Injecting environment variables

**R77.** `--env` and `--env-file` **MUST** inject into the whole sandbox, and **MUST** be resolved at create
time into one block written to the seed at mode 600.

**R78.** A bare `KEY` **MUST** pass the host's current value through. Comments and blank lines in an env
file **MUST** be ignored.

**R79.** The block **MUST** reach every SSH session, through `~/.ssh/environment` under
`PermitUserEnvironment=yes`.

**R80.** The same block **MUST** reach the guest's PID 1 environment, so the whole process tree inherits it.

**R81.** A value **MUST NOT** be passed on a command line where it would be world-readable in
`/proc/<pid>/cmdline`.

**R82.** A `.env` file **MUST NOT** be loaded automatically.

Two namespaces have to be covered because sshd runs `UsePAM=no` and so ignores `/etc/environment`.
R79 covers interactive sessions and `ssh <name> <cmd>` alike; R80 covers `exec`, services and the
agent. R81 is why Podman is handed the file rather than repeated `-e KEY=VALUE` arguments.

## 9. Nested Podman and image stores

**R83.** Nested Podman **MUST** work in every sandbox with no setup inside it.

**R84.** A shared image store **MUST** be mounted read-only, and new pulls **MUST** land in the sandbox's own
writable store.

**R85.** Image stores **MUST** work on both engines.

The inner engine is rootless on both engines, so a sandbox runs Podman the way any normal machine
does. In a microVM you are real root on your own kernel and it works; in a container it needs
a scaled-down capability set and the bootstrap specified in §11.2.

**R85a.** Every entry path **MUST** give its nested engine the same subuid/subgid ranges, so one store
serves both engines.

A store is written by a nested rootless engine and read by another one. Image uid 0 is therefore
stored as the sandbox user's own id. The ids line up in any sandbox of the same host user, so images
run with correct root and setuid ownership. R85a is what makes that true across engines. A
store records absolute ids, so a writer and a reader disagreeing about the base would disagree about
every file in it. On the microVM engine the store is delivered as a read-only ext4 disk built from the volume,
using the same content-addressed cache as the base rootfs.

**R85b.** Each engine **MUST** register a mounted store in the config its nested engine actually reads:
the sandbox user's `~/.config/containers/storage.conf`, never the system file.

A rootless engine resolves its storage config under the user's home, and does not inherit
`[storage.options]` from `/etc`. Written to the system file, the disks still mount and the stores
are then ignored in silence: `podman images` comes back empty. That silence is why each
engine's path carries its own live test.

A read-only shared base with a per-sandbox writable primary is the only supported way to share,
because independent engines writing one store risk lock contention and corruption.

## 10. Agent tools and login

**R86.** Every sandbox **MUST** ship the `cs-claude`, `cs-codex` and `cs-opencode` wrappers, each launching
its agent on a sandbox-local profile.

**R87.** A wrapper **MUST NOT** touch a personal `~/.claude`, `~/.codex` or `~/.config/opencode`.

| Wrapper | Profile | Launch defaults |
|---|---|---|
| `cs-claude` | `CLAUDE_CONFIG_DIR=~/.cs-claude` | `--permission-mode auto`, `--strict-mcp-config`; under `--yolo`, `--dangerously-skip-permissions` on a deny-free profile (R89) |
| `cs-codex` | `CODEX_HOME=~/.cs-codex` | `approval_policy=on-request`, `sandbox_mode=workspace-write` |
| `cs-opencode` | `OPENCODE_CONFIG_DIR=~/.cs-opencode` | pinned model, blanket-allow permissions, profile-scoped session database |

**R88.** Everything non-secret **MUST** be baked into the image skeleton. Everything secret **MUST** be carried
per sandbox through the seed.

**R89.** `--yolo` **MUST** write a marker the wrappers read, and the wrappers **MUST** then skip all permission
prompts. Dropping the prompts is not enough on its own. A `--yolo` instance **MUST** also get an agent
profile that carries no rule able to block a tool call. Any other instance **MUST** get the guarded
profile, including one recreated over data a previous sandbox kept.

**R90.** Each settings hub **MUST** describe all three toolsets, so an agent of one kind can drive the
others.

**R90a.** `cs-claude` **MUST** load only the MCP servers its invocation names, which is none unless the
caller passes their own.

The sandbox is the isolation boundary, which is what makes R89 safe: the thing an approval prompt
protects is a host, and there is no host here to protect. The same reasoning is why R89 reaches past
the prompts to the rules. Claude Code enforces `permissions.deny` even under
`--dangerously-skip-permissions`. The flag suppresses prompting, not rules. A deny cannot be lifted
by an allow at any later settings layer, so the guarded list would otherwise still stand in a sandbox
created to have none. A denied call is worse there than a prompt. It is a hard block, and nobody is
watching a `--yolo` sandbox, so there is no one to escalate to.

Only Claude needs the second profile. Codex carries no deny-shaped setting, and
`--dangerously-bypass-approvals-and-sandbox` overrides the approval and sandbox settings it does
carry. Every OpenCode permission is already `allow`. The two profiles differ in the deny list and
nothing else. The boot paths install whichever the marker calls for, replacing a pristine copy of the
other profile only, so rules added inside a sandbox survive.

R90a is R3 applied to a connector. An inherited Claude subscription carries the account's claude.ai
connectors with it. Gmail, Calendar and Drive would otherwise attach inside the sandbox as tools,
and offer an agent working there the mailbox of whoever created it. R90a also makes a session
reproducible. Connectors attach on their own schedule, so the tool list an agent is offered differs
between two runs of one task, and that alone stops a recorded session replaying.

### 10.1 Login

A sandbox reaches a model in one of three ways, and they differ in what it ends up holding. It can
**borrow** a credential (§10.2), which is the one to prefer: the sandbox holds a loan token and the
host holds the credential. It can be given a copy of one, which this section covers. Or it can sign
in on an account of its own.

**R91.** A sandbox **MUST NOT** have an agent login unless a flag names one, whether that flag lends
it or copies it in.

**R92.** `--inherit-agent-login` **MUST** snapshot the named agent's host credential into the seed, and
the guest **MUST** install it at mode 600 on first boot only.

**R92a.** The tree a login is read from **MUST** be overridable, and the override **MUST** move
nothing else.

**R93.** `create` **MUST** report which logins the sandbox ended up with.

R92a is `CS_SANDBOX_AGENT_HOME`, and it exists for a caller that has to supply a login it never
signed in for. A replay suite is one: its members need a credential the agent will start with, and a
cassette serves the traffic. Pointing `HOME` at a fabricated profile tree would do it too, and would
take the instance directory and every cache along with it.

R92's first-boot-only rule means a token the sandbox later refreshes is never clobbered. R93 exists
because copying a credential into a sandbox an autonomous agent drives is a decision. So it is
spelled in the command that makes it, and confirmed in the output.

`cs-sandbox agent-login <agent> <name>` is the other route. Use it to give a sandbox its own account
or rate-limit pool rather than sharing yours. It is also the only route when the host keeps the
credential in the macOS Keychain. `--inherit-agent-login` copies a file, so there is nothing to
carry, and `create` says so rather than failing quietly.

An inherited login is the same seat as your host, and the sandbox holds the credential itself.
Sandboxes sharing it share a rate-limit pool, independent OAuth refreshes can log each other out,
and anything that reads that sandbox reads a working credential. Lending (§10.2) removes all three:
the sandbox spends the seat without ever holding what pays for it. Lend a login rather than copying
one in.

The wrappers answer the agents' first-run dialogs, so an unattended turn never waits on one. For
`cs-claude` that covers the theme, the per-directory trust prompt and the custom-API-key prompt. The
theme defaults to the one your host runs, which `create` carries in. Onboarding is the exception. A
sandbox with no login has to keep the screen offering the sign-in choices, so the wrapper answers
onboarding only once a credential or a key exists.

### 10.2 Lending a credential

A sandbox does not have to hold a credential to use one. `--lend-agent-login <agent>` and
`--lend-api-key <provider>` give it a **loan token** instead. They point its agent at the **lender**,
a proxy on the host that swaps that token for the real credential on the way to the provider. What
the sandbox holds is worth nothing anywhere else, and the credential never crosses the boundary.

Two things can be lent. A **token loan** lends an agent's host login, such as the one Claude Code
signs in with. A **key loan** lends an LLM API key the host keeps in `~/.cs-keys/`.

**R144.** A lent login **MUST** be seeded as the agent's own credential file, holding fabricated
values. A lent API key **MUST** be seeded in the variable its client reads. Both **MUST** also seed the
lender's address in the variable the agent reads for a base URL.

**R144a.** A seeded credential file **MUST** be installed on every boot. An inherited one **MUST** be
installed on the first boot only.

**R144b.** A fabricated credential **MUST NOT** carry any real identity: not an account, not a
subscription, not an address of a person.

**R145.** A loan token **MUST** carry at least 128 bits of randomness, and **MUST NOT** be a credential
anywhere but on the host that minted it.

**R146.** The lender **MUST** resolve a loan token to exactly one slot. It **MUST** replace the token with
that slot's real credential, in that slot's own header shape, before forwarding.

**R147.** The upstream **MUST** be a property of the slot or of the loan. No part of a request **MUST**
select or change it: not the host, not the path, not the query, not a header.

**R147a.** A base URL the caller sets for a slot that is being lent **MUST** become that loan's
upstream, and **MUST NOT** be seeded into the sandbox. The variable **MUST** carry the lender's own
address there, and an upstream that is not an http or https address **MUST** fail before anything is
provisioned.

**R148.** A credential the lender did not mint **MUST** be refused, and **MUST NOT** be forwarded anywhere.

**R149.** A lent credential **MUST NOT** be written into the sandbox, its seed, its environment, or the
output of any command.

**R150.** The lender **MUST** read the host's credential per request, and **MUST NOT** refresh it.

**R151.** A loan **MUST** be recorded in the instance directory at mode 600. It **MUST** stop being honoured
when that directory is removed, and there **MUST** be no other revocation.

**R152.** The lender **MUST** listen on a non-loopback address, and **MUST** refuse any caller that is not
this host.

**R153.** The lender **MUST** answer `CONNECT`. It **MUST** refuse the hosts it is itself the front for and
the hosts these agents contact on their own, and **MUST** tunnel every other host. `create` **MUST** report
which hosts it refused.

**R154.** `create` **MUST** fail before anything is provisioned when a lend flag names a credential this
host cannot supply. The failure **MUST** name the file it looked for, and the command that creates one.

**R155.** `create` **MUST** report what a sandbox borrows. `ls` and `inspect` **MUST** show it, without
printing a loan token.

R144 is the decision the rest of this section rests on, and it is about durability rather than
about what works. A gateway variable would work today for both agents. The file is chosen because a
client reading its own credential file takes the code path it always takes, whatever that path
becomes in a release nobody has shipped yet. The alternative asks a vendor to keep treating two auth
modes alike, which is a promise nobody made. It is measurable that they already differ: signed in,
Codex asks its endpoint for a model list, and on the key path it does not.

The cost is that the fabrication has to satisfy the client, and R144c is how far that goes. What a
credential looks like is the provider's decision rather than this tool's. Anthropic issues an opaque
token with a vendor prefix, so a lent Claude holds one of those. Codex signs in with a pair of JWTs
and treats what it cannot decode as signed out, so a lent Codex holds forged ones. Copying either
shape into the other's file would be less faithful rather than more consistent.

Nothing fabricated survives verification anywhere, which is what R144b is for. The word `loan` sits
at the front of an opaque token, and in a claim of a forged one. Every identity in either names this
tool, and the lender puts the real account back on the way out.

R144a follows from the same place. An inherited login is refreshed in place and must never be
clobbered by a create-time snapshot. A lent one is a placeholder, so a sandbox that overwrote it
after a failed refresh would lose its loan for good, and re-installing it each boot costs nothing.

R146 is the whole mechanism, and it needs one header. Nothing signs the body, so the swap rewrites
a single value and reads nothing else. No request is parsed, a stream passes through byte for byte, and an unfamiliar request
shape still works. The shape has to be restored on this side, because an OAuth login travels with
headers an API key does not. A sandbox holding a token cannot know which it stands for.

R147 keeps this from becoming a way to steal a credential. A caller that could name its own upstream
could aim the host's real credential at a machine of its choosing. It also keeps the lender free of
any model of a provider's API. The lender joins an origin it was given to a path it did not read.
Three places may name that origin, most specific first: the loan, the lender's own `--origin`, and
the slot. A request is none of them.

R147a is the same trade the credential makes, in the same variable. `ANTHROPIC_BASE_URL` says where
this traffic goes, so a caller who sets it beside `--lend-api-key anthropic` has said where the
lender should forward. What the sandbox is handed back is the lender, exactly as an API key is read
on the host and handed back as a loan token. One shape rather than two, and a script that points an
agent somewhere works the same inside a sandbox and outside one.

The recorder or gateway it names sits BEHIND the lender, and that is the whole of the topology: one
hop is added, past the swap rather than before it. Nothing changes between the sandbox and the
lender. A sandbox reaching a recorder is configured exactly like one reaching a provider, which is
what lets a recording be added and dropped again. The upstream only has to be reachable from the
host, so it may run on another machine.

Two costs come with that, and both are the price of the hop being past the swap. The upstream is
handed the real credential, so a recorder on another machine is one the credential crosses a network
to reach. And a replay through it still needs the host to hold a credential for the lender to read,
even though nothing downstream will present it. A fabricated one serves, because a recorder replaying
a cassette reaches no provider that could refuse it.

R150 is a deliberate limit. Whatever owns a login is the thing that renews it, in the way its vendor
supports. The lender only ever reads the current value, so it holds no refresh token and implements
no vendor's sign-in. The cost is that a host login nothing has refreshed goes stale. The lender
reports that as itself, rather than as a rejection from the provider.

R151 makes the sandbox lifecycle the whole of revocation. The loan is a fact written beside the
instance. It is true from the moment `create` returns, and gone when `destroy` removes the
directory. Nothing has to be sequenced at create, and no second lifetime has to be kept in step
with the sandbox's.

R152 has one cause. A sandbox reaches the host by name (R52a), at an address that arrives on the
host's ordinary side. A server bound to `127.0.0.1` refuses that connection. Binding wider puts the port on
the network the host is on, so the caller is checked instead. A sandbox's traffic is translated to the
host's own address on the way out of the rootless namespace. A machine elsewhere cannot claim that
address without being on the path. The lender takes port 2500, above every range R42
allocates from.

R153 covers the half of an agent's traffic a base URL does not govern. These clients also reach
their provider on their own, for analytics, for news about what the agent can do, and for whatever
else a vendor adds over time. What those calls are for varies and is not this tool's to predict.
What matters is that a sandbox holding a loan token has no credential any of them would be accepted
with, so each one fails. A failure there can unsettle an agent whose model calls are working
perfectly. So the sandbox's `HTTPS_PROXY` points at the lender too.

Two kinds of host are refused, and only one of them can be derived. Every upstream the lender fronts
comes from the slot table, so a provider added there is refused on the same commit. The rest are
hosts no slot lends for, which an agent reaches anyway: Codex asks for an experiment assignment, and
OpenCode asks what models exist. Those have to be written down, and `create` prints the whole list so
that a name missing from it is visible rather than inferred. Everything else is tunnelled, because an
agent's tools share its environment. Blocking `git`, `curl` and every package manager would buy
nothing. Pass `--block-side-calls=false` to allow the direct route.

R153 keeps an agent from confusing itself, and is not a boundary. It works through a variable
clients honour. What makes a call around it pointless is R149: the sandbox holds no credential worth
spending anywhere.

### 10.3 Provider API keys

**R94.** A provider API key **MUST NOT** reach a sandbox unless a flag names it. `--inherit-api-key`
copies one in, `--lend-api-key` lends one, and `--env` passes an arbitrary value.

**R157.** The host's lendable keys **MUST** live one per file under `~/.cs-keys/<provider>`, and each file
**MUST** hold the key and nothing else.

R157 is the same move as the agent profiles and the cassette store. The directory is the
configuration. Adding a key is a shell redirect, there is nothing to keep in sync, and what a host
is willing to lend can be read with `ls`.

The two key flags name a provider rather than an agent. A key belongs to the provider that issued
it, and any agent reaching that endpoint can spend it. A login belongs to an agent, which is why the
flags either side of them name one.

**R158.** A key slot **MUST** name the variable its client reads for a base URL, and that variable
**MUST NOT** be assumed to belong to the provider.

**R159.** A key slot **MUST** name every variable its clients read the credential from.

R158 is OpenCode's doing. Its base URL belongs to the provider rather than to the client, and only
two providers have a variable of their own. A third has none, so OpenCode's own `OPENCODE_BASE_URL`
points it, aimed at whichever provider the running model names. The Fireworks slot therefore names
that variable where the others name a provider's, and a lent Fireworks key reaches OpenCode on the
model the image pins.

R159 is Codex's doing, and it is not symmetric with R158. One OpenAI key reaches two clients that
read different variables. OpenCode reads `OPENAI_API_KEY`. Codex reads `CODEX_API_KEY`, and given
only the former it attaches no authorization header at all, so the provider answers 401 while the
sandbox looks correctly configured. A key slot therefore seeds every variable its clients read, with
the same value in each.

OpenCode has no login slot, and is not getting one. It reaches an endpoint through whichever model it
runs, so a credential lent to it belongs to that provider, and the key slots already cover every one
it can reach. The subscriptions worth lending are lent through the `claude` and `codex`
slots, to the clients that own them.

For a key scoped to the agent rather than the whole sandbox, write it to `~/.cs-claude/env` inside
the sandbox, or to the equivalent for the other two. The wrappers read it at launch. An API key
overrides a subscription login, so there is rarely a reason to give a sandbox both.

## 11. The Podman container engine

**R95.** The Podman engine **MUST** be available on every host, and **MUST** be the default wherever Firecracker
is unavailable, which includes macOS and any host without x86_64 KVM.

**R96.** Podman itself **MUST** be the only prerequisite. On macOS that includes a podman machine, which
every sandbox on that host shares.

The container shares the host kernel, so it is the lighter engine and the faster to start. Isolation
rests on the container boundary rather than a separate kernel, which is why an x86_64 Linux host with
KVM defaults to the microVM instead. Autonomous agent sandboxes are what that buys most.

### 11.1 Container boot

**R97.** The container **MUST** be launched `--userns=keep-id --user 0:0`, so PID 1 runs as container root,
which keep-id maps to the caller's host uid.

**R98.** The identity and sandbox config **MUST** be passed as container environment.

**R99.** The entrypoint **MUST**, in this order:

1. create the group and user, and grant NOPASSWD sudo;
2. run the §11.2 bootstrap that nested rootless Podman needs;
3. seed and chown the home, install the seed's SSH material and agent credentials, and start sshd;
4. hand off to that user, with `runuser`, for the main process.

Because keep-id maps the created uid to your host uid, ownership stays correct on both sides.

**R100.** The home **MUST** be a named volume mounted at the user's home directory, and **MUST** persist across
stop and start. `destroy` **MUST** remove it.

**R101.** `exec` **MUST** run as the dev user in their home, passing `--user` and `--workdir` explicitly.

A named volume gives correct Linux permissions on both host operating systems, which sshd's strict
checks require, and avoids virtiofs permission problems. The trade is that you cannot `cd` into the
home from the host, so reach it with `exec`, `ssh` or `podman cp`.

R101 keeps the engines consistent. A microVM is reached over ssh, which lands as that user anyway.
So a command behaves the same whichever engine runs it, and the files it creates are owned by you
rather than by root.

### 11.2 Nested Podman

True isolated Podman-in-Podman needs a scaled-down capability set on the outer container and a
bootstrap inside it. In a microVM neither applies, but the inner engine is rootless either way.

**R102.** The container **MUST** run rootless, bounded by the caller's unprivileged host user through
`--userns=keep-id`, and **MUST** be granted only these capabilities:

| Capability | Why |
|---|---|
| `CAP_SYS_ADMIN` | the inner engine's namespaces and mounts |
| `CAP_SETFCAP` | granting `newuidmap`/`newgidmap` their file capabilities |
| `CAP_NET_RAW`, `CAP_NET_BIND_SERVICE` | ordinary network tooling, and a service on a low port |

**R103.** The default seccomp filter **MUST** stay on, and the container **MUST** see no host devices beyond
`/dev/net/tun`.

The last two capabilities are for the sandbox itself rather than for nesting. Unprivileged `ping`
comes from a `net.ipv4.ping_group_range` sysctl rather than a capability. Because the container is
rootless the capabilities are namespaced, so this is strictly safer than `--privileged`, which turns
seccomp off and unmasks everything.

**R104.** The inner Podman **MUST** run rootless, as the sandbox user, with no wrapper and no `sudo`.
Plain `podman` **MUST** be the real binary on both engines.

**R105.** The container **MUST** unmask the paths the engine masks by default, with `unmask=ALL`. A nested
user namespace cannot mount a fresh `procfs` while any of the container's `/proc` is masked, and the
inner engine needs one for every container it runs.

**R106.** An inner container's bind-mounted files **MUST** come back owned by the sandbox user, with no flag
from the caller.

R102's capability set is the whole bootstrap's mirror image. A rootless inner Podman makes its own
user namespace and holds its privileges *there*, so the outer container needs neither `CAP_NET_ADMIN`
nor `CAP_MKNOD` nor `CAP_SYS_PTRACE`. What it does need is two things a container lacks.

Rootless Podman writes each inner container's `uid_map` through `newuidmap`/`newgidmap`. The image's
copies carry no file capabilities, because none survive a rootless image build. Hence `CAP_SETFCAP`,
and a boot-time `setcap`.

It also needs subuid/subgid ranges *mapped in this user namespace*. `--userns=keep-id` lends the
container a window borrowed from the caller's host subuid range, and a stock `useradd`-allocated
range at 100000 falls outside it. So the ranges are derived from `/proc/self/uid_map`, never
assumed.

R105 is the price, and it is smaller than it looks. The container is rootless, so its root is an
unprivileged subuid. The kernel independently denies that root `/proc/kcore`, `/proc/sysrq-trigger`
and every non-namespaced sysctl. R106 falls out for free: the sandbox user *is* the inner root, so
what an inner container writes to a bind mount is already theirs.

Both halves of the bootstrap live in one script, `image/rootfs/nested-rootless`, which every entry
path runs: the container entrypoint, the microVM's guest init, and the shared-store seeder. That is
what satisfies R85a: the ranges are derived the same way and capped the same way, so a store seeded
once is readable on either engine.

Supporting that, the image carries `crun`, `slirp4netns` and `passt`, plus a `storage.conf`
defaulting to native `overlay`. That system file sets the driver and nothing per-sandbox. The
entrypoint writes the *user's* `~/.config/containers/storage.conf`, which is the config a rootless
engine reads and where R85b puts §9's shared image stores. Its `containers.conf` disables cgroups,
which silences a benign cgroup v2 warning on every nested run, at the cost of limits that could not
apply to a nested container anyway. SELinux confinement is off for the container with no relabeling,
which also avoids the macOS virtiofs relabel problem.

### 11.3 Nested container storage

**R107.** Nested container storage **MUST** be a dedicated volume mounted at the sandbox user's rootless
graphroot, on a non-overlay backing filesystem.

**R108.** The entrypoint **MUST** probe for native overlay on first boot **as the sandbox user**, **MUST** fall
back to `fuse-overlayfs` only when native overlay is unusable, and **MUST** cache that decision.

The probe runs as the user because the rootless engine is the one whose overlay mount has to work;
root's answer would not be evidence about it.

A dedicated volume keeps nested images across recreation and keeps them out of the home volume. It
also puts them on a filesystem where the kernel's native overlay works, rather than falling back to
the slower `fuse-overlayfs`. The two drivers share an on-disk format, so switching between them is
not destructive.

### 11.4 macOS

**R109.** Every capability in this document **MUST** hold on macOS, inside the podman-machine VM, with two
exceptions. Shared paths **MUST** resolve under a machine-shared root, and inner container images **MUST**
match the VM's architecture.

`host-route` is Linux-only, so it does not apply there either.

### 11.5 Private registry

**R110.** The registry a sandbox trusts **MUST** be configured at image build time, through
`CS_SANDBOX_PRIVATE_REGISTRY` and `CS_SANDBOX_PRIVATE_REGISTRY_INSECURE`.

**R111.** The setting **MUST** leave every other registry unaffected.

| Variable | Default | Meaning |
|---|---|---|
| `CS_SANDBOX_PRIVATE_REGISTRY` | none | The registry to trust, as a bare `host:port` with no scheme. |
| `CS_SANDBOX_PRIVATE_REGISTRY_INSECURE` | `0` | `1`, `true`, `yes` or `on` permits plain HTTP and skips TLS verification. Anything else requires HTTPS with a verified certificate. |

Both are read at build time, so rebuild the image after changing either:

```bash
CS_SANDBOX_PRIVATE_REGISTRY=registry.corp.example:5000 cs-sandbox build
```

Following Docker and Podman convention, the protocol is implicit in the security setting rather than
a scheme on the value. A secure entry writes only a location; an insecure one adds `insecure = true`
for that host alone.

## 12. The Firecracker microVM engine

**R112.** The Firecracker engine **MUST** require x86_64 Linux with KVM, and **MUST** be the default there.

**R113.** The engine **MUST** run rootless. Running a sandbox **MUST NOT** need host `sudo`.

**R114.** `cs-sandbox` **MUST** preflight-check every host package it shells out to, and **MUST** fail with an
actionable install line.

A separate guest kernel per sandbox removes the container engine's main residual weakness, which is
host-kernel attack surface, and that matters most for the autonomous agent type.

A bonus falls out of that. Inside a VM you are genuinely root on your own kernel, so §11.2's
capability set and `/proc` unmasking are unnecessary. Only the file capabilities and the shared subuid
ranges are still wanted, and the guest init runs the same `nested-rootless` script for them. Inner
images live under the user's own home on both engines.

The cost is that with no `virtio-fs`, the root filesystem and every shared directory reach the guest
as block devices. They are ext4 disks, built on the host and attached to the VM.

### 12.1 The Firecracker binary

**R115.** The VMM **MUST NOT** be bundled in the `cs-sandbox` binary. `build` **MUST** download it into the
artifact cache.

**R116.** The release **MUST** be pinned, never "latest", and the cached binary **MUST** record which release it
came from.

**R117.** A pinned release **MUST** be verified against a SHA256 committed in this repository.

**R118.** An overridden version pin, which has no committed digest, **MUST** fall back to the published
checksum and **MUST** say so.

Embedding the VMM would mean carrying a per-architecture blob in git and redistributing a
third-party binary under our own signatures. Downloading it costs nothing extra, because that build
already needs the network and Podman for the kernel and the base rootfs.

R117 is the one that matters. Checking against the `.sha256.txt` upstream serves next to the tarball
proves nothing about an artifact from the same origin. Bumping the version pin means bumping the
committed digests in the same commit.

### 12.2 The guest kernel

**R119.** The guest **MUST** boot an uncompressed ELF `vmlinux` plus an initrd, rather than a bzImage or the
PVH boot protocol.

**R120.** The default kernel **MUST** be built from the sandbox image in a throwaway container, pinned by
version. The same kernel then boots on any host, with no dependency on the host's `/boot`.

**R121.** The initrd **MUST** be purpose-built rather than generated with `dracut`.

**R122.** The cached initrd **MUST** be keyed by a hash of its source, so editing the source rebuilds the
boot artifacts.

An initrd is unavoidable, because Fedora builds `CONFIG_VIRTIO_MMIO` as a module: no block device
exists until it is loaded, so the kernel cannot mount its root on its own. The purpose-built init
loads the module, mounts the root filesystem, `switch_root`s and execs the guest init, and does
nothing else.

A `dracut` initrd is around 38 MB and spends most of its boot probing for the storage stacks,
network setups and hardware a microVM cannot have. Everything a full init system would set up on
the way past falls to the guest init instead.

### 12.3 Disks

**R123.** Drives **MUST** be emitted in a fixed order, and the guest init **MUST** consume the optional ones with
a single device-letter cursor in that same order.

| Device | Role | Mode |
|---|---|---|
| `/dev/vda` | root filesystem | read-write |
| `/dev/vdb` | seed | read-only |
| `/dev/vdc…` | repo disks, then snapshots, then image stores | read-only |

**R124.** Each sandbox's root disk **MUST** be a `cp --reflink=auto` copy of the base rootfs built
from that sandbox's image, and the cache **MUST** hold one base rootfs per image.

**R125.** `--disk` **MUST** be grow-only, **MUST** apply to a kept disk, and **MUST** be a no-op when the disk
already has that much.

**R126.** Repo and image-store disks **MUST** be attached straight from the cache rather than copied per
sandbox.

**R127.** Cached disks untouched for longer than `CS_SANDBOX_FC_REPO_CACHE_TTL_DAYS` days, 14 by
default and `0` to disable, **MUST** be pruned. The collector **MUST** skip any path a live `run.json`
still names.

The size in R125 is a ceiling rather than an allocation. The disk is sparse and reflink-shared with
the base, so the host pays for written blocks only, and a fresh sandbox adds almost nothing until
the guest writes. Growing costs little as well, because ext4 leaves the added block groups
uninitialized. Without reflink support the copy is not shared, and every sandbox costs a full base
up front.

R125 applying to a kept disk is what makes widening possible at all. A running VM's virtio-blk
capacity is fixed at boot, and the guest carries no `e2fsprogs` to resize itself, so `rm` followed by
`create --disk N` is the only route.

R126 is a memory argument. The host page cache is per inode, so a per-sandbox copy of a disk would
hold the same bytes in host RAM once for every sandbox reading it. Sharing one inode is safe
because the guest mounts it read-only.

R124 names the image because the base rootfs **is** that image, exported from a container made from
it. One file for all of them let whoever built last decide what every later sandbox booted. `create`
never noticed: it checks that the disk is a filesystem, not which image it came from. So a host
that had built the slim rootfs served it to a sandbox asking for the shipped image, silently.
Kept per image, that sandbox finds no rootfs under the name it asked for and is told to build one.
The key is the repository rather than the whole reference, so a host holds one per variant instead
of one per version it has ever built.

### 12.4 Returning and sharing memory

**R128.** Every microVM **MUST** get a `virtio-balloon` configured purely for free page reporting,
and **MUST NOT** inflate it.

**R129.** `cs-sandbox` **MUST** set `PR_SET_MEMORY_MERGE` around the launch, and **MUST** offer an opt-out
through `CS_SANDBOX_NO_KSM`.

**R130.** Each microVM **MUST** launch inside its own transient cgroup with a hard `memory.max` ceiling,
and `memory.high` **MUST NOT** be set.

The host cannot observe a guest-internal free, so without help a microVM's host RSS only ever
climbs. A sandbox that peaks at 3 GB and drops back to 250 MB keeps costing 3 GB. With free page
reporting the guest hands back the ranges it stops using, and Firecracker `madvise`s them away. The
balloon never inflates, so this is the guest volunteering memory rather than the host squeezing it,
and none of the usual ballooning thrash applies. Two halves have to line up, and each fails
silently on its own: the device in `run.json`, which is settable pre-boot only, and the driver in
the guest.

R129 covers the other half of the memory story. Sandboxes on one host run the same image, so most
of what they hold is byte-identical. KSM merges only memory a process has volunteered, and
Firecracker volunteers none, so `ksm/run=1` alone dedupes nothing while appearing to be on. Setting
`PR_SET_MEMORY_MERGE` around the launch is what makes guest RAM a candidate. Merging also needs
`ksmd` running on the host, which `cs-sandbox` cannot turn on for you and `doctor` reports. Set
`CS_SANDBOX_NO_KSM` to opt out where sandboxes belong to different trust domains, since page dedup
is a documented side channel.

R130 puts a runaway sandbox in a cgroup where it is charged and killed. Without one the VMM
inherits the launching shell's scope, and the host OOM killer picks its victim by heuristic. The
default ceiling sits above what the guest can reach, at its configured memory plus 256 MiB for the
VMM itself, so it is a backstop rather than a budget. `CS_SANDBOX_FC_MEMORY_MAX` tightens it, and
`CS_SANDBOX_FC_MEMORY_SWAP_MAX` sets the swap allowance, which defaults to none. A host with no
systemd user session gets no cgroup, and `cs-sandbox` says so on the way past.

`memory.high` is never set, because it throttles where a ceiling should fail. A cgroup that can no
longer reclaim under `memory.high` leaves the VM alive and making no progress, with no OOM and no
error for a supervisor to notice. The hard `memory.max` ceiling fails loudly instead.

### 12.5 The guest init

**R131.** `/fc-init` **MUST** run as PID 1 and **MUST**, in this order:

1. mount the API filesystems, make `/` rshared, and bring up loopback;
2. `modprobe` what a microVM has no udev to autoload;
3. mount the seed and source its config;
4. create the developer user with NOPASSWD sudo, then run the shared `nested-rootless` bootstrap for
   their subuid ranges and `newuidmap`'s file capabilities;
5. bring up the NIC with the seeded static address, route and resolver;
6. write `/etc/hosts` and `/etc/gai.conf`, and open the unprivileged ICMP group range;
7. seed or refresh the home as §3.3 describes, and install the agent credentials;
8. run the `--repo` clones, mount the read-only disks with the device-letter cursor, and register the
   image stores among them per R85b;
9. start sshd, signal readiness, and hand off to the vsock listener.

A kernel boots an init rather than an entrypoint, which is why this replaces the container
`ENTRYPOINT`. It skips the keep-id dance entirely, the VM being genuinely root with its own uids.
Boot to ready is one to two seconds.

### 12.6 The fabric

**R132.** A VM **MUST** run inside Podman's own rootless network namespace, with a tap on the group's
bridge. Its static address **MUST** come from the high end of the subnet, above the addresses netavark
hands containers, so the two cannot clash.

**R133.** A keepalive container **MUST** pin the namespace, bridge and DNS, and **MUST** be hidden from `ls`.

**R134.** A forwarding dnsmasq **MUST** serve VM names from an auto-reloading hosts directory and forward
everything else.

**R135.** The dnsmasq **MUST** be located by scanning for a live one on our own address rather than by
pidfile.

**R136.** Host-to-VM ssh **MUST** go through a unix socket, which ignores network namespaces.

Containers and VMs share one rootless L2 fabric per group, so they reach each other directly and by
name across engines. R133 exists because netavark builds and tears down the bridge around running
containers, so a lone VM would otherwise lose its bridge when the last container stopped.

R135 has three payoffs, each of them a real failure otherwise. A dnsmasq already serving our hosts
directory is adopted rather than duplicated. One serving a different directory is reported as a
conflict by name. A root that never started one still finds the running instance.

R136 exists because the host cannot address the rootless namespace directly. A host-side `socat`
binds the published port and relays through a unix socket to a per-VM `socat` inside the namespace,
which connects to the guest's port 22. A Firecracker vsock is retained as a no-IP standby transport,
and is not the routine path.

### 12.7 The fabric is host-global; an instances root is not

**R137.** Anything two instance roots could collide on **MUST** be decided by asking the host, not by
consulting one root's records.

There is one network namespace, one bridge and one loopback per host. But `CS_SANDBOX_HOME` and
`CS_SANDBOX_INSTANCES_DIR` can each point at several independent sandbox roots, and a root reads
only its own state. So:

- **A VM address** is taken if this root records it, or if a tap for that octet already exists under
  the group's prefix. Taps are host-global and outlive whichever root created them.
- **A host SSH port** is taken if this root records it, or if something answers on it. A stopped
  sandbox is caught by the first check and another root's running forwarder only by the second.
- **Fabric collection** treats a live tap as a VM that still needs the fabric.
- **Stale name records** are swept by looking for records whose address has no tap. Driving that
  sweep off one root's instance list would instead delete the live names of every sandbox it cannot
  see.

The managed `~/.ssh/config.d` fragment follows the same rule from the other direction, with one
fragment per root, so regenerating one root's blocks cannot delete another's.

### 12.8 Create

**R138.** A VM that never signals readiness **MUST** be torn down, and the create **MUST** fail loudly.

**R139.** A file lock **MUST** wrap only the race-sensitive prefix of `create`, leaving the long parts
unlocked so creates overlap.

**R140.** A failed create **MUST** be reaped so it cannot leak its reserved address or port, and the reap
**MUST** preserve a reused home disk.

The name is registered immediately after the tap comes up, so a record and a link share a lifetime,
which is what makes the stale-record sweep in §12.7 sound. The race-sensitive prefix is allocation,
writing the state claim, and bringing the fabric up. Disk builds and the boot wait run unlocked. The
lock lives in the instances directory, so it serializes creates within one root; the host-level
checks above are what keep different roots from colliding.

### 12.9 Constraints

Firecracker is a deliberately lean VMM, which trades features for a small surface:

- **Directory sharing is a block device.** With no `virtio-fs`, a shared directory arrives as a
  read-only ext4 disk or as the alternates clone.
- **Shared objects are point-in-time.** The repo disk is built at create time, so a commit made on
  the host afterwards reaches the sandbox through `push`, or through recreating it.
- **VM names are registered on create and destroy** rather than auto-discovered the way container
  names are. Reaching a peer is identical either way.
- **x86_64 Linux with KVM only.** Every other host uses the Podman engine.

## 13. Security model

**R141.** Sandboxes **MUST** run rootless, with a scaled-down capability set, seccomp on, and no host
device beyond `/dev/net/tun`.

**R142.** SSH ports **MUST** bind `127.0.0.1`, and any other bind **MUST** be opt-in.

**R143.** `--privileged` **MUST** be opt-in.

The engine is the trust boundary, and everything else follows from that. There is no host-root path
absent a kernel bug: the engine and the container are bounded by your unprivileged host user through
keep-id. The microVM engine removes the shared-kernel attack surface entirely. `--privileged` trades
that defence in depth for breadth, which is why it is a flag rather than a default.

`CS_SANDBOX_SSH_BIND` is R142's opt-in. It sets the host address a sandbox's published SSH port
binds, and a group gateway's with it. The cost is reach: any value other than loopback publishes
that port on an interface the rest of the network can route to. Key authentication still holds,
because sshd takes no password and authorizes only the keys §4 gives it. R142 does not, so scope the
variable to one command rather than exporting it.

R105 unmasks the container's `/proc`, which is why R141 no longer names `/proc/kcore`. The masking
was the outer of two defences, and not the load-bearing one. The container's root is an unprivileged
subuid, so the kernel denies it `/proc/kcore`, `/proc/sysrq-trigger` and every non-namespaced sysctl
on its own. Spending that layer is what buys a rootless inner engine, which holds its privileges in
its own user namespace rather than in this container's.

**Passwordless sudo inside a sandbox is safe**, and is the usual setup for an agent sandbox. On the
container engine, root inside is your own unprivileged host uid through `--userns=keep-id`. On the
microVM engine it is real root confined to the guest's kernel. Either way `sudo` grants an agent
nothing it does not already control over its own disposable sandbox, while restricting it would add
friction and move no boundary. This holds only while that boundary is intact: running the image
rootful, `--privileged`, or `--userns=host` turns the same passwordless sudo into genuine host root.

**Nothing at rest to leak.** No host private key is inside any sandbox, and peers are reached with
generated per-group tier keys. A sandbox that borrows its LLM credential (§10.2) holds nothing of
yours at all. What is on its disk is a loan token, and it stops working when that sandbox is
destroyed. A sandbox given a copy is the exception, and the copy lives only in the seed and the
home, never in the image or in git.

**Lending rather than copying, twice.** `ssh -A` gives a sandbox the use of your keys for the life
of one connection, and the keys stay on the host. Section 10.2 applies the same idea to the
credential that pays for the model. The sandbox spends it and never holds it. Both leave a window
rather than a copy. Anything running as you in the sandbox can use an agent-forwarding socket while
you are connected, though it cannot copy the key out, so scope what you load. Anything in a sandbox can
spend a loan while that sandbox exists, though it cannot learn what the loan stands for.

## 14. Limitations

- **No per-agent isolation by default.** Agent sandboxes in a group share that group's `G` key, so
  any agent sandbox can SSH into any other in the same group. Agents are walled off from you and
  from your user sandboxes, but not from each other. `--solo` narrows one sandbox to no outbound
  SSH; `--group` separates whole fleets and shares no key across the boundary.
- **Not bit-for-bit reproducible.** The image runs a package update at build time, so a rebuild can
  pick up newer upstream packages.
- **Not a hardened multi-tenant boundary.** Isolation is whatever the chosen engine provides.

## 15. Non-goals

- **No implicit sharing.** No `$PWD` mount, no inherited credential, no host key. Every one of them
  stays a flag.
- **No agent orchestration.** The remote agent tools start a session on another sandbox and hand
  back its output. Deciding what to run is the caller's job.
- **No image inside git.** The image is built, never committed.

## 16. Conformance and testing

An implementation conforms when it satisfies R1–R143, R18a, R85a, R85b, R90a and R92a included. The
test suite is the reference, and it has two tiers, split by whether they touch a real engine.

**Unit tests** (`make test`) are pure and fast, with no external processes. They cover the logic
where a silent bug would be costly: the seed trust material, agent-login inheritance, spec parsing,
instance state, the kernel rebuild decision and port allocation. They also drive the CLI through the
real cobra tree with a fake `Runner`.

**Integration tests** (`make test-integration`, behind a build tag) are live tests on a Linux host
with KVM and Podman. They create namespaced sandboxes in temporary state directories, tear them
down, and skip gracefully when Podman or the image is unavailable. The suite runs with `-p 1`,
because packages share one rootless network namespace and one host SSH port pool.

The **smoke profile** (`make test-smoke`) is not a third tier. It is the subset of the integration
tier that CI runs on every host, against a slimmed image. Keep it short.

The **replay members** are the second half of the smoke profile: they drive a real agent inside a
real sandbox, with its model turns served from a committed cassette. `make test-smoke` runs them,
and `make test-agents-shared` and `make test-agents-lent` run one half alone. They hold no
credential and reach no provider, which is what separates them from the live matrix they replay.

They boot an image carrying the agent CLIs rather than the one the members above boot, so the
profile builds two. `setup-smoke` makes both. Where the second is absent the replay members skip,
naming it, which is how every other live member behaves on a host that cannot carry it.

The two differ by one hop, and it is the hop worth a second profile for. A shared case holds a copy
of the credential and reaches the recorder itself. A lent case holds a loan token and reaches the
lender, which swaps in the host's credential and forwards. The lending path therefore runs on every
replay instead of being bypassed.

`make fixtures` records what they serve, against real providers, at a model turn per case. The
recording and the replay come out of one driver, because a cassette is only replayable while the
two agree on every byte the agent sends. `make fixtures-check` proves the committed cassettes still
key under the recorder's current normalization ruleset. It asks in one process with no sandbox,
which is what lets `make check` carry it.

That image is `cs-sandbox build --slim`, derived from the shipped Containerfiles rather than written
twice. It drops the developer toolchains, which are most of the 6.04 GB and nearly all of the build
time. It keeps the three agent CLIs, for a suite whose tests drive one inside the sandbox. A
downloaded binary can build it, so a consumer needs no checkout of this repository.

### Coverage

Every test target writes coverage into its own tier under `.coverage/`, so running several
aggregates rather than overwrites. `make coverage` merges what is there and prints the report.

`make coverage-check` runs inside `make check` and in CI. It fails when a package
`.coverage-baseline` lists stops being reached: presence, not a percentage. What it catches is a
suite that stopped running while the tests still report green. When a package is meant to lose its
coverage, rerun `make coverage-baseline` and commit the result.

In CI each job uploads its tier and one job merges them. Record a baseline only for the tiers CI
runs: `make coverage-baseline BASELINE_TIERS="unit race smoke"`.

## 17. Open questions

1. **The engine default is host-derived**, meaning Firecracker on Linux with KVM and Podman
   elsewhere. Whether a user-level default should override that, and where it would be configured,
   is undecided.
2. **A group's tap prefix is reused once its group is removed.** `allocTapPrefix` builds its
   taken set from the groups that exist now, so a removed group's prefix returns to the pool.
   A tap interface that outlives its group's record would therefore collide with the next
   group to take that prefix, and nothing measures whether that happens.
3. **Nothing garbage-collects `removed` data.** It is listed by `ls` until someone reuses or
   destroys it, deliberately, but there is no policy for a host that accumulates it.
