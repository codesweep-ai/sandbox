# Forgetting Remote OpenCode Sessions

Use `cs-opencode-remote-forget` to remove sessions from the local session list. This also tears
down the session's warm tmux on the remote (best-effort), but does **not** delete the opencode
session history there — the same session id can still be resumed later.

## How to use

```bash
cs-opencode-remote-forget <name>            # forget a specific session
cs-opencode-remote-forget <name1> <name2>   # forget several
cs-opencode-remote-forget --all             # forget all sessions
```

## Interpreting user intent

| User says | What to do |
|---|---|
| "forget opencode session <name>" / "remove opencode session <name>" | `cs-opencode-remote-forget <name>` |
| "forget all opencode sessions" / "clear opencode sessions" | `cs-opencode-remote-forget --all` |

## Notes

- Removes the local map files (`<name>`, `<name>.token`, `<name>.host`, `<name>.workdir`) and
  kills the warm tmux on the session's stored host. The remote session db is kept.
- Use `-H <host>` to force a single host for the tmux teardown (sessions may live on different
  hosts).
