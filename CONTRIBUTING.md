# Contributing to cs-sandbox

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security issue, use GitHub's private
vulnerability reporting on this repository's Security tab, rather than opening a public issue.

## Before you push

```bash
make check            # gofmt, go vet, unit tests and the linters; must pass
make test-smoke       # the subset of the live tests that CI runs, on every host
make test-integration # live engine tests; run when you touch create, engine or seed paths
```

`make check` needs three tools that do not come with Go. Install them once:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
go install golang.org/x/tools/cmd/deadcode@latest
go install github.com/codesweep-ai/lint/cmd/cs-lint@latest
```

Pin `golangci-lint` to the version above, the one CI runs. A newer release gains checks, and you
want to meet those when you upgrade the pin rather than on an unrelated pull request.

`cs-lint` is deliberately not pinned. It is this family's own linter, and CI installs it from source
the same way, so a check it gains reaches you on the day it lands.

[`.golangci.yml`](.golangci.yml) is the Go counterpart to the prose rules below, and it records why
each check is on or off. Two of them are off because their advice is wrong in this codebase. Read
the reason before you turn one back on. When a check reports noise, fix the config rather than
working around it. The prose linter earns its keep the same way: a linter that cries wolf gets
ignored, and then it protects nothing.

This repo keeps a **ledger** of open issues in `ledger/`. Read
[`ledger/AGENTS.md`](ledger/AGENTS.md) before you start work, and follow it as you go. A commit
that touches `ledger/` needs `cs-ledger render && cs-ledger check` to pass first. That tool is a
sibling project, and `make ledger` runs it:

```bash
go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest
```

## What this project will not trade away

**Nothing of yours reaches a sandbox unless a flag names it.** No implicit `$PWD` mount, no implicit
credential, no copy of your host SSH keys. That is R3 in [`SPEC.md`](SPEC.md#2-the-model), and
`TestWriteAgentLoginsIsOptIn` holds it: a login the caller did not name never lands in the seed.

**The two engines stay interchangeable.** One image, one trust model, one network fabric, the same
sharing flags and the same agent tools (R2). A feature that works under Podman and not under
Firecracker is unfinished, not shipped. The live tests run the same members against both.

**The image bakes in no identity.** No user name, uid, gid or per-user home (R6). Your user is
created at first boot, so one build serves every developer and every machine, and you never rebuild
the image to match your laptop.

**The trust matrix is the keys, not a check.** An agent sandbox cannot reach a user sandbox because
it holds no key that would open one (R19). `TestTrustMatrix` asserts the agent tier key never lands
in a user sandbox's `authorized_keys`, and it fails loudly when it does.

**A sandbox binds `127.0.0.1` and stays unprivileged.** SSH ports are loopback-only (R142) and
`--privileged` is opt-in (R143). `TestBuildRunArgsScaledDownCaps` and `TestBuildRunArgsPrivileged`
hold the capability set and the flag.

**No secret ever appears in an argv.** Injected values go through the seed's env file, because the
host's process table publishes a command line to every user on the machine.
`TestBuildRunArgsNoSecretsInArgv` greps the argv for an injected value and fails when it finds one.

## Tests are part of the change

Every behavior change ships with test coverage. A change with no test is only acceptable when the
behavior genuinely cannot be observed in a test. Say so in the PR.

- Put it in the **unit** tier (`make test`) if it can be: pure logic, or a real script/CLI driven
  with stubs. That tier is where the costly-if-silently-wrong things live: credential
  inheritance, seed trust material, instance state, the cobra tree with a fake `Runner`.
- Use the **integration** tier (`//go:build integration`) only for what needs a real engine, and
  make it skip gracefully when podman or the image is missing.
- The **smoke profile** is not a third tier: it is the subset of the integration tier that CI runs
  on every host, listed in the Makefile as `SMOKE_TESTS`. Keep it short, and add to it only for
  something the tiers above cannot reach.
- When a feature exists for all three agents (Claude, Codex, OpenCode), test all three. Usually
  one table, so a contract that drifts in one of them fails loudly.
- Test the contract, not the implementation: the exit code, the file mode, the thing another tool
  parses. Say *why* the case matters in a comment when it isn't obvious.

### Coverage

Every test target writes coverage into its own tier under `.coverage/`, so running several
aggregates rather than overwrites. `make coverage` merges what is there and prints the report.

`make coverage-check` runs inside `make check` and in CI. It fails when a package
`.coverage-baseline` lists stops being reached: presence, not a percentage. What it catches is a
suite that stopped running while the tests still report green. When a package is meant to lose its
coverage, rerun `make coverage-baseline` and commit the result.

In CI each job uploads its tier and one job merges them. Record a baseline only for the tiers CI
runs: `make coverage-baseline BASELINE_TIERS="unit race smoke"`.

## Commits

Keep one idea per commit. If a change will not fit that shape, it is doing more than one thing, so
split it.

**Subject**, always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does.

**Body**, only when the subject leaves a real question. Use bullets, one line each, under 60
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
agent's session or transcript: private to whoever ran it, dead to everyone else.

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
[`cs-lint`](https://github.com/codesweep-ai/lint) enforces the mechanical ones. It carries three
linters, and `make check` runs all three:

| Command | Target | What it checks |
|---|---|---|
| `cs-lint docs` | `make docs` | How the documents are written. |
| `cs-lint oss` | `make oss` | What this repository owes a reader as a published project. |
| `cs-lint walkthrough` | `make walkthrough` | Whether the documents still describe the software. |

The third checks the claims rather than the prose. Every command the docs name goes against the
binary's help tree, every setting against the code that reads it, and every sample output against
the command re-run now. `--run` lists every command the documents tell a reader to run, in reading
order, and `--review` prints the half that needs a reader.

Read what a rule wants with `--explain`, which prints the guidance behind each one rather than
leaving you to argue with the tool:

```bash
cs-lint oss --explain
```

1. **Write to the reader, in second person.** "Run `cs-sandbox doctor` first", not "the doctor
   command should be run first".

2. **Introduce a term where you first use it.** A reader meeting *seed*, *fabric* or *tier key* for
   the first time needs it introduced. Give a definition on the spot, an entry in a glossary table,
   or a link to the page that defines it.

3. **No em-dash.** The aside one introduces is a full stop, a comma, or a cut. It is also the
   first punctuation a model reaches for, so a page full of them reads as unedited whoever wrote it.

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

15. **State the point first, then qualify it.** Opening with the qualifier makes the reader decode
    the sentence backwards. "Rootless and per-user, so two developers on one host never collide"
    names its subject last. Start with the sandbox, and let the consequence follow it.

16. **Do not explain a design by contrast with a worse one.** "A directory, so a change reads as a
    diff rather than as one unreadable line" asks the reader to picture a format nobody proposed.
    Say what it is and what you get.

17. **A walkthrough is steps that work.** Put the reasons somewhere else. A reader working through
    one wants commands that run, not an account of which flag the engine used to spell differently.

18. **Do not make the reader hold two halves of a sentence apart.** "What the host shares may
    change; what the guest may reach may not" is a puzzle. Name the subject in each clause.

19. **Do not write in the register a model defaults to.** Untouched model output has a signature
    readers now recognise and discount. `cs-lint docs --explain` lists the words this house
    declines and what to write instead, so the table lives in one place rather than here. Two
    shapes matter as much as the words. Negative parallelism sets up a contrast nobody asked for.
    The rule of three is a rhythm rather than an argument, and a reader stops counting the third
    item as information.

These rules are about mechanics, and this project's voice is a strength: concrete, opinionated, and
free of padding. Where a rule fights the voice, the voice wins. Say so in the PR when it does.

Run the linter on its own while you write:

```bash
cs-lint docs              # check
cs-lint docs --stats      # per-file measurements
cs-lint docs --list       # which files are checked
cs-lint docs --explain    # what each rule wants, and the guidance behind it
```

Every knob lives in [`.cs-lint.yaml`](.cs-lint.yaml) at the repository root, one section per
linter. The `docs` section carries `glossary`, `skipExtra`, `lowercaseStarters` and `projectVerbs`.
When a real sentence trips the verb check, add the verb. When a report is noise, fix the config. A
linter that cries wolf gets ignored, and then it protects nothing.

A rule turned off for this repository is a waiver: a rule identifier and the reason it was traded
away, under `allow`. The reason is required, and it is printed with the finding, because a waiver
nobody can review is a rule deleted in private.

The linter is a project of its own, shared across this family. A fix to a check belongs there, and
reaches this repository the next time somebody installs it.
