# Remote Delegation to a Codex Session

You can delegate tasks to a separate **Codex** session running on a Linux machine over SSH,
the Codex counterpart of `cs-claude-remote`. The remote uses its own Codex auth (the
`~/.cs-codex` profile on that host), independent from this machine.

**The target SSH host is configurable** (see "Target SSH host"): any hostname or ssh_config
alias (e.g. another `cs-sandbox-<name>` instance), defaulting to this machine's own hostname.

## How it works

A turn is driven against an **interactive `codex` kept warm in a tmux session** on the remote
(via the `cs-codex-turn` driver), and the turn's output is read from the codex **session JSONL**
(the rollout file) — never scraped from the screen. This mirrors `cs-claude-remote`. One
difference: codex assigns its own session id, so the tmux session is named by a stable local
token and the codex session id is learned from the first turn and stored for later `--resume`.

Run `cs-codex-remote` from Bash. **A session name is MANDATORY on every call** — either start a
new session with `--new --name <name>` or resume one with `--resume <name>`. Bare calls are
rejected. `~/.cs-codex-remote-session` records the last name used, for reference only (never read
back to target a session — that was racy under parallel callers).

Rule of thumb:
- **Starting a session** — `cs-codex-remote --new --name <name> "task"`. Remember the name.
- **Every follow-up** — `cs-codex-remote --resume <name> "task"`.

```bash
# Start a new named session (optionally with a remote working directory)
cs-codex-remote --new --name api-work -d "~/projects/myproject" "investigate the auth module"

# Every follow-up: pass the session name explicitly
cs-codex-remote --resume api-work "now add unit tests"
cs-codex-remote --resume api-work "run the tests"

# Raw session JSONL for the turn (events) instead of the agent's final text
cs-codex-remote --resume api-work -j "summarize what changed"

# Run in background (async) — returns immediately, logs output locally
cs-codex-remote --resume api-work -b "refactor the module"

# Read background output / status
cs-codex-remote-output api-work
```

## Warm session lifecycle (tmux)

Each turn drives a long-lived interactive codex kept open in a tmux session on the remote, so
the process stays warm between turns. The driver auto-clears the first-run trust prompt and runs
codex with approvals/sandbox bypassed (a headless TUI can't answer approval prompts).

```bash
cs-codex-remote --kill <name>      # tear down the warm tmux process; history kept, next turn resumes
cs-codex-remote --attach <name>    # attach your terminal to watch/intervene (Ctrl-b d to detach)
```

If the tmux session has died, the next turn relaunches it with `codex resume <id>`, so the
conversation continues from the rollout history.

## Target SSH host

Resolved in priority order: `-H <host>` / `--host <host>` on the call → the **host stored for
that session** (set when created/last given `--host`) → the `CS_CODEX_REMOTE_HOST` env var →
this machine's short hostname. The resolved host is remembered per session (in
`~/.cs-codex-remote-sessions/<name>.host`), so the companion tools target the same host
automatically.

```bash
cs-codex-remote --new --name web-task -H cs-sandbox-web "investigate the build"
cs-codex-remote --resume web-task "now run the tests"   # still targets cs-sandbox-web
cs-codex-remote-status web-task                          # also targets cs-sandbox-web
```

## Permission mode (default vs YOLO)

Every turn runs codex through the `cs-codex` wrapper **on the target host**, so the permission
mode is decided there, not by this CLI:

- **Default** — the profile's `approval_policy=on-request` + `sandbox_mode=workspace-write` (from
  the target's `~/.cs-codex/config.toml`): codex asks before actions outside the sandbox.
- **YOLO** (`--dangerously-bypass-approvals-and-sandbox`, no prompts at all) kicks in when, **on
  the target host**, either `~/.cs-codex/.yolo` exists or `CS_CODEX_YOLO` is set. YOLO is
  **inherited from the target host** — there is no `--yolo` flag on `cs-codex-remote`.

What this means in practice:

- A session targeting a **cs-sandbox agent created with `--yolo`** (`-H cs-sandbox-<name>`) runs
  YOLO automatically — the marker is already on that instance. This is also the robust mode for a
  warm tmux session: a headless TUI can't answer an approval prompt, so bypassing keeps it from
  wedging (the ready-timeout exit-3 case).
- Any **other target** (a plain host, or a user/non-yolo instance) uses the on-request default. To
  opt into YOLO there, set it **on that host**: `touch ~/.cs-codex/.yolo` or export
  `CS_CODEX_YOLO=1` in its shell profile. Exporting `CS_CODEX_YOLO` **locally does not propagate**
  over SSH (the turn runs in a fresh remote shell), which is why the marker file lives on the
  target.

The launch directory is auto-trusted, so the directory-trust prompt does not appear.

## Background tasks & polling

`-b` dispatches the turn locally in the background and logs to
`~/.cs-codex-remote-logs/<name>.log`. You are responsible for reporting completion:

1. After `-b`, schedule a poll (e.g. `ScheduleWakeup(240, ...)`).
2. On wake, run `cs-codex-remote-output <name> -s` (cheap local check: prints `running`,
   `finished`, or `unknown`; exit 0=finished, 2=running, 1=crashed).
3. If `finished`, read the tail with `cs-codex-remote-output <name>`, summarize, stop polling.
4. If `running`, reschedule. Stay inside the prompt-cache window (≤300s) or jump to 1200s+.

## Companion tools

Each has its own reference next to the scripts in `~/bin` — read it for full options:

- `cs-codex-remote-output <name>` — local background log + `-s` status probe → **CS_CODEX_REMOTE_OUTPUT.md**
- `cs-codex-remote-status <name>` — remote rollout activity (agent messages, tools) → **CS_CODEX_REMOTE_STATUS.md**
- `cs-codex-remote-sessions` — list known sessions → **CS_CODEX_REMOTE_SESSIONS.md**
- `cs-codex-remote-forget <name>` — drop sessions, keep remote history → **CS_CODEX_REMOTE_FORGET.md**

## Tuning (environment variables)

- `CS_CODEX_REMOTE_HOST` — default SSH target host.
- `CS_CODEX_TIMEOUT` (default 1800s) — max wait for a turn to complete (driver-side).
- `CS_CODEX_STALL_SECS` (default 180, 0 disables) — bail early if the JSONL stops growing and the
  TUI is no longer working before a `task_complete`.
- `CS_CODEX_LOCK_WAIT` (default 900s) — how long a turn waits for the per-session lock.
- `CS_CODEX_MAX_LOG_BYTES` (default 1 MiB, 0 disables) — background log rollover to `<log>.1`.
- `CS_CODEX_TURN_SRC` — local path of the `cs-codex-turn` driver deployed to the remote.

## Exit codes

- `0` turn completed · `2` timed out or stalled · `3` launch/setup failure (e.g. the remote
  codex needs interactive sign-in — run `cs-codex login` there) · `4` session busy (another turn
  holds the lock) · `1` usage/other.

## Interpreting user intent

| User says | What to do |
|---|---|
| "codex remote, …" / "on codex remote: …" / "delegate to codex remote: …" | `cs-codex-remote --resume <name> "task"` (ask for the name if unknown, or list with `cs-codex-remote-sessions`). |
| "new codex remote session called <name>: …" | `cs-codex-remote --new --name <name> "task"` |
| "codex remote, resume <name>: …" | `cs-codex-remote --resume <name> "task"` |
| "send to codex remote in background: …" / "codex remote async: …" | `cs-codex-remote --resume <name> -b "task"` then poll. |
| "codex remote output?" / "is it done?" | `cs-codex-remote-output <name>` (or `-s`). |
| "codex remote status?" / "what's it doing?" | `cs-codex-remote-status <name> -t`. |
| "list codex remote sessions" | `cs-codex-remote-sessions` |
| "kill / attach the codex session" | `cs-codex-remote --kill <name>` / `--attach <name>` |

Always pass the session name explicitly. To delegate to **Claude** instead, use the
`cs-claude-remote` family (see `CS_CLAUDE_REMOTE.md`, alongside this file in `~/bin`).
