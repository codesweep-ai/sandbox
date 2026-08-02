# sandbox - agent login

How a sandbox gets a logged-in Claude Code or Codex, and what `cs-sandbox` will and will not copy
from your host. The design-level summary lives in
[`design.md`](design.md#bundled-agent-tools-and-login).

## Inheriting your host login is opt-in

By default a sandbox has **no** Claude or Codex login. `--inherit-agent-login` carries your host
login in at create time — usually the convenient choice, since it saves logging in inside the
sandbox:

```bash
cs-sandbox create dev --inherit-agent-login claude          # carry your Claude login
cs-sandbox create dev --inherit-agent-login claude,codex    # several at once (also: opencode)
cs-sandbox create lab                                       # default: no agent login
```

`create` snapshots the named agent's host credential (`~/.cs-claude/.credentials.json`,
`~/.cs-codex/auth.json`, `~/.cs-opencode/auth.json`) into the per-sandbox seed, and the guest
installs it into the home volume
(mode 600) on **first boot only** — so a token the sandbox later refreshes is never clobbered.

Nothing is carried unless you ask. Copying your credentials into a sandbox — especially one an
autonomous agent drives — is a decision, so it is spelled in the command that makes it, and `create`
reports what the sandbox ended up with:

```
created dev (type=agent, engine=podman, ssh port=2201)
  shell: ssh dev
  agent login: claude (inherited from your host)
```

## Logging in inside a sandbox instead

```bash
cs-sandbox agent-login claude <name>      # or: agent-login codex|opencode <name>
```

Use this when you didn't inherit a login, or when you want the sandbox on a **separate account or
rate-limit pool** rather than sharing yours.

It is also the route when the host keeps the credential in the **macOS Keychain** rather than in a
file — both agents use the Keychain when one is available, and `--inherit-agent-login` copies a file,
so there is nothing to carry (`create` says so). Where the credential does land in a file — a
headless or SSH session, or Codex with `cli_auth_credentials_store = "file"` — inheriting works just
as it does on Linux.

## One subscription, many sandboxes

An inherited login is the same seat as your host. Sandboxes sharing it share a rate-limit pool, and
independent OAuth refreshes can log each other out. That is an accepted trade for convenience — if it
bites, give the sandbox its own session with `agent-login`.

## Provider API keys are not carried

`cs-sandbox` never copies API keys or cloud-provider credentials out of your environment. If a
sandbox needs one, pass it explicitly:

```bash
# a scalar key or cloud target
cs-sandbox create dev --env ANTHROPIC_API_KEY --env CLAUDE_CODE_USE_BEDROCK --env AWS_REGION

# a credential file: share the directory, then point the variable at it
cs-sandbox create dev \
  --snapshot ~/creds/sandbox \
  --env GOOGLE_APPLICATION_CREDENTIALS=/home/$USER/sandbox/sa.json
```

`--env KEY` (no value) passes the variable through from your host environment; `--env-file` reads a
whole file. Both land in the sandbox's environment for every session.

Passing them yourself keeps the setup explicit and visible in the command that creates the sandbox,
and it works for any provider: a variable a vendor adds tomorrow needs no change here, and credential
*files* — which no environment mechanism can copy — are handled by the same `--snapshot` you would
use for any other host directory.

For a key scoped to the agent rather than the whole sandbox, write it to `~/.cs-claude/env` (or
`~/.cs-codex/env`, `~/.cs-opencode/env`) *inside* the sandbox — the `cs-claude` / `cs-codex` /
`cs-opencode` wrappers source that file at launch. For OpenCode this is the usual path rather than
the exception: it is normally driven by a provider API key (see [opencode.md](opencode.md)).

Note that an API key **overrides** a subscription login, so there is rarely a reason to inherit a
login and inject a key into the same sandbox.
