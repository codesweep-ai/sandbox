# Checking Remote OpenCode Session Progress

Use `cs-opencode-remote-status` to check what a remote OpenCode session has been doing, especially
during long-running background tasks. It exports the session as JSON on the session's
remote host and summarizes recent activity.

## How to use

**The session name (or opencode session id) is mandatory** — `cs-opencode-remote-status <name-or-id>`.

```bash
# Recent agent messages for a session
cs-opencode-remote-status <name-or-id>
cs-opencode-remote-status <name> -n 5            # last 5 agent messages
cs-opencode-remote-status <name> -t              # include tool/command calls
cs-opencode-remote-status <name> -n 10 -t        # combine
cs-opencode-remote-status <name> -H cs-sandbox-web   # override the SSH host
```

## Target SSH host

By default this runs `cs-opencode export <id>` on the **host stored for that session** (set by
`cs-opencode-remote`), falling back to `$CS_OPENCODE_REMOTE_HOST` then this machine's `hostname -s`.
Override with `-H <host>`. See `CS_OPENCODE_REMOTE.md` → "Target SSH host".

## Interpreting user intent

| User says | What to do |
|---|---|
| "opencode remote status?" / "how is opencode remote doing?" / "what's the progress?" | Run `cs-opencode-remote-status <name>` for the session you dispatched and summarize. If you don't know the name, ask or run `cs-opencode-remote-sessions`. |
| "show me more" / "more detail" | `cs-opencode-remote-status <name> -n 10 -t` |
| "what did it run?" / "what tools did it use?" | `cs-opencode-remote-status <name> -t` |

## When to use proactively

- When a `cs-opencode-remote` call is running in the background and the user asks about progress.
- When a remote turn returns truncated/empty output and you need to reconstruct what happened.
- When resuming a session and you need context on what the remote session did previously.

## Limitations

- Reflects what has been persisted to the session db so far; an in-progress turn's content
  appears as it streams, and the final agent message lands at `task_complete`.
- Very large sessions may have slow tail reads; use `-n` to limit.
