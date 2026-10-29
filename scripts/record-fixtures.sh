#!/usr/bin/env bash
# Re-record every cassette the replay profiles serve, from this machine's own
# credentials.
#
# Up to fourteen cases: seven agent-and-credential pairings, each recorded
# twice -- once with the credential copied into the sandbox, once with it lent
# through the lender. Each drives a real agent in a real sandbox and asks a real
# model for one word. This checks what all of that needs before any of it starts.
# With every key present it runs `make fixtures-strict`, which fails on a case it
# cannot sign in for rather than skipping it. An API key .env does not set skips
# the cases that need it instead, under `make fixtures`, and a cassette one of
# them is left holding from another version of its agent is dropped afterwards.
# The logins are still required: each is one command away.
#
#   ./scripts/record-fixtures.sh          record what this host can sign in for
#   ./scripts/record-fixtures.sh --check  say what would run, record nothing
#
# To re-record one, go straight to make, which clears only that cassette:
#
#   make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/codex-login-lent'
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."
repo=$PWD

# The tools this recording runs on, built into this checkout by `make tools` at
# the go.mod pin and put ahead of everything else.
#
# Built here rather than assumed. The preflight below reports what it found, and
# the question is only worth asking of the binary the run will actually get --
# `make fixtures` puts this same directory on PATH for the test itself, so
# without this the script would be vouching for one cs-vcr and recording through
# another.
make tools >/dev/null
export PATH="$repo/bin/tools:$PATH"

check_only=0
[[ ${1:-} == --check ]] && check_only=1

fail() { printf '\n%s\n' "$*" >&2; exit 1; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  MISS  %s\n' "$*"; }
skip() { printf '  SKIP  %s\n' "$*"; }

# The API keys, from the .env this repository keeps them in. Read to report on,
# and read again by the test itself, which takes them from .env and from nowhere
# else -- so a key the shell happens to export is cleared here too, or this
# would vouch for a key the run never gets.
unset ANTHROPIC_API_KEY OPENAI_API_KEY FIREWORKS_API_KEY
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

image=${CS_SANDBOX_IMAGE:-localhost/sandbox-slim:ci}

# The tier that image is built FROM, which is what decides the agent CLI
# versions the cassettes end up keyed to. Printed because it is the one input to
# a recording that nothing on the command line names.
tier=$(sed -n 's/^SLIM_AGENTS_REF=//p' "$repo/image/tiers.env")

echo "Recording from:"
echo "  repo             $repo"
echo "  branch           $(git rev-parse --abbrev-ref HEAD)"
echo "  agents image     $image"
echo "  agents tier      ${tier:-MISSING from image/tiers.env}"
echo "  cs-vcr           $(cs-vcr version 2>/dev/null | head -1 || echo MISSING)"
echo "  cassettes        $repo/test/cassettes"
echo

missing=0
skipped=0

echo "API keys (.env):"
for v in ANTHROPIC_API_KEY OPENAI_API_KEY FIREWORKS_API_KEY; do
  if [[ -n ${!v:-} ]]; then ok "$v is set"
  else skip "$v is not set -- the cases that need it are skipped"; skipped=1
  fi
done

# The subscription logins. The suite reads each from the profile the wrappers
# keep, not from the agent's own directory: ~/.claude can exist on a host that
# never signed in, and it is not what gets carried.
echo
echo "Subscription logins:"
claude_cred="$HOME/.cs-claude/.credentials.json"
if [[ -f $claude_cred ]]; then
  # The same five-minute margin the lender applies, checked here where the fix
  # is one `cs-claude` away and no sandbox has booted.
  if python3 - "$claude_cred" <<'PY'
import json, sys, time
exp = json.load(open(sys.argv[1])).get("claudeAiOauth", {}).get("expiresAt", 0) / 1000
sys.exit(0 if exp > time.time() + 300 else 1)
PY
  then ok "Claude login at $claude_cred"
  else bad "Claude login at $claude_cred is expired -- run 'cs-claude' to refresh"; missing=1
  fi
else
  bad "no Claude login at $claude_cred"; missing=1
fi

codex_cred="$HOME/.cs-codex/auth.json"
if [[ -f $codex_cred ]]; then
  ok "Codex login at $codex_cred"
else
  bad "no Codex login at $codex_cred -- run 'cs-codex login'"; missing=1
fi

echo
echo "Host prerequisites:"
if command -v podman >/dev/null; then ok "podman"; else bad "podman is not on PATH"; missing=1; fi
# Deliberately not `podman image exists`. That is what stood here, and an
# existence check cannot see staleness: an image built before a tier bump
# satisfies it forever, and a recording made against one is committed rather
# than failed. `make fixtures-strict` now rebuilds the image through
# setup-fixtures before it records, so presence is no longer the question.
if [[ -n $tier ]]; then
  ok "$image is rebuilt from $tier before recording"
else
  bad "image/tiers.env names no SLIM_AGENTS_REF, so the image has no tier to build on"
  missing=1
fi
if command -v cs-vcr >/dev/null; then ok "cs-vcr"; else bad "cs-vcr is not on PATH"; missing=1; fi

# The recorder takes a fixed port, because that port is written into every
# sandbox's environment and into the lender's upstream. Asking here costs
# nothing and names it before every sandbox boots against a dead address.
# bash's own /dev/tcp rather than ss or lsof: this script already needs bash,
# and neither of those is a prerequisite worth adding for one question.
if (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then
  exec 3>&-
  bad "something is already answering on port 8080, which the recorder takes"
  missing=1
else
  ok "port 8080 is free for the recorder"
fi

echo
if (( missing )); then
  fail "Something above is missing. Fix it before a recording starts: a login,
the engine or the recorder is not something a case can skip around."
fi

# Strict when every key is here, so a case that skips for any other reason
# still fails the run.
target=fixtures-strict
(( skipped )) && target=fixtures

if (( check_only )); then
  echo "--check: this would run \`make $target\`. Nothing recorded."
  exit 0
fi

cat <<'WARN'

This drives one real agent turn for each case it can sign in for, against real
providers, and spends real money. Re-recording REPLACES each cassette; it does
not append.

WARN
read -r -p "Type 'record' to continue: " answer
[[ $answer == record ]] || fail "Nothing recorded."

echo
make "$target"

# A case skipped for want of a key keeps the cassette it had. Once the image
# carries another version of its agent, that cassette is obsolete: replay skips
# it locally, and in CI, where each case is a job that insists on its own PASS,
# it fails. Dropped, the case is one with no cassette, which the replay tier
# passes over; take it out of the case lists in .github/workflows/ci.yml too.
# The image is asked the way the replay asks it, so the two cannot disagree.
versions=$(podman run --rm --pull=never --entrypoint sh "$image" -c \
  'for a in claude codex opencode; do v=$("$a" --version 2>/dev/null | head -1); printf "%s %s\n" "$a" "$v"; done')
VERSIONS=$versions python3 - <<'PY'
import json, os, pathlib, re, shutil

image = {}
for line in os.environ["VERSIONS"].splitlines():
    name, _, rest = line.strip().partition(" ")
    m = re.search(r"[0-9]+\.[0-9]+\.[0-9]+", rest)
    image[name] = m.group(0) if m else ""
for claim in sorted(pathlib.Path("test/cassettes").glob("*/recorded.json")):
    c = json.loads(claim.read_text())
    if c.get("cli_version") != image.get(c.get("cli"), ""):
        shutil.rmtree(claim.parent)
        print(f"dropped {claim.parent.name}: recorded with {c.get('cli')} {c.get('cli_version')}, "
              f"and the image carries {image.get(c.get('cli')) or 'none'}")
PY

cat <<'AFTER'

Recorded. Before committing, read what changed:

  git status --short
  git diff --stat test/cassettes/

Scan them for anything of yours that reached the wire, and take it out:

  go tool cs-vcr cassette scrub --from-env ANTHROPIC_API_KEY,OPENAI_API_KEY,FIREWORKS_API_KEY

Then replay them the way CI will:

  make test-agents-replay
AFTER
