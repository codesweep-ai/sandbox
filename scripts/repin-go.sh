#!/usr/bin/env bash
# Move each codesweep-ai tool pin in go.mod to its project's newest build.
#
#   scripts/repin-go.sh            the newer of the last CI build and the newest local one
#   LOCAL=0 scripts/repin-go.sh    the last CI build only
#
# A project's newest build is the newer, by UTC commit time, of two:
#
#   - the first commit in `built` of the ci-status.json its Pages site
#     publishes, which GitHub CI built and passed (codesweep-ai/dashboards
#     SPEC.md, "The status file");
#   - the newest commit the build store of this repository's owner holds for
#     it, which a clean `make ci` passed on this machine (SPEC.md, "The local
#     build store"). scripts/record-build.sh names the store. A build whose
#     entry awaits images, as sandbox's does, counts once the store holds
#     them, as CI's status file waits for a project's images. A newer one
#     still waiting is named. So is a newer one whose commit the project's
#     checkout beside this one holds on no branch, as after a rebase: it is left
#     out. Without such a checkout, as in a campaign member, the store is taken
#     as it stands.
#
# The time is the one each build's Go pseudo-version carries, which Go renders
# in UTC, so builds from machines in different zones compare as they should. A
# tie goes to the CI build, which can be pushed. A local build is the same
# module CI publishes once its commit is pushed unchanged, so pinning one pins
# what CI will build. `oss-git-push` holds the push of a project that pins a
# commit CI has not built, until it has.
#
# A pin on a project with neither is held where it is, and says so: a pin never
# moves to a commit nothing built. Each pin taken from a local build is named,
# with its commit and when it was recorded.
#
# Go resolves the pins through the store's module proxy, ahead of whichever
# proxy it is set to use. GONOSUMDB names the modules taken from the store, and
# the local builds they require in turn, since the checksum database has seen
# none of them. Every other pin is still checked against it. $CS_STATUS_SITE, or the older
# $OSS_STATUS_SITE, is read in place of each project's Pages site, as
# <site>/<name>/ci-status.json, such as a directory of files through file://.
#
# The same file is in lint, ledger, npmrevs, tracer, vcr, sandbox, campaign and
# dashboards, so a fix made in one is copied to the others rather than rewritten
# there. dashboards' tests run it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOCAL="${LOCAL:-1}"
cd "$ROOT"

tools="$(GOWORK=off go list tool 2>/dev/null | grep '^github\.com/codesweep-ai/' || true)"
if [ -z "$tools" ]; then
  echo "no codesweep-ai tools declared yet — add the first with:" >&2
  echo "  go get -tool github.com/codesweep-ai/lint/cmd/cs-lint@<commit>" >&2
  exit 1
fi
store="$("$ROOT/scripts/record-build.sh" store 2>/dev/null || true)"

# The 14 digits of UTC commit time a Go pseudo-version carries, or nothing.
stamp() { printf '%s\n' "$1" | sed -nE 's/^v[0-9.]+-(0\.)?([0-9]{14})-[0-9a-f]{12}$/\2/p'; }

# The newest local build of $1, module $2, in the store, into local_best as
# "stamp version commit recorded". A build whose entry awaits images counts once
# the store holds a file for each, as CI's status file waits for a project's
# images. The newest build still waiting goes into local_waiting as "stamp commit
# images". Where the project's own checkout sits beside this one, a build of a
# commit it holds on no branch is left out, and the newest such goes into
# local_gone as "stamp commit".
newest_local() {
  local f v c r t image missing sibling=""
  local_best="" local_waiting="" local_gone=""
  [ -n "$store" ] && [ -d "$store/status/$1" ] || return 0
  if [ -f "$(dirname "$ROOT")/$1/go.mod" ] &&
      [ "$(awk '$1 == "module" { print $2; exit }' "$(dirname "$ROOT")/$1/go.mod")" = "$2" ]; then
    sibling="$(dirname "$ROOT")/$1"
  fi
  for f in "$store/status/$1"/*.json; do
    [ -e "$f" ] || continue
    v="$(sed -nE 's/.*"go": "([^"]+)".*/\1/p' "$f" | head -1)"
    c="$(sed -nE 's/^ *"commit": "([0-9a-f]{40})".*/\1/p' "$f" | head -1)"
    r="$(sed -nE 's/^ *"recorded": "([^"]+)".*/\1/p' "$f" | head -1)"
    t="$(stamp "$v")"
    [ -n "$t" ] && [ -n "$c" ] || continue
    if [ -n "$sibling" ] && [ -z "$(git -C "$sibling" for-each-ref --count=1 --contains "$c" refs/heads 2>/dev/null)" ]; then
      if [ -z "$local_gone" ] || [ "$t" \> "${local_gone%% *}" ]; then local_gone="$t $c"; fi
      continue
    fi
    missing=""
    for image in $(sed -nE 's/^ *"awaits": \[(.*)\],?$/\1/p' "$f" | tr -d '",'); do
      [ -f "$store/images/$1/$c/$image.json" ] || missing="${missing:+$missing, }$image"
    done
    if [ -n "$missing" ]; then
      if [ -z "$local_waiting" ] || [ "$t" \> "${local_waiting%% *}" ]; then local_waiting="$t $c $missing"; fi
      continue
    fi
    if [ -z "$local_best" ] || [ "$t" \> "${local_best%% *}" ]; then local_best="$t $v $c $r"; fi
  done
}

# How long ago an ISO 8601 UTC time was, where this machine's date can tell.
ago() {
  local past now d
  past="$(date -u -d "$1" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s 2>/dev/null || true)"
  [ -n "$past" ] || return 0
  now="$(date -u +%s)"
  d=$(( (now - past) / 60 ))
  if [ "$d" -lt 60 ]; then echo "${d}m ago"; elif [ "$d" -lt 2880 ]; then echo "$((d / 60))h ago"; else echo "$((d / 1440))d ago"; fi
}

pins="" private=""
for t in $tools; do
  owner="$(echo "$t" | cut -d/ -f2)"
  repo="$(echo "$t" | cut -d/ -f3)"
  module="$(echo "$t" | cut -d/ -f1-3)"
  site="${CS_STATUS_SITE:-${OSS_STATUS_SITE:-https://$owner.github.io}}"

  # The last CI build: the first commit in `built`, and the Go version it names.
  status="$(curl -fsSL --max-time 30 "$site/$repo/ci-status.json" 2>/dev/null || true)"
  ci_commit="$(printf '%s\n' "$status" | sed -n '/^ "built": \[$/,/^ \]/s/^ *"commit": *"\([0-9a-f]\{40\}\)".*/\1/p' | head -1)"
  ci_version=""
  if [ -n "$ci_commit" ]; then
    ci_version="$(printf '%s\n' "$status" | sed -n '/^ "built": \[$/,/^ \]/s/^ *"go": *"\([^"]*\)".*/\1/p' | head -1)"
    case "$ci_version" in *"${ci_commit:0:12}") ;; *) ci_version="" ;; esac
  fi
  ci_stamp="$(stamp "$ci_version")"

  local_best="" local_waiting="" local_gone=""
  [ "$LOCAL" = 0 ] || newest_local "$repo" "$module"

  if [ -n "$local_best" ] && { [ -z "$ci_commit" ] || { [ -n "$ci_stamp" ] && [ "${local_best%% *}" \> "$ci_stamp" ]; }; }; then
    read -r _ version commit recorded <<<"$local_best"
    when="$(ago "$recorded")"
    echo "$repo: ${commit:0:7}, a local build recorded ${recorded/T/ }${when:+ ($when)}"
    pins="$pins $t@$version"
    private="${private:+$private,}$module"
  elif [ -n "$ci_commit" ]; then
    echo "$repo: ${ci_commit:0:7}, the last commit its CI built"
    pins="$pins $t@${ci_version:-$ci_commit}"
  else
    echo "$repo: held, as neither its status file nor the build store lists a build"
  fi
  # A newer local build than the one taken, left out for want of its images, or
  # because its commit is no longer on a branch of the project's checkout.
  taken="${ci_stamp:-0}"
  [ -z "$local_best" ] || [ "${local_best%% *}" \< "$taken" ] || taken="${local_best%% *}"
  if [ -n "$local_waiting" ]; then
    read -r wt wc wmissing <<<"$local_waiting"
    [ "$wt" \> "$taken" ] && echo "$repo: ${wc:0:7}, a newer local build, waits for its images: $wmissing"
  fi
  if [ -n "$local_gone" ]; then
    read -r gt gc <<<"$local_gone"
    [ "$gt" \> "$taken" ] && echo "$repo: ${gc:0:7}, a newer local build, is left out: ../$repo holds that commit on no branch"
  fi
done

[ -n "$pins" ] || exit 0

# A local build can pin other local builds, and Go asks the checksum database
# about each module version the graph brings in that go.sum lacks. So GONOSUMDB
# also names every codesweep-ai module a module taken from the store requires at
# a version the store holds, and so on down: none of those has been published.
todo="" seen=""
for p in $pins; do
  m="$(echo "${p%@*}" | cut -d/ -f1-3)"
  case ",$private," in *",$m,"*) todo="$todo $m@${p#*@}" ;; esac
done
while [ -n "${todo# }" ]; do
  # shellcheck disable=SC2086 # $todo is a list of module@version words
  set -- $todo
  mv="$1"; shift; todo="$*"
  case " $seen " in *" $mv "*) continue ;; esac
  seen="$seen $mv"
  mod="$store/goproxy/${mv%@*}/@v/${mv#*@}.mod"
  [ -f "$mod" ] || continue
  # shellcheck disable=SC2013 # each word is one module@version
  for req in $(awk '$1 ~ /^github\.com\/codesweep-ai\// && $2 ~ /^v/ { print $1 "@" $2 } $1 == "require" && $2 ~ /^github\.com\/codesweep-ai\// { print $2 "@" $3 }' "$mod"); do
    [ -f "$store/goproxy/${req%@*}/@v/${req#*@}.info" ] || continue
    case ",$private," in *",${req%@*},"*) ;; *) private="$private,${req%@*}" ;; esac
    todo="$todo $req"
  done
done

proxy="$(GOWORK=off go env GOPROXY)"
if [ -n "$store" ] && [ -d "$store/goproxy" ]; then proxy="file://$store/goproxy,$proxy"; fi
# shellcheck disable=SC2086 # $pins is a list of tool@version words
GOWORK=off GOPROXY="$proxy" GONOSUMDB="$private" go get -tool $pins
GOWORK=off GOPROXY="$proxy" GONOSUMDB="$private" go mod tidy
