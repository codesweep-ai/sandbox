# Keeping a ledger

You are working in a repository that keeps a **ledger**: a directory of JSON files recording what
is wrong with the code and what to do next. Each file is one **record**. You write the records. A
human reads them as a generated page. This guide tells you how to keep the ledger honest, whatever
harness you are running under.

For the command surface, run `cs-ledger manual`. It prints the verbs, the flags and the exit
codes from inside the binary.

## What you are maintaining

```
ledger/
  ledger.json         config: the project name, the record ID prefix, the version pin
  issues/<ID>.json    one record per issue
  drafts/<member>/    un-numbered observations, in multi-agent runs only
  queue.json          the ordered recommendation of what to fix next
  ledger.html         GENERATED: never edit it, never hand-merge it
  GUIDE.md            GENERATED: this guide
  AGENTS.md           the router that points here, written once and then yours
```

The page is a pure function of the JSON. You change a record, you re-render, and the two travel
together in one commit.

## Start here every session

Read `ledger/queue.json` and the open records before you do anything else. They are the handoff
from whoever worked here last: what is in flight, what was picked next, and why. Treat the queue
as the standing priority until the human redirects you.

## The five moves

### 1. Observe something, file a record

File the moment you find a defect, think of an improvement, or receive an idea. Do it before you
fix anything, so the observation survives even if the fix does not land.

```json
{
  "id": "MYS-014",
  "title": "Retry loop drops the last error before giving up",
  "type": "defect",
  "severity": "high",
  "status": "open",
  "foundBy": "session agent, while reading internal/client/retry.go",
  "opened": "2026-08-15",
  "resolved": null,
  "stint": null,
  "evidence": { "commits": [], "integrated": [], "verified": null },
  "resolution": null,
  "details": "After the third attempt the loop returns a generic timeout...",
  "notes": [],
  "links": ["MYS-009"]
}
```

Every one of those fields is required, including the nulls. The filename must equal the `id`.

Four fields carry judgement, and they are the ones agents get wrong:

- **`foundBy`** is real provenance. Quote the human when a human said it: `user report (host):
  "the columns shift"`. Name the process when a process found it: `nightly smoke suite`. Name
  yourself when you noticed it. Never attribute your own observation to the human because one
  happens to be driving.
- **`details`** is the original observation, in markdown, written for a reader with no context.
  Say what you saw, how to reproduce it, and why it matters. Later developments belong in `notes`.
- **`type`** is `defect`, `improvement` or `feature-idea`, and **`severity`** is `low`, `med`,
  `high` or `critical`. A roadmap direction is a `feature-idea` with status `open`, not a separate
  pile.
- **`links`** holds the IDs of records you are stating a relationship to, like
  `"found by MYS-009's verification"`. A shared resolving commit is evidence overlap rather than a
  relationship, so do not link it.

### 2. Start work, mark it

Set `status` to `in-progress` and set `stint` to whatever selected the work. A **stint** is one
scoped burst of work: an orchestrated run like `rev-5`, a branch like `exp/retry`, or a dated
session. This is what makes in-flight work visible to the human, and `check` requires it.

### 3. Learn something, append a dated note

```json
"notes": [{ "date": "2026-08-15", "text": "Reproduced on Linux only; macOS retries cleanly." }]
```

`notes` is append-only and the dates must not go backwards. Never rewrite `details` and never
rewrite history. A record earns its value by accreting honestly.

### 4. Finish, close with evidence

`status: "closed"` needs three things:

- **`resolved`**: the date.
- **`evidence.commits`**: the sha that resolved it. Add `evidence.integrated` when your workflow
  separates integration commits. A record closed by its children may instead carry `links` to them.
- **`evidence.verified`**: how you proved it, with the measurement. The word "fixed" does not
  prove anything. Write what you ran and what it returned:
  `go test ./internal/client/ -count=1: TestRetryKeepsTheLastError PASS, 20 runs`. If you did not
  verify it, you are not done.

Two rules about the order you do this in. Write `verified` only after the verification has
actually run: a pre-drafted claim is a lie waiting for a probe to find it. And land the fix
before you close the record, because you have to cite a sha that exists. Never predict one, and
never amend the fix to contain its own closure.

Abandonment is honest too. Use `wont-fix` or `moved-to-roadmap`, set `resolved`, and write a
`resolution` saying why. The ID stays dead.

### 5. Land work, re-triage the queue

The **queue** is the ordered recommendation of what to fix next:

```json
{
  "recommendedBy": "session agent",
  "updated": "2026-08-15",
  "items": [{ "id": "MYS-014", "why": "Hides the real failure from every caller." }]
}
```

Every item names a live record and gives a one-line reason. After you close something queued, or
learn something that reorders priorities, update the items and the `updated` date. `check` warns
when an open `critical` record is missing from the queue. That warning is a triage gap: queue the
record, or lower its severity and say why in a dated note. Silencing the warning is not a reason.

## Write so a reader can act

A human opens the page to find out where the project stands. They were not in the session that
produced the record, and they may be reading it months later. Every field you write is read cold,
by someone deciding what to do next. A record they have to decode is a record that does not help
them.

- **Write sentences, not labels.** `Retry handling: the loop and its backoff` names a topic and
  stops. `The retry loop drops the last error before giving up` says what is wrong.
- **Put the point first, then qualify it.** A reader who meets the qualifier first has to decode
  the sentence backwards.
- **One idea per sentence, and under 30 words.** A record built from semicolons, parentheses and
  bold labels reads as notes you left for yourself.
- **Expand the shorthand.** A term you coined this session means nothing next quarter, so introduce
  it where you first use it. A bare ID is not a reference: write `the queue warning added in
  MYS-009` rather than `MYS-009`.
- **Use at most one em-dash per paragraph.** Where a second one appears, a full stop usually works
  better.
- **Say what is, not what was tried.** The approach you abandoned belongs in a note if it belongs
  anywhere.
- **Delete the frame.** `It is worth noting that` and `put simply` comment on your writing instead
  of getting on with it. Say the thing.

The fields owe a reader different things:

| Field | What it has to give them |
|---|---|
| `title` | One sentence naming the problem, readable on its own in a list of fifty. |
| `details` | The observation in full, for someone with no context: what you saw, how to reproduce it, why it matters. |
| `notes` | What changed and what it means, rather than a status ping. |
| `evidence.verified` | The command you ran, and the number it gave back. |
| `resolution` | Why this stopped, in terms someone could disagree with. |
| `queue[].why` | One line on why this is next, that works without opening the record. |

These rules do not tell you what to record. They tell you how to write it so a reader can use it.

## Always

- **Render, then check, then commit.** Run `cs-ledger render && cs-ledger check`. Commit the
  records and `ledger.html` together, or the freshness gate fails on the next run.
- **Never hand-edit or hand-merge `ledger.html`.** On a merge conflict, re-render it.
- **IDs are forever.** They are zero-padded and ascending. Never renumber one and never reuse one.
- **Unknown is never zero, and absence is never guessed.** No invented dates, no verification you
  did not measure, no scope you narrowed without saying so.

`render` is the only command that writes. It rewrites the page, records which renderer wrote it in
`ledger.json`, and brings this guide up to the binary. When `check` says the page came from a
different renderer than yours, that is what `render` fixes.

## Which mode are you in

**A human is driving.** You are the only writer. Mint the next ID directly and skip `drafts/`.
The human's words are your `foundBy` provenance. File records as the conversation produces them
rather than in a batch at the end. The evidence rules still bind you: verify before closing, even
when nobody asked.

**You are one member of a multi-agent run.** Only the integration branch mints IDs. Write a
**draft** instead: a file at `ledger/drafts/<member>/<slug>.json`, in the same shape as a record
but with no `id` field. The orchestrator promotes drafts to numbered records at integration.
Creating `issues/<ID>.json` on a member branch risks a silent clobber, because a colliding
filename does not conflict under a pathspec checkout. The queue has one writer, and it is the
orchestrator.

<!-- LEDGER:PROJECT -->

## Conventions for this project

Everything above this marker is generated, and `cs-ledger check` holds it byte-for-byte against
the binary. Everything below is yours to write, and the tool never touches it.

Record what an agent cannot infer from the schema: the exact gate command for this repo, what a
good `evidence.verified` line looks like here, and who owns the queue.
