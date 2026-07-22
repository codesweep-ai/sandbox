# Viewing Remote Output

Use `cs-claude-remote-output` to read the local output log from background (`-b`) remote tasks.

## How to use

**The session name is mandatory** — `cs-claude-remote-output <name>`. The tool no longer accepts a bare call; pass the session name every time.

```bash
# Output from a specific session
cs-claude-remote-output <name>
cs-claude-remote-output <name> -n 100
cs-claude-remote-output <name> --full
cs-claude-remote-output <name> -f

# Status-only probe: prints "running", "finished", or "unknown" and exits.
# Exit code: 0=finished, 2=running, 1=unknown (crashed mid-turn). Cheap local check.
cs-claude-remote-output <name> -s
```

## Interpreting user intent

| User says | What to do |
|---|---|
| "remote claude output?" / "what did remote claude produce?" / "show me the output" | Run `cs-claude-remote-output <name>` for the session you dispatched and summarize. The session name is required. |
| "show me all the output" / "full output" | Run `cs-claude-remote-output <name> --full` |
| "is remote claude still running?" / "is it done?" | Run `cs-claude-remote-output <name> -s` — prints `running`, `finished`, or `unknown` (crashed mid-turn) |

## When to use proactively

- After dispatching a background task, when the user asks about progress or output
- When a user asks "what did the remote do?" and the task was run with `-b`
- Prefer this over `-status` for background tasks since it shows the actual streamed output (text format), while `-status` reads the remote JSONL (more structured but requires SSH)

## Polling discipline for background tasks

When you dispatch a task with `-b`, **you** are responsible for reporting completion back to the user. Do not wait for the user to ask — poll with `ScheduleWakeup` using a cache-aware cadence.

### The cadence

Anthropic's prompt cache has a 5-minute TTL. Sleeping past 300s means the next wake-up reads the full conversation uncached — slower and more expensive. So:

- **Default tick: 240s (4 min).** Stays inside the cache window, gives the remote task time to make progress.
- **Short tick: 60–120s** when you expect completion imminently (e.g. last status check said "almost done" in the log tail).
- **Long tick: 1200–1800s (20–30 min)** only if the task is genuinely long-running and there is nothing to see sooner.
- **Never pick 300–600s.** It's the worst-of-both: you pay the cache miss without amortizing it. Drop to 240s or jump to 1200s+.

### The loop

1. Right after `cs-claude-remote -b`, call `ScheduleWakeup(240, "polling <name>")` with the same `/loop`-style continuation.
2. On wake, run `cs-claude-remote-output <name> -s` (cheap — local-only, no SSH, no log read).
3. If `finished` (exit 0): run `cs-claude-remote-output <name>` to read the tail, summarize for the user, stop the loop.
4. If `running` (exit 2): schedule another tick. Shorten the interval if the log tail or `-status` output suggests the task is close to done.
5. If `unknown` (exit 1): something broke — surface to the user instead of silently retrying.

### When NOT to poll

- If the user explicitly said "I'll check later" or "no need to track it" — don't schedule a wakeup.
- If the task is expected to run for hours and the user is walking away — confirm the tick interval with the user before committing.

The point is: **never tell the user "I'll check in 5 minutes"** as a vague promise. Either schedule a concrete wakeup with a justified interval, or report status now.

## How `-s` decides running vs finished

Each background turn brackets its output in the log with a header it writes at the
start (`--- <timestamp> --- prompt: ...`) and an authoritative footer it writes
when it ends (`--- <timestamp> --- finished (exit N) ---`). `-s` treats that
footer as the source of truth:

- **`finished`** (exit 0) — a finished footer follows the most recent prompt header. This is read from the log, so it stays correct even if the background PID was recycled by an unrelated process.
- **`running`** (exit 2) — no footer yet for the latest turn, and the worker process is still alive.
- **`unknown`** (exit 1) — no footer and no live process: the worker died mid-turn (crash). Surface this to the user rather than retrying silently.

## Log rotation

Background logs roll over to `<name>.log.1` once they exceed
`CS_CLAUDE_MAX_LOG_BYTES` (default 1 MiB; `0` disables), so a long-lived
session's log can't grow unbounded. `cs-claude-remote-output` always reads
the current `<name>.log`; the previous generation is kept alongside as
`<name>.log.1` if you need it.

## Output vs Status

- **`cs-claude-remote-output`**: Reads the *local* log file. Fast, no SSH. Shows raw text output. Only available for `-b` tasks.
- **`cs-claude-remote-status`**: Reads the *remote* JSONL. Requires SSH. Shows parsed assistant messages and tool calls. Works for all tasks (foreground and background).

Use output for quick local checks; use status for deeper inspection of what the remote Claude did.
