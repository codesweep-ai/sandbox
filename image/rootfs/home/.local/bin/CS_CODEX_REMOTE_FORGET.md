# Forgetting Remote Codex Sessions

Use `cs-codex-remote-forget` to remove sessions from the local session list. This also tears
down the session's warm tmux on the remote (best-effort), but does **not** delete the codex
rollout history there — the same session id can still be resumed later.

## How to use

```bash
cs-codex-remote-forget <name>            # forget a specific session
cs-codex-remote-forget <name1> <name2>   # forget several
cs-codex-remote-forget --all             # forget all sessions
```

## Interpreting user intent

| User says | What to do |
|---|---|
| "forget codex session <name>" / "remove codex session <name>" | `cs-codex-remote-forget <name>` |
| "forget all codex sessions" / "clear codex sessions" | `cs-codex-remote-forget --all` |

## Notes

- Removes the local map files (`<name>`, `<name>.token`, `<name>.host`, `<name>.workdir`) and
  kills the warm tmux on the session's stored host. Remote rollout JSONL is kept.
- Use `-H <host>` to force a single host for the tmux teardown (sessions may live on different
  hosts).
