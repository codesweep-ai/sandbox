# Forgetting Remote Sessions

Use `cs-claude-remote-forget` to remove sessions from the local session list. This does NOT delete session history on the remote machine.

## How to use

```bash
# Forget a specific session
cs-claude-remote-forget <name>

# Forget multiple sessions
cs-claude-remote-forget <name1> <name2>

# Forget all sessions
cs-claude-remote-forget --all
```

## Interpreting user intent

| User says | What to do |
|---|---|
| "forget remote claude session <name>" / "delete remote claude session <name>" / "remove remote claude session <name>" | Run `cs-claude-remote-forget <name>` |
| "forget all remote claude sessions" / "clear remote claude sessions" | Run `cs-claude-remote-forget --all` |

## Notes

- If the forgotten session was the one recorded in `~/.cs-claude-remote-session` (the last-used marker), that pointer is cleared. The pointer is only a historical record — no tool reads it to decide which session to target.
- The next `cs-claude-remote` call still requires an explicit session name (either `--new --name <name>` or `--resume <name>`).
- Tearing down live tmux sessions targets each session's **own stored host** (sessions may live on different hosts); the per-session `.host` record is removed along with the session. Use `-H <host>` to force a single host. See `CS_CLAUDE_REMOTE.md` → "Target SSH host".
