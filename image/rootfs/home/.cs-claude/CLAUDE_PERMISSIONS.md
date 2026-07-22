# Claude Code Permissions Setup

## Goal

Configure Claude Code with a two-tier permission model:
- **Allow (auto-approve):** All commands run without prompts by default.
- **Deny (auto-block):** Destructive actions (recursive/force deletions, dangerous git operations, database destruction) are blocked automatically.

## How Claude Code permissions work

Claude Code evaluates each command against three tiers:

1. **Matches a `deny` rule** — auto-blocked, no prompt shown. Deny takes priority over allow.
2. **Matches an `allow` rule** — auto-approved, no prompt shown.
3. **Matches neither** — the user is prompted and can approve or reject.

We use `Bash(*)` as a catch-all allow rule so that only explicitly denied commands are blocked. This avoids excessive prompts caused by compound commands (`cmd1 && cmd2`), pipes, and shell constructs that don't match individual prefix patterns.

**Why not enumerate individual commands?** Claude Code is shell-operator-aware — a rule like `Bash(git status*)` does NOT match `git status && git diff`. Compound commands are split into subcommands, and the overall expression fails to match any single allow pattern, causing a prompt. `Bash(*)` eliminates this problem while deny rules still block dangerous operations.

## Instructions

Apply the following permissions block to the user-level `settings.json` in **each** of these directories under the user's home directory:

- `~/.claude/`
- `~/.cs-claude/`

**Note:** Not all of these directories will exist on every machine. Only apply changes to directories that already exist — do not create missing directories.

### Steps

1. For each directory listed above **that exists**, open or create `settings.json`.
2. Merge the `permissions` block below into the file, preserving any existing settings (e.g. `model`, `autoUpdatesChannel`).
3. If a `permissions` key already exists, replace it entirely with the block below.

### Permissions block

```json
{
  "permissions": {
    "allow": [
      "Read",
      "Write",
      "Edit",
      "Glob",
      "Grep",
      "WebFetch",
      "WebSearch",
      "Bash(*)"
    ],
    "deny": [
      "Bash(rm -r *)",
      "Bash(rm -rf *)",
      "Bash(rm -f *)",
      "Bash(git push*)",
      "Bash(git push --force*)",
      "Bash(git reset --hard*)",
      "Bash(git clean *)",
      "Bash(git branch -D *)",
      "Bash(git checkout -- *)",
      "Bash(git restore *)",
      "Bash(git rebase *)",
      "Bash(git merge *)",
      "Bash(*DROP TABLE*)",
      "Bash(*DROP DATABASE*)",
      "Bash(*TRUNCATE *)",
      "Bash(*drop table*)",
      "Bash(*drop database*)",
      "Bash(*truncate *)"
    ]
  }
}
```

### What this does

**Auto-approved (allow rules):**
- All file operations: Read, Write, Edit, Glob, Grep
- Web access: WebFetch, WebSearch
- All Bash commands via `Bash(*)` — including compound commands, pipes, and shell constructs

**Auto-blocked (deny rules):**

These are blocked automatically. Deny takes priority over allow. If Claude needs one of these, discuss an alternative approach instead.

| Category | Patterns |
|----------|----------|
| **Recursive / force deletion** | `rm -r *`, `rm -rf *`, `rm -f *` |
| **Git push (any form)** | `git push*`, `git push --force*` |
| **Git destructive** | `git reset --hard*`, `git clean *`, `git branch -D *`, `git checkout -- *`, `git restore *`, `git rebase *`, `git merge *` |
| **Database destruction** | `DROP TABLE`, `DROP DATABASE`, `TRUNCATE` (case-insensitive) |

### Notes

- Deny rules take priority over allow rules. `Bash(*)` allows everything, but `Bash(git push*)` in deny still blocks pushes.
- Claude Code is shell-operator-aware: compound commands (`cmd1 && cmd2`) are split into subcommands for deny checking. A deny rule like `Bash(rm -rf *)` blocks `rm -rf` even if it appears as part of a compound command.
- Simple `rm file.txt` (without `-r`, `-rf`, or `-f` flags) is allowed. Only recursive/force deletion variants are blocked.
