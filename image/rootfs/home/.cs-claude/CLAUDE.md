# Claude sandbox profile

You're running as Claude Code inside an isolated cs-sandbox dev instance (container or microVM).

## Remote delegation — three toolsets

You can delegate a task to an agent session on **another host** over SSH: to a remote **Claude**
(`cs-claude-remote`), a remote **Codex** (`cs-codex-remote`), or a remote **OpenCode**
(`cs-opencode-remote`). Each keeps the agent warm in a tmux session and reads each turn's output
from the agent's own session store. A session name is **mandatory** on every call; default to
foreground unless the user asks for `-b` (background).

### Remote Claude — `cs-claude-remote`

```bash
cs-claude-remote --new --name <name> [-H <host>] [-d <dir>] "task"   # start a named session
cs-claude-remote --resume <name> "task"                              # follow-up turn
cs-claude-remote --resume <name> -b "task"                           # background (async), then poll
cs-claude-remote-output <name> [-s]    # background log / status (running|finished); local-only
cs-claude-remote-status <name> [-t]    # remote activity (messages, tools)
cs-claude-remote-sessions [-v|-q]      # list sessions
cs-claude-remote --kill <name> | --attach <name>     # free / watch the warm session
cs-claude-remote-forget <name> | --all # drop from the local list (keeps remote history)
```

### Remote Codex — `cs-codex-remote`

```bash
cs-codex-remote --new --name <name> [-H <host>] [-d <dir>] "task"
cs-codex-remote --resume <name> "task"
cs-codex-remote --resume <name> -b "task"
cs-codex-remote-output <name> [-s]
cs-codex-remote-status <name> [-t]
cs-codex-remote-sessions [-v|-q]
cs-codex-remote --kill <name> | --attach <name>
cs-codex-remote-forget <name> | --all
```

### Remote OpenCode — `cs-opencode-remote`

```bash
cs-opencode-remote --new --name <name> [-H <host>] [-d <dir>] "task"
cs-opencode-remote --resume <name> "task"
cs-opencode-remote --resume <name> -b "task"
cs-opencode-remote-output <name> [-s]
cs-opencode-remote-status <name> [-t]
cs-opencode-remote-sessions [-v|-q]
cs-opencode-remote --kill <name> | --attach <name>
cs-opencode-remote-forget <name> | --all
```

### Full references — in `~/.local/bin`, alongside the scripts (read on demand)

- Claude: `CS_CLAUDE_REMOTE.md` + `CS_CLAUDE_REMOTE_{STATUS,OUTPUT,SESSIONS,FORGET}.md`
- Codex: `CS_CODEX_REMOTE.md` + `CS_CODEX_REMOTE_{STATUS,OUTPUT,SESSIONS,FORGET}.md`
- OpenCode: `CS_OPENCODE_REMOTE.md` + `CS_OPENCODE_REMOTE_{STATUS,OUTPUT,SESSIONS,FORGET}.md`

Read the matching reference for details (host resolution, background polling, exit codes, intent
tables) — e.g. before driving a Codex session, read `~/.local/bin/CS_CODEX_REMOTE.md`. Pick the toolset
the user names ("claude remote …" → `cs-claude-remote`; "codex remote …" → `cs-codex-remote`;
"opencode remote …" → `cs-opencode-remote`); if unspecified, default to your own kind
(`cs-claude-remote`).
