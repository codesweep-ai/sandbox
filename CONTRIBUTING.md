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
3. Run `make check`, which is the same gate CI runs.
4. Open a pull request against `main`, and say what the change does and why.

Expect comments rather than silence, and expect a small change to move quickly. A reviewer asks
whether the change keeps the design rules below, whether a test fails without it, and where a reader
would find it documented.

By opening a pull request you agree that your contribution ships under the
[Apache 2.0 licence](LICENSE) this project is released under.

## Before you push

```bash
make check            # gofmt, go vet, unit tests and the linters; must pass
make test-smoke       # the subset of the live tests that CI runs, on every host
make test-integration # live engine tests; run when you touch create, engine or seed paths
make test-live-agents # the credential matrix against real providers; see below
```

`test-live-agents` is the one tier that is not part of any gate. For every supported credential
combination, shared and lent, it drives a live agent inside a sandbox and asks the model for one
word. Run it when you touch a credential path, and expect it to cost money: it spends provider quota
on every member.

Its keys come from a git-ignored `.env` at the repository root. The suite writes them into a
throwaway agent home, so it never touches your own profiles, and it skips any member whose key or
host login is absent.

`make check` needs three tools that do not come with Go. Install them once:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
go install golang.org/x/tools/cmd/deadcode@latest
go install github.com/codesweep-ai/lint/cmd/cs-lint@latest
```

A sandbox built from this repository carries all of these already, along with goreleaser and
`cs-sandbox` itself, so working on this project from inside one needs no setup.

Pin `golangci-lint` to the version above, the one CI runs. A newer release gains checks, and you
want to meet those when you upgrade the pin rather than on an unrelated pull request.

`cs-lint` is not pinned. CI installs it from source the same way you do, so a check it gains reaches
you on the day it lands.

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
internal shorthand. Use no conventional-commit prefix: `fix(proxy):` names a category rather than a
change, and the category is already in the diff.

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
4. **A walkthrough is steps that work.** Put the reasons somewhere else. A reader working through
   one wants commands that run.
5. **Describe what the software does, not how it came to do it.** Leave out what the project used
   to do, what was tried and dropped, and numbers from a run somebody did once.
6. **Do not explain a design by contrast with a worse one.** Say what it is and what you get,
   rather than asking the reader to picture a design nobody proposed.

The mechanical rules are enforced rather than restated here.
[`cs-lint`](https://github.com/codesweep-ai/lint) carries them, and `make check` runs it over this
repository. To read what a rule wants and the guidance behind it:

```bash
cs-lint docs --explain
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
- You ran `make check` and it passed.
- You cut what the tool added to fill space. A model pads a commit body to the shape it was shown,
  and comments that restate the code around them. Both read as noise to a maintainer, and both are
  yours to remove.

Keep the `Co-Authored-By:` trailer, which is how the work is disclosed. An unattended agent must not
open pull requests or comment on this repository.
