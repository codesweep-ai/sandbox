# Contributing

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security issue, use GitHub's private
vulnerability reporting on this repository's Security tab, rather than opening a public issue.

## Before you push

```bash
make check            # gofmt, go vet, unit tests and both linters — must pass
make test-smoke       # the subset of the live tests that CI runs, on every host
make test-integration # live engine tests; run when you touch create, engine or seed paths
```

This repo keeps a **ledger** of open issues in `ledger/`. Read
[`ledger/AGENTS.md`](ledger/AGENTS.md) before you start work, and follow it as you go. A commit
that touches `ledger/` needs `cs-ledger render && cs-ledger check` to pass first.

## Tests are part of the change

Every behavior change ships with test coverage. A change with no test is only acceptable when the
behavior genuinely cannot be observed in a test — say so in the PR.

- Put it in the **unit** tier (`make test`) if it can be: pure logic, or a real script/CLI driven
  with stubs. That tier is where the costly-if-silently-wrong things live — credential
  inheritance, seed trust material, instance state, the cobra tree with a fake `Runner`.
- Use the **integration** tier (`//go:build integration`) only for what needs a real engine, and
  make it skip gracefully when podman or the image is missing.
- The **smoke profile** is not a third tier: it is the subset of the integration tier that CI runs
  on every host, listed in the Makefile as `SMOKE_TESTS`. Keep it short, and add to it only for
  something the tiers above cannot reach.
- When a feature exists for all three agents (Claude, Codex, OpenCode), test all three — usually
  one table, so a contract that drifts in one of them fails loudly.
- Test the contract, not the implementation: the exit code, the file mode, the thing another tool
  parses. Say *why* the case matters in a comment when it isn't obvious.

## Commits

Keep one idea per commit. If a change will not fit that shape, it is doing more than one thing, so
split it.

**Subject** — always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does.

**Body** — only when the subject leaves a real question. Use bullets, one line each, under 60
characters, describing the design: the shape the change takes, or the constraint that ruled out the
obvious alternative. Do not describe the diff, and do not describe how you arrived at it.

Write as many bullets as there are points and no more. Most commits need none, one is common, and
three is the rare maximum. Reaching for a third to fill the shape is how messages turn into noise.

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

Behavior a user can see belongs in the docs, updated in the same commit as the code. Each document
has one job, so a fact lives in exactly one of them and the others link to it.

| If you are writing | It goes in |
|---|---|
| Why someone would want a sandbox, and the first five minutes | `README.md` |
| How to get the binary and set the host up once | `INSTALL.md` |
| What a command does, what a flag means, what an error means | `MANUAL.md` |
| A rule the tool enforces, or the reason a rule exists | `SPEC.md` |
| How to work on `cs-sandbox` itself | `CONTRIBUTING.md` |

The `CS_*.md` files under `image/rootfs/home/.local/bin/` are a different thing. They are shipped
payload, embedded in the binary and installed inside every sandbox, so they document the in-sandbox
tools for the agent using them. Editing one changes what every sandbox ships.

## Writing

Docs drift into a style that reads as terse and knowing rather than clear. These rules push back.
`scripts/lint-docs.py` enforces the mechanical ones, and `make check` runs it.
`scripts/lint-oss.py` is its sibling, and `make oss` runs it. It checks what this repository has to
satisfy as a published project, and `--explain` lists every rule it applies. Its knobs live beside
it in `scripts/lint-oss.config.py`.

1. **Write to the reader, in second person.** "Run `cs-sandbox doctor` first", not "the doctor
   command should be run first".

2. **Introduce a term where you first use it.** A reader meeting *seed*, *fabric* or *tier key* for
   the first time needs it introduced. Give a definition on the spot, an entry in a glossary table,
   or a link to the page that defines it.

3. **One em-dash per paragraph at most.** Two or three read as a writer who will not commit to a
   sentence. A colon or a full stop nearly always works better.

4. **Sentences under 30 words.** Longer than that and a sentence is carrying two ideas. A list of
   ordered steps belongs in a numbered list, not in one sentence separated by semicolons.

5. **Every sentence has a verb.** "One commit per idea" is an epigram, not a sentence. It sounds
   knowing and tells the reader nothing.

6. **Delete the frame.** "It is worth noting that", "put simply", "in other words", "to be clear".
   Each one comments on the writing instead of getting on with it. Say the thing.

7. **Do not say a thing twice in one sentence.** A sentence that circles back on its own subject
   lands nowhere.

8. **Show a file before running it.** A block that runs `./build.sh` has to have shown the reader
   what is in `build.sh`.

9. **Explain the case, or leave it out.** If a walkthrough has two shapes, walk through both fully,
   or pick one. Half an explanation, hedged, is worse than either.

10. **Do not mention what does not happen.** "The `--disk` flag is ignored here" makes a reader
    wonder why they were told. Cut it.

11. **Do not document the absence of a feature** as a section of its own. Non-goals belong in the
    spec, where a reader is looking for the boundary.

12. **Prefer a concrete example to a general statement.** A runnable block teaches a flag faster
    than a paragraph about it.

13. **Say what it costs.** If a flag needs `sudo`, makes output uncommittable, or is Linux-only, say
    so where the reader meets it.

14. **Describe what the software does, not how it came to do it.** Leave out what the project used
    to do, what was tried and dropped, and numbers from a run someone did once. The reason a rule
    exists belongs beside the rule in `SPEC.md`; the investigation that found it belongs in the PR.

Run the linter on its own while you write:

```bash
python3 scripts/lint-docs.py            # check
python3 scripts/lint-docs.py --stats    # per-file measurements
python3 scripts/lint-docs.py --list     # which files are checked
```

The knobs live beside it in `scripts/lint-docs.config.py`: `GLOSSARY`, `SKIP_EXTRA`,
`LOWERCASE_STARTERS` and `PROJECT_VERBS`. When a real sentence trips the verb check, add the verb.
When a report is noise, fix the check. A linter that cries wolf gets ignored, and then it protects
nothing.

The linter itself is vendored and stays byte-identical across projects. A fix to a check belongs in
the shared copy, and comes back here the next time it is copied out.
