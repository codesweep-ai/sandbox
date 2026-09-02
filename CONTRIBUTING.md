# Contributing to cs-sandbox

These rules apply to **humans and coding agents alike**. If you are an agent working in this repo,
read this file before you change anything and follow it.

Bug reports and pull requests are welcome. For a security issue, use GitHub's private
vulnerability reporting on this repository's Security tab, rather than opening a public issue.

## Submitting a change

File a bug or an idea as a GitHub issue on this repository. For a fix that stands on its own, a pull
request on its own is enough. For anything that changes behaviour a user can see, open an issue
first, so the design gets settled before you write it.

1. Fork the repository, and create a branch off `main`.
2. Make the change, with its test.
3. Run `make ci`, which is every gate CI runs.
4. Open a pull request against `main`, and say what the change does and why.

Expect comments rather than silence, and expect a small change to move quickly. A reviewer asks
whether the change keeps the design rules below, whether a test fails without it, and where a reader
would find it documented.

By opening a pull request you agree that your contribution ships under the
[Apache 2.0 licence](LICENSE) this project is released under.

## Before you push

One command:

```bash
make ci
```

That is every gate the CI workflow has, on this machine and in the order the workflow takes them,
so a green run here is a green run there. `make check` is the faster subset to keep beside you
while you work, and `make ci` is the one that has to pass.

No linter needs installing. Every one the gates shell out to is pinned and built from the module
cache on first use: `golangci-lint`, `deadcode`, `actionlint`, `cs-lint` and `cs-ledger`.
`make repin` moves the `cs-` pins to the branch tip, and `make versions` says which builds the
gates used.

Moving a linter pin is an edit to `go.mod`, or to `go.golangci.mod` for `golangci-lint`. A linter
release reaches you when you ask for it, not on an unrelated pull request.

`goreleaser` is the one program still expected on the PATH. `make ci` validates the release
manifest with it, and `make build` falls back to `go build` where it is absent.

`make test-smoke` is the subset CI runs on every host, and `make ci` runs it last. A host that
cannot boot a sandbox skips those members rather than failing, which is what lets the same command
be right everywhere.

Two tiers still sit outside the gate, because each needs a host the workflow cannot assume.
`make test-integration` covers the live engine, so run it when you touch create, engine or seed.
`make test-live-agents` drives a real agent for every credential combination, shared and lent, and
it spends provider quota on every member: run it when you touch a credential path.

Its keys come from a git-ignored `.env` at the repository root. The suite writes them into a
throwaway agent home, so it never touches your own profiles, and it skips any member whose key or
host login is absent.

`make test-smoke` runs that same matrix with the model turns served from committed cassettes, as its
second half. It spends nothing and holds no credential, so the gate you already run covers the
credential paths. Keep the live matrix for when you need to know a provider still accepts what this
tool builds. `make test-agents-shared` and `make test-agents-lent` run one credential mode alone,
and `make test-agents-replay` runs both without the rest of the profile.

Re-record with `scripts/record-fixtures.sh`, which checks everything the matrix needs before it
clears a single cassette. Replay them before you commit. A recording can come out truncated when an
agent prints its answer and exits mid-response, and the replay is what says whether that mattered.

A recording writes `recorded.json` beside the cassette, naming the agent CLI and the version that
made it. The replay members compare that with the image they are about to boot. A cassette whose
agent has moved is skipped rather than failed. The CLI carries its own system prompt and tool list,
so a bump rewrites every request and the cassette can no longer match. The skip names both versions
and the command that re-records it. Cases whose agent did not move still run.

A sandbox built from this repository carries goreleaser and `cs-sandbox` itself as well, so
working on this project from inside one needs no setup.

This repository keeps a **ledger** of open issues in `ledger/`. Read
[`ledger/AGENTS.md`](ledger/AGENTS.md) before you start work, and follow it as you go. A commit
that touches `ledger/` needs `cs-ledger render && cs-ledger check` to pass first, and
`make ledger` runs the check half.

## Design rules

Your change has to keep these. Each one names the test or the review that holds it.

**Nothing of yours reaches a sandbox unless a flag names it.** No implicit `$PWD` mount, no implicit
credential, no copy of your host SSH keys. That is R3 in [`SPEC.md`](SPEC.md#2-the-model), and
`TestWriteAgentLoginsIsOptIn` holds it: a login the caller did not name never lands in the seed.

**The two engines stay interchangeable.** One image, one trust model, one network fabric, the same
sharing flags and the same agent tools (R2). A feature that works under Podman and not under
Firecracker is unfinished, not shipped. The live tests run the same members against both.

**The image bakes in no identity.** No user name, uid, gid or per-user home (R6). Your user is
created at first boot, so one build serves every developer and every machine.

**The trust matrix is the keys, not a check.** An agent sandbox cannot reach a user sandbox because
it holds no key that would open one (R19). `TestTrustMatrix` asserts the agent tier key never lands
in a user sandbox's `authorized_keys`.

**A sandbox binds `127.0.0.1` and stays unprivileged.** SSH ports are loopback-only (R142) and
`--privileged` is opt-in (R143). `TestBuildRunArgsScaledDownCaps` and `TestBuildRunArgsPrivileged`
hold the capability set and the flag.

**No secret ever appears in an argv.** Injected values go through the seed's env file, because the
host's process table publishes a command line to every user on the machine.
`TestBuildRunArgsNoSecretsInArgv` greps the argv for an injected value and fails when it finds one.

## Tests

Ship a test with your change. Where a behaviour genuinely cannot be observed in a test, say so in
the pull request.

Put it in the unit tier (`make test`) if it can go there. That is where the
costly-if-silently-wrong things live: credential inheritance, seed trust material, instance state,
and the cobra tree with a fake `Runner`. Use the integration tier only for what needs a real engine,
and make it skip gracefully when podman or the image is missing.

When a feature exists for all three agents (Claude, Codex, OpenCode), test all three. Usually one
table, so a contract that drifts in one of them fails loudly.

Test the contract, not the implementation: the exit code, the file mode, the thing another tool
parses. Say why the case matters in a comment when it is not obvious.

The live-agents tier is deliberately outside the coverage tiers. A provider being slow or
rate-limiting is not a defect in this repository, and a gate that fails for that reason teaches
people to ignore it.

Never lower a coverage baseline to make a run green. [`SPEC.md`](SPEC.md#16-conformance-and-testing)
holds the tiers, what each one proves, the smoke profile, and how coverage is measured and gated.

## Commits

**Keep it short.** One idea per commit, and a message a reader takes in at a glance. If a change
will not fit one idea, split it.

**Subject**, always. Under 60 characters, imperative, no trailing period, completing *"If applied,
this commit will …"*. Say what the change does, in plain English rather than in this project's
internal shorthand. Use no category label: `fix(proxy):`, `bugfix:` and `[docs]` each name a class
of change rather than the change itself, which the diff already shows. The gate fails on one, so
amend before you push.

**Body**, rarely. Most commits need none. Add one only when the subject leaves a question a reader
would otherwise have to open the diff to answer, and then answer that question. A sentence or two
does it. Wrap it at 72 columns.

Leave out how the work was scheduled, how you tested it, and what led you to it, and stop once the
question is answered. A second paragraph usually means the message has turned into a report of the
session. A rule's reason belongs beside the rule in [`SPEC.md`](SPEC.md), and the investigation that
found it belongs in the pull request.

```
Fix the typo in the firecracker boot arg name
```

```
Reject a base rootfs that is not a filesystem

A blkid probe answers before the boot does, so the happy path
costs nothing and the failure names the real cause.
```

Keep the `Co-Authored-By:` trailer when an agent wrote the change. Drop any trailer linking to the
agent's session or transcript. Such a link is private to whoever ran it and dead to everyone else,
and it cannot be fixed after publication.

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

Six principles do most of the work. Read them before you write a document, and apply them when you
edit one:

1. **Introduce a term where you first use it**, in the same sentence, or link to the page that
   defines it. A reader should never meet a word the docs have not explained.
2. **State the point first, then qualify it.** Opening with the qualifier makes the reader decode
   the sentence backwards.
3. **Give every sentence a subject and a verb.** "Two version numbers, one verdict, one remedy"
   reads as knowing rather than clear. Say what the thing is.
4. **A how-to is steps that work.** Put the reasons somewhere else. A reader working through
   one wants commands that run.
5. **Describe what the software does, not how it came to do it.** Leave out what the project used
   to do, what was tried and dropped, and numbers from a run somebody did once.
6. **Do not explain a design by contrast with a worse one.** Say what it is and what you get,
   rather than asking the reader to picture a design nobody proposed.

The mechanical rules are enforced rather than restated here.
[`cs-lint`](https://github.com/codesweep-ai/lint) carries them, and `make check` runs it over this
repository. To read what a rule wants and the guidance behind it:

```bash
cs-lint prose --explain
```

That listing is the authority. Where this section and the linter disagree, the linter is right.
Turning a check off is a waiver: write it under `allow` in [`.cs-lint.yaml`](.cs-lint.yaml) with the
reason, which is printed with the finding.

## AI-assisted contributions

An agent wrote most of this repository, and you are welcome to use one. The standard is the same
either way: you are responsible for what you submit.

Point your tool at [`AGENTS.md`](AGENTS.md), which routes it to the documents that hold the
conventions, and check three things before you open the pull request:

- You understand every line, and can answer a question about it without going back to the tool.
- You ran `make ci` and it passed.
- You cut what the tool added to fill space. A model pads a commit body to the shape it was shown,
  and comments that restate the code around them. Both read as noise to a maintainer, and both are
  yours to remove.

Keep the `Co-Authored-By:` trailer, which is how the work is disclosed. An unattended agent must not
open pull requests or comment on this repository.
