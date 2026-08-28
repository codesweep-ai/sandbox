#!/usr/bin/env bash
# Re-record every cassette the replay profiles serve, from this machine's own
# credentials.
#
# Fourteen cases: seven agent-and-credential pairings, each recorded twice --
# once with the credential copied into the sandbox, once with it lent through
# the lender. Each drives a real agent in a real sandbox and asks a real model
# for one word. This checks what all of that needs before any of it starts, and
# `make fixtures-strict` fails on a case it cannot sign in for rather than
# skipping it. Recording thirteen of fourteen and reporting green is the outcome
# worth refusing.
#
#   ./scripts/record-fixtures.sh          record all fourteen
#   ./scripts/record-fixtures.sh --check  say what would run, record nothing
#
# To re-record one, go straight to make, which clears only that cassette:
#
#   make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/codex-openai-lent'
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

# The three API keys, from the .env this repository already keeps them in. Read
# to report on, and read again by the test itself: nothing here exports them.
[[ -f .env ]] || fail ".env not found in $repo -- it holds the three API keys."
set -a
# shellcheck disable=SC1091
. ./.env
set +a

image=${CS_SANDBOX_IMAGE:-localhost/sandbox-slim-agents:ci}

echo "Recording from:"
echo "  repo             $repo"
echo "  branch           $(git rev-parse --abbrev-ref HEAD)"
echo "  agents image     $image"
echo "  cs-vcr           $(cs-vcr version 2>/dev/null | head -1 || echo MISSING)"
echo "  cassettes        $repo/test/cassettes"
echo

missing=0

echo "API keys (.env):"
for v in ANTHROPIC_API_KEY OPENAI_API_KEY FIREWORKS_API_KEY; do
  if [[ -n ${!v:-} ]]; then ok "$v is set"; else bad "$v is not set"; missing=1; fi
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
if podman image exists "$image" 2>/dev/null; then
  ok "$image is built"
else
  bad "$image is not built -- run: make build-ci-image CI_SLIM_KEEP_AGENTS=1"
  missing=1
fi
if command -v cs-vcr >/dev/null; then ok "cs-vcr"; else bad "cs-vcr is not on PATH"; missing=1; fi

# The recorder takes a fixed port, because that port is written into every
# sandbox's environment and into the lender's upstream. Asking here costs
# nothing and names it before fourteen sandboxes boot against a dead address.
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
  fail "Something above is missing. fixtures-strict fails on it rather than
skipping, so fix it before a recording starts."
fi

if (( check_only )); then
  echo "--check: everything the fourteen cases need is present. Nothing recorded."
  exit 0
fi

cat <<'WARN'

This drives fourteen real agent turns against real providers, and spends real
money. Re-recording REPLACES each cassette; it does not append.

WARN
read -r -p "Type 'record' to continue: " answer
[[ $answer == record ]] || fail "Nothing recorded."

echo
make fixtures-strict

cat <<'AFTER'

Recorded. Before committing, read what changed:

  git status --short
  git diff --stat test/cassettes/

Scan them for anything of yours that reached the wire, and take it out:

  go tool cs-vcr cassette scrub --from-env ANTHROPIC_API_KEY,OPENAI_API_KEY,FIREWORKS_API_KEY

Then replay them the way CI will:

  make test-agents-replay
AFTER
