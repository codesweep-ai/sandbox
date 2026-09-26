#!/usr/bin/env bash
# Record a clean, passing `make ci` as a local build that sibling projects can pin.
#
#   scripts/record-build.sh start               the first step of the gate
#   scripts/record-build.sh finish [GATE]       the last, once every gate passed
#   scripts/record-build.sh store               print this repository's store, up to date
#
# A commit GitHub CI built is one a sibling may pin and push. A commit this gate
# passed on, on a clean tree, is one a sibling may pin while the work is still
# local. `finish` records it in the build store of the repository's owner:
#
#   ${CS_BUILDS_DIR:-${XDG_DATA_HOME:-~/.local/share}/cs-builds}/<owner>/
#     status/<name>/<commit>.json   the build: its commit, its times and its versions
#     goproxy/                      the module zip, in Go's proxy layout
#     npm/                          the npm packages, flat, for cs-npmrevs to serve
#     images/<name>/<commit>/       one file per image an image build of it made
#
# The owner is the GitHub owner origin names, SSH host aliases included. Where
# origin is no GitHub URL, it is the name of the directory the repository sits
# in. CS_BUILD_STORE names the store outright instead, as a campaign does for
# its members. The name is the one siblings pin it by: the last element of its
# Go module path, or its npm package's name.
#
# A store that is a git repository, as a campaign member's clone is, keeps each
# build as a commit of its own. Before it is read or written, it takes in the
# builds the campaign's orchestrator pushed to refs/campaign/orchestrator, fast
# forward or by a merge. Every file is named by what it holds, so the merge
# never conflicts.
#
# Nothing is recorded unless the tree was clean at the same commit when the gate
# started and when it finished, and it says so. A gate run over uncommitted work
# still checks it; it just leaves nothing to pin. A build is recorded once: a
# second run on the same commit finds its entry and does no work.
#
# A project that publishes images, as sandbox does, names them in CS_BUILD_IMAGES.
# Its entry then lists them under "awaits". A sibling's repin takes the build
# once the store holds a file for each under images/, which the image build
# writes (`cs-sandbox build`), just as CI's status file waits for the images.
#
# Go and npm builds of a clean commit are byte-identical to what CI publishes for
# it, so what this records is what CI will publish once the commit is pushed.
# The module zip comes from Go itself, resolving the commit out of this
# checkout, so its pseudo-version and its hash are the ones the proxy will give.
#
# The same file is in lint, ledger, npmrevs, tracer, vcr, sandbox, campaign, ui
# and dashboards, so a fix made in one is copied to the others rather than
# rewritten there. dashboards' SPEC.md describes the store, and its tests run
# this script.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
usage() { echo "usage: record-build.sh start | finish [GATE] | store" >&2; exit 2; }
[ "$#" -ge 1 ] || usage

say() { echo "record-build: $*"; }
skip() { say "not recorded: $*"; exit 0; }
clean() { [ -z "$(git -C "$ROOT" status --porcelain)" ]; }

# Per worktree, beside the index, so two checkouts of one repository keep apart.
mark() { printf '%s/cs-build-start\n' "$(git -C "$ROOT" rev-parse --absolute-git-dir)"; }

if [ "$1" = start ]; then
  MARK="$(mark)"
  sha="$(git -C "$ROOT" rev-parse -q --verify HEAD || true)"
  if clean; then state=clean; else state=dirty; fi
  printf '%s %s\n' "${sha:-none}" "$state" >"$MARK"
  exit 0
fi
[ "$1" = finish ] || [ "$1" = store ] || usage

# The owner: origin's GitHub owner, else the directory the repository sits in.
owner() {
  local url o
  url="$(git -C "$ROOT" remote get-url origin 2>/dev/null || true)"
  case "$url" in
    *github.com*) o="$(printf '%s\n' "$url" | sed -E 's#^.*github\.com[^:/]*[:/]+##; s#/.*$##')" ;;
    *) o="" ;;
  esac
  case "$o" in
    "" | *[!A-Za-z0-9_.-]*) o="$(basename "$(dirname "$ROOT")")" ;;
  esac
  printf '%s\n' "$o"
}
if [ -n "${CS_BUILD_STORE:-}" ]; then
  STORE="$CS_BUILD_STORE"
else
  OWNER="$(owner)"
  case "$OWNER" in
    "" | . | .. | *[!A-Za-z0-9_.-]*)
      why="no owner to file it under: origin names no GitHub owner, and the directory name '$OWNER' is not one"
      if [ "$1" = store ]; then echo "record-build: $why" >&2; exit 1; fi
      skip "$why" ;;
  esac
  STORE="${CS_BUILDS_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/cs-builds}/$OWNER"
fi

# Whether the store is the top of a git work tree, rather than a directory in one.
store_repo() {
  [ -d "$STORE" ] && git -C "$STORE" rev-parse --is-inside-work-tree >/dev/null 2>&1 &&
    [ -z "$(git -C "$STORE" rev-parse --show-prefix 2>/dev/null)" ]
}
# The builds the orchestrator delivered, taken into the store's branch.
take_in() {
  local ref=refs/campaign/orchestrator
  store_repo || return 0
  git -C "$STORE" rev-parse -q --verify "$ref^{commit}" >/dev/null || return 0
  git -C "$STORE" merge-base --is-ancestor "$ref" HEAD 2>/dev/null && return 0
  git -C "$STORE" merge -q --ff-only "$ref" 2>/dev/null ||
    git -C "$STORE" merge -q --no-edit -m "Take in the builds the orchestrator delivered" "$ref" >/dev/null ||
    echo "record-build: could not take in $ref in $STORE" >&2
  return 0
}

if [ "$1" = store ]; then
  take_in
  printf '%s\n' "$STORE"
  exit 0
fi

MODULE=""
if [ -f "$ROOT/go.mod" ]; then
  MODULE="$(awk '$1 == "module" { print $2; exit }' "$ROOT/go.mod")"
  NAME="$(printf '%s\n' "$MODULE" | cut -d/ -f3)"
elif [ -f "$ROOT/package.json" ]; then
  NAME="$(node -e 'console.log(require(process.argv[1]).name.split("/").pop())' "$ROOT/package.json")"
else
  skip "no go.mod or package.json names what siblings pin"
fi
[ -n "$NAME" ] || skip "no name for siblings to pin it by"

GATE="${2:-make ci}"

MARK="$(mark)"
[ -f "$MARK" ] || skip "the gate did not run record-build.sh start"
read -r sha0 state0 <"$MARK"
rm -f "$MARK"
[ "$state0" = clean ] || skip "the tree had changes when the gate started"
SHA="$(git -C "$ROOT" rev-parse -q --verify HEAD || true)"
[ -n "$SHA" ] && [ "$SHA" = "$sha0" ] || skip "HEAD moved while the gate ran"
clean || skip "the tree changed while the gate ran"

take_in
ENTRY="$STORE/status/$NAME/$SHA.json"
if [ -f "$ENTRY" ]; then
  say "$NAME ${SHA:0:7} is already recorded in $STORE"
  exit 0
fi

# The images the build waits for, as the entry lists them.
awaits_line=""
if [ -n "${CS_BUILD_IMAGES:-}" ]; then
  list=""
  for image in $CS_BUILD_IMAGES; do
    case "$image" in
      *[!a-z0-9._-]*) skip "CS_BUILD_IMAGES names '$image', which is no image repository name" ;;
    esac
    list="${list:+$list, }\"$image\""
  done
  awaits_line=" \"awaits\": [$list],
"
fi

# Every tool the record needs, before anything is written.
[ -z "$MODULE" ] || command -v go >/dev/null || skip "go is not on PATH, and the module zip needs it"
pack=""
if [ -x "$ROOT/npm/local-registry.sh" ]; then
  for t in goreleaser node npm; do
    command -v "$t" >/dev/null || skip "$t is not on PATH, and packing the npm packages needs it"
  done
  pack="$ROOT/npm/local-registry.sh pack"
elif [ -f "$ROOT/scripts/npmrevs-registry.mjs" ]; then
  pack="node $ROOT/scripts/npmrevs-registry.mjs pack"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Into the store under a name of its own, and never over a file already there:
# the file for a version is the same whichever build wrote it first.
put() { # source destination
  [ -e "$2" ] && return 0
  mkdir -p "$(dirname "$2")"
  cp "$1" "$2.tmp.$$"
  mv "$2.tmp.$$" "$2"
}

# The module zip, from Go resolving this commit out of this checkout. GOPRIVATE
# names this module alone, so Go takes it from git rather than a proxy and asks
# no checksum database, and the insteadOf rewrite points that git at the checkout.
GO_VERSION=""
if [ -n "$MODULE" ]; then
  case "$MODULE" in
    *[A-Z]*) skip "module path $MODULE has capitals, which the store's proxy layout would have to escape" ;;
  esac
  # With the toolchain this repository builds with, which the gate has just used,
  # and GOTOOLCHAIN=local, so the download fetches no other one. Its go line may
  # be newer than the go on PATH, and a download refuses a module that asks for
  # a newer Go than it runs. Through GOPROXY=direct a switch could hang besides.
  gobin="$(cd "$ROOT" && GOWORK=off go env GOROOT)/bin/go"
  if ! out="$(cd "$TMP" && env GOWORK=off GOTOOLCHAIN=local GOFLAGS=-modcacherw GOMODCACHE="$TMP/mod" \
      GOPROXY=direct GOPRIVATE="$MODULE" \
      GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="url.file://$ROOT.insteadOf" GIT_CONFIG_VALUE_0="https://$MODULE" \
      "$gobin" mod download -json "$MODULE@$SHA" 2>&1)"; then
    echo "$out" >&2
    say "go mod download of $MODULE@${SHA:0:12} failed" >&2
    exit 1
  fi
  field() { printf '%s\n' "$out" | sed -nE "s/^[[:space:]]*\"$1\": \"([^\"]*)\".*/\\1/p" | head -1; }
  GO_VERSION="$(field Version)"
  [ -n "$GO_VERSION" ] || { echo "$out" >&2; say "go mod download named no version" >&2; exit 1; }
  at="$STORE/goproxy/$MODULE/@v/$GO_VERSION"
  put "$(field Info)" "$at.info"
  put "$(field GoMod)" "$at.mod"
  put "$(field Zip)" "$at.zip"
fi

# The npm packages, packed by the project's own script into a directory of their
# own, then filed. The package named after the project is the one a sibling pins.
NPM_NAME="" NPM_VERSION=""
if [ -n "$pack" ]; then
  mkdir -p "$TMP/npm"
  # shellcheck disable=SC2086 # $pack is a command and its arguments
  if ! (cd "$ROOT" && CS_NPMREVS_DATA="$TMP/npm" $pack) >"$TMP/pack.log" 2>&1; then
    cat "$TMP/pack.log" >&2
    say "packing the npm packages failed" >&2
    exit 1
  fi
  for f in "$TMP"/npm/*.tgz; do
    [ -e "$f" ] || { say "packing wrote no package" >&2; exit 1; }
    meta="$(tar -xzOf "$f" package/package.json | node -e \
      'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const p=JSON.parse(s);console.log(p.name+" "+p.version)})')"
    case "${meta%% *}" in
      */"$NAME") NPM_NAME="${meta%% *}" NPM_VERSION="${meta#* }" ;;
    esac
    put "$f" "$STORE/npm/$(basename "$f")"
  done
  [ -n "$NPM_NAME" ] || { say "no package packed is named after $NAME" >&2; exit 1; }
fi

# The entry last, so a build is listed only once everything it names is there.
committed="$(TZ=UTC git -C "$ROOT" show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ "$SHA")"
recorded="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
go_json="null"
[ -n "$GO_VERSION" ] && go_json="\"$GO_VERSION\""
npm_json="{}"
[ -n "$NPM_NAME" ] && npm_json="{ \"$NPM_NAME\": \"$NPM_VERSION\" }"
mkdir -p "$(dirname "$ENTRY")"
cat >"$ENTRY.tmp.$$" <<EOF
{
 "schema": 1,
 "name": "$NAME",
 "commit": "$SHA",
 "committed": "$committed",
 "recorded": "$recorded",
 "gate": "$GATE",
 "local": true,
${awaits_line} "versions": { "go": $go_json, "images": {}, "npm": $npm_json }
}
EOF
mv "$ENTRY.tmp.$$" "$ENTRY"

what="${GO_VERSION:+go $GO_VERSION}"
[ -n "$NPM_NAME" ] && what="${what:+$what, }npm $NPM_NAME@$NPM_VERSION"
say "recorded $NAME ${SHA:0:7} as a local build in $STORE ($what)"
[ -z "${CS_BUILD_IMAGES:-}" ] || say "siblings take it once its images are built: $CS_BUILD_IMAGES"

# In a store that is a repository, the build is a commit, which the orchestrator
# carries to the other members.
if store_repo; then
  if git -C "$STORE" add -A . && git -C "$STORE" commit -q -m "Record $NAME ${SHA:0:7}" >/dev/null; then
    say "committed it in $STORE, for the orchestrator to carry"
  else
    say "could not commit it in $STORE; it is recorded, and stays uncommitted" >&2
  fi
fi
