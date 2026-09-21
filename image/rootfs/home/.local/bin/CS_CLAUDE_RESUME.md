# Resuming Local Sessions

Use `cs-claude-resume` to find a recent `cs-claude` session on this machine, whatever directory it ran in, and resume it in that directory.

## How to use

```bash
# Pick from the 20 most recent sessions, newest first
cs-claude-resume

# Keep only sessions whose title, directory or id contains every term (case-insensitive)
cs-claude-resume ci build

# Show more (0 shows all)
cs-claude-resume -n 50

# Print the list with session ids and exit, without prompting
cs-claude-resume -l
cs-claude-resume -l verdaccio

# Pass what follows -- to cs-claude
cs-claude-resume -- --fork-session
```

Each row shows the session's age, directory and title. Picking a number changes into that session's directory and runs `cs-claude --resume <id>` there.

## Interpreting user intent

| User says | What to do |
|---|---|
| "which sessions can I resume?" / "list my recent claude sessions" / "find the session where I worked on X" | Run `cs-claude-resume -l [terms]` and show the output |
| "resume my session about X" / "reopen that session" | The picker needs the user's terminal, so don't run it yourself. Run `cs-claude-resume -l X` to find it, then give the user `cd <dir> && cs-claude --resume <id>`, or tell them to run `cs-claude-resume X` |

## When to use proactively

- When the user wants to go back to earlier work but doesn't remember which directory it ran in
- Before handing the user a resume command, to confirm the session id and its directory

## Notes

- It lists only sessions started with `cs-claude` on this machine, read from `~/.cs-claude/projects`. Sessions on another host are listed by `cs-claude-remote-sessions`.
- `[running]` marks a session that a live Claude Code process still has open. Picking one is refused, because two processes would write the same transcript. Close the other one first, or add `-- --fork-session` to branch off a copy.
- A session whose directory no longer exists is refused too. Claude Code's own picker resumes it in the current directory: `cs-claude --resume`, then Ctrl+A.
- That picker's Ctrl+A lists every directory, but for a session from another directory it prints `cd <dir> && claude --resume <id>` instead of resuming. Plain `claude` reads `~/.claude`, where the session is not, so run that line with `cs-claude` in place of `claude`.
- Requires `jq` on PATH.
