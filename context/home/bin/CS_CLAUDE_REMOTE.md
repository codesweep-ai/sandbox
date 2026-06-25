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
- The driver (`cs-claude-turn`) is auto-deployed to the remote `~/bin` and refreshed when its source changes (override with `CS_CLAUDE_TURN_SRC`). If the local source is missing but a driver already exists on the remote, it is used as-is.
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

- `CS_CLAUDE_STALL_SECS` (default `180`, `0` disables) — the stall watchdog. A healthy in-progress turn keeps appending to the JSONL; if it stops growing **and** the remote TUI is no longer working before a `turn_duration` marker appears, the turn is declared stalled and returns early (exit 2) with a diagnostic instead of blocking for the full `--timeout`. Raise it for very tool-heavy turns that legitimately go quiet for long stretches.
- `CS_CLAUDE_LOCK_WAIT` (default `900`s) — how long a turn waits to acquire the per-session lock before giving up. Turns are serialized per session so two callers cannot interleave keystrokes; if a previous turn was hard-killed and left a stale lock, the next call fails fast (exit 4) and prints the lock file to remove, rather than hanging forever.
- `CS_CLAUDE_MAX_LOG_BYTES` (default `1048576` = 1 MiB, `0` disables) — background (`-b`) logs roll over to `<log>.1` once they exceed this size, so a long-lived session's log can't grow unbounded. See `CS_CLAUDE_REMOTE_OUTPUT.md`.
- `CS_CLAUDE_TURN_SRC` — override the local path of the `cs-claude-turn` driver that is deployed to the remote (see the driver note above).
- `CS_CLAUDE_REMOTE_HOST` — default SSH target host when neither `--host` nor a per-session stored host applies (see "Target SSH host"). Falls back to `hostname -s`.

## Exit codes

A turn surfaces the remote driver's exit status:

- `0` — turn completed.
- `2` — turn timed out (`--timeout`) **or** the stall watchdog tripped (`CS_CLAUDE_STALL_SECS`).
- `3` — launch/setup failure, including screens that need a human: the remote Claude is at an **OAuth sign-in** or **first-run onboarding** wizard (attach with `--attach <name>` to complete it), or is wedged on a **tool-approval prompt** (the warm session is meant to run with permissions skipped — attach to inspect).
- `4` — the session is **busy**: another turn holds the lock and it could not be acquired within `CS_CLAUDE_LOCK_WAIT`. If you are certain no turn is running, the lock is stale — the error prints the exact file to `rm`.
- `1` — usage or other error.

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
