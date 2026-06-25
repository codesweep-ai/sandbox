# Checking Remote Session Progress

Use `cs-claude-remote-status` to check what the remote Claude instance has been doing, especially during long-running background tasks.

## How to use

**The session name is mandatory** — `cs-claude-remote-status <name-or-id>`. The tool no longer accepts a bare call; you must pass the session name (or UUID) every time.

```bash
# Check a specific session by name (or UUID)
cs-claude-remote-status <name-or-id>
cs-claude-remote-status <name-or-id> -n 10 -t

# Show last N messages
cs-claude-remote-status <name> -n 5

# Include tool calls (Bash, Read, Edit, etc.)
cs-claude-remote-status <name> -t

# Combine options
cs-claude-remote-status <name> -n 10 -t

# Override the SSH host (otherwise the session's stored host is used)
cs-claude-remote-status <name> -H cs-sandbox-web
```

## Target SSH host

By default this reads the session's JSONL on the **host stored for that session** (set
by `cs-claude-remote`), falling back to `$CS_CLAUDE_REMOTE_HOST` then this
machine's `hostname -s`. Override with `-H <host>` / `--host <host>`. See
`CS_CLAUDE_REMOTE.md` → "Target SSH host".

## Interpreting user intent

| User says | What to do |
|---|---|
| "remote claude status?" / "how is remote claude doing?" / "what's the progress?" / "check on remote claude" | Run `cs-claude-remote-status <name>` for the session you dispatched in this conversation and summarize. The session name is required — if you don't know it, ask the user or run `cs-claude-remote-sessions`. |
| "show me more" / "more detail" | Run `cs-claude-remote-status <name> -n 10 -t` for more messages with tool calls |
| "what tools did it use?" / "what did it run?" | Run `cs-claude-remote-status <name> -t` to include tool calls |

## When to use proactively

- When a `cs-claude-remote` call is running in background and the user asks about progress
- When a remote task completes with truncated or empty output and you need to reconstruct what happened
- When resuming a session and you need context on what the remote instance did previously

## Limitations

- Only shows completed turns — if the remote instance is mid-turn (thinking/generating), the in-progress response won't appear until that turn finishes
- Very large sessions may have slow tail reads; use `-n` to limit
