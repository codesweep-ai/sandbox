# The OpenCode adapter

How cs-sandbox integrates [OpenCode](https://github.com/anomalyco/opencode) as its third agent
CLI, alongside Claude Code and Codex. This is the reference for the adapter's design decisions,
the upstream behaviors it depends on, and the version-bump procedure. Usage of the remote tools is
documented next to the scripts (`~/.local/bin/CS_OPENCODE_REMOTE*.md`); the general
bundled-agent-tools model is in [design.md](design.md#bundled-agent-tools-and-login).

Validated against **opencode v1.18.10**, live against a real provider — see
[Live validation record](#live-validation-record) for what was actually measured.

## Install and pinning

The image installs the single bun-compiled release binary (~180 MB) into read-only
`/opt/opencode/bin`, pinned by `ARG OPENCODE_VERSION` in `image/Containerfile`. Unlike Claude Code
(manifest checksum) and Codex (`codex-package_SHA256SUMS`), upstream publishes **no SHA256SUMS for
the CLI tarballs** — its minisign signatures cover only the desktop assets — so the Containerfile
carries **self-recorded** per-arch `ARG OPENCODE_SHA256_*` digests, captured from the release
assets. opencode's one external runtime dependency (ripgrep) is already in the image.

**The first run in a fresh profile pays a one-time cost**: opencode installs ~40 MB of plugin
state (`node_modules`, `package.json`) into `OPENCODE_CONFIG_DIR`, silently. Measured in a
Firecracker sandbox: ~13 s for the first turn against ~5 s warm — noticeable, not alarming. Leave
`CS_OPENCODE_TIMEOUT` generous anyway: one first run on a workstation stalled past 180 s with no
output, and the cause was never identified.

## Profile isolation (the `cs-opencode` wrapper)

The wrapper isolates everything under `~/.cs-opencode` using opencode's first-class overrides —
never blunt XDG redirection, which children of opencode's bash tool would inherit:

| Mechanism | Effect |
|---|---|
| `OPENCODE_CONFIG_DIR=~/.cs-opencode` | config (`opencode.json` with the pinned model and blanket-allow permissions), global `AGENTS.md` hub |
| `OPENCODE_DB=~/.cs-opencode/opencode.db` | the SQLite session store, isolated per profile |
| `OPENCODE_AUTH_CONTENT` (inline, from `~/.cs-opencode/auth.json` when seeded) | credentials **never touch opencode's data dir**, where upstream keeps `auth.json` beside the session db |
| `~/.cs-opencode/env` (sourced with `set -a`) | provider API keys (e.g. `FIREWORKS_API_KEY`), scoped to the agent |
| `OPENCODE_DISABLE_AUTOUPDATE=1`, `OPENCODE_DISABLE_SHARE=1` | /opt is read-only anyway; silence the nag, keep sessions private |

Opencode's XDG data/cache dirs (`~/.local/share/opencode`, `~/.cache/opencode`) deliberately stay
put: `OPENCODE_CONFIG_DIR` does not relocate them, and redirecting XDG wholesale would leak into
every child of opencode's bash tool. That is fine with one user and one profile, and it is exactly
why the credential goes in inline — upstream's `auth.json` would otherwise land in that *shared*
data dir rather than in the profile.

YOLO (`CS_OPENCODE_YOLO` or the `~/.cs-opencode/.yolo` marker, written by `create --yolo`) appends
`--auto`, and only to the TUI and `run`, since the other subcommands reject it. The shipped config
already blanket-allows tool permissions; `--auto` covers the residual asks (`doom_loop`,
out-of-workspace paths, `.env` reads). Unlike claude and codex there is no directory-trust gate.

### The model pin, and why it fails closed

Two config keys do that job together. `model` pins the model —
`fireworks-ai/accounts/fireworks/models/kimi-k3`, via `FIREWORKS_API_KEY` — because opencode
resolves no deterministic one without it. `disabled_providers: ["opencode"]` then turns off the
**OpenCode Zen** gateway, which upstream loads automatically and which serves free models to
anonymous callers: it is the only provider that works with no credential, so it is the only thing a
broken pin could silently fall back to — and that fallback would send the sandbox's code to a third
party the user never chose. Verified at 1.18.10 with the pin set and `FIREWORKS_API_KEY` absent:

| | Zen loaded (upstream default) | Zen disabled (shipped) |
|---|---|---|
| `opencode run` (what the turn driver uses) | fails, exit 1 `ProviderModelNotFoundError` | fails, exit 1 |
| the interactive TUI (`cs-opencode`) | **silently selects `opencode/big-pickle`** | no model available |

Because the TUI row differs and the `run` row does not, the fix has to be config rather than wrapper
logic. It also removes a driver failure mode: an attached `run` against a TUI that had fallen back
to Zen would *wedge* until the stall watchdog (exit 2), where now it fails in seconds with a real
reason (exit 5).

To run a different provider, edit `~/.cs-opencode/opencode.json` in the sandbox (or pass
`-m provider/model` for one launch) and put that provider's key in `~/.cs-opencode/env`; dropping
`"opencode"` from `disabled_providers` re-enables the free gateway.

**The Zen guard alone is duplicated at `~/.config/opencode/opencode.json`.** `/opt/opencode/bin` is
on `PATH`, so running bare `opencode` is an easy slip, and it picks up none of the profile above.
That slip is survivable; silently shipping the sandbox's code to an anonymous gateway is not, so the
XDG default closes that one path and nothing else. The binary is deliberately not aliased or
shimmed — bare `claude` and `codex` are unconfigured in just the same way.

## Turn driver architecture (`cs-opencode-turn`)

The driver never scrapes the TUI pane (opencode's TUI strings are localized and unstable). It
drives the HTTP API that the TUI hosts when launched with `--port`:

1. **The session id exists before the TUI launches.** A transient `cs-opencode serve` on the same
   derived port mints it via `POST /session`, then the warm tmux TUI comes up as `cs-opencode
   --port <p> -s <ses_id>`. Launched bare it would sit on its home screen and never show
   externally driven turns; with `-s`, a human attaching watches the driven session render live.
2. **Readiness** is `GET /global/health`, plus a `GET /session/<id>` check that the server on
   this port actually knows our session (guards against a foreign server on a colliding port).
3. **The turn** is a blocking `cs-opencode run --attach http://127.0.0.1:<p> -s <id> --auto
   "<prompt>"`. Unattended runs must always pass `--auto`: without it, any permission ask in
   non-interactive `run` is silently auto-REJECTED.
4. **Stall watchdog**: `GET /session/status` returns a map that *deletes* a session's entry when
   it goes idle — absent key ⇒ idle. Continuously idle past `CS_OPENCODE_STALL_SECS` (default
   180) while the attached client has not returned ⇒ exit 2.
5. **The turn is delimited by message IDENTITY, not position.** The driver anchors on the `id` of
   the last message existing before submission and takes everything after it, because the history
   is not append-only — compaction rewrites it and inserts `summary` messages, so a count-based
   slice silently goes wrong. If compaction removed the anchor, `time.created >= turn start`
   bounds the turn instead.
6. **Completion requires a postcheck** (see the first upstream reality below), scoped to THIS
   turn: its last assistant message must have no `info.error` and a set `info.time.completed`.
   Checking the *session's* last assistant message would instead pass on the previous turn's reply
   whenever this one produced nothing — a silent empty success.
7. **Output is read back from the session API**, not the client's stdout, and from the SAME fetch
   as the postcheck — two fetches could straddle a concurrent write and disagree.

Exit codes: `0` ok, `1` usage, `2` timed out/stalled, `3` launch/setup failure, `5` turn failed.
`0`–`3` match the claude/codex drivers; `4` is skipped because `cs-opencode-remote` spends it on
"session busy" as its siblings do, and `5` is the state only opencode has — a turn that ran to
completion and still failed. The session id comes back as a trailing
`__CS_OPENCODE_SESSION_ID__ <ses_id>` sentinel, which `cs-opencode-remote` maps to the session name
for `--resume`.

The TUI port is derived deterministically from the tmux token: `21000 + (first 12 bits of
md5(token))`. Nothing is persisted; every resume of the same token computes the same port. A
collision between two live sessions on one guest is **fail-closed** — the session-known check
refuses to drive a foreign TUI (exit 3) — at the cost of availability for the losing session.

## Upstream realities the design depends on (verified at 1.18.10)

Each one shaped the driver, and "simplifying" any of them away reintroduces a silent failure mode:

1. **An attached `run` exits 0 on provider-side errors.** Client-path failures (bad model slug,
   bad session) exit 1, but an error raised inside the server's session loop — a 401 from the
   provider, say — is recorded only on the final assistant message (`info.error` set,
   `info.time.completed` absent) and reaches neither the client's exit code nor its `--format json`
   stream, which emits nothing at all. Hence the mandatory postcheck. An UNATTACHED `run` gets all
   three right, so the postcheck, the identity anchoring and the derived port are the price of
   attaching.
2. **An attached `run` prints nothing to its own stdout** when a TUI hosts the server — the TUI is
   the renderer. Hence reading turn output back from the API.
3. **Session ids are mixed-case base62**: `ses_` + 26 chars matching `[A-Za-z0-9]` (e.g.
   `ses_04198268affeeKLgivDfdCnHrm`). Every validation regex uses `^ses_[A-Za-z0-9]{20,40}$`,
   with the length range left loose for upstream drift.
4. **Only OpenCode Zen serves an anonymous caller.** Every other provider needs a credential, so
   with `disabled_providers: ["opencode"]` a sandbox that has not been given a key has no model at
   all — which is the point. If upstream adds a second zero-credential provider that loads
   automatically, it has to be disabled here too or the pin stops failing closed.
5. **Sessions live in SQLite** (`opencode.db`, WAL mode, `OPENCODE_DB` honored), not a JSON tree.
   `opencode export <id>` writes the whole conversation to stdout — tool inputs/outputs and the
   per-message `error`/`time.completed` fields, no credential material — with its banner on stderr,
   so a capture with `2>/dev/null` parses as JSON. Concurrent WAL reads while a TUI is live are
   fine.

## Remote tool family

`cs-opencode-remote{,-output,-sessions,-forget}` are pattern copies of the codex family with the
identical host-side contract — name→{token,id,host,workdir} maps, per-session flock, driver
scp-deploy on checksum change, the authoritative `--- … --- finished (exit N) ---` footer, and the
`-s` status contract (`finished=0 / unknown=1 / running=2`).

The two data-plane tools differ, because there is no rollout JSONL to tail — opencode keeps
everything in one SQLite db. Both go through supported CLI surfaces rather than that schema:

- `cs-opencode-remote-status` runs `cs-opencode export <id>` over SSH (one JSON document; not
  directory-scoped, so it works from the login directory).
- `cs-opencode-remote-sessions -v` runs `cs-opencode session list --format json`. That one IS
  directory-scoped, so the query is grouped by (host, workdir) using the workdir recorded for each
  session — the directory the driver created it in.

Neither touches opencode's internal tables. Reading last-activity with a raw `SELECT id,
time_updated FROM session` works today, but that schema is not a surface the project owes us: an
upstream rename would blank the column silently.

## Credentials

- **The usual auth path is a provider API key** in `~/.cs-opencode/env` (0600). `cs-sandbox
  create --env FIREWORKS_API_KEY` injects the variable by NAME; writing the env file inside the
  guest keeps the value durable for warm tmux TUIs whose shells don't inherit create-time env.
  Values never appear in host argv, state, or logs.
- `--inherit-agent-login opencode` seeds `~/.cs-opencode/auth.json` (installed 0600, first boot
  only), which the wrapper folds into inline `OPENCODE_AUTH_CONTENT`. As for claude and codex,
  only the credential file is carried — never `env`, never any other profile state.

## Live validation record

Measured at 1.18.10 on the pinned kimi-k3 with a real Fireworks key, one-word prompts, in two
places. **Workstation**: isolated `$HOME`, the credential through the process environment only,
never to disk. **Sandbox**: after `cs-sandbox build`, inside a real `--yolo` Firecracker sandbox on
the rebuilt image, key injected by `create --env` — the path a user actually takes.

| What | Where | Result |
|---|---|---|
| Full turn through `cs-opencode-turn` (mint → warm TUI → attached run → postcheck → read-back), driven by `cs-opencode-remote --new` | both | reply returned, exit 0; 29.5 s in the sandbox |
| Second turn on the same warm session (`--resume`) | both | returned only its own reply — the identity anchor, on real history |
| `cs-opencode-remote-status` | both | both turns rendered with timestamps |
| `cs-opencode-remote-sessions -v` | sandbox | last-activity resolved through `session list --format json` |
| `-b` background turn | sandbox | `running`/exit 2 → `finished`/exit 0, footer written |
| `cs-opencode run` in the guest | sandbox | reply on the pinned kimi-k3; 12.9 s cold, 5.1 s warm |
| Unattached `run`, `run -s <id>`, `run --format json` | workstation | reply on stdout, exit 0; resumed and recalled the earlier turn; structured event stream |

The `sessions -v`, `-status` and `-b` rows also record why the driver is shaped the way it is: they
are what a **CLI-only driver** would rest on — one `cs-opencode run -s <id>` per turn, no HTTP, no
warm TUI. That design is viable and would delete the postcheck, the identity anchoring and the
derived port. It is not the one taken, because it costs `cs-opencode-remote --attach` (watching a
live session), which the claude and codex families both have.

Testing note: `tmux` resets `HOME` from the passwd entry, so a driver test cannot relocate the
profile with `HOME=…` alone — the TUI it launches will read the real one. Inside a sandbox this is
moot (`HOME` *is* the passwd home); on a workstation, put a `cs-opencode` shim on `PATH` that
re-exports `HOME` before exec'ing the real wrapper.

## Version bumps

Upstream releases multiple times per week, so bumps are routine and MUST re-verify the behavior
contract, not just the checksum:

1. Update `OPENCODE_VERSION` in `image/Containerfile`; download both release tarballs
   (`opencode-linux-{x64,arm64}.tar.gz`) and re-record both `OPENCODE_SHA256_*` digests.
2. Re-probe the five upstream realities above against the new binary (an isolated `$HOME`, a
   real provider key, one cheap model): attached-run exit codes on a forced provider failure,
   attached-run stdout, the session-id shape, `/session/status` idle semantics, and — with the key
   removed — that `opencode models` is empty and the TUI offers no fallback model.
3. `go test ./...` — the contract tests stub the endpoints and pin the driver's semantics.
4. Rebuild the **host binary first** (`make build && make install` — the image assets are
   embedded in it), then the image (`cs-sandbox build`). Skipping the binary rebuild
   re-propagates stale guest scripts: `deploy_driver` pushes the caller's baked copy over any
   newer one on the target.
