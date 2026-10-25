# Remote Delegation to a Linux Machine

You can delegate tasks to a separate Claude Code instance running on a Linux machine over SSH.
That instance uses its own API credentials, independent from this machine's configuration.

**The target SSH host is configurable** (see "Target SSH host" below): it can be any hostname or
ssh_config alias (e.g. another `cs-sandbox-<name>` dev container), and defaults to this machine's own
hostname. Historically this delegated to `your-remote-host`; that is now just the default when you
run on that host.

## How to use

Run `cs-claude-remote` from Bash.

**The session name is a MANDATORY parameter on every call.** Either start a new session with `--new --name <name>` or resume an existing one with `--resume <name>`. Calls without a session name are rejected.

A `~/.cs-claude-remote-session` file is still written on each call to record the last session used, but no tool ever reads it to decide which session to target. This eliminates the race where a parallel caller's session pointer could silently redirect your next call.

**The local session name is also set as the remote display name.** When `--new` creates a session, the script passes `--name <name>` to the remote `claude`, which stores it as the session's `customTitle`. That makes the same name visible in the `/resume` picker on the remote machine and means you can resume it manually there with `claude --resume <name>` (the picker treats the value as a search term and matches the display name).

Rule of thumb:
- **Starting a session** — `cs-claude-remote --new --name <name> "task"`. Remember the name for the rest of the conversation.
- **Every follow-up in that session** — `cs-claude-remote --resume <name> "task"`.
- **A bare `cs-claude-remote "task"` will now fail** with an error telling you to pass a session name.

```bash
# Start a new session with a known name
cs-claude-remote --new --name auth-work -d "~/projects/myproject" "investigate auth module"

# Every follow-up: pass the session name explicitly
cs-claude-remote --resume auth-work "now refactor the auth module"
cs-claude-remote --resume auth-work "run the unit tests"

# Get full JSON output (cost, session ID, etc.)
cs-claude-remote --resume auth-work -j "do something"

# Run in background (async) — returns immediately, logs output locally
cs-claude-remote --resume auth-work -b "refactor the auth module"

# Check output from a specific session (session name is required)
cs-claude-remote-output auth-work
```

## How a turn runs

Every turn — new or resume — drives a long-lived interactive Claude Code kept open in a tmux session on the remote, so the process stays warm between turns (no per-call boot/MCP-init cost). Output is read from the session JSONL — never scraped from the tmux pane — and turn completion is detected from the JSONL's `turn_duration` marker. A plain `cs-claude-remote --new --name X "task"` is all you need.

```bash
cs-claude-remote --new --name auth-work -d "~/projects/myproject" "investigate auth module"
cs-claude-remote --resume auth-work "now refactor the auth module"

# Force re-copy of the remote driver (cs-claude-turn), then run:
cs-claude-remote --resume auth-work --redeploy "continue"

# Lifecycle (no prompt needed):
cs-claude-remote --kill auth-work      # tear down the warm tmux process; history kept, next turn resumes
cs-claude-remote --attach auth-work    # attach your terminal to watch/intervene (Ctrl-b d to detach)
```

Notes:
- The driver (`cs-claude-turn`) is auto-deployed to the remote `~/.local/bin` and refreshed when its source changes (override with `CS_CLAUDE_TURN_SRC`). If the local source is missing but a driver already exists on the remote, it is used as-is.
- `-j` emits the turn's new raw JSONL lines.
- `cs-claude-remote-forget <name>` also tears down the live tmux session (best-effort); it still does not delete remote history.
- `-b` (background), `cs-claude-remote-output`, and `cs-claude-remote-status` all work the same way.

## Target SSH host

The host the task is delegated to is resolved, in priority order:

1. `-H <host>` / `--host <host>` on the call (a hostname or ssh_config alias);
2. the **host stored for that session** (set when the session was created/last given a `--host`);
3. the `CS_CLAUDE_REMOTE_HOST` environment variable;
4. **this machine's own short hostname** (`hostname -s`).

The resolved host is **remembered per session** (in `~/.cs-claude-remote-sessions/<name>.host`),
so once a session is created against a host, the companion tools (`-status`, `-forget`, and verbose
`-sessions`) target that same host automatically — you don't pass `--host` again.

```bash
# Create a session that runs inside the cs-sandbox-web container, then resume it (host remembered)
cs-claude-remote --new --name web-task -H cs-sandbox-web "investigate the build"
cs-claude-remote --resume web-task "now run the tests"      # still targets cs-sandbox-web
cs-claude-remote-status web-task                            # also targets cs-sandbox-web

# No --host inside a container -> defaults to the container's own name (e.g. cs-sandbox-web).
# Set a default for a whole shell:
export CS_CLAUDE_REMOTE_HOST=your-remote-host
```

## Permission mode (Auto vs YOLO)

Every turn runs Claude through the `cs-claude` wrapper **on the target host**, so the
permission mode is decided there, not by this CLI:

- **Default: Auto** (`--permission-mode auto`). Honors the allow/deny rules in that
  host's `~/.cs-claude/settings.json`; only actions not covered by a rule prompt.
- **YOLO** (`--dangerously-skip-permissions`, no prompts at all) kicks in when, **on the
  target host**, either `~/.cs-claude/.yolo` exists or `CS_CLAUDE_YOLO` is
  set. YOLO is **inherited from the target host** — there is no `--yolo` flag on
  `cs-claude-remote`.

What this means in practice:

- A session targeting an **cs-sandbox agent created with `--yolo`** (`-H cs-sandbox-<name>`) runs YOLO
  automatically — the marker is already on that instance. This is also the robust mode for
  a warm tmux session: a headless session can't answer a prompt, so skip-permissions keeps
  it from wedging (the exit-3 "meant to run skip-permissions" case below).
- Any **other target** (a plain host, or a user/non-yolo instance) uses Auto. To opt into
  YOLO there, set it **on that host**: `touch ~/.cs-claude/.yolo` or export
  `CS_CLAUDE_YOLO=1` in its shell profile. Note that exporting
  `CS_CLAUDE_YOLO` **locally does not propagate** over SSH (the turn runs in a fresh
  remote shell), which is exactly why the marker file lives on the target.

## Tuning (environment variables)

These tune the remote-delegation path; all are optional with sensible defaults:

- `CS_CLAUDE_TIMEOUT` (default `600`, `0` waits indefinitely) — how long a turn may take before it is
  abandoned (exit 2). `0` removes the wall-clock limit and leaves the stall watchdog below as the
  only liveness check, which is the right shape for a genuinely long turn — but setting BOTH to `0`
  leaves nothing to end a wedged turn. Per-call, use `--turn-timeout <secs>`, which does not depend
  on the environment surviving the hop to the remote host.
- `CS_CLAUDE_STALL_SECS` (default `180`, `0` disables) — the stall watchdog. A healthy in-progress turn keeps appending to the JSONL; if it stops growing **and** the remote TUI is no longer working before a `turn_duration` marker appears, the turn is declared stalled and returns early (exit 2) with a diagnostic instead of blocking for the full `--timeout`. Raise it for very tool-heavy turns that legitimately go quiet for long stretches.
- `CS_CLAUDE_LOCK_WAIT` (default `900`s) — how long a turn waits to acquire the per-session lock before giving up. Turns are serialized per session so two callers cannot interleave keystrokes. The lock is a directory naming the process that holds it, so a lock left behind by a hard-killed turn is not waited on at all: the next call sees that the owner is gone and takes it. Reaching this limit therefore means a live turn is still holding the session.
- `CS_CLAUDE_MAX_LOG_BYTES` (default `1048576` = 1 MiB, `0` disables) — background (`-b`) logs roll over to `<log>.1` once they exceed this size, so a long-lived session's log can't grow unbounded. See `CS_CLAUDE_REMOTE_OUTPUT.md`.
- `CS_CLAUDE_TURN_SRC` — override the local path of the `cs-claude-turn` driver that is deployed to the remote (see the driver note above). The default is the
  driver beside `cs-claude-remote` itself, so a tool and its driver are always one version, and
  `~/.local/bin` only when there is none beside it.
- `CS_CLAUDE_REMOTE_HOST` — default SSH target host when neither `--host` nor a per-session stored host applies (see "Target SSH host"). Falls back to `hostname -s`.
- `CS_HANDOFF_TOKENS`, `CS_HANDOFF_FILE`, `CS_HANDOFF_ASK` — a context line the agent hands off
  at, on the agent's machine. See "Handing off at a context line".

## Is the agent in a turn

Run this on the machine the agent runs on:

```bash
cs-claude-turn --state
```

It prints one line and always exits 0. The first word is `busy`, `idle`, `blocked`, `unknown` or
`absent`, and `absent` means no Claude Code session exists there. `busy` can be followed by a second
word, `busy retrying`, while the agent waits out a provider error. `blocked` is a screen that needs a person, such as sign-in, onboarding or a tool approval.
It also covers any dialog the driver does not know that asks for a numbered choice or a confirmation.
`unknown` is a screen the driver does not recognise.

A few one-time dialogs are safe to refuse, such as Claude Code's offer to try its fullscreen
renderer. The driver refuses them itself at the start of a turn, and the turn goes on, so `--state`
reads such a screen as `idle`.

It answers for a turn whoever started it. An agent can start one of its own, when a background
command it left running finishes, and no turn driver wraps that turn. A count of live
`cs-claude-turn` processes misses it, and this does not. Nudge a member only when the word is `idle`.

With no handle it answers for every Claude Code session on that machine: `busy` if any is, and `idle`
only if all are. `--uuid <uuid>` narrows it to one. It starts nothing, wakes nothing and types nothing,
so it is safe to run every few seconds. It is the same judgement the driver waits on while it runs
a turn, so the two cannot disagree.

## How full the context is

Run this on the machine the agent runs on:

```bash
cs-claude-turn --context
```

It reads the agent's own record and prints one line, and it always exits 0:

```
tokens=<n or -> window=<n or -> compactions=<n> compacted=<epoch or -> model=<id or ->
```

`tokens` is the context the latest model call carried. `compactions` counts the times Claude Code
has compacted this conversation, and `compacted` is when it last did. The line reads `absent` when
there is no record, and `unknown` when the record cannot be read. Claude Code does not record its window, so `window` is always `-`. The reading after a compaction is the size of the summary, until the next model call replaces it.

With no handle it answers for the newest record, and `--uuid <uuid>` names one. Like `--state` it starts
nothing and writes nothing, so it is safe to run every few seconds. A caller that wants a member to
hand off its work before Claude Code compacts reads `tokens` here, against a line it sets itself.

With a hand-off line set (below), a reading carries two more fields: `handoff=<n>`, the line, and
`floor=<n or ->`, the session's first reading when that was already over the line.

## Handing off at a context line

A long turn fills the context, and then Claude Code compacts the conversation on its own. A
caller that would rather have the agent stop and write a handoff sets a line, in tokens, on the
agent's machine:

- `CS_HANDOFF_TOKENS` — the line. Unset or `0`, nothing below happens, and the agent runs as it
  always has.
- `CS_HANDOFF_FILE` — the file the agent writes its handoff to (default `~/.cs-turns/handoff.md`).
- `CS_HANDOFF_ASK` — the sentence the agent reads when it crosses the line; `{tokens}` and `{h}`
  are filled in. The default says to hand off as the agent's instructions describe, then end the
  turn. It names no task: what a handoff holds is the caller's business, and the agent's
  instructions say it.

In a sandbox, `cs-sandbox create --env` sets them, and they reach the CLI and its hooks.

Over the line, `cs-context-hook` appends the ask to every tool result until the handoff file is
newer than the first ask. Then it tells the agent once more to end its turn, and after two more
tool calls it ends the turn itself. The agent writes the handoff with its own tools, so it can
check its own state first. The hook skips a sub-agent's tool calls, and any Claude Code session
other than the one this driver drives, so a session the agent starts itself is never asked.

The hook's reading is the context the next model call will carry. It starts from the latest model
call in Claude Code's record, read as `--context` reads it, and adds an estimate of what came after.
That is the tool results the record holds, the tool calls Claude Code has not written to it yet, and
the result about to be added. Claude Code writes a model call to its record only once the call's
whole response has ended. So when a tool returns, the record often lacks the call that asked for
it. The estimate counts 2.4 characters of JSON as one token. Of an output Claude Code saved to a
file, it counts only the preview the model sees.

A turn driver the agent starts from inside the driven session runs with no line, and says so on
stderr. With the line, it would name its own session as the driven one, and the hook would skip this
one. `cs-claude-remote` run from a tmux pane passes that pane's session to the driver it starts over
ssh, as `CS_PARENT_TMUX_SESSION`, so a driver started that way is seen as nested too. A driver that
finds the driven session still alive under another name is nested too, because the harness ends a
session before it starts the next. A caller that starts a session while the last one it drove is
still alive therefore gets turns with no line. A nested driver writes its turn-log line to
`~/.cs-turns/claude-nested.log` rather than `claude.log`, so the last line of `claude.log` is the
driven session's.

A session whose first reading is already over the line is never asked. The line is below what a
fresh session costs, and asking would hand off forever. `--context` then reports `floor=<tokens>`.

A turn that ended this way is a hand-off, and the driver says so: exit `0`, one line on stderr
(`cs-claude-turn: handoff at <tokens> tokens, over the line of <n>`), and class `handoff` in the
turn log. A turn is read as one only when the hook asked in this session, during this turn, and
the handoff file is newer than the ask. A turn that ends any other way is what it was, with or
without a handoff file on disk, and a turn the provider refused is never a hand-off. A caller
that reads `handoff` can start the agent fresh and point it at the file.

A caller can instead keep a session that handed off, for example to have it commit its work first.
The hook asks again in a later turn while the session is still over the line, because the driver
counts only an ask made in its own turn.

Compaction stays on as a safety net. After one, the hook tells the agent to read its handoff
file, if there is one, before carrying on.

## How the last turn ended

Every turn end appends one line to `~/.cs-turns/claude.log`, on the machine the agent runs on:

```
<epoch seconds> <exit code> <class or -> <retry_after or -> <first line of the reason, or ->
1789000000 5 throttled 12.5 turn failed: Rate limit reached. Please try again in 12.5s.
1789000420 0 - - -
1789000900 0 handoff - handoff at 61240 tokens, over the line of 60000
```

The class and the wait are the ones on the `failure class=` line. That line goes to whoever
started the turn, which may be another machine. This file is for a caller that asks the agent's
own machine, as `--state` is. Read the last line: a turn that ended well is recorded too, so an
older failure is known to be over. `--state`, `--help` and a usage error are not turns and write
nothing. A driver that was killed writes nothing, so a missing line means the ending is not known.
A turn that ended well has `-` for its reason, or `dismissed a dialog: <its text>` when the driver
refused a one-time dialog on the way. A turn the agent ended by handing off has class `handoff`
on an exit-0 line ("Handing off at a context line").
The file keeps between 200 and 400 lines. `CS_TURN_LOG` names another path.

## Exit codes

A turn surfaces the remote driver's exit status:

- `0` — turn completed.
- `2` — turn timed out (`--timeout`) **or** the stall watchdog tripped (`CS_CLAUDE_STALL_SECS`).
- `3` — launch/setup failure, including screens that need a human: the remote Claude is at an **OAuth sign-in** or **first-run onboarding** wizard (attach with `--attach <name>` to complete it), or is wedged on a **tool-approval prompt** (the warm session is meant to run with permissions skipped — attach to inspect). A dialog the driver does not know fails the turn at once, and the message quotes the screen, so it says what a person has to answer.
- `4` — the session is **busy**: a live turn holds the lock and it could not be acquired within `CS_CLAUDE_LOCK_WAIT`. The error prints the lock directory, which is what to remove if you are certain the process holding it is not a turn.
- `5` — the turn **failed**: the provider ended it with an error, such as a rate limit, an overloaded API, a rejected credential or a prompt that is too long. It is not a finished turn, and the text Claude shows for it ("API Error: …") is not a reply.
- `1` — usage or other error.

A failed turn prints the provider's message on stderr, then one line to parse:

```
cs-claude-turn: failure class=throttled retry_after=-
```

`class` is `throttled`, `capacity`, `unauthorized`, `context`, `unreachable` or `other`. `unreachable`
means no provider answered at all, as when the network is down, and a caller waits it out as it
does a throttle. `retry_after` is the wait
in seconds when the message names one, and `-` when it does not. After `throttled` or `capacity`,
wait and send the prompt again to the same session. The session is still alive, so do not restart
it: a restart sends the whole context again into the same limit.

`capacity` with `retry_after=0` is a response that broke after it began, which Claude Code reports
as `Server error mid-response`. The provider was answering, and only that one request failed. Send
the prompt again to the same session at once, without a back-off.

After `unauthorized`, do not send it again. The provider rejected the credential, and waiting does
not fix that. Claude Code retries a rejected credential for about three minutes by itself, so the
driver ends its TUI and reports the turn within seconds. The next turn resumes the same conversation.

## Interpreting user intent

| User says | What to do |
|---|---|
| "remote claude, ..." / "on remote claude: ..." / "do on remote claude: ..." / "delegate to remote claude: ..." / "on the remote machine: ..." | Resume the session by name: `cs-claude-remote --resume <name> "task"`. You must know the session name — if you don't, ask the user or list sessions with `cs-claude-remote-sessions`. |
| "new remote claude session called <name>: ..." / "start remote claude session <name>: ..." / "remote claude, new session <name>: ..." | New named session: `cs-claude-remote --new --name <name> "task"` |
| "new remote claude session: ..." / "start fresh on remote claude: ..." | Ask for a session name (or propose one), then `cs-claude-remote --new --name <name> "task"`. The tool will reject `--new` without `--name`. |
| "remote claude session?" / "what remote claude session?" / "which remote claude session?" | The last-used session name is recorded at `~/.cs-claude-remote-session`, but it is only a historical record and may reflect a different caller. Prefer `cs-claude-remote-sessions` to list all known sessions. |
| "remote claude, resume <name>: ..." / "switch remote claude session to <name>: ..." | Resume specific session: `cs-claude-remote --resume <name> "task"` |
| "send to remote claude in background: ..." / "remote claude async: ..." / "kick off on remote claude: ..." | Background async: `cs-claude-remote --resume <name> -b "task"` |
| "remote claude output?" / "what did remote claude produce?" / "show remote claude output" | Run `cs-claude-remote-output <name>` for the session dispatched in this conversation and show the result |
| "is remote claude still running?" / "remote claude done?" | Run `cs-claude-remote-output <name>` (shows running/finished status + recent output) |
| "kill the remote claude session" / "tear down the warm session" / "free the remote process" | `cs-claude-remote --kill <name>` (history kept; next turn resumes) |
| "attach to the remote claude session" / "let me watch it" / "open the remote tmux" | `cs-claude-remote --attach <name>` (Ctrl-b d to detach) |

`--kill` also stops an in-flight background (`-b`) turn on this side: it verifies the recorded
PID really is that session's runner before signalling (so a stale PID file cannot take out an
unrelated process), tears down the runner and its SSH child along with the remote tmux, and
records exit 130 in the log — which `-output -s` then reports as `failed`.

Any mention of "remote claude" as a prefix or target means delegate to the remote machine.
Always pass the session name explicitly — the tools will refuse bare calls. Track the session name you are using from the moment you start or resume a session and pin it on every call.
Default to **foreground mode** for delegation. The user can background the running command themselves using Ctrl+B if needed.
Only use background (`-b`) when the user explicitly asks for async/background execution.

## Interaction patterns

### Sync (default)

1. Start or resume a session with its name recorded in the conversation context: `cs-claude-remote --new --name <name> "task"` (or `--resume <name>` if continuing prior work).
2. Show the result to the user.
3. User decides next step.
4. Send follow-ups with the name: `cs-claude-remote --resume <name> "task"`. The tool requires it — there is no "current session" fallback.
5. Repeat.

### Async (when explicitly requested)

1. Start or resume a session and send the task with its name: `cs-claude-remote --new --name <name> -b "task"` (or `--resume <name> -b "task"`). Returns immediately.
2. Tell the user the task was dispatched and the session name.
3. **Immediately schedule a poll**: `ScheduleWakeup(240, "polling <name>", ...)` — do not wait for the user to ask. See `CS_CLAUDE_REMOTE_OUTPUT.md` → "Polling discipline for background tasks" for the cadence rules.
4. On wake: run `cs-claude-remote-output <name> -s` (cheap, local-only; prints `running` or `finished`). If `finished`, read the tail with `cs-claude-remote-output <name>`, summarize for the user, stop polling. If `running`, reschedule.
5. Send follow-ups with `--resume <name> -b "next task"` and repeat the polling loop.

### Why the session name is mandatory

Previously, the script would fall back to reading `~/.cs-claude-remote-session` (a single shared "current session" pointer) when you omitted the name. That file is rewritten by every caller, so a parallel Claude Code instance, another shell, or any concurrent workflow could silently redirect your next "bare" call to the wrong session — losing all your context. Requiring the name on every call removes the race entirely.
