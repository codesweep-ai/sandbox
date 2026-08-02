# Viewing Remote Codex Output

Use `cs-codex-remote-output` to read the **local** log written by a background (`-b`)
`cs-codex-remote` task. Fast, local-only (no SSH).

## How to use

**The session name is mandatory** — `cs-codex-remote-output <name>`.

```bash
cs-codex-remote-output <name>             # last 30 lines + running/finished status
cs-codex-remote-output <name> -n 100      # last N lines
cs-codex-remote-output <name> --full      # the entire log
cs-codex-remote-output <name> -f          # follow (tail -f)
cs-codex-remote-output <name> -s          # status: finished | running | failed | unknown
```

`-s` is the cheap status probe, and the contract an orchestrator polls. It prints one word
and exits: `finished` (`0`), `running` (`2`), `failed` (`3`), `unknown` (`1`).

`failed` means the turn ran to completion and ended badly — the footer carried a nonzero exit.
That is different from `unknown`, which means no footer at all: the worker died without
recording anything. A turn cancelled with `--kill` is recorded as exit 130, so it reports
`failed`.

## How running vs finished is decided

Each background turn brackets its log with a header (`--- <ts> --- prompt: …`) and an
authoritative footer (`--- <ts> --- finished (exit N) ---`). `-s` treats the footer as the
source of truth: footer after the latest prompt ⇒ **finished** (or **failed**, if its exit code
is nonzero); no footer + worker still alive ⇒ **running**; no footer + no live worker ⇒
**unknown** (crashed). Background logs roll over to
`<name>.log.1` past `CS_CODEX_MAX_LOG_BYTES` (default 1 MiB).

## Interpreting user intent

| User says | What to do |
|---|---|
| "codex remote output?" / "what did codex produce?" | `cs-codex-remote-output <name>` and summarize. |
| "full output" | `cs-codex-remote-output <name> --full` |
| "is it still running?" / "is it done?" | `cs-codex-remote-output <name> -s` |

## Output vs Status

- **`cs-codex-remote-output`** — reads the *local* log. Fast, no SSH. Background (`-b`) tasks only.
- **`cs-codex-remote-status`** — reads the *remote* rollout JSONL over SSH; works for any session.
