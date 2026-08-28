# The cs-sandbox manual

## Name

`cs-sandbox`: create and manage disposable Linux dev sandboxes for AI coding agents.

## Synopsis

```
cs-sandbox create <name> [--repo PATH] [--snapshot PATH] [--type agent|user]
                         [--engine podman|firecracker] [--group NAME]
                         [--inherit-agent-login AGENT] [--lend-agent-login AGENT]
                         [--inherit-api-key PROVIDER] [--lend-api-key PROVIDER]
                         [--yolo] [--solo] …
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
cs-sandbox agent-tools [--json]
cs-sandbox lender [--addr ADDR]
cs-sandbox completion bash|zsh|fish|powershell
cs-sandbox version [--images]

Global: [-v|--verbose] [-q|--quiet] [--dry-run]
```

## Description

`cs-sandbox` is a host tool. It creates named Linux sandboxes, each a rootless Podman container or
a Firecracker microVM built from one image. A shared network joins them, so every sandbox is
reachable by name over SSH.

Nothing on your host enters a sandbox unless you name it. Code goes in through `--repo` or
`--snapshot`, commits come back out with `fetch`, and the loop is create, work, fetch, destroy.

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
`default` group, sharing nothing and holding no credential of yours. The engine defaults to
Firecracker where the host has KVM, and to Podman otherwise.

| Flag | Meaning |
|---|---|
| `--repo PATH[@REF][:NAME]` | Share a git repo. It lands at `~/<name>` on branch `cs-sandbox/<sandbox>`, borrowing the source's objects read-only. Repeatable. |
| `--snapshot PATH[:NAME]` | Share a frozen read-only copy of any directory at `~/<name>`. Repeatable. |
| `--type agent\|user` | What this sandbox may reach over SSH. Default `agent`. See [SSH trust](#ssh-trust). |
| `--engine podman\|firecracker` | Which engine backs it. Default: Firecracker on Linux with KVM, Podman otherwise. |
| `--group NAME` | The group whose network, keys and gateway it joins. Default `default`. |
| `--lend-agent-login AGENT` | **Lend** a host agent login: `claude` or `codex`. The sandbox gets a fabricated credential; yours never enters it. Repeatable and comma-separated. |
| `--lend-api-key PROVIDER` | **Lend** an LLM API key from `~/.cs-keys/<provider>`: `anthropic`, `openai` or `fireworks`. Same trade. Repeatable and comma-separated. |
| `--inherit-agent-login AGENT` | Copy a host agent login in: `claude`, `codex` or `opencode`. The sandbox then holds the real credential. Repeatable and comma-separated. |
| `--inherit-api-key PROVIDER` | Copy an LLM API key in from `~/.cs-keys/<provider>`: `anthropic`, `openai` or `fireworks`. Repeatable and comma-separated. |
| `--block-side-calls` | Refuse the sandbox a direct route to the hosts the lender fronts. Default true, and only meaningful with a loan. |
| `--yolo` | Drop the agents' approval prompts *and* the rules behind them. The sandbox is the boundary. |
| `--solo` | Withhold the group's SSH key, so this agent sandbox can reach no peer while staying reachable itself. Agent type only. |
| `-e`, `--env KEY=VALUE` | Inject an environment variable, or `KEY` alone to pass the host's value. Repeatable. |
| `--env-file PATH` | Inject variables from a file. Repeatable. |
| `--image-store NAME` | Mount a shared image store read-only. Repeatable. |
| `--cpus N` | Firecracker vCPUs. Default 4. |
| `--mem MiB` | Firecracker memory. Default 4096. |
| `--disk GiB` | Firecracker disk size, grow-only. Default: the base rootfs size, 32. |
| `--privileged` | Podman: use `--privileged` instead of the scaled-down capability set. |

`create` prints what the sandbox ended up with, borrowed or held, so neither is ever a silent
assumption. Prefer the `--lend-…` pair: they give an agent everything it needs while your credential
stays on the host. [Lending a credential](#lending-a-credential) is the whole of that story.

`create` also carries your Claude Code theme in, so `cs-claude` opens looking the way claude does on
your host. Pick a different theme inside the sandbox and that choice wins from then on.

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
cs-sandbox ls [--json] [-q]         # GROUP NAME STATUS AGE TYPE ENGINE YOLO SOLO CREDS
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
cs-sandbox build --slim [--with-agents]   # the CI image instead: no developer toolchains
cs-sandbox build --local-sandbox          # take cs-sandbox from this checkout, not the proxy
cs-sandbox doctor [--engine ENGINE]       # check prerequisites, print the fix for each gap
cs-sandbox install-agent-tools [dir]      # the agent tools onto your PATH
cs-sandbox agent-tools [--json]           # what those tools are, with their sha256
cs-sandbox agent-login <agent> <name>     # log an agent in inside a sandbox
cs-sandbox lender [--addr ADDR]           # run the credential lender in the foreground
cs-sandbox sync-ssh-config                # regenerate the SSH config fragment
cs-sandbox completion <shell>             # a completion script for bash, zsh, fish or powershell
```

The credential flags read from `~/.cs-<agent>` and `~/.cs-keys`, whether they lend or copy.
`CS_SANDBOX_AGENT_HOME` points that lookup at a different tree, and nothing else moves. The
instance, its seed and the caches stay where they were.

It is for a caller that has to supply a login it never signed in for. A replay suite is one. Its
members need a credential the agent will start with, and a cassette serves the traffic. Pointing
`HOME` at a fake profile tree would do it too, and would take the instance directory and every
cache along with it.

With no `--engine`, `build` sets up every engine the host supports, and fails on a
Firecracker-capable host whose Firecracker packages are missing. Restrict it with `--engine podman`
for the image alone. The flag is repeatable, so `--engine podman --engine firecracker` names both.

`--slim` builds the CI image instead of the shipped one: the same Containerfile with the developer
toolchains removed. That drops Go, Node, Python, the JDK, Maven, Neovim and its language servers,
and Chromium. The image weighs about 474 MB against 6.04 GB, and it builds in minutes against tens
of them. That difference is what lets a job boot real sandboxes on a hosted runner. Building the
full image on every push would cost more time and disk than such a job has. The derivation lives in
`image/ci-slim.sh` and is applied to
the real Containerfile, so the slim image can lag the shipped one in weight but never diverge from
it in content.

Add `--with-agents` when the tests being run drive `claude`, `codex` or `opencode` **inside** the
sandbox. Those three CLIs are dropped with everything else otherwise, and a member without them
fails its readback at `command -v`. They cost about 858 MB.

`--local-sandbox` installs `cs-sandbox` in the image from this checkout rather than from the module
proxy. The image installs it by version, which needs that revision pushed. On one you have not, the
build stops at `unknown revision`. The flag writes the module zip the proxy would have served, out
of your git tree, and the build reads it over a temporary `file://` mount. The binary still reports
its own version. It takes the commit rather than the working tree, and needs a checkout to read.

A slim build goes to `ghcr.io/codesweep-ai/sandbox-slim`, or
`ghcr.io/codesweep-ai/sandbox-slim-agents` with `--with-agents`, tagged with the same version as the
shipped image. None of the three images is interchangeable with another, and the name is all a later
`create` has to tell them apart. `CS_SANDBOX_IMAGE` names one directly, which is how to build and
test against a name of your own:

```
CS_SANDBOX_IMAGE=localhost/sandbox-slim-agents:ci cs-sandbox build --engine firecracker --slim --with-agents
CS_SANDBOX_IMAGE=localhost/sandbox-slim-agents:ci make test-smoke
```

### Which image a sandbox runs

The image is named after the version of `cs-sandbox` that built it:
`ghcr.io/codesweep-ai/sandbox:v0.1.0` for a release, and a pseudo-version such as
`ghcr.io/codesweep-ai/sandbox:v0.0.0-20260826171442-c36e1fe91606` between releases. Every sandbox a
binary creates runs that image and no other, so the `cs-sandbox` inside a sandbox is the one that
built it. `cs-sandbox version` prints the name in full:

```
$ cs-sandbox version
cs-sandbox v0.1.0 (linux/amd64, go1.27.0)
image      ghcr.io/codesweep-ai/sandbox:v0.1.0
```

`--images` prints every reference this binary names instead, one per line, including the two CI
images that `--slim` builds:

```
$ cs-sandbox version --images
image              ghcr.io/codesweep-ai/sandbox:v0.1.0
image-slim         ghcr.io/codesweep-ai/sandbox-slim:v0.1.0
image-slim-agents  ghcr.io/codesweep-ai/sandbox-slim-agents:v0.1.0
```

`build` looks for that image on the registry and builds it only when there is none. A released
binary usually reaches a working image in the time a download takes. `create` does neither: when the
image is absent it says so and names `build`.

A binary built from a modified tree names a `-dirty` tag. No `-dirty` image is ever published, so
that binary always builds its own. That is what keeps a Containerfile you are editing from being
answered by a published image. A binary that reports no version at all names no image, and says so
rather than guessing; `make build` from a git clone gives it one.

Images are published by CI alone, on every push to `main` and on every release tag. A commit that
has not reached `main` therefore has no image to pull, and `build` builds one. Images for release
tags are kept; the rest expire ten days after they are published.

`completion` writes a script to stdout. It completes sandbox names, store names and flag values
live, by asking the binary. [INSTALL.md](INSTALL.md#optional-shell-completion) has the per-shell
install path.

`doctor` is the first thing to run when something does not work. It checks each prerequisite and
prints the remedy for anything missing.

Two of its checks are about identity rather than presence. The agent tools on your `PATH` are
compared byte for byte against the ones this build ships. A host that installed them from another
build runs a harness the source does not describe, and nothing else would say so. Every sibling
`cs-` tool on your `PATH` is compared against the version this build's `go.mod` names. Neither has
to be there. An operator who only boots sandboxes needs none of the siblings, and `doctor` says so
rather than counting it against the host.

`agent-tools` prints what `install-agent-tools` would install, each with the sha256 of the file this
build carries. It is the reference a host or a sandbox is compared against, so it reports the
shipped bytes rather than what is on the `PATH`. Read from the `PATH`, a drifted host would agree
with itself. `--json` emits the build version alongside the table, for a caller checking a
sandbox's tools from outside it.

`agent-login` launches the agent inside the named sandbox so you can complete its login there. The
login stays in that sandbox and goes when it does.

### What `--yolo` changes in a profile

`--yolo` writes a marker each wrapper reads, and the wrapper then launches its agent with approvals
off: `claude --dangerously-skip-permissions`, `codex --dangerously-bypass-approvals-and-sandbox`,
`opencode --auto`.

For Claude that is only half of it. Claude Code enforces the `permissions.deny` list in
`~/.cs-claude/settings.json` even under `--dangerously-skip-permissions`. The flag drops the prompt,
not the rule, and no allow at any later settings layer can lift a deny. A sandbox created to run
without approvals would still hard-block `git push`, `git rebase`, `rm -rf` and the rest of the
shipped list, with nobody there to ask. So a `--yolo` sandbox is given a second profile instead,
identical to the default but denying nothing. The other two agents need no equivalent: Codex has no
deny-shaped setting, and every OpenCode permission is already `allow`.

The swap happens at boot, in both directions. A sandbox `rm`'d with its data kept and recreated with
the flag flipped gets the profile that matches. A `settings.json` you edited inside the sandbox is
left alone, because only a pristine copy of the other profile is ever replaced.

### Pointing an agent somewhere else

`cs-claude`, `cs-codex` and `cs-opencode` launch their agent under a profile of its own. Each
respects the base URL its agent already reads, so pointing one at an endpoint of your own needs
nothing but the variable. A gateway, a proxy and a traffic recorder are all reached this way:

| Wrapper | Variable | What it points |
|---|---|---|
| `cs-claude` | `ANTHROPIC_BASE_URL` | Claude Code, however it is signed in |
| `cs-codex` | `OPENAI_BASE_URL` | Codex |
| `cs-opencode` | `OPENCODE_BASE_URL` | the provider the pinned model names |
| `cs-opencode` | `OPENAI_BASE_URL` | OpenCode's openai provider alone |

Claude reads its own, and one variable covers it whatever serves it. Codex reads none: it takes a
whole provider declaration instead, so `cs-codex` builds one from `OPENAI_BASE_URL` and passes it as
`-c` overrides. Nothing is written to any configuration file, so the endpoint applies to that
invocation alone and an unset variable changes nothing.

OpenCode is the awkward one. Its base URL belongs to the *provider*, and only the openai and
anthropic providers have a variable. A model on any other one ignores `OPENAI_BASE_URL` completely:
the agent reaches the real endpoint while whatever you pointed it at sits idle.

Use `OPENCODE_BASE_URL` for those. `cs-opencode` reads the provider from the pinned model in the
profile's `opencode.json`, and supplies a base URL for it inline. Nothing is written, and the pinned
model, the permissions and the disabled providers all survive.

Codex authenticates two ways, and the wrapper follows what it finds. With `OPENAI_API_KEY` set, the
provider names that variable. Without one it asks for Codex's own sign-in, which is the ChatGPT
subscription path and takes a base URL with no `/v1` on the end. A key present wins, so a sandbox
meant to run on its subscription should carry no `OPENAI_API_KEY`.

Set any of these per sandbox with `create --env`, or write them into the agent's own profile
env file inside the sandbox.

One case reads the variable rather than passing it on. When the same slot is being lent, the address
becomes the **lender's** upstream and the sandbox is handed the lender instead. See
[Lending a credential](#lending-a-credential).

### Lending a credential

A sandbox can call an LLM API without holding the credential that pays for it, and this is the
recommended way to give one an agent. Lend a credential instead of copying it, and what lands inside
is a **loan token**: an unguessable string worth nothing anywhere but this host. Your login and your
keys stay where they are.

```bash
# The sandbox's Claude Code works. Your login never enters it.
cs-sandbox create feature --repo ~/projects/api --lend-agent-login claude

# It works the same for an LLM API key you keep on the host.
cs-sandbox create feature --lend-api-key anthropic
```

Behind it is the **lender**, a proxy this tool runs on your host. The sandbox's agent is pointed at
it by the base URL the agent already reads. Each call arrives carrying the loan token, and the
lender swaps that token for your real credential before passing the call to the provider. Only the
lender knows which credential a token names.

Lending comes in two kinds. A **token loan** lends an agent's login, and arrives as the credential
file that agent's own sign-in would have written, holding fabricated values. The agent therefore
runs exactly as it does when signed in, rather than on the separate path a gateway variable would
put it on. A **key loan** lends an LLM API key, and arrives in the variables its clients read, which
is already the shape that credential has.

`create` starts the lender the first time a sandbox needs one, the way `forward` starts its `ssh`
child. `destroy` stops it once no sandbox is borrowing anything.

| What you lend | Flag | Read from |
|---|---|---|
| A Claude Code login | `--lend-agent-login claude` | `~/.cs-claude/.credentials.json` |
| A Codex login | `--lend-agent-login codex` | `~/.cs-codex/auth.json` |
| An Anthropic API key | `--lend-api-key anthropic` | `~/.cs-keys/anthropic` |
| An OpenAI API key | `--lend-api-key openai` | `~/.cs-keys/openai` |
| A Fireworks API key | `--lend-api-key fireworks` | `~/.cs-keys/fireworks` |

Which agent can spend which:

| Agent | Its own login | Keys it can spend |
|---|---|---|
| Claude Code | `--lend-agent-login claude` | `anthropic` |
| Codex | `--lend-agent-login codex` | `openai` |
| OpenCode | none: lend it a key | `anthropic`, `openai`, `fireworks` |

To lend a key, save it first. The file holds the key and nothing else:

```bash
mkdir -p ~/.cs-keys
printf %s "$ANTHROPIC_API_KEY" > ~/.cs-keys/anthropic
chmod 600 ~/.cs-keys/anthropic
```

A key lands in every variable its clients read. An OpenAI key becomes both `OPENAI_API_KEY` and
`CODEX_API_KEY`, because Codex reads only the second and attaches no credential at all when it sees
only the first.

`--inherit-api-key anthropic` copies that same key into the sandbox instead. It is the key
counterpart of `--inherit-agent-login`, and the opposite trade: convenience inside, and a credential
you have to assume is spent if the sandbox leaks.

**What a lent sandbox cannot do.** It cannot read the credential, because the credential is never
there, and it cannot keep spending one after the sandbox is destroyed.

**Side calls.** Agents sometimes call their provider directly, outside the base URL: analytics, news
about what the agent can do, and whatever else a vendor adds. A lent sandbox holds no credential for
those. Rather than let them go out and fail, and unsettle the agent in the process, the calls are
refused locally. Everything else it reaches as usual, and `--block-side-calls=false` turns the
refusal off.

**OpenCode follows its model.** A lent key reaches it only when the model it is running belongs to
that provider, because its base URL belongs to the provider rather than to the client. The image
pins a Fireworks model, so `--lend-api-key fireworks` works with no further arguments. For the other
two, name a model on that provider: `cs-opencode run -m anthropic/<model>`. See
[Pointing an agent somewhere else](#pointing-an-agent-somewhere-else) for the whole picture.

OpenCode has no login to lend. Lend it a key, or lend a subscription to the agent that owns it.

**A loan looks like a credential, deliberately.** Each fabricated value takes the form that provider
issues, prefix and length included, so the client stays on the code path it takes when signed in.
Scanning a sandbox for a vendor prefix therefore matches. What says a value is a loan is the word
`loan` just after that prefix, and `cs-sandbox-loan` where an account would be.

**Sending a lent slot somewhere else.** Set the base URL the slot owns, and the lender forwards
there instead of to the provider:

```bash
cs-sandbox create feature --lend-agent-login claude \
  --env ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/anthropic/build-auth
```

That value is read at create and does not reach the sandbox. The sandbox is handed the lender's
address in the same variable, exactly as a lent key is read on the host and handed back as a loan
token. The upstream is recorded on the loan, so it steers this sandbox alone and goes when the
sandbox does. Only the lender has to reach it, so it can be a recorder or a gateway on another
machine.

It sits behind the credential swap, so it is handed the real credential. That is the thing to weigh
before pointing one at a machine that is not this one. See
[Recording a lent sandbox](#recording-a-lent-sandbox-with-cs-vcr) for what it buys.

**Revoking a loan** is destroying the sandbox. The record lives in the instance directory and goes
when that does, and the lender stops honouring the token within seconds.

**Two sandboxes, one seat.** A lent login is still your seat, so sandboxes sharing it share a
rate-limit pool. The difference from `--inherit-agent-login` is that nothing in the sandbox can
refresh, revoke or copy the login itself.

**When a login goes stale.** The lender reads your credential fresh on every call and never
refreshes it. If nothing has used your host login for long enough that it expires, calls start
failing with a message saying so. Run `cs-claude` once on the host to renew it.

### Recording a lent sandbox with cs-vcr

Point a lent slot's base URL at a [cs-vcr](https://github.com/codesweep-ai/vcr) instead of at a
provider, and the lender forwards there. The session is recorded, and replayed later with no
provider reached and nothing spent:

```bash
cs-vcr record &                       # on this host, listening where the LENDER can reach it
cs-sandbox create feature --lend-agent-login claude \
  --env ANTHROPIC_BASE_URL=http://127.0.0.1:8080/c/anthropic/build-auth
```

Two things have to be true of that cs-vcr, and both follow from where it sits. It is dialled by the
lender rather than by the sandbox, so `127.0.0.1` is enough. It never has to be reachable from a
guest. And the provider entry the URL names points at the real provider, because the recorder is the
last hop before one.

The `/c/<provider>/<cassette>` prefix is cs-vcr's own addressing, and which provider to name follows
from what is being lent:

| The slot | Name the provider |
|---|---|
| `--lend-agent-login claude`, `--lend-api-key anthropic` | `anthropic` |
| `--lend-agent-login codex` | `chatgpt`, the endpoint a ChatGPT subscription is spent at |
| `--lend-api-key openai` | `openai`, the versioned API a key is accepted at |
| `--lend-api-key fireworks` | `fireworks` |

One vendor can be two entries: a lent Codex login is a subscription, which the versioned API
refuses. A sandbox borrowing two credentials sets two variables, each naming its own entry.

Nothing about the sandbox changes. It holds a loan token, it is handed the lender's address in that
variable, and the lender refuses its side calls. That is true with a recorder and without one, which
is what makes a recording free to add to a working setup and free to drop again.

`cs-sandbox doctor` reports whether the recorder answers, as one hop of the lending chain.

### The lender

```
cs-sandbox lender [--addr ADDR] [--origin SLOT=URL]
```

Runs the lender in the foreground. Most people never type it, because `create` starts one when a
sandbox needs it. Use it on a host that keeps a lender up on purpose, under a service manager, or
when you want to watch what it is doing.

| Flag | Meaning |
|---|---|
| `--addr ADDR` | Listen address. Default `0.0.0.0:2500`. It must not be loopback: a sandbox arrives on this host's ordinary side, where a loopback socket refuses the connection. |
| `--origin SLOT=URL` | Send one slot's traffic somewhere else, such as a gateway or a recorder in front of the provider. Repeatable. |

The port is open to the network the host is on, and the lender refuses every caller that is not this
host. `cs-sandbox doctor` reports which address it bound and says so if that address is one no
sandbox can reach.

### Connectors an account carries

An inherited Claude subscription carries more than the credential. The account's claude.ai
connectors (Gmail, Calendar, Drive) would otherwise attach inside the sandbox as MCP tools, and an
agent working there could reach the mailbox of whoever created it.

`cs-claude` loads only the MCP servers the invocation names, which is none unless you pass
`--mcp-config` yourself. Nothing about creating a sandbox implies handing it your mail, so nothing
here does.

This also makes a session reproducible. Connectors attach on their own schedule, so the tool list an
agent is offered differs between two runs of the same task. That alone is enough to stop a recorded
session replaying.

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
| `~/.cs-keys/<provider>` | An LLM API key this host will lend or copy in. One file per provider, holding the key alone. Secret. |
| `$XDG_DATA_HOME/cs-sandbox/instances/<group>/<name>/loans.json` | What one sandbox borrows, and the token it borrows with. Secret, and removed with the sandbox. |
| `$XDG_DATA_HOME/cs-sandbox/instances/lender` | The running lender's process id and address. |

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
| `CS_SANDBOX_AGENT_HOME` | Where a login or a key is read from, by `--inherit-agent-login` and by the lender alike. Your home, unless this names another profile tree. |
| `CS_SANDBOX_LEND_ADDR` | The address the lender listens on. Default `0.0.0.0:2500`. |

The second group changes what gets built or run.

| Variable | Default | Effect |
|---|---|---|
| `CS_SANDBOX_ENGINE` | unset | The engine `create` uses when no `--engine` is given. Unset, it picks Firecracker on Linux with KVM and Podman otherwise. |
| `CS_SANDBOX_IMAGE` | the image named after this binary's version | The sandbox image to run. |
| `CS_SANDBOX_ASSETS_DIR` | the embedded copy | An `image/` asset tree for `build` to use instead of the one embedded in the binary. |
| `CS_SANDBOX_PRIVATE_REGISTRY` | none | A registry the image should trust, as a bare `host:port`. Read at `build` time. |
| `CS_SANDBOX_PRIVATE_REGISTRY_INSECURE` | `0` | `1`, `true`, `yes` or `on` lets that registry use plain HTTP. |
| `CS_SANDBOX_DNS_SUFFIX` | `cs.sandbox` | The domain `host-route` resolves sandbox names under. |
| `CS_SANDBOX_GROUP` | `default` | The group `create` puts a sandbox in when no `--group` is given. |
| `CS_SANDBOX_TZ` | `America/Los_Angeles` | The timezone a sandbox boots with. |
| `CS_SANDBOX_SSH_BIND` | `127.0.0.1` | The host address a sandbox's SSH port, and its group gateway's, binds. Any other value publishes it beyond loopback: see [SPEC.md §13](SPEC.md#13-security-model). |

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

**A name that exists in several groups**

The error names every candidate. Address the sandbox in full as `<name>.<group>`. A bare name only
ever means the `default` group.

**`ssh <name>` does not resolve**

The SSH config fragment is stale, or the include is missing. Run `cs-sandbox sync-ssh-config`.

**A sandbox cannot reach another**

Check they are in the same group, and check the type matrix above. An agent sandbox cannot reach a
user sandbox by design, and `--solo` denies outbound SSH entirely.

**`create` rejects a path on macOS**

Everything runs in one podman-machine VM there, so `--repo` and `--snapshot` sources must live under
`$HOME`.

**An agent in a lent sandbox says it is not signed in**

Run `cs-sandbox doctor`. It walks the lending chain and names the hop that is dark. The candidates
are a lender that is not running, one bound where no sandbox can reach it, an expired host login,
and an upstream that does not answer. It says which side of the lender that upstream is on, because
the remedy differs. One in front has to listen where a sandbox can reach it. One behind only has to
answer on this host.

**`no loan matches the credential this request carried`**

The lender does not recognize what the sandbox presented. Its sandbox was destroyed, or the loan
came from a different host. Recreate the sandbox to mint a new one. The lender never prints the
value it refused, so look in the sandbox rather than in the lender's log.

**`no anthropic key to lend`**

`--lend-api-key` needs the key in `~/.cs-keys/<provider>`. The message prints the command that saves
one.

**`no systemd user session; running without a memory cgroup`**

A microVM is starting outside the cgroup that would cap it. The sandbox runs, but a runaway one is
charged to the shell that launched it. Under WSL2, enable systemd as
[INSTALL.md](INSTALL.md#windows-wsl2) describes.

**Anything about a missing prerequisite**

Run `cs-sandbox doctor`, which names the gap and the fix. Add `--engine podman` or `--engine
firecracker` to check a specific engine.

## Notes for agents

- Every command is non-interactive except two. `agent-login` launches an agent inside a sandbox and
  waits for you to complete its sign-in, and `host-route up` asks for `sudo`. Do not call either
  unattended.
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

Create a sandbox with a repo and a logged-in agent, work in it, take the commits, throw it away.
The agent is signed in as you, and your login never enters the sandbox:

```bash
cs-sandbox create feature --repo ~/projects/api --lend-agent-login claude
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
