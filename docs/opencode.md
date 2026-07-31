# The OpenCode adapter

How cs-sandbox integrates [OpenCode](https://github.com/anomalyco/opencode) as its third agent
CLI, alongside Claude Code and Codex. This is the persistent reference for the adapter's design
decisions, the upstream behaviors it depends on, and the version-bump procedure. Usage of the
remote tools is documented next to the scripts (`~/.local/bin/CS_OPENCODE_REMOTE*.md`); the
general bundled-agent-tools model is in [design.md](design.md).

Validated against **opencode v1.18.10** (2026-07-31), live on a Firecracker guest and through
both `cs-campaign` roles.

## Install and pinning

The image installs the single bun-compiled release binary (~179 MB) into read-only
`/opt/opencode/bin`, pinned by `ARG OPENCODE_VERSION` in `image/Containerfile`. Upstream
publishes **no SHA256SUMS for the CLI tarballs** (its minisign signatures cover only the desktop
assets), so the Containerfile carries **self-recorded** per-arch sha256 constants, captured from
the release assets at authoring time. opencode's one external runtime dependency (ripgrep) is
already in the image.

## Profile isolation (the `cs-opencode` wrapper)

The wrapper isolates everything under `~/.cs-opencode` using opencode's first-class overrides —
never blunt XDG redirection, which children of opencode's bash tool would inherit:

| Mechanism | Effect |
|---|---|
| `OPENCODE_CONFIG_DIR=~/.cs-opencode` | config (`opencode.json` with the pinned model and blanket-allow permissions), global `AGENTS.md` doctrine |
| `OPENCODE_DB=~/.cs-opencode/opencode.db` | the SQLite session store, isolated per profile |
| `OPENCODE_AUTH_CONTENT` (inline, from `~/.cs-opencode/auth.json` when seeded) | credentials **never touch opencode's data dir**, where upstream keeps `auth.json` beside the session db |
| `~/.cs-opencode/env` (sourced with `set -a`) | provider API keys (e.g. `FIREWORKS_API_KEY`), scoped to the agent |
| `OPENCODE_DISABLE_AUTOUPDATE=1`, `OPENCODE_DISABLE_SHARE=1` | /opt is read-only anyway; silence the nag, keep sessions private |

YOLO (`CS_OPENCODE_YOLO` or the `~/.cs-opencode/.yolo` marker, written by `create --yolo`)
appends `--auto` — only to the TUI and `run`, since other subcommands reject the flag. The
shipped profile config already blanket-allows tool permissions; `--auto` covers the residual
asks (`doom_loop`, out-of-workspace paths, `.env` reads). There is no directory-trust gate to
pre-answer, unlike claude/codex.

The shipped model pin is `fireworks-ai/accounts/fireworks/models/kimi-k3` (provider env var
`FIREWORKS_API_KEY`). A zero-config opencode does NOT resolve a deterministic model — it picks
the first provider's top-sorted model, which can be an unauthenticated gateway route — so the
profile `model` key must always be set.

## Turn driver architecture (`cs-opencode-turn`)

The driver never scrapes the TUI pane (opencode's TUI strings are i18n-localized and unstable).
It drives the HTTP API that the TUI hosts when launched with `--port`:

1. **Session id exists before the TUI launches.** New sessions are minted by a transient
   `cs-opencode serve` on the same derived port + `POST /session`, then the warm tmux TUI is
   launched `cs-opencode --port <p> -s <ses_id>`. A TUI launched bare sits on its home screen
   and never shows externally driven turns; launched with `-s`, a human attaching watches the
   driven session render live.
2. **Readiness** is `GET /global/health`, plus a `GET /session/<id>` check that the server on
   this port actually knows our session (guards against a foreign server on a colliding port).
3. **The turn** is a blocking `cs-opencode run --attach http://127.0.0.1:<p> -s <id> --auto
   "<prompt>"`. Unattended runs must always pass `--auto`: without it, any permission ask in
   non-interactive `run` is silently auto-REJECTED.
4. **Stall watchdog**: `GET /session/status` returns a map that *deletes* a session's entry when
   it goes idle — absent key ⇒ idle. Continuously idle past `CS_OPENCODE_STALL_SECS` (default
   180) while the attached client has not returned ⇒ exit 2.
5. **Completion requires a postcheck** (see the first upstream reality below): after the client
   exits 0, the last assistant message from `GET /session/<id>/message` must have no
   `info.error` and a set `info.time.completed`, else the turn failed (exit 4).
6. **Output is read back from the session API**, not from the client's stdout: the driver
   snapshots the session's message count before submitting and afterwards emits the text parts
   of the new assistant messages (`--format json` mode emits the raw new message objects).

Exit codes: `0` ok, `1` usage, `2` timed out/stalled, `3` launch/setup failure, `4` turn failed.
The session id is emitted as a trailing `__CS_OPENCODE_SESSION_ID__ <ses_id>` sentinel, which
`cs-opencode-remote` maps to the session name for `--resume`.

The TUI port is derived deterministically from the tmux token: `21000 + (first 12 bits of
md5(token))`. Nothing is persisted; every resume of the same token computes the same port. A
collision between two live sessions on one guest is **fail-closed** — the session-known check
refuses to drive a foreign TUI (exit 3) — but costs availability; widening the hash with
collision detection is tracked follow-up work.

## Upstream realities the design depends on (verified at 1.18.10)

These were established by live probes and re-confirmed by the live gates; each one shaped the
driver, and "simplifying" any of them away will reintroduce a silent failure mode:

1. **An attached `run` exits 0 on provider-side errors.** Client-path failures (bad model slug,
   bad session) exit 1, but errors raised inside the server's session loop — e.g. a 401 from the
   provider — are recorded only on the session's final assistant message (`info.error` set,
   `info.time.completed` absent) and are NOT propagated to the attached client's exit code or
   its `--format json` event stream (which emits nothing at all). Hence the mandatory postcheck.
2. **An attached `run` prints nothing to its own stdout** when a TUI hosts the server — the TUI
   is the renderer. Hence the message-count snapshot and API read-back for turn output.
3. **Session ids are mixed-case base62**: `ses_` + 26 chars matching `[A-Za-z0-9]` (e.g.
   `ses_04a3a7944ffePl0Z5e97dF33WK`). Every validation regex uses
   `^ses_[A-Za-z0-9]{20,40}$` with length tolerance for upstream drift.
4. **Sessions live in SQLite** (`opencode.db` + `-wal`/`-shm`, WAL mode; `OPENCODE_DB`
   honored). The legacy JSON `storage/session*` tree is gone. `opencode export <id>` emits the
   full conversation (`{info, messages: [{info, parts}]}`) including tool inputs/outputs and the
   per-message `error`/`time.completed` fields, with no credential material; `opencode db
   "<sql>" --format json|tsv` gives raw queries. Concurrent WAL reads while a TUI is live are
   fine.

## Remote tool family

`cs-opencode-remote{,-output,-sessions,-forget}` are pattern copies of the codex family with the
identical host-side contract — name→{token,id,host,workdir} maps, per-session flock,
driver scp-deploy on checksum change, `setsid`-hardened background runners, the authoritative
`--- ... --- finished (exit N) ---` footer, PID-verified `--kill`, and the `-s` status contract
(`finished=0 / unknown=1 / running=2 / failed=3`) that `cs-campaign` consumes. The two
data-plane tools differ because there is no rollout JSONL to tail: `cs-opencode-remote-status`
runs `cs-opencode export <id>` over SSH, and `cs-opencode-remote-sessions -v` queries
`cs-opencode db "SELECT id, time_updated FROM session"`.

## Credentials and evidence

- **Primary auth path is a provider API key** in `~/.cs-opencode/env` (0600). `cs-sandbox
  create --env FIREWORKS_API_KEY` injects the variable by NAME; writing the env file inside the
  guest keeps the value durable for warm tmux TUIs whose shells don't inherit create-time env.
  Values never appear in host argv, state, or logs.
- `--inherit-agent-login opencode` seeds `~/.cs-opencode/auth.json` (installed 0600, first boot
  only), which the wrapper folds into inline `OPENCODE_AUTH_CONTENT`. There is deliberately
  **no fallback** to a personal opencode profile: upstream keeps its `auth.json` in the DATA
  dir (`~/.local/share/opencode`, beside the session db), and API keys are the primary path.
- **Archive/evidence allowlist** (consumed by `cs-campaign`): exactly the SQLite trio plus
  per-session `opencode export` JSON files — two layers, so evidence stays readable even if a
  live db copy is torn mid-checkpoint. `~/.cs-opencode/env` and `auth.json` are excluded and
  covered by credential-canary scans in the live gates.

## Version bumps

Upstream releases multiple times per week, so bumps are routine and MUST re-verify the behavior
contract, not just the checksum:

1. Update `OPENCODE_VERSION` in `image/Containerfile`; download both release tarballs
   (`opencode-linux-{x64,arm64}.tar.gz`) and re-record both sha256 constants.
2. Re-probe the four upstream realities above against the new binary (an isolated `$HOME`, a
   real provider key, one cheap model): attached-run exit codes on a forced provider failure,
   attached-run stdout, the session-id shape, and `/session/status` idle semantics.
3. `go test ./...` (the contract tests stub the endpoints and pin the driver's semantics).
4. Rebuild the **host binary first** (`make build && make install` — the image assets are
   embedded in it), then the image (`cs-sandbox build`), then re-run the live gates here and in
   `cs-campaign` (`make check-live-opencode`, `check-live-opencode-orch`). Skipping the binary
   rebuild re-propagates stale guest scripts: `deploy_driver` pushes the orchestrator's baked
   copy over any newer one on the target.

## Live validation record

2026-07-31, opencode 1.18.10, rebuilt Firecracker image: full remote contract from the host
(`--new` with sentinel id learning; `--resume` on the same `ses_` id; `-b` with
running→finished; `--kill` mid-turn → footer exit 130 → `failed`/3; resume-after-kill; tmux pane
renders the driven session for human attach; VM stop/start then resume; a bash-tool turn under
`--auto`; guest scan confirming the API key exists only in `~/.cs-opencode/env`). Downstream,
`cs-campaign`'s gated suites passed with opencode in both roles: agent
(`make check-live-opencode`, 166s) and orchestrator (`make check-live-opencode-orch`, 212s —
including a real committed task fetched back through the scoped helper and model-driven
delegation to a Claude agent from inside an `--auto` bash-tool turn).
