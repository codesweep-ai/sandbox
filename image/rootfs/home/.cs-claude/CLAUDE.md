# Claude sandbox profile

You're running as Claude Code inside an isolated cs-sandbox dev instance (container or microVM).

This file routes; it holds no knowledge of its own. Each doc sits in `~/.local/bin` beside the
tool it describes, and is authoritative for it. Read the one you need before your first call
rather than guessing a command surface. When nothing covers it, say so instead of guessing.

- `CS_CLAUDE_REMOTE.md` (+ `_STATUS`, `_OUTPUT`, `_SESSIONS`, `_FORGET`) · `cs-claude-remote` —
  start or resume a Claude session on another host over SSH, kept warm between turns.
- `CS_CODEX_REMOTE.md` / `CS_OPENCODE_REMOTE.md` (+ the same companions) · the Codex and OpenCode
  equivalents, same shape.
- `CS_SANDBOX.md` · `cs-sandbox` — this sandbox carries the CLI too, so it can drive a peer:
  create one, `ls` what is running, `fetch` a branch back.
- `MDTOHTML.md` · `mdtohtml` / `mdview` — render a Markdown deliverable to standalone HTML.

Delegating a task to another host means one of the `cs-*-remote` families. Pick the one the user
names ("codex remote …" → `cs-codex-remote`); unnamed, default to your own
kind, `cs-claude-remote`.
