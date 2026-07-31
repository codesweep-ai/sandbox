# Campaign network groups

## Status

Implemented on the `campaign-groups` branch. This document is the single record for the campaign
work in `cs-sandbox`: the network-isolation primitive, the supporting remote-session and
authentication changes, backward-compatibility notes, verification, and the open items. The
higher-level campaign harness lives in `sandbox-agent-dev-multi` (`cs-campaign`); this branch does
not make a campaign a first-class `cs-sandbox` resource.

## Result

A campaign should use:

- one controller created with `--type agent`;
- one or more workers created with `--type agent --solo`; and
- one unique `--network` value shared by the controller and its workers.

For example:

```sh
cs-sandbox create search-ctl \
  --network campaign-search \
  --type agent

cs-sandbox create search-01 \
  --network campaign-search \
  --type agent \
  --solo \
  --repo ~/projects/app
```

Members of the same campaign resolve and connect to one another by name. Members of different
campaign networks cannot resolve one another or connect by raw IP through the sandbox fabric.
Campaign members retain outbound internet access and do not receive the host Podman socket.

The campaign harness remains responsible for choosing a unique network name, creating members
consistently, tracking them, fetching results, and destroying them after completion.

## Why network grouping is required

`--solo` is an SSH credential control, not a network boundary. It prevents a worker from initiating
SSH with sandbox-managed credentials, but without network grouping that worker can still contact
application services exposed by other sandboxes on the shared fabric.

Network groups add the missing all-protocol boundary. The controls serve different purposes:

- `--type agent` prevents a controller from authenticating to a user-tier sandbox.
- `--solo` withholds outbound sandbox SSH credentials from a worker.
- `--network` limits which sandboxes are reachable at all through the rootless fabric.

## Connectivity model

Within one campaign, network reachability and SSH authentication are separate questions. Both the
controller and solo workers can open TCP, UDP, and other network connections to services exposed by
any member on their shared campaign network:

| Client | Controller services | Solo-worker services |
|---|:---:|:---:|
| Controller (`agent`) | yes | yes |
| Solo worker (`agent --solo`) | yes | yes |

The sandbox-managed SSH credentials are deliberately asymmetric:

| Client | SSH to controller | SSH to solo worker |
|---|:---:|:---:|
| Host | yes | yes |
| Controller (`agent`) | yes | yes |
| Solo worker (`agent --solo`) | no | no |

Thus, a solo worker can call a controller API or another worker's application port, but it cannot
use sandbox-managed credentials to obtain a shell there. A TCP connection to port 22 may still be
possible; authentication is what fails. User-tier sandboxes are not campaign members, and the agent
tier key is not accepted by user-tier sandboxes even if some separate routing change makes one
reachable.

Across campaign networks, sandbox-to-sandbox traffic is denied regardless of port or protocol. The
host can still enter every member through its published SSH port for observation, fetch/push, and
recovery.

## CLI, state, and inventory

`create` accepts a DNS-label network name:

```text
cs-sandbox create <name> --network <name>
```

`CS_SANDBOX_NETWORK` supplies the default for new sandboxes. An explicit flag takes precedence. If
neither is set, the existing `cs-sandbox-net` default is used.

The selected network is stored in the instance's `state.json` and is immutable. Lifecycle commands
use that recorded value rather than the current environment. Records created before this feature,
which have no `network` field, continue to mean `cs-sandbox-net`.

`cs-sandbox ls` presents `NETWORK` as its first column and sorts entries by network and then sandbox
name, grouping campaign members together in human-readable output.

`cs-sandbox ls --json` is the stable machine-readable inventory interface consumed by `cs-campaign`.
It emits a JSON array sorted in the same network-then-name order; each entry can contain `name`,
`status`, `created`, `type`, `engine`, `yolo`, `solo`, and `network`. `--json` and the name-only
`--quiet` mode are mutually exclusive.

## Isolation implementation

### Rootless Podman fabric

Every custom network created by `cs-sandbox` is a separate rootless Podman bridge with:

```text
--opt isolate=true
--label cs-sandbox.managed=1
```

Testing found that distinct bridge names and subnets are not sufficient: netavark otherwise routes
raw-IP traffic between bridges in the same rootless network namespace. `isolate=true` blocks that
inter-bridge forwarding without the loss of internet access caused by `--internal`.

Podman's network DNS scopes container name resolution to members of the selected bridge.

The historical default network is not changed to isolated mode, preserving existing behavior for
callers that do not opt into grouping.

### Firecracker fabric

Each network has independent Firecracker resources:

- keepalive container;
- working and hosts directories;
- dnsmasq state and VM name records;
- bridge attachment; and
- tap names derived from both the network and VM address.

The network component in tap names is necessary because Linux interface names are host-global, even
when the taps attach to different bridges. A VM is attached only to the bridge recorded in its state.
Start recreates that network if it has disappeared and does not fall back to the default fabric.

### Host routing

`host-route` remains limited to `cs-sandbox-net`. Publishing several isolated bridge subnets through
one host route/DNS view would weaken the boundary. Custom campaign members remain available from the
host through their normal published SSH ports and managed SSH configuration.

Individual application ports can still be exposed selectively through SSH forwarding, for example
`cs-sandbox forward campaign-a-worker 3000`. This forwards only the requested service through that
member's host-published SSH connection. It does not publish the campaign subnet, install a host
route, or provide a path between campaigns.

## Network lifecycle

Destroying one member does not disturb peers on its network or members of another network.
Firecracker garbage collection is scoped to the selected network.

After the final member is removed, `cs-sandbox` reclaims a custom network only when all of the
following are true:

- no instance record in the current root names it;
- no host-global Firecracker VM registration names it;
- no Podman endpoint remains attached; and
- the network carries `cs-sandbox.managed=1`.

The default network and user-managed networks are never automatically removed.

## SSH key boundary

Controllers continue to share the existing agent-tier SSH key. The key cannot be used across
campaigns while the networks have no route between them.

This is intentionally a network-backed boundary, not per-campaign authentication. Any future change
that joins networks, adds inter-network routing, or proxies another campaign's SSH endpoint would
also make the shared credential usable across that path. Such a change requires a separate security
review. Per-campaign SSH keys remain a possible defense-in-depth extension.

## Supporting remote-session changes

End-to-end campaign validation exposed problems in the Claude and Codex remote-session helpers. A
campaign orchestrator must be able to submit a turn, disconnect, poll its status later, distinguish
success from failure, and cancel it reliably; the tools were designed for more interactive use and
did not consistently satisfy that contract. The changes below fix that. They do not implement
campaign orchestration; `cs-campaign` remains responsible for campaign membership, workflows,
transcripts, and archives.

### Remote-turn execution model

`cs-claude-remote` and `cs-codex-remote` still use SSH to reach a target sandbox and tmux to keep
the agent CLI session warm on that target. Their `cs-claude-turn` and `cs-codex-turn` drivers
interact with the CLI inside tmux. The `cs-claude-remote-output` and `cs-codex-remote-output`
helpers expose logs and status. The change is in how a background turn is managed on the initiating
sandbox.

Previously, a background turn was an ordinary child process of the invoking shell; when the SSH
connection or invoking shell ended, the process could be reaped before the remote turn finished,
leaving an incomplete log, a stale status, or a turn that appeared to run forever.

Background turns now:

1. Generate a private, per-turn runner script.
2. Start that runner as a transient user systemd service when a user systemd instance is available.
3. Fall back to `nohup` when user systemd is unavailable.
4. Ignore `SIGHUP`, so disconnecting the launching shell does not terminate the turn.
5. Record the runner PID for status checks and cancellation.
6. Append an authoritative completion footer containing the actual exit code.
7. Remove the runner and PID files after completion.

The completion footer, rather than the continued existence of an SSH connection, is the durable
record that a turn ended.

### Cancellation

Killing only the tmux session on the target sandbox was insufficient: it could leave the initiating
sandbox's background runner and SSH child alive, causing status to remain `running` indefinitely.

Cancellation of a named session now finds the recorded background runner, terminates its child
processes (including SSH), terminates the runner (escalating to forced termination if needed),
removes stale PID and runner files, writes a completion footer with exit code `130` if the turn did
not already have one, and kills the target sandbox's tmux session.

The agent's persisted history is retained, so a later turn can resume the logical agent session.
Cancellation is represented as a failed turn with exit code `130`; there is no separate `cancelled`
status string.

### Machine-readable turn status

The remote-output helpers expose the following status contract:

| Status | Exit code | Meaning |
|---|---:|---|
| `finished` | 0 | The turn completed with exit code zero. |
| `running` | 2 | The background runner is still alive and no completion footer exists. |
| `failed` | 3 | The turn completed with a nonzero exit code, including cancellation. |
| `unknown` | 1 | No completion footer exists and no live runner can be found; the runner likely crashed. |

This prevents an orchestrator from treating every completed process as a successful model turn. The
log retains the underlying turn exit code even though the status helper uses exit code `3` as its
stable interface for all failed turns. Cancellation and expired authentication are failure causes,
not additional status strings: cancellation records underlying exit code `130`, while an expired
Claude login makes the turn driver exit nonzero; both are reported as `failed`.

### Claude CLI compatibility

The tmux driver determines readiness from terminal state. Current Claude versions display
`bypass permissions on`, older ones `auto mode on`; both are recognized as ready. The driver also
recognizes `Login expired`, `Please run /login`, and the Claude OAuth/login screens as
authentication states. Because Claude can accept a prompt, append a normal-looking assistant
message reporting an expired login, and still emit its turn-completion marker, the driver rechecks
terminal state after completion and fails the turn if Claude is at a login screen.

A guest Claude with a valid credential but no `.claude.json` install state presents the interactive
first-run onboarding wizard; the driver reports "needs a human" rather than keystroking blind.
Seeding both `.credentials.json` and matching install state avoids this.

### Codex CLI compatibility

Codex stores model activity in rollout JSONL files. Current versions create the rollout when the
TUI starts; older ones created it after the first prompt. For a new session, the driver snapshots
existing rollout files before launching Codex and binds the turn to the rollout created by that
launch, retaining the post-submission detection path for older versions. This avoids associating a
turn with the newest rollout of a different concurrent session.

### Claude credential inheritance

`--inherit-agent-login claude` uses this precedence:

1. Use `~/.cs-claude/.credentials.json` when it exists (the isolated cs-sandbox host profile,
   deliberately preferred).
2. Otherwise, use Claude's standard host login at `~/.claude/.credentials.json`.
3. Otherwise, inherit no Claude credential and report how to log in.

Only the specific credential file is copied into the per-sandbox seed with restricted permissions;
the entire Claude profile is not copied. The fallback did not broaden Codex credential discovery,
because Codex already uses its explicitly supported cs-sandbox profile path.

Inheritance remains opt-in, and the command reports when the fallback is used. A user who
previously received "no host Claude login to inherit" may now inherit their standard personal
login, which can affect subscription usage expectations.

### Security and scope boundaries

These changes do not add general-purpose secret injection. `cs-sandbox` does not copy arbitrary
environment variables or API keys, entire CLI or home-directory profiles, or unrelated provider
credentials; and does not give `cs-campaign` access to secrets that were not explicitly inherited
at creation. (OpenCode credential and session support was added later on this branch — open item
9 — under the same rules: an isolated `~/.cs-opencode` profile, opt-in `--inherit-agent-login`,
and inline-env auth that never lands beside the session db.) The copied login is a snapshot in
that sandbox's seed. Campaign network isolation and `--solo` restrictions are independent of
model-login credentials.

## Backward compatibility

Existing default-network sandboxes (state without a `network` field) map to the historical
`cs-sandbox-net` with unchanged paths, resources, and isolation behavior. The reverse is not
guaranteed: an older binary does not understand custom-network state and can assume the default
fabric; custom-network sandboxes must not be managed with an older binary, and downgrading is
unsupported until they are destroyed with a compatible one.

| Area | Potential impact | Disposition |
|---|---|---|
| Human-readable `ls` | Positional parsers and ordering assumptions can break (NETWORK is now first; network-then-name sort). | Intentional; automation must use `--json` or `--quiet`. |
| Remote status | Failed turns no longer look successfully finished. | Intentional semantic correction; consumers must handle all four states and exit codes. |
| Remote cancellation | `--kill` now cancels the runner and SSH child as well as remote tmux. | Intentional; PID-verification hardening still advisable (see open items). |
| Background execution | User-systemd launch can expose host-specific failure modes. | `nohup` fallback exists; harden the case where systemd appears available but launch fails. |
| Claude login inheritance | An explicit request can now find the standard Claude profile. | Intentional opt-in expansion; source and precedence are reported. |
| Binary downgrade | An old binary can mishandle new custom-network state. | Unsupported; destroy custom-network sandboxes first. |
| Existing custom networks | A reused network may not provide the expected isolation. | Open item; harnesses must generate fresh collision-resistant names. |
| Network-name validation | Names with dots, underscores, or slashes are rejected (DNS label, ≤63 chars, alphanumeric ends). | Intentional safety constraint. |
| `CS_SANDBOX_NETWORK` | A globally set default places new sandboxes onto that network unless `--network` overrides. | Documented behavior; existing sandboxes use persisted state. |
| Podman engine | Full runtime validation was not performed on the Firecracker development host. | Unit-covered; integration risk remains (see open items). |
| Host/image version skew | A rebuilt host binary does not update guest-side remote helpers under `image/rootfs`. | Rebuild the Firecracker image after guest-side changes and recreate test sandboxes. |

## Verification

Automated tests cover flag/environment precedence; network-name validation; state persistence and
old-state compatibility; Podman launch arguments and managed isolated-network creation;
network-scoped Firecracker keepalives, paths, taps, DNS records, and garbage collection;
persisted-network use across lifecycle commands; safe managed-network reclamation; the listing
changes and the stable `ls --json` inventory (incompatible with `--quiet`); remote status
classification including nonzero completion and missing-runner cases; durable cancellation markers
and cleanup; and Claude credential lookup precedence. The full Go test suite passes.

Live Firecracker validation used two concurrent campaigns (agent controller plus solo worker each)
and confirmed same-network DNS and controller-to-worker SSH; denial of cross-network DNS and raw-IP
connections; denial of outbound SSH from a solo worker; retained HTTPS internet access; correct
membership after stop/start; campaign B surviving campaign A's destruction; and automatic removal
of the final unused managed network. All disposable test resources were removed. Podman-engine
runtime testing was intentionally not run on this Ubuntu Firecracker host; the underlying netavark
isolation was exercised by the Firecracker VMs on the same bridges.

The `cs-campaign` repository additionally exercises this branch end to end through its gated live
suites (`make check-live`, `check-live-claude`, `check-live-claude-orch`), including
orchestrator-driven remote turns, cancellation/status semantics, and credential-canary archive
scans; see its `IMPL.md`.

## Non-goals

- A first-class `campaign` command or resource.
- Multiple network attachments for one sandbox.
- Inter-network routing.
- Blocking internet or explicitly allowed host services.
- Per-campaign SSH keys.
- Sharing the host Podman socket with campaign members.

## Open items and pre-release hardening

| Priority | Item | Notes |
|---:|---|---|
| 1 | ~~Verify a saved PID belongs to the expected session runner before cancellation signals it~~ | Resolved 2026-07-30: cancellation verifies the PID's cmdline references the per-session runner file before any signal; contract tests cover the kill and the stale-PID-bystander cases |
| 2 | ~~Fall back to `nohup` when `systemd-run --user` itself fails; survive process-group kills~~ | Resolved 2026-07-30: launch falls through to the detached path when `systemd-run` itself fails, and the fallback uses `setsid nohup` so callers that kill their tool call's process group (agent CLIs do) cannot take the runner with them; the runner's self-recorded PID is authoritative |
| 3 | ~~Reject or verify pre-existing custom networks that lack managed `isolate=true`~~ | Resolved 2026-07-30: `EnsureNetwork` fails closed — an existing custom network must inspect as `isolate=true` plus the managed label (unit tests plus a live refusal check); the default network keeps historical behavior |
| 4 | Run the complete campaign-network lifecycle on the Podman engine | Firecracker host covered the shared netavark bridges only |
| 5 | Add an upgrade/downgrade warning for custom-network instance state | Downgrade is silently unsupported today |
| 6 | Widen the 16-bit Firecracker tap-prefix hash and add collision detection | A collision is an availability problem (resource-name conflicts, conservative GC), not a route between bridges |
| 7 | Release this branch so dependents can pin a normal versioned dependency | `cs-campaign` currently pins a local build |
| 8 | Per-member provisioned agent logins | Cloned OAuth grants rotate and race (first refresher wins); the campaign harness works around this with test-only independent logins, but the product fix belongs here |
| 9 | ~~OpenCode binary, wrapper, remote tools, and credential support~~ | Resolved 2026-07-31: full adapter (pinned 1.18.10 image install, isolated `cs-opencode` profile wrapper, HTTP-API turn driver, remote family, seeding/doctor/install surface, contract tests), live-validated end to end including both `cs-campaign` roles. Design, verified upstream behaviors, evidence contract, and the version-bump procedure are in [docs/opencode.md](docs/opencode.md) — the persistent record that survives this file |
| 10 | A secret-typed injection API instead of general `--env` | Explicit secret intent, redaction, rotation, and inspection policy |
| 11 | Widen the 12-bit OpenCode turn-driver port hash and add collision detection | Fail-closed availability caveat, same spirit as item 6's tap-prefix hash; detailed in [docs/opencode.md](docs/opencode.md) |
