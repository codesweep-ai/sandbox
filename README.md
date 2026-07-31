# sandbox

> **Disposable, isolated Linux dev sandboxes (rootless Podman containers or Firecracker microVMs)
> for running AI coding agents.**

[![CI](https://github.com/codesweep-ai/sandbox/actions/workflows/ci.yml/badge.svg)](https://github.com/codesweep-ai/sandbox/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Engines](https://img.shields.io/badge/engines-podman%20%C2%B7%20firecracker-informational)
![Platforms](https://img.shields.io/badge/platform-Linux%20%C2%B7%20macOS-lightgrey)

`cs-sandbox` is a self-contained CLI that creates and manages these sandboxes. Each one is a rootless
Linux environment built from a single image, with a modern toolchain and the **Claude Code**,
**Codex** & **OpenCode** agents preinstalled. Spin up many named sandboxes, reach each by name over SSH, and share
**only** the repos or directories you choose. Nothing on your host is shared unless you ask — not
your files, not your SSH keys.

By default your agent logins aren't shared either — but a sandbox can easily inherit them
when you want it to, with `--inherit-agent-login`. That's usually the convenient choice: it saves you
logging in inside the sandbox. Some of the walkthroughs below show it. The loop is
**create → work → fetch → destroy**.

<p align="center">
  <img alt="cs-sandbox: create → work → fetch → destroy" src="docs/demo.gif" width="760">
</p>

<p align="center"><sub><i>Walkthrough&nbsp;1, end to end. Source cast: <a href="docs/demo.cast">docs/demo.cast</a> (<code>asciinema play docs/demo.cast</code>).</i></sub></p>

## Contents

- [Quickstart](#quickstart)
- [How it fits together](#how-it-fits-together)
- [What's in a sandbox](#whats-in-a-sandbox) · toolchain, agents, and the agent tools
- [Before you start](#before-you-start) · one-time install + login ([INSTALL.md](INSTALL.md))
- [Walkthroughs](#walkthroughs) · the fastest way to get a feel for it
- [Choosing an engine](#choosing-an-engine-podman-vs-firecracker)
- [SSH trust](#ssh-trust) · [Security model](#security-model) · [Docs](#docs)
- [Contributing](#contributing)

## Quickstart

One-time host setup (the binary, Podman, `cs-sandbox build`, and a Claude or Codex login if you want
sandboxes to inherit it) is in **[INSTALL.md](INSTALL.md)** — then the whole loop is:

```bash
# Create a sandbox named "feature": share the ~/projects/api repo into it, and
# carry your host Claude login in so the agent inside is already logged in.
cs-sandbox create feature --repo ~/projects/api --inherit-agent-login claude
ssh feature                                        # shell in by name
[feature]$ cd ~/api && cs-claude                   # run the agent — logged in; then exit
cs-sandbox fetch feature                           # pull the agent's commits back to the host
cs-sandbox destroy feature -f                      # throw the whole sandbox away
```

The [Walkthroughs](#walkthroughs) below unpack this and the rest (user sandboxes, `--yolo --solo`,
containers inside a sandbox, port forwarding, agents driving agents).

## How it fits together

You drive everything from your host with the `cs-sandbox` CLI. Each sandbox runs on one of two
engines (a Podman container or a Firecracker microVM). By default they join one shared network; use
`--network` to create isolated groups whose members remain reachable to each other by name. You
share data into a sandbox explicitly, as a git repo
or a frozen snapshot, and pull commits back out. The diagram below traces that: the
host on top, the shared network holding the sandboxes, and how data moves in and out.

```
   your host:  cs-sandbox CLI  +  cs-claude / cs-codex   (logged in once)
               create · fetch · push · forward · host-route
                                   │
                        ssh <name> │ forward · host-route
                                   ▼
   ┌──────────────── shared rootless network ─────────────────────┐
   │            sandboxes reach each other by name, any engine     │
   │                                                               │
   │   ┌─ box-a ──────────────┐       ┌─ box-b ──────────────┐     │
   │   │ Podman container     │  ssh  │ Firecracker microVM  │     │
   │   │ shares host kernel   │ ◀───▶ │ own guest kernel     │     │
   │   │ cs-claude / cs-codex │       │ cs-claude / cs-codex │     │
   │   │ ~/<repo>  (opt-in)   │       │ ~/<repo>  (opt-in)   │     │
   │   └──────────────────────┘       └──────────────────────┘     │
   └───────────────────────────────────────────────────────────────┘

   host data in:  --repo · --snapshot        commits out:  fetch
```

Every sandbox runs from one generic image (no identity baked in; your user is created at first
boot). A shared rootless network joins them all, so any sandbox reaches any other **by name**, across
engines. The host reaches a sandbox by name over SSH, and a port inside a sandbox via `forward` or the
optional `host-route`.

## What's in a sandbox

Every sandbox boots from the same image, so there is nothing to install inside one:

- **A broad dev toolchain** — git, Podman, Node, Python, Go, Java, Neovim, tmux, and the usual CLI
  helpers (ripgrep, fd, fzf, bat, jq/yq, gh, uv). Full list in
  [`image/Containerfile`](image/Containerfile).
- **The Claude Code, Codex, and OpenCode agents**, with **`cs-claude` / `cs-codex` /
  `cs-opencode`** — small wrappers that launch each agent on a ready-to-use, sandbox-local
  configuration. Each agent gets its own profile directory (never your personal `~/.claude` /
  `~/.codex` / `~/.config/opencode`), sane permission defaults, and the working directory
  pre-trusted, so it starts working instead of asking setup questions. Run `cs-claude`
  rather than `claude` and it's configured.
- **Remote agent tools** — `cs-claude-remote`, `cs-codex-remote`, and `cs-opencode-remote` (each with `-status`,
  `-output`, `-sessions`, `-forget` companions). These tools start or resume an agent session on
  *another* sandbox over SSH, keep it warm, and hand back its output — so an agent working in one
  sandbox can give a task to an agent in another
  ([walkthrough 7](#7-let-one-coding-agent-drive-another)).

The same tools run on your host too: `cs-sandbox install-agent-tools` puts them on your PATH, which
is how you log in once on the host so sandboxes can inherit that login.

## Before you start

The walkthroughs assume the one-time host setup in **[INSTALL.md](INSTALL.md)** — install the
`cs-sandbox` binary and its prerequisites, run `cs-sandbox build`, and log in once to Claude Code and
Codex on the host. It's a handful of commands, and `cs-sandbox doctor` checks every prerequisite and
prints the fix for anything missing.

**About the agent login.** A sandbox does *not* get your Claude or Codex login by default. Some of the
walkthroughs below pass `--inherit-agent-login claude` (or `codex`/`opencode`), which copies that host
login into the sandbox as it is created — the convenient choice we normally make, since it saves
logging in inside every sandbox. When you'd rather keep a sandbox on its own account, leave the flag
off and log in inside it with `cs-sandbox agent-login claude <name>`. See
[agent login](docs/agent-login.md).

## Walkthroughs

Each block is runnable end to end (5 and 6 continue from 4). Skim the comments to get the gist; run
them when you want to play.

### 1. Share two repos, let an agent edit one, pull the changes out

Two repos go into one agent sandbox; the agent edits one, and you fetch its commits back.

```bash
# Each repo lands at ~/<basename> on its own branch  cs-sandbox/<sandbox-name>.
# --inherit-agent-login carries your host Claude login into the sandbox.
cs-sandbox create feature --repo ~/projects/api --repo ~/projects/web --inherit-agent-login claude

# shell in, by name
ssh feature
# Launch Claude - logged in, because we inherited the host login above.
[feature]$ cd ~/api && cs-claude
# then type a prompt like "add a /version endpoint and commit when done"
# ...let it work, then exit Claude
[feature]$ exit

# Pull the agent's commits back to the host (its branch: cs-sandbox/feature).
cs-sandbox fetch feature

# Done with it? Throw the whole sandbox away (-f skips the confirmation prompt).
cs-sandbox destroy feature -f
```

### 2. Fetch an agent's work into a user sandbox

Same idea as walkthrough 1, but the destination is a **user sandbox** you drive rather than the host.
A user sandbox sits a layer above and can `ssh` into agent sandboxes, so it fetches a peer's branch
with plain git — no host round-trip.

```bash
# An agent sandbox does the work (its branch: cs-sandbox/worker).
cs-sandbox create worker --repo ~/projects/api --inherit-agent-login claude
ssh worker
[worker]$ cd ~/api && cs-claude    # "add a /health endpoint and commit when done", then exit
[worker]$ exit

# A user sandbox with the same repo — your workspace to review the agent's work.
cs-sandbox create dev --type user --repo ~/projects/api
ssh dev
[dev]$ cd ~/api
# Fetch the agent's branch straight from the agent sandbox, by name.
[dev]$ git fetch worker:api cs-sandbox/worker
[dev]$ git log --oneline FETCH_HEAD    # the agent's commits; merge/cherry-pick as usual
[dev]$ exit

cs-sandbox destroy worker -f && cs-sandbox destroy dev -f
```

### 3. Run a throwaway experiment (`--yolo --solo`)

A disposable playground: the agent builds and runs a small HTTP API that you then hit from inside the
sandbox. It's an experiment, so we let the agent work without stopping for approvals (**`--yolo`**)
and make sure it can't reach our other sandboxes (**`--solo`**) — the sandbox itself is the boundary.
Here we also skip `--inherit-agent-login`, so the sandbox needs its own Claude login instead of
sharing yours.

```bash
#   --yolo  agent works with no approval prompts (the sandbox is the boundary)
#   --solo  agent can't SSH out to your other sandboxes (it stays reachable)
#   no --inherit-agent-login, so this sandbox starts with no Claude login
cs-sandbox create lab --yolo --solo

# Log it in: launches the agent inside the sandbox so you can complete its login.
cs-sandbox agent-login claude lab
# ...follow Claude's prompts, then exit. The login stays in this sandbox only.

ssh lab
[lab]$ cs-claude
# then type a prompt like "run a tiny HTTP API on port 8000 in the background"
# ...let it work, then exit Claude

# Hit the API from inside the sandbox.
[lab]$ curl http://localhost:8000
```

### 4. Run an app in a container, inside the sandbox

A sandbox can run Podman containers of its own, and it just works — nothing to set up, no flags.

```bash
cs-sandbox create web

ssh web
# Run stock nginx in a container inside the sandbox, on its port 8080.
[web]$ podman run -d -p 8080:80 docker.io/library/nginx
# Hit it from inside the sandbox (nginx's welcome page).
[web]$ curl http://localhost:8080
[web]$ exit
# That's Podman nested inside Podman, or Podman inside a Firecracker microVM,
# depending on the engine - both are already set up for you.
```

### 5. Reach a sandbox port from the host with `forward`

A port a sandbox binds is private to it. `cs-sandbox forward` maps one of your host's ports onto a
port inside a sandbox, so you can reach the service from your own browser or terminal. Continuing
from walkthrough 4 (the `web` sandbox running nginx):

```bash
# host :9000  ->  web's :8080
cs-sandbox forward web 9000:8080
curl http://localhost:9000

# See what's wired up, then tear the forward down.
cs-sandbox forwards web
cs-sandbox unforward web all
```

### 6. Reach a sandbox by name from the host with `host-route`

Instead of mapping ports one at a time, `host-route` makes **every** port bound inside a sandbox
reachable from the host at `<name>.cs.sandbox`. Setting the route up asks for `sudo` once
(`host-route up`); after that, creating sandboxes and reaching their ports needs none. Optional and
Linux-only. It publishes only members of the default `cs-sandbox-net`; isolated `--network` groups
remain reachable through SSH and explicit `forward`. Continuing from walkthrough 4:

```bash
cs-sandbox host-route up                 # sets up the host route; asks for sudo, once

# Reach web's nginx from the host by name, on whatever port it bound.
curl http://web.cs.sandbox:8080

cs-sandbox host-route down               # remove it when done
cs-sandbox destroy web -f                # done with the walkthrough-4 sandbox
```

### 7. Let one coding agent drive another

The [agent tools](#whats-in-a-sandbox) in every sandbox include `cs-claude-remote` and
`cs-codex-remote`: they start or resume an agent session on another sandbox over SSH, keep it warm,
and hand back its output. That's how you orchestrate several agent tasks running in different
sandboxes — one agent driving the others, which is what this walkthrough does.

```bash
# Two agent sandboxes. Agents can SSH to each other
# (but never into your user sandboxes).
cs-sandbox create driver --inherit-agent-login claude,codex
# worker holds the repo to work on
cs-sandbox create worker --repo ~/projects/api --inherit-agent-login codex

ssh driver
[driver]$ cs-claude
# then type a prompt like:
#   "Run a codex session on worker to add a /health route and run it"
#
# That's all - Claude knows the remote tooling. Under the hood it runs these
# on driver (you can run them by hand too, to see the mechanism):
#   cs-codex-remote --new --name add-health -H worker "add /health, run it"
#   cs-codex-remote-output add-health   # see what codex did on worker
```

> Each sandbox above inherited the host login it needs (`--inherit-agent-login`), so Codex on
> `worker` is already logged in.
> `cs-claude-remote` is the mirror of `cs-codex-remote`; either agent can drive either, on any host.

## Choosing an engine: Podman vs Firecracker

Both engines work the same way for almost everything: same image, trust model, sharing flags, and
network behavior. They differ mostly in isolation versus weight. Pick with `--engine podman|firecracker`.

| | **Podman container** | **Firecracker microVM** |
|---|---|---|
| Isolation | shares the host kernel, scaled-down capabilities | **own kernel**, hardware virtualization |
| Root inside | rootful-in-userns (sudo wrapper) | **real root** |
| Nested Podman | via a rootful-inside wrapper | native |
| Requires | Podman | `/dev/kvm`, Linux x86_64 |
| Default on | macOS / non-KVM hosts | Linux + KVM |
| **Reach for it when** | speed, macOS | stronger isolation, untrusted workloads, nested root |
| Deep dive | [`docs/podman.md`](docs/podman.md) | [`docs/firecracker.md`](docs/firecracker.md) |

## SSH trust

Every sandbox runs an SSH server, so you reach each one by name. What a sandbox can reach in turn
depends on its **type**, set with `--type` (independent of engine): you (and your user sandboxes) can
`ssh` into either type, but an agent sandbox can never `ssh` in as you.

- **user sandbox** (`--type user`): yours, to work in interactively. It has its own home and can
  `ssh` into **every** sandbox.
- **agent sandbox** (default): one you hand to a coding agent. It has its own home and can `ssh` only
  into **other agent sandboxes**, never into a user sandbox.

It's convenient to think of these as **two layers**: a user sandbox can `ssh` into any sandbox (user
or agent), while an agent sandbox can `ssh` only into other agent sandboxes — never back into a user
sandbox. Aside from that SSH direction the two types are identical (same image, same capabilities).
Typically you spawn one user sandbox and oversee the work running across several agent sandboxes from
there.

| client ↓ \ server → | user sandbox | agent sandbox |
|---|:---:|:---:|
| **host** | ✓ | ✓ |
| **user** sandbox | ✓ | ✓ |
| **agent** sandbox | ✗ | ✓ |

So you and your user sandboxes reach everything, but a coding agent can never `ssh` into a user
sandbox.

### Lending a sandbox specific SSH keys with `ssh -A`

Sometimes a sandbox needs to reach another machine over SSH — run a tool on a remote test box, copy
files with `scp`. `ssh -A <name>` lends it a specific set of your host keys for the length of that
one session: the keys stay on the host, and nothing is copied into the sandbox. It's opt-in and
independent of sandbox type. Lend deliberately — two conditions to keep in mind:

- **Scope the keys.** Load only the key you need into your agent (`ssh-add -c` prompts on the host
  for each use), rather than exposing your whole keyring.
- **Trust the operator.** Only forward into a sandbox whose operator you trust for the duration of
  the session, because while you're connected anything running as you there can *use* the forwarded
  socket (it can't copy the key out).

## Security model

- **The boundary is the engine.** A Podman container shares the host kernel; a Firecracker microVM
  boots its own kernel under hardware virtualization, the stronger choice for untrusted or
  autonomous work.
- **Nothing shared by default.** Host data enters a sandbox only through `--repo` (a git checkout)
  or `--snapshot` (a frozen read-only copy). Results come back out with `cs-sandbox fetch`.
- **Your agent logins are not shared either**, unless you name one: `create
  --inherit-agent-login claude` copies that login in, and `create` prints what the sandbox ended up
  with. Without it the sandbox has no agent login — log in inside it, on its own account if you
  prefer. Provider API keys are never copied at all; pass one with `--env` when a sandbox needs it.
- **No host SSH keys in any sandbox.** Neither type receives a copy of your host keys; sandboxes
  reach each other with generated per-tier keys. If a sandbox ever needs your own keys, you can lend
  a specific set for a session ([`ssh -A`](#lending-a-sandbox-specific-ssh-keys-with-ssh--a)) — they
  stay on the host.
- **Agent/user SSH isolation.** Per-type [SSH trust](#ssh-trust) (the "two layers, user above agent"
  model) is enforced: a user sandbox can `ssh` into any sandbox, but an agent sandbox reaches only
  other agent sandboxes — never a user sandbox — so an agent can't pivot through SSH into your
  workspace.
- **`--yolo`** (agent sandboxes) drops the agents' approval prompts, safe because the sandbox itself
  is the isolation boundary. **`--solo`** additionally denies the agent any *outbound* SSH into the
  fabric, while keeping it reachable for you to drive.
- It is not a hardened multi-tenant boundary; isolation is whatever the chosen engine provides.

## Docs

- [`INSTALL.md`](INSTALL.md): one-time host setup. Podman, the Firecracker/KVM prerequisites,
  building the image, installing the agent tools, and the agent login.
- [`docs/design.md`](docs/design.md): the cross-engine model (types & trust, networking, the generic
  image, the seed, shared image stores, agent tools & login, security).
- [`docs/podman.md`](docs/podman.md): the Podman container engine (boot, nested Podman, storage,
  macOS, private registry).
- [`docs/firecracker.md`](docs/firecracker.md): the Firecracker microVM engine.
- [`docs/repo-sharing.md`](docs/repo-sharing.md): the `--repo` checkout / fetch / push model.
- [`docs/agent-login.md`](docs/agent-login.md): how a sandbox gets a logged-in agent, and what is never copied.

`cs-sandbox help` is the full command reference.

## Contributing

Bug reports and pull requests are welcome. Run `make check` (formatting, `go vet`, unit tests)
before opening one. For a security-sensitive issue, please ask for a private contact rather than
posting details in a public issue.

**Testing.** `make test` runs the unit suite; `make test-integration` runs the live tests against a
real podman/Firecracker host (each skips gracefully when podman or the image is unavailable). See
[design.md → Testing](docs/design.md#testing).

## License

[Apache-2.0](LICENSE).
