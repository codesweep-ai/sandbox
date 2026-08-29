# sandbox

> **Disposable, isolated Linux dev sandboxes (rootless Podman containers or Firecracker microVMs)
> for running AI coding agents.**

[![CI](https://github.com/codesweep-ai/sandbox/actions/workflows/ci.yml/badge.svg)](https://github.com/codesweep-ai/sandbox/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Engines](https://img.shields.io/badge/engines-podman%20%C2%B7%20firecracker-informational)
![Platforms](https://img.shields.io/badge/platform-Linux%20%C2%B7%20macOS%20%C2%B7%20Windows%20%28WSL2%29-lightgrey)

`cs-sandbox` is a self-contained CLI that creates and manages these sandboxes. Each one is a
rootless Linux environment built from a single image, with a modern toolchain and the **Claude
Code**, **Codex** & **OpenCode** agents preinstalled. Spin up many named sandboxes, reach each by
name over SSH, and share **only** the repos or directories you choose. Nothing on the host is shared
unless you ask: not your files, not your SSH keys, and not the LLM provider keys you pay with.

Agents inside a sandbox are given loan tokens, which a proxy outside it exchanges for the real
credentials. An autonomous agent can work all day without any credential of yours.

The loop is **create → work → fetch → destroy**.

<p align="center">
  <img alt="cs-sandbox: create → work → fetch → destroy" src="docs/demo.gif" width="760">
</p>

<p align="center"><sub><i>Walkthrough&nbsp;1, end to end. Source cast: <a href="docs/demo.cast">docs/demo.cast</a> (<code>asciinema play docs/demo.cast</code>).</i></sub></p>

## Contents

- [Quickstart](#quickstart)
- [How it fits together](#how-it-fits-together)
- [What's in a sandbox](#whats-in-a-sandbox) · toolchain, agents, and the agent tools
- [Agent credentials](#agent-credentials) · lending your logins and keys
- [Before you start](#before-you-start) · one-time install + login ([INSTALL.md](INSTALL.md))
- [Walkthroughs](#walkthroughs) · the fastest way to get a feel for it
- [Choosing an engine](#choosing-an-engine-podman-vs-firecracker)
- [Who can SSH into what](#who-can-ssh-into-what) · [Security model](#security-model) · [Docs](#docs)
- [Contributing](#contributing)

## Quickstart

One-time host setup (the binary, Podman, `cs-sandbox build`, and a Claude, Codex, or OpenCode login
for sandboxes to borrow) is in [INSTALL.md](INSTALL.md). Then the whole loop is:

```bash
# Create a sandbox named "feature": share the ~/projects/api repo into it, and
# lend it your host Claude login, which never enters the sandbox.
cs-sandbox create feature --repo ~/projects/api --lend-agent-login claude
ssh feature                                        # shell in by name
[feature]$ cd ~/api && cs-claude                   # run the agent, logged in; then exit
cs-sandbox fetch feature                           # pull the agent's commits back to the host
cs-sandbox destroy feature -f                      # throw the whole sandbox away
```

The agent in the sandbox is operating as you but holds no credential of yours.

The [Walkthroughs](#walkthroughs) below unpack this and the rest (user sandboxes, `--yolo --solo`,
containers inside a sandbox, port forwarding, agents driving agents).

## How it fits together

You drive everything from your host with the `cs-sandbox` CLI. The diagram traces the shape: the
host on top, the shared network holding the sandboxes, and how data moves in and out.

```
   your host:  cs-sandbox CLI  +  cs-claude / cs-codex / cs-opencode
               create · fetch · push · forward · host-route      (log in once here)
                                   │
                        ssh <name> │ forward · host-route
                                   ▼
   ┌──────────────────── shared rootless network ────────────────────┐
   │          sandboxes reach each other by name, any engine         │
   │                                                                 │
   │   ┌─ box-a ───────────────┐       ┌─ box-b ───────────────┐     │
   │   │ Podman container      │  ssh  │ Firecracker microVM   │     │
   │   │ shares host kernel    │ ◀───▶ │ own guest kernel      │     │
   │   │ agent CLIs + wrappers │       │ agent CLIs + wrappers │     │
   │   │ ~/<repo>   (opt-in)   │       │ ~/<repo>   (opt-in)   │     │
   │   └───────────────────────┘       └───────────────────────┘     │
   └─────────────────────────────────────────────────────────────────┘

   host data in:  --repo · --snapshot        commits out:  fetch
```

Every sandbox runs from one generic image, with no identity baked in: your user is created at
first boot. A shared rootless network joins them, so any sandbox reaches any other **by name**,
across engines. The host reaches a sandbox by name over SSH, and a port inside one via `forward` or
the optional `host-route`.

That network belongs to a **group**, and without `--group` every sandbox joins one called
`default`, which is why they all see each other above. If you ever need two experiments on one host
that must *not* interfere, [walkthrough 8](#8-run-two-isolated-experiments-with---group) shows how.
Until then you can ignore groups entirely.

## What's in a sandbox

Every sandbox boots from the same image, so there is nothing to install inside one:

- **A broad dev toolchain**: git, Podman, Node, Python, Go, Java, Neovim, tmux, and the usual CLI
  helpers (ripgrep, fd, fzf, bat, jq/yq, gh, uv). Full list in
  [`image/Containerfile`](image/Containerfile).
- **The Claude Code, Codex, and OpenCode agents**, with **`cs-claude` / `cs-codex` /
  `cs-opencode`**, wrappers that launch each on a sandbox-local profile (never your personal
  `~/.claude` / `~/.codex` / `~/.config/opencode`), with sane permission defaults and the working
  directory pre-trusted. Run `cs-claude` rather than `claude` and it starts working instead of
  asking setup questions.
- **Remote agent tools**: `cs-claude-remote`, `cs-codex-remote` and `cs-opencode-remote`, each
  with `-status`, `-output`, `-sessions` and `-forget` companions. They start or resume an agent
  session on *another* sandbox over SSH, keep it warm, and hand back its output. That is how one
  agent gives a task to another ([walkthrough 7](#7-let-one-coding-agent-drive-another)).

The same tools run on your host too: `cs-sandbox install-agent-tools` puts them on your PATH, which
is how you log in once on the host. Sandboxes borrow that login without ever holding it.

## Agent credentials

An agent needs a credential to call a model. Lending is the recommended way to give it one: the
agent cannot leak your login or your LLM provider key, because it never sees either.

```bash
cs-sandbox create feature --repo ~/projects/api --lend-agent-login claude
```

What lands in the sandbox is a **loan token**, never your credential. The real one stays on your
host, held by the **lender**, a small proxy this tool runs for you. Every model call the agent makes
arrives at the lender carrying its token, and gets the real credential attached there before going
on to the provider. The agent runs exactly as it would signed in.

You can lend an agent login, or an LLM API key:

| Flag | What it lends |
|---|---|
| `--lend-agent-login claude` | Your `claude` or `codex` login |
| `--lend-api-key anthropic` | Your `anthropic`, `openai` or `fireworks` key |

Keys live in `~/.cs-keys`, one file per provider:

```bash
mkdir -p ~/.cs-keys && printf %s "$ANTHROPIC_API_KEY" > ~/.cs-keys/anthropic && chmod 600 ~/.cs-keys/anthropic

cs-sandbox create feature --repo ~/projects/api --lend-api-key anthropic
```

Sharing is yours to choose too. `--inherit-agent-login` and `--inherit-api-key` copy the real
credential into the sandbox. A sandbox can also hold its own account, with `cs-sandbox agent-login
claude <name>`.

`cs-sandbox ls` marks each sandbox `lent` or `held`, and `create` prints which one you got.
[MANUAL.md](MANUAL.md#lending-a-credential) has the whole surface.

## Before you start

The walkthroughs assume the one-time host setup in [INSTALL.md](INSTALL.md). Install the
`cs-sandbox` binary and its prerequisites, run `cs-sandbox build`, then log in once to Claude Code,
Codex or OpenCode on the host. It is a handful of commands, and `cs-sandbox doctor` checks every
prerequisite and prints the fix for anything missing.

**About the agent login.** The walkthroughs below pass `--lend-agent-login claude`, which is the
recommended path: your host login stays on your host, and the sandbox never holds it.
[Agent credentials](#agent-credentials) covers the alternatives.

## Walkthroughs

Each block is runnable end to end (5 and 6 continue from 4; 8 stands alone). Substitute your own
checkout for `~/projects/api` and the other repo paths, which stand in for repos you already have.
Skim the comments to get the gist; run them when you want to play.

### 1. Share two repos, let an agent edit one, pull the changes out

Two repos go into one agent sandbox; the agent edits one, and you fetch its commits back.

```bash
# Each repo lands at ~/<basename> on its own branch  cs-sandbox/<sandbox-name>.
# --lend-agent-login lends your host Claude login without putting it in the sandbox.
cs-sandbox create feature --repo ~/projects/api --repo ~/projects/web --lend-agent-login claude

# shell in, by name
ssh feature
# Launch Claude - logged in, on the login we lent it above.
[feature]$ cd ~/api && cs-claude
# then type a prompt like "add a /version endpoint and commit when done"
# ...let it work, then exit Claude
[feature]$ exit

# Pull the agent's commits back to the host (its branch: cs-sandbox/feature).
cs-sandbox fetch feature

# Done with it? Throw the whole sandbox away (-f confirms; without it, destroy
# only prints what it would delete).
cs-sandbox destroy feature -f
```

### 2. Fetch an agent's work into a user sandbox

Same idea as walkthrough 1, but the destination is a **user sandbox** you drive rather than the host.
A user sandbox sits a layer above and can `ssh` into agent sandboxes, so it fetches a peer's branch
with plain git, and no host round-trip.

```bash
# An agent sandbox does the work (its branch: cs-sandbox/worker).
cs-sandbox create worker --repo ~/projects/api --lend-agent-login claude
ssh worker
[worker]$ cd ~/api && cs-claude    # "add a /health endpoint and commit when done", then exit
[worker]$ exit

# A user sandbox with the same repo: your workspace to review the agent's work.
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
sandbox. It is an experiment, so the agent works without stopping for approvals (**`--yolo`**) and
stays away from your other sandboxes (**`--solo`**). The sandbox itself is the boundary. This block
also lends nothing, so this one gets its own Claude login rather than borrowing yours.

```bash
#   --yolo  agent works with no approval prompts (the sandbox is the boundary)
#   --solo  agent can't SSH out to your other sandboxes (it stays reachable)
#   no --lend-agent-login, so this sandbox starts with no Claude login at all
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
[lab]$ exit

cs-sandbox destroy lab -f                # the login goes with it
```

### 4. Run an app in a container, inside the sandbox

A sandbox can run Podman containers of its own, and it just works: nothing to set up, no flags.

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
#
# The nested engine is rootless and runs as you, so a bind mount works the way
# you would expect: files an inner container writes come back owned by you.
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
reachable from the host at `<name>.cs.sandbox`. Setting the route up asks for `sudo` once, and
after that creating sandboxes and reaching their ports needs none. It is optional and Linux-only.
This block continues from walkthrough 4:

```bash
cs-sandbox host-route up                 # sets up the host route; asks for sudo, once

# Reach web's nginx from the host by name, on whatever port it bound.
curl http://web.cs.sandbox:8080

cs-sandbox host-route down               # remove it when done
cs-sandbox destroy web -f                # done with the walkthrough-4 sandbox
```

### 7. Let one coding agent drive another

Every sandbox carries `cs-claude-remote`, `cs-codex-remote` and `cs-opencode-remote`, part of the
[agent tools](#whats-in-a-sandbox). Each starts or resumes an agent session on another sandbox over
SSH, keeps it warm, and hands back its output. That's how you orchestrate several agent tasks
running in different sandboxes, with one agent driving the others. That is what this walkthrough
does.

```bash
# Two agent sandboxes. Agents can SSH to each other
# (but never into your user sandboxes).
cs-sandbox create driver --lend-agent-login claude,codex
# worker holds the repo to work on
cs-sandbox create worker --repo ~/projects/api --lend-agent-login codex

ssh driver
[driver]$ cs-claude
# then type a prompt like:
#   "Run a codex session on worker to add a /health route and run it"
#
# That's all - Claude knows the remote tooling. Under the hood it runs these
# on driver (you can run them by hand too, to see the mechanism):
#   cs-codex-remote --new --name add-health -H worker "add /health, run it"
#   cs-codex-remote-output add-health   # see what codex did on worker
[driver]$ exit

cs-sandbox fetch worker                  # the agent's commits, before they go
cs-sandbox destroy driver -f && cs-sandbox destroy worker -f
```

> Each sandbox above borrows the host login it needs (`--lend-agent-login`), so Codex on
> `worker` is already logged in.
> `cs-claude-remote`, `cs-codex-remote`, and `cs-opencode-remote` mirror each other; any agent can
> drive any of the three, on any host.

### 8. Run two isolated experiments with `--group`

Everything above lived in one group called `default`, and you never had to think about it. Reach for
`--group` when two experiments share your host and must **not** interfere. Each one needs the same
set of sandboxes, or the comparison is not fair.

```bash
# Two experiments comparing caching strategies. Each --group gets its own network,
# SSH keys and gateway, created on demand. Each holds the SAME two names, so
# the fixture is identical and only the approach differs.
cs-sandbox create api   --group cache-redis  --repo ~/projects/api --lend-agent-login claude
cs-sandbox create bench --group cache-redis  --repo ~/projects/bench
cs-sandbox create api   --group cache-memory --repo ~/projects/api --lend-agent-login claude
cs-sandbox create bench --group cache-memory --repo ~/projects/bench

# Identity is (group, name), and a bare name always means the default group,
# never "whichever group has it", so a reference can't change meaning later.
cs-sandbox exec api.cache-redis  pwd
cs-sandbox exec api.cache-memory pwd     # same name, a different sandbox
cs-sandbox exec api pwd                  # error, and it names both candidates

# Inside a group, members still reach each other by bare name, and bench always
# reaches its OWN api, which is the whole point: neither run taints the other.
ssh bench.cache-redis
[bench]$ ssh api hostname                # prints "api": cache-redis's, not the other one
[bench]$ ssh api.cache-memory            # ssh: Could not resolve hostname
[bench]$ exit

# Each group's gateway is its ssh jump host, reaching members by name on any port.
ssh cache-redis-gw                       # a shell inside that experiment
ssh -L 8080:api:8000 cache-redis-gw      # forward that experiment's api, once it serves

cs-sandbox group rm cache-redis -f       # -f destroys the group's sandboxes too
cs-sandbox group rm cache-memory -f
```

> Groups are an isolation boundary rather than just a namespace. Members of different groups get no
> DNS for one another, no route between their networks, and no SSH key the other side would accept.
> The full model is in [SPEC.md](SPEC.md#5-groups).

## Choosing an engine: Podman vs Firecracker

Both engines work the same way for almost everything: same image, trust model, sharing flags, and
networking. They differ mostly in isolation versus weight. Pick with `--engine podman|firecracker`.

| | **Podman container** | **Firecracker microVM** |
|---|---|---|
| Isolation | shares the host kernel, scaled-down capabilities | **own kernel**, hardware virtualization |
| Root inside | rootful-in-userns (`sudo`) | **real root** |
| Nested Podman | rootless | rootless |
| Requires | Podman | `/dev/kvm`, Linux x86_64 |
| Default on | macOS, and any host without x86_64 KVM | x86_64 Linux + KVM |
| **Reach for it when** | speed, macOS | stronger isolation, untrusted workloads, nested root |
| Specified in | [SPEC.md §11](SPEC.md#11-the-podman-container-engine) | [SPEC.md §12](SPEC.md#12-the-firecracker-microvm-engine) |

## Who can SSH into what

Every sandbox runs an SSH server, so you reach each one by name. What a sandbox can reach in turn
depends on its **type**, set with `--type` and independent of engine. There are two layers:

- **user sandbox** (`--type user`): yours, to work in interactively. It can `ssh` into **every**
  sandbox.
- **agent sandbox** (default): one you hand to a coding agent. It can `ssh` only into **other agent
  sandboxes**, never into a user sandbox.

| client ↓ \ server → | user sandbox | agent sandbox |
|---|:---:|:---:|
| **host** | ✓ | ✓ |
| **user** sandbox | ✓ | ✓ |
| **agent** sandbox | ✗ | ✓ |

Aside from that SSH direction the two are identical: same image, same capabilities. Typically you
spawn one user sandbox and oversee the work running across several agent sandboxes from there.

The matrix describes reach **within a group**. Sandboxes in different groups cannot connect at all:
no DNS for one another, no route, and no key the other would accept. Without `--group` everything is
in one group, and the matrix is the whole story.

### Lending a sandbox specific SSH keys with `ssh -A`

Sometimes a sandbox needs to reach another machine over SSH, to run a tool on a remote test box or
copy files with `scp`. `ssh -A <name>` lends it a specific set of your host keys for the length of
one session. The keys stay on the host, and nothing is copied into the sandbox. It is opt-in and
independent of sandbox type. Lend deliberately, with two conditions in mind:

- **Scope the keys.** Load only the key you need into your agent (`ssh-add -c` prompts on the host
  for each use), rather than exposing your whole keyring.
- **Trust the operator.** Only forward into a sandbox whose operator you trust for the length of
  the session. While you are connected, anything running as you there can *use* the forwarded
  socket, though it cannot copy the key out.

## Security model

- **The boundary is the engine.** A Podman container shares the host kernel; a Firecracker microVM
  boots its own kernel under hardware virtualization, the stronger choice for untrusted or
  autonomous work.
- **Nothing shared by default.** Host data enters a sandbox only where a flag names it: `--repo` (a
  git checkout), `--snapshot` (a frozen read-only copy), and `--env` / `--env-file` (values you pass
  in). Results come back out with `cs-sandbox fetch`.
- **Your credentials are not shared either**, and naming one need not mean handing it over.
  `--lend-agent-login` and `--lend-api-key` leave the credential on the host behind a proxy, and
  give the sandbox a loan token worth nothing elsewhere. `--inherit-…` copies the real one in
  instead, and `create` prints which you got. See [Agent credentials](#agent-credentials).
- **No host SSH keys in any sandbox.** Neither type receives a copy of your host keys; sandboxes
  reach each other with generated per-group tier keys. If a sandbox ever needs your own keys, you
  can lend a specific set for a session ([`ssh -A`](#lending-a-sandbox-specific-ssh-keys-with-ssh--a)), and
  they stay on the host. It is the same trade lending makes for a credential.
- **Agent/user SSH isolation.** The per-type [SSH trust](#who-can-ssh-into-what) matrix is enforced
  by the keys themselves, so an agent can't pivot through SSH into your workspace.
- **`--yolo`** drops the agents' approval prompts, safe because the sandbox itself is the isolation
  boundary. **`--solo`** (agent sandboxes only) additionally denies the agent any *outbound* SSH into
  its group, while keeping it reachable for you to drive.
- **Groups are an optional second boundary.** `--group <name>` puts a set of sandboxes on their own
  network with their own SSH keys, so they can neither resolve, reach nor authenticate to sandboxes
  in another group. The matrix above applies inside a group rather than across groups. Use it when
  unrelated experiments share a host, or ignore it and everything lives in one
  ([SPEC.md](SPEC.md#5-groups)).
- It is not a hardened multi-tenant boundary; isolation is whatever the chosen engine provides.

## Docs

- [INSTALL.md](INSTALL.md) · one-time host setup: Podman, the Firecracker prerequisites, building
  the image, the agent tools, and the agent login
- [MANUAL.md](MANUAL.md) · every command, flag, file, exit status and diagnostic
- [SPEC.md](SPEC.md) · what a sandbox guarantees and how it is built: types and trust, the seed,
  groups, networking, sharing, lending, both engines, and the security model
- [CONTRIBUTING.md](CONTRIBUTING.md) · working on the tool: coverage, commit shape and writing

## Contributing

Bug reports and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) has the rules on
test coverage, commit shape and writing, and applies to coding agents as much as to people. It also
covers how to report a security issue privately.

**Testing.** `make ci` must pass before you open a PR. It runs every gate CI runs, formatting,
`go vet`, the unit tests and the prose linter among them. `make test-integration` runs the live tests against a real host, and each
skips gracefully when Podman or the image is unavailable. See
[SPEC.md](SPEC.md#16-conformance-and-testing).

## License

[Apache-2.0](LICENSE).
