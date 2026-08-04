# Contributing

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security-sensitive issue, please ask for a private
contact rather than posting details in a public issue.

## Before you push

```bash
make check            # gofmt + go vet + unit tests — must pass
make test-integration # live engine tests; run when you touch create/engine/seed paths
```

## Tests are part of the change

Every behavior change ships with test coverage. A change with no test is only acceptable when the
behavior genuinely cannot be observed in a test — say so in the PR.

- Put it in the **unit** tier (`make test`) if it can be: pure logic, or a real script/CLI driven
  with stubs. That tier is where the costly-if-silently-wrong things live — credential
  inheritance, seed trust material, instance state, the cobra tree with a fake `Runner`.
- Use the **integration** tier (`//go:build integration`) only for what needs a real engine, and
  make it skip gracefully when podman or the image is missing.
- When a feature exists for all three agents (Claude, Codex, OpenCode), test all three — usually
  one table, so a contract that drifts in one of them fails loudly.
- Test the contract, not the implementation: the exit code, the file mode, the thing another tool
  parses. Say *why* the case matters in a comment when it isn't obvious.

## Commits

One commit per idea, and each one has to be explainable in this shape:

- **Subject**: one line, under 60 characters, imperative mood.
- **Body**: at most three bullets, one line each and also under 60 characters. Write the *why* —
  the background that lets a reader place the change — plus anything non-obvious about how it was
  done. Not a list of the changes; the diff already says that.

Sixty characters is tight on purpose: it forces one idea per bullet. When the *why* will not fit,
it belongs in a comment beside the code or in `docs/`, where whoever needs it is actually looking
— not in a longer bullet.

If a commit can't be described that way, it is doing more than one thing: split it.

```
Reject a base rootfs that is not a filesystem

- A truncated download broke boot much later.
- A blkid probe, so the happy path costs nothing.
```

Keep the `Co-Authored-By:` trailer when an agent wrote the change. Drop any trailer linking back to
the agent's session or transcript — those URLs are private to whoever ran the agent and dead to
everyone else reading the log.

## Docs

Behavior a user can see belongs in the docs next to it: `README.md` for the tour, `docs/` for the
deep dives, and the `~/.local/bin/CS_*.md` references for the in-sandbox tools. Update them in the
same commit as the code.
