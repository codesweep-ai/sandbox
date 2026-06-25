# Checking Remote Codex Session Progress

Use `cs-codex-remote-status` to check what a remote Codex session has been doing, especially
during long-running background tasks. It reads the codex **rollout JSONL** on the session's
remote host and summarizes recent activity.

## How to use

**The session name (or codex session id) is mandatory** — `cs-codex-remote-status <name-or-id>`.

```bash
# Recent agent messages for a session
cs-codex-remote-status <name-or-id>
cs-codex-remote-status <name> -n 5            # last 5 agent messages
cs-codex-remote-status <name> -t              # include tool/command calls
cs-codex-remote-status <name> -n 10 -t        # combine
cs-codex-remote-status <name> -H cs-sandbox-web   # override the SSH host
```

## Target SSH host

By default this reads the rollout JSONL on the **host stored for that session** (set by
`cs-codex-remote`), falling back to `$CS_CODEX_REMOTE_HOST` then this machine's `hostname -s`.
Override with `-H <host>`. See `CS_CODEX_REMOTE.md` → "Target SSH host".

## Interpreting user intent

| User says | What to do |
|---|---|
| "codex remote status?" / "how is codex remote doing?" / "what's the progress?" | Run `cs-codex-remote-status <name>` for the session you dispatched and summarize. If you don't know the name, ask or run `cs-codex-remote-sessions`. |
| "show me more" / "more detail" | `cs-codex-remote-status <name> -n 10 -t` |
| "what did it run?" / "what tools did it use?" | `cs-codex-remote-status <name> -t` |

## When to use proactively

- When a `cs-codex-remote` call is running in the background and the user asks about progress.
- When a remote turn returns truncated/empty output and you need to reconstruct what happened.
- When resuming a session and you need context on what the remote session did previously.

## Limitations

- Reflects what has been written to the rollout JSONL so far; an in-progress turn's content
  appears as it streams, and the final agent message lands at `task_complete`.
- Very large sessions may have slow tail reads; use `-n` to limit.
