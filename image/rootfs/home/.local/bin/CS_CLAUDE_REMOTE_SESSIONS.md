# Listing Remote Sessions

Use `cs-claude-remote-sessions` to list available remote claude sessions that can be resumed.

## How to use

```bash
# List all sessions (shows name, UUID, working directory, last-used marker)
cs-claude-remote-sessions

# Verbose: also check each session's remote host for last activity timestamp
cs-claude-remote-sessions -v

# Quiet: print session names only (one per line, useful for scripting)
cs-claude-remote-sessions -q
```

In verbose mode, each session is queried on its **own stored host** (sessions may live
on different hosts). Use `-H <host>` to force a single host for the query. See
`CS_CLAUDE_REMOTE.md` → "Target SSH host".

## Interpreting user intent

| User says | What to do |
|---|---|
| "remote claude sessions?" / "what sessions are available?" / "list remote claude sessions" / "which sessions can I resume?" | Run `cs-claude-remote-sessions` and show the output |
| "when was that session last active?" / "show session details" | Run `cs-claude-remote-sessions -v` for timestamps |

## When to use proactively

- When the user wants to resume a session but doesn't remember the name
- When the user asks what work has been done on the remote machine
- Before resuming a session by name, to confirm it exists

## Note on the `*` marker

The `*` marker shows the session most recently written to `~/.cs-claude-remote-session`. That file is shared across all callers and is overwritten on every call, so it reflects only the last invocation (possibly from a different Claude Code instance or shell). It is **never** used to target commands — every tool now requires the session name to be passed explicitly. The marker is informational only; track the session name you dispatched from this conversation and pass it on every call.
