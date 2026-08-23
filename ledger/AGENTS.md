# Working in the ledger

This file routes; it holds no knowledge of its own. The repository's own `AGENTS.md` governs
everything outside this directory, and it still applies here.

- [GUIDE.md](GUIDE.md) · how to keep this ledger. Read it before you touch a record.
  `cs-ledger guide` prints the same text from inside the binary.
- `cs-ledger manual` · the command surface: verbs, flags, exit codes.

Run `cs-ledger render && cs-ledger check` before any commit that touches this directory.
