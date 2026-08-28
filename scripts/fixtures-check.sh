#!/usr/bin/env bash
# Prove the committed cassettes can still be replayed by the pinned cs-vcr.
#
# A cassette is keyed by a normalization ruleset, and cs-vcr bumps that ruleset
# when the meaning of a key changes. Replaying a cassette across such a bump does
# not miss a few entries: every key means something else now, so every model call
# misses at once. What that looks like from the outside is not an error but a
# hang -- the agent waits out its whole turn for an answer that can never arrive,
# and says nothing about why.
#
# `make test-agents-replay` asks this per case before it boots anything, which is
# the check that matters most. But that tier needs podman and the agents image,
# so on most machines -- and in most CI jobs -- it never runs at all, and the
# fixtures rot unobserved. This asks the same question with one process and no
# sandbox, which is what lets `make check` carry it.
#
# Exit 0 when every cassette replays under the current ruleset, or when there is
# nothing to check: no cassettes at all is a repository that has not recorded
# any. cs-vcr is pinned in go.mod and run with `go tool`, so it is always there
# and this never skips for a missing tool.
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."
root=${1:-test/cassettes}

shopt -s nullglob
cassettes=()
for index in "$root"/*/index.jsonl; do
  cassettes+=("$(basename "$(dirname "$index")")")
done
if [ ${#cassettes[@]} -eq 0 ]; then
  echo "fixtures: no cassettes under $root; nothing to check" >&2
  exit 0
fi

# cs-vcr names cassettes relative to a configured root, so the root goes in a
# config and the cassettes are named beneath it.
config=$(mktemp)
trap 'rm -f "$config"' EXIT
printf 'cassettes: %s\n' "$(cd "$root" && pwd)" > "$config"

# One at a time, so a failure can name the one command that re-records what
# broke rather than the whole matrix.
status=0
for name in "${cassettes[@]}"; do
  if ! output=$(go tool cs-vcr cassette verify --config "$config" "$name" 2>&1); then
    echo "$output" >&2
    echo "  re-record with: make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/$name'" >&2
    status=1
  fi
done

if [ $status -eq 0 ]; then
  echo "fixtures: ${#cassettes[@]} cassette(s) replay under this cs-vcr"
fi
exit $status
