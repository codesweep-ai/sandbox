#!/usr/bin/env bash
# Coverage: where the data comes from, how it aggregates, and the gate.
#
# Nothing here runs tests. The Makefile's test targets write Go binary coverage
# data into one directory per tier under $COVERDIR:
#
#   .coverage/unit         make test
#   .coverage/race         make test-race
#   .coverage/integration  make test-integration
#   .coverage/smoke        make test-smoke
#   .coverage/cli          the built binary, driven directly rather than through
#                          `go test` -- built by `make build-cover` and pointed
#                          here with GOCOVERDIR. A gate that runs the real
#                          command contributes nothing without it.
#
# One directory per tier is what makes `make test test-integration` aggregate
# instead of overwrite: `reset` clears only the tier about to run, so a tier
# that ran earlier keeps its data and `report` merges every tier present.
#
# A tier may have more than one directory. CI has each job upload its tier as an
# artifact and the aggregating job unpack them side by side as
# .coverage/<tier>@<job>, so one tier carries data from every machine that ran
# it -- including machines running different operating systems, since covdata
# unions data written by independent builds. They stay separate directories
# rather than being merged into one because the file names encode a build hash
# and would collide; covdata takes any number of input directories instead.
#
# Binary coverage data rather than a -coverprofile text file, because it is the
# only form a program built with `go build -cover` can emit. That is what lets
# the black-box tests -- the ones that exec the real CLI rather than calling
# into it -- contribute their coverage instead of silently counting for nothing.
#
# Every tier must use the same -covermode. `go tool covdata merge` cannot union
# `set` data with `atomic` data: it reports the mismatch and STILL EXITS 0, so
# `report` below checks that the merge produced output rather than trusting the
# exit status.
set -uo pipefail
cd "$(dirname "$0")/.."

COVERDIR=${COVERDIR:-.coverage}
BASELINE=${BASELINE:-.coverage-baseline}
TIERS="unit race integration smoke cli"

die() { printf '\033[31mcoverage: %s\033[0m\n' "$*" >&2; exit 1; }

# Every directory holding data for one tier: $COVERDIR/<tier> as written by the
# Makefile, plus any $COVERDIR/<tier>@<job> unpacked from a CI artifact. A
# directory with no covmeta file is one that was reset and then did not run, and
# is skipped -- an empty directory means "did not run", not "ran and covered
# nothing".
tier_dirs() { # tier_dirs <tier>
  local d
  for d in "$COVERDIR/$1" "$COVERDIR/$1"@*; do
    [ -d "$d" ] || continue
    compgen -G "$d/covmeta.*" >/dev/null 2>&1 && printf '%s\n' "$d"
  done
}

# Tiers that actually have data, so that `make test` alone is not reported as a
# collapse of every other tier.
present_tiers() {
  local tier
  for tier in $TIERS; do
    [ -n "$(tier_dirs "$tier")" ] && printf '%s\n' "$tier"
  done
  return 0
}

# Per-package statement counts from a textfmt profile. Emits "<pkg> <covered>
# <total>". The block spec is `import/path/file.go:12.3,14.4 <numstmt> <count>`,
# so the package is the path with the file name trimmed off.
pkg_stats() {
  awk '
    /^mode:/ { next }
    {
      split($1, spec, ":")
      path = spec[1]
      n = split(path, part, "/")
      pkg = part[1]
      for (i = 2; i < n; i++) pkg = pkg "/" part[i]
      total[pkg] += $2
      if ($3 + 0 > 0) covered[pkg] += $2
      seen[pkg] = 1
    }
    END { for (p in seen) printf "%s %d %d\n", p, covered[p], total[p] }
  ' "$1" | sort
}

# Merge the given tiers into one directory and render it as a text profile.
# Both outputs are checked for existence: covdata reports some failures on
# stderr and exits 0 regardless.
merge_tiers() { # merge_tiers <outdir> <profile> <tier...>
  local out="$1" profile="$2"; shift 2
  local inputs="" tier d
  for tier in "$@"; do
    while read -r d; do inputs="${inputs:+$inputs,}$d"; done < <(tier_dirs "$tier")
  done
  [ -n "$inputs" ] || die "no coverage data in $COVERDIR -- no test tier has run"

  rm -rf "$out" && mkdir -p "$out"
  go tool covdata merge -i="$inputs" -o="$out" || true
  compgen -G "$out/covmeta.*" >/dev/null 2>&1 \
    || die "merging $inputs produced no data (mixed -covermode across tiers?)"

  go tool covdata textfmt -i="$out" -o="$profile" || true
  [ -s "$profile" ] || die "could not render a text profile from $out"
}

case "${1:-}" in
# reset <tier>: clear one tier's directory, leaving every other tier intact.
# Called by each test target immediately before it runs, so that a deleted test
# stops counting instead of lingering in yesterday's data.
reset)
  tier="${2:?usage: coverage.sh reset <tier>}"
  rm -rf "${COVERDIR:?}/$tier" "${COVERDIR:?}/$tier"@*
  mkdir -p "$COVERDIR/$tier"
  ;;

# report: merge every tier present and print per-package and total coverage.
# In CI it also appends the table to the job summary.
report)
  tiers=$(present_tiers)
  [ -n "$tiers" ] || die "no coverage data in $COVERDIR -- no test tier has run"
  profile="$COVERDIR/coverage.txt"
  # shellcheck disable=SC2086
  merge_tiers "$COVERDIR/merged" "$profile" $tiers

  stats=$(pkg_stats "$profile")
  read -r cov tot <<<"$(printf '%s\n' "$stats" | awk '{c+=$2; t+=$3} END {print c+0, t+0}')"
  pct=$(awk -v c="$cov" -v t="$tot" 'BEGIN { printf "%.1f", t ? 100*c/t : 0 }')

  printf '\n\033[1mcoverage\033[0m  tiers: %s\n\n' "$(printf '%s' "$tiers" | tr '\n' ' ')"
  printf '%s\n' "$stats" | awk '{ printf "  %-6.1f %s\n", $3 ? 100*$2/$3 : 0, $1 }'
  printf '\n  %-6s %s\n' "$pct" "TOTAL ($cov/$tot statements)"
  printf '\n  profile: %s\n\n' "$profile"

  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
      printf '## Coverage: %s%%\n\n' "$pct"
      printf 'Tiers merged: %s\n\n' "$(printf '%s' "$tiers" | tr '\n' ' ')"
      printf '| Package | Coverage |\n|---|---:|\n'
      printf '%s\n' "$stats" | awk '{ printf "| %s | %.1f%% |\n", $1, $3 ? 100*$2/$3 : 0 }'
      printf '| **Total** | **%s%%** |\n' "$pct"
    } >>"$GITHUB_STEP_SUMMARY"
  fi
  ;;

# check: the gate. It asserts that every package a tier is RECORDED as reaching
# is still reached by that tier -- nothing about percentages. What it catches is
# a suite that stopped running: a build tag that no longer matches, a helper
# that skips instead of failing, a package whose tests were deleted with the
# code they covered. Those leave every test command green, and this is the only
# thing that goes red.
#
# The baseline is per tier and only tiers present in $COVERDIR are checked, so
# a unit-only `make check` is judged against the unit tier alone.
check)
  [ -f "$BASELINE" ] || die "$BASELINE is missing -- run 'make coverage-baseline' to record one"
  tiers=$(present_tiers)
  [ -n "$tiers" ] || die "no coverage data in $COVERDIR -- no test tier has run"

  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  failures=0
  for tier in $tiers; do
    merge_tiers "$tmp/$tier" "$tmp/$tier.txt" "$tier"
    pkg_stats "$tmp/$tier.txt" | awk '$2 > 0 { print $1 }' >"$tmp/$tier.reached"

    while read -r want; do
      grep -qxF "$want" "$tmp/$tier.reached" && continue
      printf '\033[31mcoverage: %s is no longer reached by the %s tier\033[0m\n' "$want" "$tier" >&2
      failures=$((failures + 1))
    done < <(awk -v t="$tier" '!/^#/ && NF == 2 && $1 == t { print $2 }' "$BASELINE")
  done

  if [ "$failures" -gt 0 ]; then
    printf '\n%s package(s) lost their coverage. Either the tests that reached them\n' "$failures" >&2
    printf 'stopped running, or they were removed on purpose -- if on purpose, rerun\n' >&2
    printf "'make coverage-baseline' and commit the change so it is reviewed.\n" >&2
    exit 1
  fi
  printf 'coverage: every package in %s is still reached (tiers: %s)\n' \
    "$BASELINE" "$(printf '%s' "$tiers" | tr '\n' ' ')"
  ;;

# baseline [tier...]: record what the named tiers now reach, defaulting to every
# tier present. Committing the result is what makes a later loss reviewable
# rather than invisible.
#
# Name the tiers when the machine can run more than CI does. A baseline is only
# useful for a tier something actually runs: recording one from a laptop that
# ran the live tier, where CI never does, commits a promise nothing keeps and
# gates developers on data CI cannot reproduce.
baseline)
  shift
  tiers=$(present_tiers)
  [ -n "$tiers" ] || die "no coverage data in $COVERDIR -- no test tier has run"
  if [ "$#" -gt 0 ]; then
    for want in "$@"; do
      printf '%s\n' "$tiers" | grep -qx "$want" \
        || die "tier '$want' has no data in $COVERDIR -- run it before recording it"
    done
    tiers=$(printf '%s\n' "$@")
  fi
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  {
    printf '# Packages each test tier is known to reach, as "<tier> <package>".\n'
    printf '# Regenerate with `make coverage-baseline` after running the tiers you\n'
    printf '# want recorded; `make coverage-check` fails when a listed package stops\n'
    printf '# being reached. Percentages are deliberately not recorded.\n'
    printf '#\n'
    printf '# Only the tiers CI actually runs belong here. Restrict what a run\n'
    printf '# records with, for example:\n'
    printf '#   make coverage-baseline BASELINE_TIERS="unit race smoke"\n'
    for tier in $tiers; do
      merge_tiers "$tmp/$tier" "$tmp/$tier.txt" "$tier"
      pkg_stats "$tmp/$tier.txt" | awk -v t="$tier" '$2 > 0 { print t, $1 }'
    done
  } >"$BASELINE"
  printf 'coverage: wrote %s (%s entries)\n' "$BASELINE" "$(grep -cv '^#' "$BASELINE")"
  ;;

*)
  printf 'usage: %s {reset <tier>|report|check|baseline}\n' "$0" >&2
  exit 2
  ;;
esac
