# Remote Delegation to an OpenCode Session

You can delegate tasks to a separate **OpenCode** session running on a Linux machine over SSH,
the OpenCode counterpart of `cs-codex-remote`/`cs-claude-remote`. The remote uses its own
OpenCode auth (the `~/.cs-opencode` profile on that host), independent from this machine.

**The target SSH host is configurable** (see "Target SSH host"): any hostname or ssh_config
alias (e.g. another `cs-sandbox-<name>` instance), defaulting to this machine's own hostname.

## How it works

A turn is driven against an **opencode server kept warm in a tmux session** on the remote (via
the `cs-opencode-turn` driver). The server runs in the tmux session's `server` window, on a port
derived from the session token. The interactive TUI runs in its `tui` window as a client of that
server, opened on the driven session. The driver submits each turn with a blocking
`opencode run --server`, the prompt on its stdin, and watches the server's list of running
sessions for stalls. It judges the turn by the outcome opencode records on the session when the
session stops, not by the client's exit code. Nothing is ever scraped from the screen. opencode assigns its own session id (`ses_…`), so the
tmux session is named by a stable local token and the opencode session id is learned from the
first turn and stored for later `--resume`.

The server is the only one on that port. Its port is derived from the session token, so a server
holding it with no tmux session behind it is a turn that died before it could clean up: the driver
says which port and which process, ends it, and starts the turn. Nothing else would, because
`--kill` ends a tmux session and that process is not in one.

Run `cs-opencode-remote` from Bash. **A session name is MANDATORY on every call** — either start
a new session with `--new --name <name>` or resume one with `--resume <name>`. Bare calls are
rejected. `~/.cs-opencode-remote-session` records the last name used, for reference only (never
read back to target a session — that was racy under parallel callers).

Rule of thumb:
- **Starting a session** — `cs-opencode-remote --new --name <name> "task"`. Remember the name.
- **Every follow-up** — `cs-opencode-remote --resume <name> "task"`.

```bash
# Start a new named session (optionally with a remote working directory)
cs-opencode-remote --new --name api-work -d "~/projects/myproject" "investigate the auth module"

# Every follow-up: pass the session name explicitly
cs-opencode-remote --resume api-work "now add unit tests"
cs-opencode-remote --resume api-work "run the tests"

# Raw JSON events for the turn instead of the agent's final text
cs-opencode-remote --resume api-work -j "summarize what changed"

# Run in background (async) — returns immediately, logs output locally
cs-opencode-remote --resume api-work -b "refactor the module"

# Read background output / status
cs-opencode-remote-output api-work
```

## Warm session lifecycle (tmux)

Each turn drives a long-lived opencode server kept in a tmux session on the remote, so the process
stays warm between turns, and the TUI beside it shows a human attaching the driven turns as they
render. The session id is created **before** the TUI opens (the TUI is always started with
`-s <id>`), so the TUI never sits on its home screen showing nothing. A human who quits the TUI
gets it back on the next turn.

```bash
cs-opencode-remote --kill <name>      # tear down the warm tmux process; history kept, next turn resumes
cs-opencode-remote --attach <name>    # attach your terminal to watch/intervene (Ctrl-b d to detach)
```

`--kill` also stops an in-flight background (`-b`) turn on this side: it verifies the recorded
PID really is that session's runner before signalling (so a stale PID file cannot take out an
unrelated process), tears down the runner and its SSH child along with the remote tmux, and
records exit 130 in the log — which `-output -s` then reports as `failed`.

If the tmux session has died, the next turn starts the server again and opens the TUI with
`-s <id>`, so the conversation continues from the session history in the profile db.

## Target SSH host

Resolved in priority order: `-H <host>` / `--host <host>` on the call → the **host stored for
that session** (set when created/last given `--host`) → the `CS_OPENCODE_REMOTE_HOST` env var →
this machine's short hostname. The resolved host is remembered per session (in
`~/.cs-opencode-remote-sessions/<name>.host`), so the companion tools target the same host
automatically.

```bash
cs-opencode-remote --new --name web-task -H cs-sandbox-web "investigate the build"
cs-opencode-remote --resume web-task "now run the tests"   # still targets cs-sandbox-web
cs-opencode-remote-status web-task                         # also targets cs-sandbox-web
```

## Permission mode (default vs YOLO)

Every turn runs opencode through the `cs-opencode` wrapper **on the target host**. The profile's
`opencode.json` already blanket-allows tool permissions, and every driven turn additionally
passes `--auto` (auto-approve anything not explicitly denied) — a headless turn must never wedge
on a permission ask, and a non-`--auto` unattended `run` would silently auto-REJECT asks. The
target's `~/.cs-opencode/.yolo` marker (or `CS_OPENCODE_YOLO`) additionally applies `--auto` to
interactive TUI use on that host; cs-sandbox creates the marker for `--yolo` agent instances.

## Background tasks & polling

Default to **foreground** for delegation: the turn is finished when the command returns, and
the user can background a running command themselves with Ctrl+B. Use `-b` only when they ask
for async, and then report completion yourself rather than waiting to be asked.

`-b` dispatches the turn locally in the background and logs to
`~/.cs-opencode-remote-logs/<name>.log`. You are responsible for reporting completion:

1. After `-b`, schedule a poll (e.g. `ScheduleWakeup(240, ...)`).
2. On wake, run `cs-opencode-remote-output <name> -s` (cheap local check: prints `running`,
   `finished`, or `unknown`; exit 0=finished, 2=running, 1=crashed).
3. If `finished`, read the tail with `cs-opencode-remote-output <name>`, summarize, stop polling.
4. If `running`, reschedule. Stay inside the prompt-cache window (≤300s) or jump to 1200s+.

## Companion tools

Each has its own reference next to the scripts in `~/.local/bin` — read it for full options:

- `cs-opencode-remote-output <name>` — local background log + `-s` status probe → **CS_OPENCODE_REMOTE_OUTPUT.md**
- `cs-opencode-remote-status <name>` — remote session activity (agent messages, tools) → **CS_OPENCODE_REMOTE_STATUS.md**
- `cs-opencode-remote-sessions` — list known sessions → **CS_OPENCODE_REMOTE_SESSIONS.md**
- `cs-opencode-remote-forget <name>` — drop sessions, keep remote history → **CS_OPENCODE_REMOTE_FORGET.md**

## Tuning (environment variables)

- `CS_OPENCODE_REMOTE_HOST` — default SSH target host.
- `CS_OPENCODE_TIMEOUT` (default 1800s) — max wait for a turn to complete (driver-side).
- `CS_OPENCODE_TIMEOUT` (default `1800`, `0` waits indefinitely) — how long a turn may take before it is
  abandoned (exit 2). `0` removes the wall-clock limit and leaves the stall watchdog below as the
  only liveness check, which is the right shape for a genuinely long turn — but setting BOTH to `0`
  leaves nothing to end a wedged turn. Per-call, use `--turn-timeout <secs>`, which does not depend
  on the environment surviving the hop to the remote host.
- `CS_OPENCODE_STALL_SECS` (default 180, 0 disables) — bail early if the server reports the
  session idle this long while the attached run has not returned.
- `CS_OPENCODE_LOCK_WAIT` (default 900s) — how long a turn waits for the per-session lock. The lock names the
  process holding it, so one left behind by a hard-killed turn is taken rather than waited on;
  reaching this limit means a live turn still holds the session.
- `CS_OPENCODE_MAX_LOG_BYTES` (default 1 MiB, 0 disables) — background log rollover to `<log>.1`.
- `CS_OPENCODE_TURN_SRC` — local path of the `cs-opencode-turn` driver deployed to the remote. The default is the
  driver beside `cs-opencode-remote` itself, so a tool and its driver are always one version, and
  `~/.local/bin` only when there is none beside it.
- `CS_HANDOFF_TOKENS`, `CS_HANDOFF_FILE`, `CS_HANDOFF_ASK` — a context line the agent hands off
  at, on the agent's machine. See "Handing off at a context line".

## Is the agent in a turn

Run this on the machine the agent runs on:

```bash
cs-opencode-turn --state
```

It prints one line and always exits 0. The first word is `busy`, `idle`, `blocked`, `unknown` or
`absent`, and `absent` means no OpenCode session exists there. `busy` can be followed by a second
word, `busy retrying`, while the agent waits out a provider error. OpenCode is asked through its own server and not read off a screen, so it is never `blocked`.
`unknown` is a server that does not answer.

It answers for a turn whoever started it. An agent can start one of its own, when a background
command it left running finishes, and no turn driver wraps that turn. A count of live
`cs-opencode-turn` processes misses it, and this does not. Nudge a member only when the word is `idle`.

With no handle it answers for every OpenCode session on that machine: `busy` if any is, and `idle`
only if all are. `--tmux <token>` narrows it to one. It starts nothing, wakes nothing and types nothing,
so it is safe to run every few seconds. It is the same judgement the driver waits on while it runs
a turn, so the two cannot disagree.

## How full the context is

Run this on the machine the agent runs on:

```bash
cs-opencode-turn --context
```

It reads the agent's own record and prints one line, and it always exits 0:

```
tokens=<n or -> window=<n or -> compactions=<n> compacted=<epoch or -> model=<id or ->
```

`tokens` is the context the latest model call carried. `compactions` counts the times OpenCode
has compacted this conversation, and `compacted` is when it last did. The line reads `absent` when
there is no record, and `unknown` when the record cannot be read. OpenCode does not record its window, so `window` is always `-`. The call that writes a compaction's summary is not read as the conversation, and a subagent's session is not either, so the reading is `-` after a compaction until the next model call.

With no handle it answers for the newest record, and `--uuid <ses_id>` names one. Like `--state` it starts
nothing and writes nothing, so it is safe to run every few seconds. A caller that wants a member to
hand off its work before OpenCode compacts reads `tokens` here, against a line it sets itself.

With a hand-off line set (below), a reading carries two more fields: `handoff=<n>`, the line, and
`floor=<n or ->`, the session's first reading when that was already over the line.

## Handing off at a context line

A long turn fills the context, and then OpenCode compacts the conversation on its own. A caller
that would rather have the agent stop and write a handoff sets a line, in tokens, on the agent's
machine:

- `CS_HANDOFF_TOKENS` — the line. Unset or `0`, nothing below happens, and the agent runs as it
  always has.
- `CS_HANDOFF_FILE` — the file the agent writes its handoff to (default `~/.cs-turns/handoff.md`).
- `CS_HANDOFF_ASK` — the sentence the agent reads when it crosses the line; `{tokens}` and `{h}`
  are filled in. The default says to hand off as the agent's instructions describe, then end the
  turn. It names no task: what a handoff holds is the caller's business, and the agent's
  instructions say it.

In a sandbox, `cs-sandbox create --env` sets them, and they reach OpenCode and its plugins. The
hook is a plugin here, because OpenCode has no command hooks. It ships at
`~/.local/share/cs-opencode/plugins/cs-context-hook.ts`, and `cs-opencode` links it into
`~/.cs-opencode/plugins/` when it starts OpenCode with a line or a token budget meter set
("Stopping at the token budget"). With neither, the profile holds no plugin, so such a session
starts exactly as it would without the hook. The plugin runs
in the server the turn driver starts, which takes these variables from the driver's environment.

Over the line, the plugin appends the ask to every tool result until the handoff file is newer
than the first ask. Then it tells the agent once more to end its turn, and after two more tool
calls it interrupts the session itself. The agent writes the handoff with its own tools, so it can
check its own state first. The plugin skips a child session's tool calls (a sub-agent the
subagent tool runs, which OpenCode records with a `parentID`), and any OpenCode session other than the one
this driver drives, so a session the agent starts itself is never asked.

A turn driver the agent starts from inside the driven session runs with no line, and says so on
stderr. With the line, it would name its own session as the driven one, and the plugin would skip
this one. `cs-opencode-remote` run from a tmux pane passes that pane's session to the driver it
starts over ssh, as `CS_PARENT_TMUX_SESSION`, so a driver started that way is seen as nested too. A
driver that finds the driven session still alive under another name is nested too, because the
harness ends a session before it starts the next. A caller that starts a session while the last
one it drove is still alive therefore gets turns with no line. A nested driver writes its turn-log line to
`~/.cs-turns/opencode-nested.log` rather than `opencode.log`, so the last line of `opencode.log` is
the driven session's.

A session whose first reading is already over the line is never asked. The line is below what a
fresh session costs, and asking would hand off forever. `--context` then reports `floor=<tokens>`.

A turn that ended this way is a hand-off, and the driver says so: exit `0`, one line on stderr
(`cs-opencode-turn: handoff at <tokens> tokens, over the line of <n>`), and class `handoff` in
the turn log. A turn is read as one only when the hook asked in this session, during this turn,
and the handoff file is newer than the ask. A turn that ends any other way is what it was, with
or without a handoff file on disk, and a turn the provider refused is never a hand-off. A caller
that reads `handoff` can start the agent fresh and point it at the file.

A caller can instead keep a session that handed off, for example to have it commit its work first.
The plugin asks again in a later turn while the session is still over the line, because the driver
counts only an ask made in its own turn.

Compaction stays on as a safety net. When OpenCode compacts, the plugin has the summary end by
pointing at the handoff file, so the agent reads it before carrying on.

## Stopping at the token budget

A sandbox can borrow its agent's credential from the cs-sandbox credential lender on the host,
and the operator can give such a sandbox a token budget. The lender counts every call the sandbox
makes, in three types: new-input, cached-input and output tokens. Its meter reaches the sandbox
with the loan:

- `CS_METER_URL` — where the lender answers with this sandbox's count and budget. Unset, the
  sandbox has no meter, and nothing below happens.
- `CS_METER_TOKEN` — the loan the reading is made with.
- `CS_BUDGET_ASK` — the sentence the agent reads once the budget asks; `{types}` and `{h}`, the
  handoff file, are filled in. The default names the budget, the types and the file.

Each limited type has an ask line: its limit under a soft budget, and 95% of it under a hard one.
Past it, the sandbox is asked to stop, at two points.

Before it sends a prompt, the driver takes a reading. When a type is past its ask line, it sends
nothing, and starts no server and no session. It prints the lender's figures on stderr, then
`cs-opencode-turn: failure class=spent retry_after=-`, and exits `5`. When the lender gives no
reading, the turn runs after one warning on stderr: a lender that cannot answer does not stop work.

During a turn, the plugin takes a reading after each tool result of the session this driver
drives, with the same skips as at a context line. Past the ask line, it runs the same ask, nudge
and interrupt, in its own words. It asks at once, even a session whose first reading is already
past the line. When both lines would ask on one tool result, the agent reads the budget's ask. A
turn driver the agent starts from inside the driven session still reads the budget before its
turn, and the plugin never asks its session.

A turn that ended this way is a budget hand-off, and the driver says so: exit `0`, one line on
stderr (`cs-opencode-turn: handoff on the token budget: <type> at <used> of its <limit> limit; …`),
and class `spent` in the turn log. That line, and the one for a turn not sent, name each type
that ran out by its bare name, `input`, `cached` or `output`, and no other type. It is read as
one under the same conditions as a context hand-off, and it is `spent` even when the context line
asked in the same turn.

To ask whether a turn would be sent, without sending one, run `cs-opencode-turn --budget` on the agent's
machine. It takes one reading and prints one line, and always exits 0: `ok`, `ask <types>` with
the types at their ask line comma-separated, `none` when the lender gives no reading, or `off`
when the sandbox has no meter. It starts nothing and writes nothing, so a caller holding a
session on a spent budget can ask it every few minutes and try again as soon as it says `ok`.

Under a hard budget, the lender also refuses the sandbox's calls once a type reaches its limit.
That ends a turn partway through, with exit `5` and class `spent`.

Whichever way the budget stopped it, do not send the prompt again: only the operator can raise the
budget, on the host. The session is still alive, and after a budget hand-off its handoff is
written. Once the budget is raised, a session that handed off can start fresh on its handoff, and
any other carries on where it stopped.

## How the last turn ended

Every turn end appends one line to `~/.cs-turns/opencode.log`, on the machine the agent runs on:

```
<epoch seconds> <exit code> <class or -> <retry_after or -> <first line of the reason, or ->
1789000000 5 throttled 12.5 turn failed: Rate limit reached. Please try again in 12.5s.
1789000420 0 - - -
1789000900 0 handoff - handoff at 61240 tokens, over the line of 60000
1789001500 5 spent - turn not sent, token budget spent: cached at 1.00G of its 1.00G limit; the operator raises it on the host
```

The class and the wait are the ones on the `failure class=` line. That line goes to whoever
started the turn, which may be another machine. This file is for a caller that asks the agent's
own machine, as `--state` is. Read the last line: a turn that ended well is recorded too, so an
older failure is known to be over. `--state`, `--help` and a usage error are not turns and write
nothing. A driver that was killed writes nothing, so a missing line means the ending is not known.
The file keeps between 200 and 400 lines. `CS_TURN_LOG` names another path. A turn the agent
ended by handing off has class `handoff` on an exit-0 line ("Handing off at a context line"), or
`spent` when it handed off on the token budget ("Stopping at the token budget"). A turn that ran
without a budget reading has the driver's warning as its reason.

## Exit codes

- `0` turn completed · `2` timed out or stalled · `3` launch/setup failure · `4` session busy
  (another turn holds the lock) · `5` turn failed (client error, or a provider error recorded on
  the session — read from the session's own record, whatever the client exited with — or, with
  class `spent`, the token budget stopped it) · `1` usage/other.

`0`–`4` mean the same thing here as in the `cs-claude-remote` / `cs-codex-remote` families; `5`
is the one opencode adds, because only opencode can run a turn to completion and still have
failed.

When a provider refused the turn, exit `5` comes with one line a caller can parse, the same one
`cs-codex-turn` and `cs-claude-turn` print:
`cs-opencode-turn: failure class=<throttled|capacity|unauthorized|context|unreachable|spent|other> retry_after=<secs|->`.
The class is read from the kind of error OpenCode records and the HTTP status it keeps, and from
the message when both are silent. `retry_after` is the wait the provider asked for, in whole
seconds. A turn that failed for another reason, such as one with no recorded outcome, prints no
class.

`spent` from a refused call is read from the message first, before the kind and the status. The
lender's refusal is a 400, as a malformed request is, and OpenCode may file it under its own
budget kind, which is otherwise a throttle. After `spent`, do not send the prompt again. With exit
`5`, no turn ran because the sandbox's token budget was past its ask line, or the cs-sandbox
credential lender refused a call under a hard budget. With exit `0`, the agent handed off on the
budget's ask. Only the operator can raise the budget, on the host. The session is still alive,
and after a budget hand-off its handoff is written ("Stopping at the token budget").

Do not confuse these with the `-s` status codes. These are the codes **this command** exits with
after driving a turn; `cs-opencode-remote-output <name> -s` has its own, unrelated scale
(`finished` 0 / `unknown` 1 / `running` 2 / `failed` 3) describing a *background* turn's state.
A turn that exits `5` here lands in the log footer, which `-s` then reports as `failed`.

## Interpreting user intent

| User says | What to do |
|---|---|
| "opencode remote, …" / "on opencode remote: …" / "delegate to opencode remote: …" | `cs-opencode-remote --resume <name> "task"` (ask for the name if unknown, or list with `cs-opencode-remote-sessions`). |
| "new opencode remote session called <name>: …" | `cs-opencode-remote --new --name <name> "task"` |
| "opencode remote, resume <name>: …" | `cs-opencode-remote --resume <name> "task"` |
| "send to opencode remote in background: …" / "opencode remote async: …" | `cs-opencode-remote --resume <name> -b "task"` then poll. |
| "opencode remote output?" / "is it done?" | `cs-opencode-remote-output <name>` (or `-s`). |
| "opencode remote status?" / "what's it doing?" | `cs-opencode-remote-status <name> -t`. |
| "list opencode remote sessions" | `cs-opencode-remote-sessions` |
| "kill / attach the opencode session" | `cs-opencode-remote --kill <name>` / `--attach <name>` |

Always pass the session name explicitly. To delegate to **Codex** or **Claude** instead, use the
`cs-codex-remote` / `cs-claude-remote` families (see their references alongside this file).
