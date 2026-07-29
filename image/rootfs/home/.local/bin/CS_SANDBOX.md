# Managing Dev Sandboxes with cs-sandbox

`cs-sandbox` creates and manages disposable Linux dev sandboxes — rootless **Podman containers** or
**Firecracker microVMs** — that all join one shared network, so each is reachable by name over SSH.
Nothing on the host is shared unless you ask: code goes in through `--repo` / `--snapshot`, and
commits come back out with `fetch`. The loop is **create → work → fetch → destroy**.

Every sandbox already carries a broad dev toolchain, the Claude Code and Codex agents with their
`cs-claude` / `cs-codex` launch wrappers (ready-to-use, sandbox-local agent config), and the
`cs-claude-remote` / `cs-codex-remote` tools for running an agent session on another sandbox.

> **This is a host tool.** `cs-sandbox` is not installed *inside* sandboxes. If it is not on PATH,
> you are probably already inside one — reach peers with plain `ssh <name>` and move commits with
> plain `git` (see "From inside a sandbox" below) rather than looking for this CLI.

## How to use

```bash
# Create: name + optional shares. Default type is agent; engine defaults to
# firecracker on Linux/KVM, else podman.
cs-sandbox create feature --repo ~/projects/api        # repo lands at ~/api on branch cs-sandbox/feature
cs-sandbox create feature --repo ~/projects/api --inherit-agent-login claude   # ...agent logged in
cs-sandbox create dev --type user --repo ~/projects/api
cs-sandbox create lab --yolo --solo                    # throwaway playground, no outbound ssh
cs-sandbox create web --engine podman --snapshot ~/data # frozen read-only copy at ~/data

ssh feature                     # shell in by name (preferred for interactive work)
cs-sandbox exec feature ls      # run one command instead
cs-sandbox ls                   # what exists: STATUS (running/stopped/removed), AGE, type, engine
cs-sandbox ls -q                # names only, one per line — pipe it into other commands
cs-sandbox port feature         # its host SSH port, if a tool needs it (ssh <name> does not)

cs-sandbox fetch feature        # pull the sandbox's commits back to the host (fast-forward only)
cs-sandbox push feature         # send host commits in (fast-forward, clean tree)

cs-sandbox stop feature         # keep everything, shut it down
cs-sandbox start feature        # bring it back
cs-sandbox rm feature           # remove the sandbox, KEEP its data (recreate to reuse)
cs-sandbox destroy feature -f   # delete the sandbox AND its data (-f skips the prompt)
```

Data kept by `rm` stays listed by `ls` with STATUS `removed`, so it can't sit on disk unnoticed:
`create` with the same name reuses it, `destroy <name> -f` deletes it.

Names must be a single DNS-style label: letters, digits and dashes, starting and ending
alphanumeric, 63 characters max. Dots are rejected (a dotted name would not resolve as a peer).

## Sandbox types — what each can reach

Set with `--type` (independent of engine). Think of it as **two layers, user above agent**:

- **agent** (default): hand it to a coding agent. It can `ssh` only into *other agent* sandboxes.
- **user** (`--type user`): your own workspace. It can `ssh` into **every** sandbox.

You and your user sandboxes reach everything; an agent can never `ssh` into a user sandbox. No host
SSH keys are copied into either type.

- `--yolo` drops the agents' approval prompts (the sandbox is the boundary).
- `--solo` additionally denies an agent *any* outbound SSH, while leaving it reachable. Agent only.

## Sharing code in, getting commits out

- `--repo PATH[@REF][:NAME]` — a per-sandbox git checkout at `~/<name>` on branch
  `cs-sandbox/<sandbox>`, borrowing the source's objects read-only. Repeatable.
- `--snapshot PATH[:NAME]` — a frozen, read-only copy of any directory at `~/<name>`. Repeatable.
- `cs-sandbox fetch <name> [dir]` / `push <name> [dir]` — move commits host↔sandbox, fast-forward
  only. `[dir]` picks one repo when several are shared.

**Fetch before you destroy** — `destroy` deletes the sandbox's commits with its data.

## Reaching a port

A port bound inside a sandbox is private to it. Map one onto a host port with `forward`, or make them
all reachable by name with `host-route`:

```bash
cs-sandbox forward web 9000:8080   # map host :9000 onto the port :8080 bound inside web
cs-sandbox forward web --socks=1080 # SOCKS proxy into the sandbox (note the '=')
cs-sandbox forwards web             # list active forwards
cs-sandbox unforward web all        # tear them down

# host-route makes EVERY port a sandbox binds reachable from the host at <name>.cs.sandbox.
# Optional, Linux-only; setting the route up asks for sudo once, nothing after that.
cs-sandbox host-route up
curl http://web.cs.sandbox:8080
cs-sandbox host-route down
```

## Shared image stores

Reuse one image set across sandboxes instead of pulling per sandbox:

```bash
cs-sandbox create-store base
cs-sandbox seed-store base docker.io/library/nginx    # or --from-host to copy an image you already have
cs-sandbox stores                                     # list stores + their images
cs-sandbox create web --image-store base              # mount it read-only
cs-sandbox rm-store base -f
```

## From inside a sandbox (no cs-sandbox CLI there)

A **user** sandbox can fetch a peer **agent** sandbox's work directly with plain git — same
fast-forward rules, no host round-trip:

```bash
git fetch worker:api cs-sandbox/worker     # <agent>:<dir> is scp-style; <dir> is relative to ~
git log --oneline FETCH_HEAD
git push worker:api HEAD:cs-sandbox/worker # the other direction
```

## Interpreting user intent

| User says | What to do |
|---|---|
| "spin up a sandbox for this repo" / "give the agent a sandbox with X" | `cs-sandbox create <name> --repo <path> --inherit-agent-login claude` (drop the flag if they want it without a login); report the name and that `ssh <name>` works |
| "it should already be logged in" / "don't make me log in again" | add `--inherit-agent-login claude` (or `codex`, or both) at create |
| "make me a workspace I can drive" / "a user sandbox" | `cs-sandbox create <name> --type user --repo <path>` |
| "throwaway / no prompts / let it rip" | `cs-sandbox create <name> --yolo` (add `--solo` to also deny outbound ssh) |
| "stronger isolation" / "untrusted work" | `--engine firecracker` (Linux + `/dev/kvm` only) |
| "what sandboxes do I have?" / "is it still running?" | `cs-sandbox ls` — the STATUS column says `running`, `stopped`, or `removed` (data an `rm` kept) |
| "get its changes" / "pull the work back" | `cs-sandbox fetch <name>`, then report the branch |
| "send my commits in" | `cs-sandbox push <name>` |
| "I need to hit its port 8080" | `cs-sandbox forward <name> 8080` (or `HOSTPORT:VMPORT`) |
| "clean it up" / "I'm done with it" | Ask whether data should be kept: `rm` keeps it, `destroy -f` deletes it |
| "free up the disk" / "what's still taking space?" | `cs-sandbox ls` — anything with STATUS `removed` is data only; `destroy <name> -f` reclaims it |
| "get rid of all of them" | Confirm first, then `cs-sandbox ls -q \| xargs -n1 cs-sandbox destroy -f` |
| "pause it" / "free the resources" | `cs-sandbox stop <name>` (then `start` later) |
| "why doesn't this work?" / "check my setup" | `cs-sandbox doctor` (add `--engine podman` to check that engine) |
| "set it up" / "first run" | `cs-sandbox build`, then `cs-sandbox install-agent-tools` |

## When to use proactively

- The user wants an agent to work on a repo without touching their working tree — create a sandbox
  with `--repo` instead of editing in place.
- A task needs to run something risky, install packages, or run containers — put it in a sandbox
  rather than on the host.
- Before `destroy`, check for unfetched commits and offer `fetch` first.
- After creating, tell the user the `ssh <name>` shortcut; it is friendlier than `cs-sandbox exec`.

## Notes

- **`destroy` is irreversible** (it deletes the home volume / rootfs). Confirm before running it,
  and prefer `rm` when the data might still be wanted — recreating with the same name reuses it.
- Firecracker needs Linux, `/dev/kvm` and x86_64; on macOS or a non-KVM host sandboxes use Podman
  automatically. `--cpus` (default 4) and `--mem` MiB (default 4096) apply to Firecracker only.
- On macOS everything runs in one podman-machine VM, and `--repo` / `--snapshot` sources must live
  under `$HOME` — `create` rejects paths outside it.
- The agent login is **not** inherited by default — a sandbox starts with none. Pass
  `--inherit-agent-login claude` (or `codex`, or both comma-separated) at create to carry the host
  login in; that is the usual choice, since otherwise someone has to log in inside every
  sandbox. `create` reports what the sandbox ended up with. Without the flag, log it in with
  `cs-sandbox agent-login claude <name>`, which is also how you give a sandbox its own account
  instead of sharing yours.
- Provider API keys are never carried. Pass one explicitly with `--env ANTHROPIC_API_KEY`, and use
  `--snapshot` plus `--env` for a credential file.
- Global flags: `-v` (per-command progress), `-q` (silence), `--dry-run` (print commands instead of
  running them — useful to show a user what would happen).
