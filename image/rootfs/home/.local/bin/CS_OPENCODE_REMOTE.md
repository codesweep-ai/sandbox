# Remote Delegation to an OpenCode Session

You can delegate tasks to a separate **OpenCode** session running on a Linux machine over SSH,
the OpenCode counterpart of `cs-codex-remote`/`cs-claude-remote`. The remote uses its own
OpenCode auth (the `~/.cs-opencode` profile on that host), independent from this machine.

**The target SSH host is configurable** (see "Target SSH host"): any hostname or ssh_config
alias (e.g. another `cs-sandbox-<name>` instance), defaulting to this machine's own hostname.

## How it works

A turn is driven against an **interactive opencode TUI kept warm in a tmux session** on the
remote (via the `cs-opencode-turn` driver). The TUI hosts opencode's HTTP API on a per-session
port; the driver submits each turn with a blocking `opencode run --attach`, watches the server's
session status for stalls, and — because an attached client can exit 0 even when the provider
errored — verifies the turn on the session's final assistant message before reporting success.
Nothing is ever scraped from the screen. opencode assigns its own session id (`ses_…`), so the
tmux session is named by a stable local token and the opencode session id is learned from the
first turn and stored for later `--resume`.

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

Each turn drives a long-lived interactive opencode TUI kept open in a tmux session on the remote,
so the process stays warm between turns and a human attaching sees driven turns render live. The
session id is created **before** the TUI launches (the TUI is always started with `-s <id>`), so
the TUI never sits on its home screen showing nothing.

```bash
cs-opencode-remote --kill <name>      # tear down the warm tmux process; history kept, next turn resumes
cs-opencode-remote --attach <name>    # attach your terminal to watch/intervene (Ctrl-b d to detach)
```

If the tmux session has died, the next turn relaunches the TUI with `-s <id>`, so the
conversation continues from the session history in the profile db.

## Target SSH host

Resolved in priority order: `-H <host>` / `--host <host>` on the call → the **host stored for
that session** (set when created/last given `--host`) → the `CS_OPENCODE_REMOTE_HOST` env var →
this machine's short hostname. The resolved host is remembered per session (in
`~/.cs-opencode-remote-sessions/<name>.host`), so the companion tools target the same host
automatically.

## Permission mode (default vs YOLO)

Every turn runs opencode through the `cs-opencode` wrapper **on the target host**. The profile's
`opencode.json` already blanket-allows tool permissions, and every driven turn additionally
passes `--auto` (auto-approve anything not explicitly denied) — a headless turn must never wedge
on a permission ask, and a non-`--auto` unattended `run` would silently auto-REJECT asks. The
target's `~/.cs-opencode/.yolo` marker (or `CS_OPENCODE_YOLO`) additionally applies `--auto` to
interactive TUI use on that host; cs-sandbox creates the marker for `--yolo` agent instances.

## Background tasks & polling

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
- `CS_OPENCODE_STALL_SECS` (default 180, 0 disables) — bail early if the server reports the
  session idle this long while the attached run has not returned.
- `CS_OPENCODE_LOCK_WAIT` (default 900s) — how long a turn waits for the per-session lock.
- `CS_OPENCODE_MAX_LOG_BYTES` (default 1 MiB, 0 disables) — background log rollover to `<log>.1`.
- `CS_OPENCODE_TURN_SRC` — local path of the `cs-opencode-turn` driver deployed to the remote.

## Exit codes

- `0` turn completed · `2` timed out or stalled · `3` launch/setup failure · `4` turn failed
  (client error, or a provider error recorded on the session — detected by the driver's
  postcheck even when the attached client exited 0) or session busy (another turn holds the
  lock) · `1` usage/other.

## Interpreting user intent

| User says | What to do |
|---|---|
| "opencode remote, …" / "delegate to opencode remote: …" | `cs-opencode-remote --resume <name> "task"` (ask for the name if unknown, or list with `cs-opencode-remote-sessions`). |
| "new opencode remote session called <name>: …" | `cs-opencode-remote --new --name <name> "task"` |
| "send to opencode remote in background: …" | `cs-opencode-remote --resume <name> -b "task"` then poll. |
| "opencode remote output?" / "is it done?" | `cs-opencode-remote-output <name>` (or `-s`). |
| "opencode remote status?" / "what's it doing?" | `cs-opencode-remote-status <name> -t`. |
| "list opencode remote sessions" | `cs-opencode-remote-sessions` |
| "kill / attach the opencode session" | `cs-opencode-remote --kill <name>` / `--attach <name>` |

Always pass the session name explicitly. To delegate to **Codex** or **Claude** instead, use the
`cs-codex-remote` / `cs-claude-remote` families (see their references alongside this file).
