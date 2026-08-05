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

One commit per idea. If it will not fit this shape, it is doing more than one thing: split it.

**Subject** — always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does.

**Body** — only when the subject leaves a real question. Bullets, one line each, under 60
characters, describing the design: the shape the change takes, or the constraint that ruled out the
obvious alternative. Not the diff, and not how you arrived at it. As many bullets as there are
points and no more — most commits need none, one is common, three is the rare maximum. Reaching for
a third to fill the shape is how messages turn into noise.

Leave out why the work was scheduled, how it was tested, and what prompted it: rationale belongs in
a comment or `docs/`, evidence in the PR.

```
Fix the typo in the firecracker boot arg name
```

```
Reject a base rootfs that is not a filesystem

- A blkid probe, so the happy path costs nothing.
```

```
Key forward records on the sandbox, not the reference

- Identity is (group, name); a reference is user input.
- Records outside the group survived `group rm`.
- Teardown sweeps the old path so nothing is stranded.
```

Keep the `Co-Authored-By:` trailer when an agent wrote the change. Drop any trailer linking to the
agent's session or transcript — private to whoever ran it, dead to everyone else.

## Docs

Behavior a user can see belongs in the docs next to it: `README.md` for the tour, `docs/` for the
deep dives, and the `~/.local/bin/CS_*.md` references for the in-sandbox tools. Update them in the
same commit as the code.
