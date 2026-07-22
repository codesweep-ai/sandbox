# Listing Remote Codex Sessions

Use `cs-codex-remote-sessions` to list the Codex remote sessions you can resume. Reads the local
session map and shows each session's name, codex session id, and working directory.

## How to use

```bash
cs-codex-remote-sessions        # name, codex session id, dir, last-used marker
cs-codex-remote-sessions -v     # verbose: query each session's remote host for last activity
cs-codex-remote-sessions -q     # quiet: names only (one per line)
```

In verbose mode each session is queried on its **own stored host** (sessions may live on
different hosts). Use `-H <host>` to force a single host. See `CS_CODEX_REMOTE.md` → "Target SSH
host".

## Interpreting user intent

| User says | What to do |
|---|---|
| "codex remote sessions?" / "which sessions can I resume?" | `cs-codex-remote-sessions` and show the output. |
| "when was that session last active?" | `cs-codex-remote-sessions -v` |

## When to use proactively

- When the user wants to resume a session but doesn't remember the name.
- Before resuming a session by name, to confirm it exists.

## Notes

- A session's codex session id shows as `(pending first turn)` until its first turn completes
  (codex assigns the id, captured from that turn).
- The `*` marker is the last session used locally — informational only; every tool requires the
  session name to be passed explicitly.
