# cs-sandbox(1) — manual

## Name

**cs-sandbox** — create and manage disposable Linux dev sandboxes for AI coding agents.

## Synopsis

```
cs-sandbox create <name> [--repo PATH] [--snapshot PATH] [--type agent|user]
                         [--engine podman|firecracker] [--group NAME]
                         [--inherit-agent-login AGENT] [--yolo] [--solo] …
cs-sandbox ls [--json] [-q]            cs-sandbox inspect <name> [--json]
cs-sandbox ssh <name> [args...]        cs-sandbox exec <name> [cmd...]
cs-sandbox fetch <name> [dir]          cs-sandbox push <name> [dir]
cs-sandbox stop|start <name>           cs-sandbox rm <name>
cs-sandbox destroy <name> [-f]         cs-sandbox port <name>

cs-sandbox forward <name> [HOSTPORT:]VMPORT... | --socks [PORT]
cs-sandbox forwards [name]             cs-sandbox unforward <name> [HOSTPORT|all]
cs-sandbox host-route up|down|refresh|status
cs-sandbox sync-ssh-config

cs-sandbox group create|ls|rm <group> [-f]
cs-sandbox create-store <name>         cs-sandbox seed-store [--from-host] <name> <image>...
cs-sandbox stores                      cs-sandbox rm-store [-f] <name>

cs-sandbox build [--engine ENGINE]...  cs-sandbox doctor [--engine ENGINE]
cs-sandbox agent-login <agent> <name>  cs-sandbox install-agent-tools [dir]
cs-sandbox completion bash|zsh|fish|powershell
cs-sandbox version

Global: [-v|--verbose] [-q|--quiet] [--dry-run]
```

## Description

`cs-sandbox` is a host tool. It creates named Linux sandboxes, each a rootless Podman container or
a Firecracker microVM built from one image. A shared network joins them, so every sandbox is
reachable by name over SSH.

Nothing on your host enters a sandbox unless you name it. Code goes in through `--repo` or
`--snapshot`, commits come back out with `fetch`, and the loop is create, work, fetch, destroy.

**`cs-sandbox` is not installed inside a sandbox.** If the command is missing, you are probably
already in one. Reach peers with plain `ssh <name>` and move commits with plain `git`.

For the tour and the walkthroughs see [README.md](README.md). For what the tool guarantees and how
it is built see [SPEC.md](SPEC.md).

## Naming and identity

A sandbox name is one DNS label: letters, digits and dashes, starting and ending alphanumeric, up
to 63 characters. A name never contains a dot.

Identity is the pair (group, name), written `<name>.<group>`. A bare name always means the
`default` group, and never "whichever group happens to have it", so a reference cannot change
meaning as sandboxes come and go. Where a name exists in more than one group, a bare reference is
an error that names the candidates.

## Commands

### create

```
cs-sandbox create <name> [flags]
```

Creates a sandbox and starts it. The defaults suit the common case: an agent sandbox in the
`default` group, sharing nothing and inheriting no agent login. The engine defaults to Firecracker
where the host has KVM, and to Podman otherwise.

| Flag | Meaning |
|---|---|
| `--repo PATH[@REF][:NAME]` | Share a git repo. It lands at `~/<name>` on branch `cs-sandbox/<sandbox>`, borrowing the source's objects read-only. Repeatable. |
| `--snapshot PATH[:NAME]` | Share a frozen read-only copy of any directory at `~/<name>`. Repeatable. |
| `--type agent\|user` | What this sandbox may reach over SSH. Default `agent`. See [SSH trust](#ssh-trust). |
| `--engine podman\|firecracker` | Which engine backs it. Default: Firecracker on Linux with KVM, Podman otherwise. |
| `--group NAME` | The group whose network, keys and gateway it joins. Default `default`. |
| `--inherit-agent-login AGENT` | Carry a host agent login in: `claude`, `codex` or `opencode`. Repeatable and comma-separated. Default: inherit nothing. |
| `--yolo` | Drop the agents' approval prompts. The sandbox is the boundary. |
| `--solo` | Withhold the group's SSH key, so this agent sandbox can reach no peer while staying reachable itself. Agent type only. |
| `-e`, `--env KEY=VALUE` | Inject an environment variable, or `KEY` alone to pass the host's value. Repeatable. |
| `--env-file PATH` | Inject variables from a file. Repeatable. |
| `--image-store NAME` | Mount a shared image store read-only. Repeatable. |
| `--cpus N` | Firecracker vCPUs. Default 4. |
| `--mem MiB` | Firecracker memory. Default 4096. |
| `--disk GiB` | Firecracker disk size, grow-only. Default: the base rootfs size, 32. |
| `--privileged` | Podman: use `--privileged` instead of the scaled-down capability set. |

`create` prints which agent logins the sandbox ended up with, so an inherited login is never a
silent assumption.

Recreating with the name of a sandbox that `rm` kept data for reuses that data.

### Working in a sandbox

```
cs-sandbox ssh <name> [args...]     # a shell, or one command, over SSH
cs-sandbox exec <name> [cmd...]     # run a command through the engine
cs-sandbox port <name>              # its host SSH port
```

Prefer plain `ssh <name>`, which works because `create` maintains an SSH config fragment. Use
`exec` when you want the engine's own channel rather than SSH.

A command that exits non-zero inside the sandbox gives `cs-sandbox` that same exit status, with no
message of its own. The command's output already reached your terminal.

### Moving code

```
cs-sandbox fetch <name> [dir]       # sandbox commits -> host
cs-sandbox push  <name> [dir]       # host commits -> sandbox
```

Both are fast-forward only, and both refuse rather than rewrite. `[dir]` picks one repo when a
sandbox shares several.

**Fetch before you destroy.** `destroy` deletes the sandbox's commits along with its data.

### Lifecycle

```
cs-sandbox ls [--json] [-q]         # GROUP NAME STATUS AGE TYPE ENGINE YOLO SOLO
cs-sandbox inspect <name> [--json]  # everything recorded about one
cs-sandbox stop <name>              # shut it down, keep everything
cs-sandbox start <name>             # bring it back
cs-sandbox rm <name>                # remove the sandbox, KEEP its data
cs-sandbox destroy <name> [-f]      # delete the sandbox AND its data
```

`rm` leaves the data listed by `ls` with status `removed`, so nothing sits on disk unnoticed.
`destroy` is irreversible, and `-f` (spelled `--force` in full) is the confirmation rather than a
way past one. Without it, `destroy` names what it would delete, deletes nothing and exits 0.

`ls -q` prints refs one per line for scripting. `--json` output is stable and meant to be parsed.

### Reaching a port

A port bound inside a sandbox is private to it.

```
cs-sandbox forward <name> 9000:8080     # host :9000 -> the sandbox's :8080
cs-sandbox forward <name> 8080          # same port on both sides
cs-sandbox forward <name> --socks=1080  # a SOCKS proxy into the sandbox
cs-sandbox forwards [name]              # what is wired up
cs-sandbox unforward <name> all         # tear them down
```

Note the `=` in `--socks=1080`. Without a value the flag defaults to 1080.

`--bind ADDR` sets the host address forwarded ports bind to, and defaults to `127.0.0.1`.

`host-route` is the alternative to forwarding ports one at a time. It makes every port a sandbox
binds reachable from the host at `<name>.cs.sandbox`. It is optional and Linux-only, and `up` asks
for `sudo` once:

```
cs-sandbox host-route up|down|refresh|status
```

### Groups

Groups are optional. Without `--group` every sandbox joins one called `default` and every sandbox
sees every other, which is all most setups need. Reach for a group when unrelated fleets share a
host and must not see each other: each group gets its own network, its own SSH keys and its own
gateway.

```
cs-sandbox group create <group>
cs-sandbox group ls
cs-sandbox group rm <group> [-f]     # -f destroys the group's sandboxes first
```

`create --group NAME` makes the group if it does not exist, so `group create` is only needed to
make one ahead of time.

### Shared image stores

A store lets several sandboxes reuse one set of container images instead of pulling per sandbox.

```
cs-sandbox create-store <name>
cs-sandbox seed-store [--from-host] <name> <image>...
cs-sandbox stores
cs-sandbox rm-store [-f] <name>
```

`--from-host` copies and re-owns images already in your host store rather than pulling them again.

### Host setup

```
cs-sandbox build [--engine ENGINE]...     # the image, and the Firecracker artifacts
cs-sandbox doctor [--engine ENGINE]       # check prerequisites, print the fix for each gap
cs-sandbox install-agent-tools [dir]      # the agent tools onto your PATH
cs-sandbox agent-login <agent> <name>     # log an agent in inside a sandbox
cs-sandbox sync-ssh-config                # regenerate the SSH config fragment
cs-sandbox completion <shell>             # a completion script for bash, zsh, fish or powershell
```

With no `--engine`, `build` sets up every engine the host supports, and fails on a
Firecracker-capable host whose Firecracker packages are missing. Restrict it with `--engine podman`
for the image alone. The flag is repeatable, so `--engine podman --engine firecracker` names both.

`completion` writes a script to stdout. It completes sandbox names, store names and flag values
live, by asking the binary. [INSTALL.md](INSTALL.md#optional-shell-completion) has the per-shell
install path.

`doctor` is the first thing to run when something does not work. It checks each prerequisite and
prints the remedy for anything missing.

`agent-login` launches the agent inside the named sandbox so you can complete its login there. The
login stays in that sandbox and goes when it does.

## Global options

| Option | Meaning |
|---|---|
| `-v`, `--verbose` | Per-command progress, full build output, and the external commands run. |
| `-q`, `--quiet` | Silence all output, including build progress. |
| `--dry-run` | Print the external commands instead of running them. |

`--dry-run` is the way to show someone what a command would do before it does it. It changes
nothing: a dry run of `create` prints the plan, reports the sandbox it would create, and leaves no
record, no directory and no sandbox behind.

## SSH trust

What a sandbox can reach depends on its `--type`, which is independent of its engine.

| client ↓ \ server → | user sandbox | agent sandbox |
|---|:---:|:---:|
| **host** | ✓ | ✓ |
| **user** sandbox | ✓ | ✓ |
| **agent** sandbox | ✗ | ✓ |

An agent sandbox cannot SSH into a user sandbox, which is what stops an agent pivoting into your
workspace. `--solo` narrows an agent further: it is seeded no tier key, so it can SSH into no peer
at all. Its own network access is untouched, and peers still reach the services it runs.

The matrix describes reach within one group. Across groups nothing connects: no DNS, no route, and
no key the other side would accept.

## Files

| Path | What it is |
|---|---|
| `$XDG_DATA_HOME/cs-sandbox/instances/` | One record per sandbox. Persistent. macOS: `~/Library/Application Support/`. |
| `$XDG_DATA_HOME/cs-sandbox/keys/` | The generated per-group SSH tier keys. Secret. |
| `$XDG_CACHE_HOME/cs-sandbox/` | The Firecracker kernel, rootfs and disk cache. Regenerable. macOS: `~/Library/Caches/`. |
| `$XDG_CACHE_HOME/cs-sandbox/net/` | The fabric's working directory. Host-global, so it stays put when you relocate the rest. |
| `~/.ssh/config.d/cs-sandbox` | The generated SSH config that makes `ssh <name>` work. Included from `~/.ssh/config`. |
| `~/.ssh/known_hosts.cs-sandbox` | Sandbox host keys, kept out of your own `known_hosts` and keyed by `<name>.<group>` rather than by port. |

No sandbox state lives in the source tree.

## Environment

Everything here is optional. The first group relocates state, and is how you run an isolated fleet
without disturbing your real one, which is what the test suite does.

| Variable | Effect |
|---|---|
| `CS_SANDBOX_HOME` | Relocate all state under one root. Sets the three directories below at once. |
| `CS_SANDBOX_INSTANCES_DIR` | The instance records directory. |
| `CS_SANDBOX_TIER_DIR` | The tier keys directory. |
| `CS_SANDBOX_FC_CACHE` | The Firecracker artifact cache. |
| `CS_SANDBOX_FC_NET` | The fabric working directory, which `CS_SANDBOX_HOME` deliberately leaves alone. |
| `XDG_DATA_HOME`, `XDG_CACHE_HOME` | The defaults the paths above derive from. |

The second group changes what gets built or run.

| Variable | Default | Effect |
|---|---|---|
| `CS_SANDBOX_ENGINE` | unset | The engine `create` uses when no `--engine` is given. Unset, it picks Firecracker on Linux with KVM and Podman otherwise. |
| `CS_SANDBOX_IMAGE` | `localhost/cs-sandbox:44` | The sandbox image to run. |
| `CS_SANDBOX_ASSETS_DIR` | the embedded copy | An `image/` asset tree for `build` to use instead of the one embedded in the binary. |
| `CS_SANDBOX_PRIVATE_REGISTRY` | none | A registry the image should trust, as a bare `host:port`. Read at `build` time. |
| `CS_SANDBOX_PRIVATE_REGISTRY_INSECURE` | `0` | `1`, `true`, `yes` or `on` lets that registry use plain HTTP. |
| `CS_SANDBOX_DNS_SUFFIX` | `cs.sandbox` | The domain `host-route` resolves sandbox names under. |
| `CS_SANDBOX_GROUP` | `default` | The group `create` puts a sandbox in when no `--group` is given. |
| `CS_SANDBOX_TZ` | `America/Los_Angeles` | The timezone a sandbox boots with. |

The third group tunes the Firecracker engine. Leave these alone unless `doctor` or this manual sends
you to one.

| Variable | Default | Effect |
|---|---|---|
| `CS_SANDBOX_FC_VERSION` | `v1.16.0` | The Firecracker release `build` downloads. An override has no committed digest and falls back to the published checksum. |
| `CS_SANDBOX_FC_KVER` | a pinned Fedora NVR | The guest kernel version `build` compiles. |
| `CS_SANDBOX_FC_KERNEL` | the built kernel | A guest kernel image to boot instead. |
| `CS_SANDBOX_FC_ROOTFS_GB` | `32` | The base rootfs size. Applies to a rootfs built after it is set. |
| `CS_SANDBOX_FC_REPO_CACHE_TTL_DAYS` | `14` | How long an unused repo or image-store disk stays cached. `0` disables the sweep. |
| `CS_SANDBOX_FC_MEMORY_MAX` | the guest's memory + 256 MiB | The cgroup ceiling a microVM is killed at. |
| `CS_SANDBOX_FC_MEMORY_SWAP_MAX` | `0` | The swap allowance on top of that ceiling. |
| `CS_SANDBOX_FC_NO_CGROUP` | unset | Set to anything to launch microVMs outside a cgroup. |
| `CS_SANDBOX_NO_KSM` | unset | Set to anything to stop offering guest memory to KSM. |

## Exit status

| Code | Meaning |
|---|---|
| 0 | Success. |
| 1 | A `cs-sandbox` failure. The message goes to stderr, prefixed `cs-sandbox:`. `doctor` also exits 1 when a check fails, having already printed its report. |
| *n* | `ssh` and `exec` pass through the status of the command they ran inside the sandbox, and say nothing of their own. |

## Diagnostics

**A name that exists in several groups** — the error names every candidate. Address the sandbox in
full as `<name>.<group>`. A bare name only ever means the `default` group.

**`ssh <name>` does not resolve** — the SSH config fragment is stale or the include is missing. Run
`cs-sandbox sync-ssh-config`.

**A sandbox cannot reach another** — check they are in the same group, and check the type matrix
above. An agent sandbox cannot reach a user sandbox by design, and `--solo` denies outbound SSH
entirely.

**`create` rejects a path on macOS** — everything runs in one podman-machine VM there, so `--repo`
and `--snapshot` sources must live under `$HOME`.

**`no systemd user session; running without a memory cgroup`** — a microVM is starting outside the
cgroup that would cap it. The sandbox runs, but a runaway one is charged to the shell that launched
it. Under WSL2, enable systemd as [INSTALL.md](INSTALL.md#windows-wsl2) describes.

**Anything about a missing prerequisite** — run `cs-sandbox doctor`, which names the gap and the
fix. Add `--engine podman` or `--engine firecracker` to check a specific engine.

## Notes for agents

- Every command is non-interactive. Nothing waits on an answer, so nothing hangs unattended.
- `destroy <name>` without `-f` deletes nothing. It prints what it would delete and exits 0, so it
  is safe to run to find out, and it is not a dialog to answer.
- `ls --json` and `inspect --json` are the machine-readable surfaces, and their shape is stable.
  Everything else is line-oriented text.
- `ls -q` prints refs one per line, which pipes into `xargs`.
- `--dry-run` prints what would run and changes nothing, `create` included. Its summary line reads
  `would create <name>` rather than `created <name>`.
- **`destroy` is irreversible.** Confirm with the user first, and check for unfetched commits.
  Prefer `rm` when the data might still be wanted.
- `cs-sandbox` does not exist inside a sandbox. If it is missing from PATH, use `ssh` and `git`.

## Examples

Create a sandbox with a repo and a logged-in agent, work in it, take the commits, throw it away:

```bash
cs-sandbox create feature --repo ~/projects/api --inherit-agent-login claude
ssh feature
[feature]$ cd ~/api && cs-claude
[feature]$ exit
cs-sandbox fetch feature
cs-sandbox destroy feature -f
```

Reach a service running inside a sandbox from your own browser:

```bash
cs-sandbox forward web 9000:8080
curl http://localhost:9000
cs-sandbox unforward web all
```

Run two experiments that must not interfere:

```bash
cs-sandbox create api --group cache-redis  --repo ~/projects/api
cs-sandbox create api --group cache-memory --repo ~/projects/api
cs-sandbox exec api.cache-redis pwd
cs-sandbox group rm cache-redis -f
```

Reclaim disk from sandboxes that `rm` kept the data of:

```bash
cs-sandbox ls                       # anything with STATUS "removed" is data only
cs-sandbox destroy <name> -f
```

## See also

[README.md](README.md) for the tour, [INSTALL.md](INSTALL.md) for host setup, [SPEC.md](SPEC.md)
for the model and the guarantees, and [CONTRIBUTING.md](CONTRIBUTING.md) for working on
`cs-sandbox` itself. Both engines are specified in [SPEC.md](SPEC.md), sections 11 and 12.
