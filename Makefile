# cs-sandbox — build/test/release.
# `make build` produces bin/cs-sandbox via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
CS_LINT    ?= go tool cs-lint
# The linters the gates shell out to, all pinned and all built from the module
# cache, so a fresh checkout runs `make check` with nothing installed by hand.
# deadcode and actionlint are `tool` directives in go.mod and run with `go tool`.
# golangci-lint is one in go.golangci.mod, which says at its head why it needs a
# module file of its own.
GOLANGCI   := bin/tools/golangci-lint
BIN        := bin/cs-sandbox
DOC        := image/rootfs/home/.local/bin/CS_SANDBOX.md
PKG        := ./cmd/cs-sandbox
PREFIX     ?= $(HOME)/.local
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w
GO_FILES   := $(shell git ls-files '*.go')

# Coverage is not a separate mode: every test target below writes Go binary
# coverage data into its own tier directory under $(COVERDIR), and `make
# coverage` merges whichever tiers are present. That is what lets
# `make test test-integration` report one aggregate number instead of the last
# tier overwriting the one before it, and it is how CI merges tiers that ran on
# different machines. scripts/coverage.sh documents the layout.
# -test.gocoverdir must be absolute: `go test` runs each package's test binary
# with that package's directory as its working directory, so a relative path
# would scatter the data one directory per package.
# CS_COVERDIR, passed per tier below, tells a test that builds and execs the
# real binary where the instrumented child should write. It is not GOCOVERDIR
# because `go test` overwrites that one in the test process with a directory of
# its own, and does not fold what lands there back into the profile.
COVERDIR   ?= .coverage
COVER_ABS  := $(abspath $(COVERDIR))
COVERFLAGS := -covermode=atomic -coverpkg=./...

.PHONY: help build build-go build-ci-image build-ci-fc install uninstall test test-race tools setup-smoke test-smoke test-integration test-live-agents fixtures fixtures-strict fixtures-check test-agents-replay test-agents-shared test-agents-lent coverage coverage-check coverage-baseline vet fmt fmt-check check ci prose refs oss surface ledger lint deadcode actionlint snapshot release release-check clean

.DEFAULT_GOAL := help

## help: list available targets (this menu)
help:
	@echo "cs-sandbox make targets:"
	@grep -E '^## [a-z][a-z0-9-]*: ' $(MAKEFILE_LIST) | sed -E 's/^## ([^:]+): (.*)/  \1|\2/' | column -t -s '|'
	@echo ""
	@echo "  PREFIX=$(PREFIX) (install location; override with make install PREFIX=/usr/local)"

## build: host binary at bin/cs-sandbox via goreleaser (single target)
build:
	@mkdir -p $(dir $(BIN))
	@if command -v $(GORELEASER) >/dev/null 2>&1; then \
		VERSION='$(VERSION)' $(GORELEASER) build --single-target --snapshot --clean --output $(BIN); \
	else \
		echo "goreleaser not found; using go build (run 'make build-go' explicitly to force)"; \
		$(MAKE) build-go; \
	fi

## build-go: host binary via plain go build (no goreleaser needed)
build-go:
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

## CI_IMAGE: the slimmed image CI builds and the live tests boot.
##
## One image, not two. There used to be an agent-free variant as well, ~325 MB
## smaller on the CI artifact, and it cost a seventh published package plus a
## name (sandbox-slim-agents) that meant a product in one family and a tier in
## the other. Every slim image carries the three CLIs now, so nothing here has
## to choose between them.
CI_IMAGE ?= localhost/sandbox-slim:ci
CI_SLIM_FLAGS := --slim

# Through the SHIPPED command, which is the whole point of these two targets
# having no build of their own any more. `publish-images` runs the same
# `cs-sandbox build --slim` to make what consumers pull, so what CI tests and
# what ghcr.io serves now differ in name and in nothing else. They did differ:
# this used to reach podman directly, with no --build-arg, and left an image
# whose version and revision labels were empty while the published one carried
# both.
#
# Same recipe, not the same artifact. The image is not bit-for-bit reproducible
# (SPEC §14) — it runs a package update at build time — so the copy CI boots and
# the copy that is published are two builds of one commit, not one build used
# twice. Testing the exact bytes that ship would mean publishing before testing.
build-ci-image: build-go
	CS_SANDBOX_IMAGE=$(CI_IMAGE) ./$(BIN) build --engine podman $(CI_SLIM_FLAGS)

## build-ci-fc: the Firecracker artifacts the microVM smoke test needs — the
## pinned firecracker binary, a guest kernel extracted from Fedora's kernel-core,
## and a base rootfs built from the CI image. Measured at ~1m35s cold and ~676 MB
## on disk (337 MB packed), which is what makes caching them in CI worthwhile.
## `du --apparent-size` reports 33 GB instead: the base rootfs is a sparse file,
## which is why the CI job packs the cache with `tar --sparse`.
## Needs /dev/kvm writable and the FC host packages (see `cs-sandbox doctor
## --engine firecracker`).
##
## It writes into the developer's own cache, and that is now safe: the cache
## keeps one base rootfs per image (SPEC R124), so this leaves a full-image
## rootfs beside the one it builds instead of over it. CS_SANDBOX_FC_CACHE still
## moves the whole cache where a run wants its own — CI sets it so the artifacts
## land somewhere it can pack:
##   CS_SANDBOX_FC_CACHE=/tmp/fc make build-ci-fc
##
## It builds the image too: one `cs-sandbox build` makes the image and then the
## artifacts from it, which is why there is no dependency on build-ci-image here.
build-ci-fc: build-go
	CS_SANDBOX_IMAGE=$(CI_IMAGE) ./$(BIN) build --engine firecracker $(CI_SLIM_FLAGS)

## versions: what this build is made of — this repo's binary, every pinned tool,
## the Go toolchain, and whether a workspace is overriding the go.mod pins. The
## binary answers for itself; every tool is read out of the module file that
## pins it, which is the one place a `go tool` run can get it from. It
## deliberately depends on nothing and runs from source: reporting a version
## must not trigger a build.
## -buildvcs=true because `go run` leaves out the VCS stamp by default, and that
## stamp is the version now that nothing injects one with -X.
.PHONY: versions
versions:
	@if out="$$(go run -buildvcs=true -ldflags '$(LDFLAGS)' $(PKG) version 2>&1)"; then \
		printf '%-14s %-42s %s\n' '$(notdir $(BIN))' "$$(printf '%s\n' "$$out" | awk 'NR==1{print $$2}')" 'this repo'; \
	else \
		printf '%-14s %s\n' '$(notdir $(BIN))' "FAILED — $$(printf '%s\n' "$$out" | head -1)"; \
	fi
	@ver='{{with .Module}}{{if .Replace}}{{.Replace.Path}}{{else if .Version}}{{.Version}}{{else}}{{.Dir}}{{end}}{{end}}'; \
	for t in $$(go list tool 2>/dev/null); do \
		v="$$(go list -f "$$ver" $$t 2>/dev/null)"; \
		printf '%-14s %s\n' "$$(basename $$t)" "$${v:-FAILED}"; \
	done; \
	for t in $$(GOWORK=off go list -modfile=go.golangci.mod tool 2>/dev/null); do \
		v="$$(GOWORK=off go list -modfile=go.golangci.mod -f "$$ver" $$t 2>/dev/null)"; \
		printf '%-14s %s\n' "$$(basename $$t)" "$${v:-FAILED}"; \
	done
	@printf '%-14s %s\n' 'go' "$$(go env GOVERSION)"
	@w="$$(go env GOWORK)"; \
	case "$$w" in \
		''|off) printf '%-14s %s\n' 'workspace' 'off — versions above are go.mod pins' ;; \
		*)      printf '%-14s %s\n' 'workspace' "$$w — local checkouts override the go.mod pins" ;; \
	esac

## repin: move every codesweep-ai tool pin to its branch tip, then report. Uses
## GOPROXY=direct because the module proxy caches branch resolution and `@main`
## can come back a commit behind origin/main. Uses GOWORK=off so this edits the
## recorded pins even while a workspace is serving local checkouts.
.PHONY: repin
repin:
	@tools="$$(go list tool 2>/dev/null | grep codesweep-ai || true)"; \
	if [ -z "$$tools" ]; then \
		echo "no codesweep-ai tools declared yet — add the first with:" >&2; \
		echo "  GOPROXY=direct go get -tool github.com/codesweep-ai/lint/cmd/cs-lint@main" >&2; \
		exit 1; \
	fi; \
	GOWORK=off GOPROXY=direct go get -tool $$(echo "$$tools" | sed 's|$$|@main|')
	@GOWORK=off go mod tidy
	@$(MAKE) versions

## install: copy bin/cs-sandbox into $(PREFIX)/bin (default ~/.local/bin), plus
## CS_SANDBOX.md beside it — the companion doc that teaches coding agents how to
## drive the CLI, installed the same way `install-agent-tools` installs the other
## companions. Both are real copies, so the installed command keeps working if the
## checkout is moved or deleted; re-run this after a rebuild to pick up changes.
## State lives in XDG dirs, so the binary runs from anywhere.
install: build
	@mkdir -p $(PREFIX)/bin
	install -m 0755 $(BIN) $(PREFIX)/bin/cs-sandbox
	install -m 0644 $(DOC) $(PREFIX)/bin/$(notdir $(DOC))
	@echo "installed $(PREFIX)/bin/cs-sandbox ($(VERSION))"
	@echo "installed $(PREFIX)/bin/$(notdir $(DOC))"
	@case ":$(PATH):" in *":$(PREFIX)/bin:"*) : ;; *) echo "note: add $(PREFIX)/bin to PATH" ;; esac

## uninstall: remove the installed binary and companion doc
uninstall:
	rm -f $(PREFIX)/bin/cs-sandbox $(PREFIX)/bin/$(notdir $(DOC))

## test: unit tests
test:
	@scripts/coverage.sh reset unit
	CS_COVERDIR=$(COVER_ABS)/unit go test $(COVERFLAGS) ./... -args -test.gocoverdir=$(COVER_ABS)/unit

## test-race: the unit tier under the race detector. This is what CI runs on
## Linux; it is a separate coverage tier so that running both aggregates rather
## than the second overwriting the first.
test-race:
	@scripts/coverage.sh reset race
	CS_COVERDIR=$(COVER_ABS)/race go test -race $(COVERFLAGS) ./... -args -test.gocoverdir=$(COVER_ABS)/race

# The sibling tools a tier resolves from PATH at run time, in a directory of
# this repository's own rather than on the developer's.
#
# `go tool` cannot serve this the way it serves the gates above: a tier that
# reaches a tool does it by name, through PATH, and cs-vcr is also handed to a
# container to run — so what it needs is a real file in a real directory.
# doctor already holds this repository to the same standard, comparing the
# cs-vcr on PATH against the go.mod pin, and this is what makes that comparison
# something a test run cannot fail by accident.
TOOLSDIR   := $(abspath bin/tools)
WITH_TOOLS := PATH="$(TOOLSDIR):$$PATH"

## tools: the sibling cs- tools a tier needs on PATH, at the go.mod pins
##
## `go install` with no @version resolves through go.mod, so what lands here is
## the pin by construction and `make repin` moves it — the same pin the image
## build reads out of the embedded manifest to put cs-vcr inside a guest, so the
## host and the guest cannot end up on different builds of it. About a second
## with a warm module cache, and near nothing when the binary is current, which
## is what lets it be a prerequisite rather than a step somebody remembers.
##
## CGO_ENABLED=0 is load-bearing wherever this binary is handed to a container
## to run: an image with no libc and no dynamic loader kills a cgo build at exec
## with "No such file or directory" — the kernel reporting the missing ELF
## interpreter, not a missing binary — and a proxy that dies there takes every
## model call with it, so a run times out rather than saying what broke.
##
## Only cs-vcr for now. The gates run cs-lint, cs-ledger and cs-tracer with
## `go tool`, which needs nothing installed; a tool joins this list when a tier
## has to reach it by name.
tools:
	@mkdir -p $(TOOLSDIR)
	@CGO_ENABLED=0 GOBIN=$(TOOLSDIR) go install github.com/codesweep-ai/vcr/cmd/cs-vcr

## setup-smoke: the guest image the smoke profile boots, and the tools beside it
##
## A prerequisite of test-smoke rather than a line in a document, so a machine
## that has never built the CI image reaches a running profile with one command.
## `make build-ci-image && make test-smoke` still works and is still what the
## target above spells out; this is the same two steps for somebody who did not
## know there were two. CI reaches the same image by a third route — build-ci-fc
## on one leg, a saved archive on the rest — and every leg names it in
## CS_SANDBOX_IMAGE, which is the name the build below reads.
##
## The build runs UNCONDITIONALLY, and that is the point. This used to ask podman
## whether the image existed and build only on its absence — an existence check
## standing in for a freshness one. Nothing tied that image to the sources it came
## from, so one built days ago satisfied it forever, and a change under image/
## passed `make ci` locally without ever reaching the image the smoke profile
## booted. A green run then proved nothing about the thing that changed. CI never
## had that hole: it keys its artifact cache on hashFiles('image/**',
## 'internal/fcdisk/**') and rebuilds on exactly those changes, so the local gate
## advertised itself as what CI runs while being quietly weaker. Building every
## time closes it, and errs toward work rather than toward a false pass.
##
## Unconditional is affordable because both builders are already incremental, and
## both key on content rather than on existence:
##
##   * podman's layer cache carries the image. The Containerfile defers
##     `COPY . /sandbox` to the very bottom on purpose, so a change under image/
##     invalidates that one small layer and nothing before it.
##   * the firecracker cache carries the microVM artifacts. EnsureArtifacts
##     rebuilds only what is missing or stale, and the base rootfs stamp is the
##     podman image ID plus the kernel version and a hash of image/guest/init
##     (internal/fcdisk/build.go), so a rebuilt image or an edited guest init
##     invalidates it and an unchanged tree does not.
##
## Measured on this host: 3.1s for the image and 5.9s for the artifacts to pick up
## a change under image/, 1.4s each for a no-op, against ~70s and ~1m35s cold.
## Neither is a timestamp comparison, so a touched file or a skewed clock cannot
## fake currency in either direction.
##
## The image built is the one the run will boot — CS_SANDBOX_IMAGE where it is
## set, $(CI_IMAGE) otherwise — which is the same expression test-smoke below
## passes down. Building the default while the run boots an override is how a
## target builds an image nobody asked for and still leaves the run without one.
##
## Where the host has /dev/kvm it builds through build-ci-fc, which makes the
## microVM artifacts as well as the image, so the nested-VM member has something
## to run rather than skipping on the machine most able to run it. A host that
## kept the image but lost its Firecracker cache now rebuilds it here, where
## before that member skipped itself and only named the command.
##
## That was unsafe until the cache began keeping one base rootfs per image (SPEC
## R124). It wrote the CI image's rootfs over the one a full-image sandbox boots
## from, so the two rebuilt it out from under each other, 32 GiB at a time, every
## time they were used in turn. Keyed, they sit side by side — measured at 6.2 GB
## and 752 MB on disk against 64 GiB apparent, because both are sparse.
##
## Nothing here fails on a host that cannot carry it. The live members of this
## profile skip themselves on a host with no engine and the engine-free members
## still run, which is what makes the same `make test-smoke` correct on every leg
## — a setup step that turned that skip into a failure would take the profile away
## from the hosts it was written for. A build that was actually attempted and then
## failed does fail, because a host that got that far has a fault rather than a
## limitation.
## SMOKE_AGENTS: whether the profile carries its replay members. On by default,
## which is what makes a bare `make test-smoke` cover the credential paths.
##
## It governs the run only. It used to govern an image as well — the replay
## members booted a second, agent-carrying build that setup-smoke made just for
## them — but there is one slim image now and it carries the agents, so a leg
## with SMOKE_AGENTS=0 saves the minutes of running them and nothing else. CI
## turns it off on the legs that are not the one host per architecture the
## members need, where it was measured costing nine minutes to run.
SMOKE_AGENTS ?= 1

setup-smoke: tools
	@if ! command -v podman >/dev/null 2>&1; then \
		echo "setup-smoke: no podman on this host — the live members of the smoke profile will skip themselves"; \
	elif [ -w /dev/kvm ]; then \
		$(MAKE) --no-print-directory build-ci-fc; \
	else \
		$(MAKE) --no-print-directory build-ci-image; \
	fi

## test-smoke: the smoke profile — the subset of the live tests below that CI
## runs, on Linux, macOS and Windows/WSL2. Same command on every host: where an
## engine and a sandbox image are present the *Live members boot real sandboxes,
## and where they are not (macOS on Apple Silicon, which has no hypervisor to
## run a podman machine in) those skip themselves and the engine-free members
## still run. -run is the selector, and is also what keeps this from dragging in
## each package's unit tests, which the `test` target above already ran.
##
## Two kinds of member, both spelled out here rather than inferred:
##   TestSmoke*  purpose-built for this profile — the host-side behaviour that
##               differs per OS and that the unit tier cannot reach (the real
##               state root, an ssh config the host's own ssh must parse).
##   *Live       existing live tests, run verbatim. Keep this list short: it is
##               a smoke test, not the suite. `make test-integration` is that.
##
## One member is deliberately heavier than that rule: TestCLINestedSandboxInVMLive
## boots a microVM, ships the CLI and the image into it, creates a sandbox inside
## it, and (its "workload" subtest) runs a container inside that sandbox. It is
## the only cover for cs-sandbox running in its own sandbox, and for the whole
## microVM -> sandbox -> container stack; it self-skips without /dev/kvm, the FC
## artifacts, or a slim image — so on the legs that lack them (macOS, WSL2) it
## costs nothing at all.
SMOKE_TESTS ?= Smoke \
               TestPodmanCreateLive \
               TestCLICreateExecDestroyLive \
               TestCLIAgentToolSetLive \
               TestCLIListShowsInstanceLive \
               TestCLINetworkReachabilityLive \
               TestCLILendKeyLive \
               TestCLIPortForwardLive \
               TestCLINestedSandboxInVMLive

# Joined into one -run alternation. A backslash continuation in make becomes a
# space, so the list is written space-separated and the spaces are substituted
# out here — leaving them in would give `-run 'Smoke |Test…'`, whose every
# alternative has a trailing space and so matches nothing at all.
comma_empty :=
space := $(comma_empty) $(comma_empty)
SMOKE_RUN := $(subst $(space),|,$(strip $(SMOKE_TESTS)))

## -p 1 for the same reason test-integration uses it: the *Live members share
## one network fabric and one host SSH port pool, so packages running in
## parallel collide on both ("address already in use", then IPAM exhaustion).
## -v likewise: these boot real sandboxes and a package otherwise prints nothing
## for minutes, and it is the only way a CI log shows WHICH members ran — the
## live ones skip themselves on a host with no engine, and a run that skipped
## everything looks exactly like a run that passed.
## The image defaults to the slim one this profile is built around. Overriding it
## is honoured, but a bare `make test-smoke` should not silently spend minutes
## seeding the 6 GB image into a store disk for the microVM member.
##
## -timeout stays under the 25 minutes the smoke-firecracker job allows, so that
## when a member wedges it is Go that ends the run. Go names the test and prints
## every goroutine; the job timeout above it kills the runner and reports only
## that time ran out. Raise the job first if this ever has to grow.
## The replay members are the second half of this profile, and they are a
## separate `go test` for two reasons that are both real: a build tag ADDS files
## to a package rather than selecting among them, so one invocation cannot carry
## two tags; and they boot the agents image while the members above boot the
## slim one.
##
## They hold no credential and reach no provider, which is what lets them sit in
## the tier CI runs on every push. What they cost is the minutes: SMOKE_AGENTS=0
## leaves them unspent. They boot the same image as the rest of the profile.
##
## Their coverage lands in the same tier directory, appended rather than reset,
## because `reset smoke` ran once above and both halves are this one profile.
test-smoke: setup-smoke
	@scripts/coverage.sh reset smoke
	$(WITH_TOOLS) CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} CS_COVERDIR=$(COVER_ABS)/smoke \
	  go test -tags smoke $(COVERFLAGS) -count=1 -p 1 -v -timeout 1200s -run '$(SMOKE_RUN)' ./... \
	  -args -test.gocoverdir=$(COVER_ABS)/smoke
	@if [ "$(SMOKE_AGENTS)" = 1 ]; then \
		set -x; \
		$(WITH_TOOLS) CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} CS_COVERDIR=$(COVER_ABS)/smoke \
		  go test -tags agents_replay $(COVERFLAGS) -count=1 -p 1 -v -timeout 1200s \
		  -run '$(AGENTS_REPLAY_CASES)' ./internal/cli/ \
		  -args -test.gocoverdir=$(COVER_ABS)/smoke; \
	else \
		echo "test-smoke: SMOKE_AGENTS=0 — the replay members were not run"; \
	fi

## test-integration: live tests (real podman/firecracker on a Linux/KVM host);
## each skips gracefully when podman or the sandbox image is unavailable.
## -p 1 serializes packages: they share one network fabric + host SSH port pool
## (each uses its own temp state dir), so parallel packages would collide.
## -v streams each test's start/result as it happens: these tests boot real
## containers and microVMs, so without it a package prints nothing for minutes.
## The smoke suite is tagged for this run too, so a full local pass covers it
## first — its failures are cheap and point at the host, not the engine.
##
## -timeout is per package, and it is a deadlock detector rather than a budget:
## it exists to end a wedged test, not to hold the suite to a pace. Set near the
## real runtime it reports the slowest host instead, and the panic names whichever
## test the clock happened to land on rather than the one at fault. internal/cli
## alone runs half an hour when nothing is cached, since seeding the image store
## for the nested-microVM tests costs minutes before a VM even boots.
## SBX_IMAGE: the sandbox image this checkout's cs-sandbox names. Asked of the
## binary rather than written down here, because the name carries the version and
## only the binary knows its own. -buildvcs=true for the same reason `versions`
## needs it: `go run` leaves the VCS stamp out by default, and without the stamp
## there is no version and so no name. Recursive (=) so it costs a `go run` only
## when a target actually reads it.
SBX_IMAGE = $(shell go run -buildvcs=true $(PKG) version 2>/dev/null | awk '$$1=="image"{print $$2}')

test-integration:
	@scripts/coverage.sh reset integration
	CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(SBX_IMAGE)} CS_COVERDIR=$(COVER_ABS)/integration \
	  go test -tags integration $(COVERFLAGS) -p 1 -v -timeout 3600s ./... \
	  -args -test.gocoverdir=$(COVER_ABS)/integration

## test-live-agents: the credential matrix, against real providers.
##
## Every supported agent and credential combination, shared and lent, each one
## driving a real agent inside a real sandbox and asking a real model to say one
## word. Nothing else proves that a credential this tool fabricated is one the
## provider accepts, and nothing cheaper catches the day a client changes which
## code path a credential puts it on.
##
## Deliberately outside CI and outside test-integration: it spends money and
## quota on every run, it needs credentials no runner should hold, and a
## provider being slow or rate-limiting is not a defect in this repository. Run
## it by hand when the credential paths change.
##
## Credentials come from .env at the repository root (git-ignored), and never
## from your own profiles: the suite builds a throwaway agent home and points
## CS_SANDBOX_AGENT_HOME at it. Members skip themselves when a key is absent, so
## a partial .env runs the part it can.
##
## -p 1 and -v for the reasons test-integration gives. The timeout is generous
## because a member waits on a model rather than on this code.
test-live-agents:
	CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} \
	  go test -tags live_agents -count=1 -p 1 -v -timeout 3600s ./internal/cli/ \
	  -run 'TestLiveAgentCredentialMatrix'

## fixtures: LIVE — record the cassettes the replay profiles serve.
##
## The same matrix as test-live-agents, driven through a cs-vcr in record mode,
## writing test/cassettes/<case>/. Costs a real model turn per case and needs
## that case's credential; one it cannot sign in for skips. Commit the result
## with the code: the replay profiles serve what this records.
##
## Re-record one at a time when a client version moves, since each costs a turn:
##   make fixtures FIXTURE_CASES='TestLiveAgentRecordsCassettes/codex-openai-lent'
##
## Re-record when a client version moves, when the pinned model moves, or when a
## replay starts missing. `scripts/record-fixtures.sh` is the way in: it checks
## what the whole matrix needs before it clears a single cassette.
FIXTURE_CASES ?= TestLiveAgentRecordsCassettes

fixtures: tools
	$(WITH_TOOLS) CS_SANDBOX_RECORD=1 CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} \
	  go test -tags live_agents -count=1 -p 1 -v -timeout 3600s ./internal/cli/ -run '$(FIXTURE_CASES)'

## fixtures-strict: the same recording, with a skip treated as a failure. For a
## host that holds every credential and means to re-record the whole matrix: a
## missing one skips under `fixtures`, and a run that recorded nothing reports
## the same green as one that recorded everything.
fixtures-strict: tools
	$(WITH_TOOLS) CS_SANDBOX_RECORD=1 CS_SANDBOX_STRICT=1 \
	  CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} \
	  go test -tags live_agents -count=1 -p 1 -v -timeout 3600s ./internal/cli/ -run '$(FIXTURE_CASES)'

## test-agents-replay: the credential matrix with the model turns replayed.
##
## Boots real sandboxes and runs the real agent binaries, serving every model
## call from a committed cassette through a cs-vcr on this host. It holds no
## credential and reaches no provider, which is what makes it affordable to run
## on every push. Needs podman and the agents image; a case with no cassette is
## passed over, and a host with no engine skips the tier.
##
## -p 1 and -v for the reasons test-integration gives. The timeout is a deadlock
## detector rather than a budget: a replayed turn answers in milliseconds, and
## what this bounds is a sandbox that wedged.
AGENTS_REPLAY_CASES ?= TestAgentReplay

## `make test-smoke` runs these too, as its second half. This target is the way
## to run them alone, and the way to run one of them.
test-agents-replay: tools
	$(WITH_TOOLS) CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} \
	  go test -tags agents_replay -count=1 -p 1 -v -timeout 1800s ./internal/cli/ \
	  -run '$(AGENTS_REPLAY_CASES)'

## test-agents-shared: replay the cases that hold a copy of the credential
##
## The sandbox is pointed straight at the recorder and signs itself in with what
## it was given. No lender is in the path.
test-agents-shared:
	@$(MAKE) --no-print-directory test-agents-replay AGENTS_REPLAY_CASES='TestAgentReplay/-shared$$'

## test-agents-lent: replay the cases that borrow the credential
##
## The sandbox holds a loan token and reaches the lender, which swaps in the
## host's credential and forwards to the recorder. The whole lending path runs
## on every replay, which is the reason this profile exists beside the one above.
test-agents-lent:
	@$(MAKE) --no-print-directory test-agents-replay AGENTS_REPLAY_CASES='TestAgentReplay/-lent$$'

## fixtures-check: prove the committed cassettes still replay under this cs-vcr
##
## A cassette is keyed by a normalization ruleset, and cs-vcr bumps that ruleset
## when the meaning of a key changes. Replaying across such a bump does not miss
## a few entries: every key means something else now, so every model call misses
## at once, and what that looks like from the outside is a hang rather than an
## error. The replay tier asks this per case before it boots anything, which is
## the check that matters most — but that tier needs podman and the agents
## image, so on most machines it never runs and the fixtures rot unobserved.
## This asks the same question with one process and no sandbox, which is what
## lets `make check` carry it.
fixtures-check:
	@scripts/fixtures-check.sh

## coverage: merge every tier present under $(COVERDIR) and print the report
coverage:
	@scripts/coverage.sh report

## coverage-check: report, then fail if a package .coverage-baseline records as
## covered has stopped being reached. It checks presence, never a percentage:
## what it exists to catch is a suite that quietly stopped running.
coverage-check: coverage
	@scripts/coverage.sh check

## coverage-baseline: re-record .coverage-baseline. Records every tier present
## by default; pass BASELINE_TIERS to restrict it to the tiers CI actually runs,
## e.g. `make coverage-baseline BASELINE_TIERS="unit race smoke"`. Recording a
## tier CI never runs commits a promise nothing keeps.
coverage-baseline:
	@scripts/coverage.sh baseline $(BASELINE_TIERS)

## vet / fmt / lint
vet:
	go vet ./...
fmt:
	gofmt -w $(GO_FILES)
fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-formatted:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
## prose: check how this repository's documents are written
prose:
	$(CS_LINT) prose

## refs: check that everything the documents point at is there
refs:
	$(CS_LINT) refs

## oss: the rules this repo has to satisfy as a published project
oss:
	$(CS_LINT) oss

## surface: check the docs against the binary, the code and the build
surface: build
	$(CS_LINT) surface

# The four targets above are one shared tool: github.com/codesweep-ai/lint,
# pinned in go.mod and run with `go tool`, so the gates use the version this
# repo records rather than whatever a machine happens to have installed. `make
# repin` moves that pin. prose and refs ask for no binary and run first;
# surface reads the one build makes.
# Its knobs for this repo live in .cs-lint.yaml, and `cs-lint <linter> --explain`
# says what each rule wants.

## ledger: validate the issue records and prove ledger.html is current
ledger:
	go tool cs-ledger check ledger

## check: the full local gate — fmt-check, vet, the linters, and unit tests
check: fmt-check vet lint deadcode test coverage-check fixtures-check prose refs oss surface

# say prints a heading above each gate, so a long run reads as a list rather
# than as a wall. Bold where a terminal is reading it and plain where a pipe
# is: `make ci > ci.log` should leave a log somebody can read. The escapes are
# the same ones scripts/check.sh uses in tracer, which is where the shape came
# from.
define say
@if [ -t 1 ]; then printf '\n\033[1m==> %s\033[0m\n' "$(1)"; else printf '\n==> %s\n' "$(1)"; fi
endef

## ci: every gate the CI workflow runs, on this machine
##
## One Linux leg of .github/workflows/ci.yml, in the order CI runs it, so a
## red build is something you can see before you push rather than after. The
## build-tag vets are here because a tag hides a file from the compiler as
## surely as from the linter, and only CI vetted behind them until now.
##
## The smoke profile is in, because CI runs it and this target exists to be what
## CI is. It provisions itself — setup-smoke builds the image where the host has
## none — so it costs minutes on a cold machine and boots real sandboxes on a
## capable one. Where the host cannot carry it the members skip themselves and
## this stays green, which is the one thing a reader has to know about a pass
## here: it is the gates, plus as much of the smoke profile as this machine can
## run. The firecracker leg is not reproduced — CI selects a different set for
## it (see the smoke-firecracker job).
ci:
	$(call say,the gate a contributor runs before pushing)
	@$(MAKE) --no-print-directory check
	$(call say,actionlint)
	@$(MAKE) --no-print-directory actionlint
	$(call say,module verification)
	go mod verify
	$(call say,vet behind the build tags)
	go vet -tags integration ./...
	go vet -tags smoke ./...
	$(call say,compile every package)
	go build ./...
	$(call say,build)
	@$(MAKE) --no-print-directory build-go
	$(call say,release manifest)
	@$(MAKE) --no-print-directory release-check
	$(call say,ledger)
	@$(MAKE) --no-print-directory ledger
	$(call say,the smoke profile on real sandboxes)
	@$(MAKE) --no-print-directory test-smoke
	@printf '\nci: every gate ran. Not reproduced here: build-test on macOS and\n'
	@printf 'WSL, the firecracker smoke leg, and the coverage job that merges tiers.\n'

# Built rather than run with `go tool`, because -modfile is refused in workspace
# mode. The build is the only step that reads go.golangci.mod, so only the build
# turns the workspace off; the linter then runs with it back on, against the
# checkouts a workspace is there to serve. A rebuild costs about a fifth of a
# second once the binary is current, which is what lets it be a prerequisite
# rather than a step somebody remembers.
$(GOLANGCI): go.golangci.mod
	@mkdir -p $(@D)
	@GOWORK=off go build -modfile=go.golangci.mod -o $@ \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint

## lint: the Go rules from .golangci.yml (see that file for what is on and why).
## Three passes for the same reason vet takes three: a build tag hides a file
## from the linter as surely as from the compiler, and the live tests are where
## an unread helper survives longest.
lint: $(GOLANGCI)
	$(GOLANGCI) run
	$(GOLANGCI) run --build-tags=integration
	$(GOLANGCI) run --build-tags=smoke

## deadcode: functions no entry point reaches (golangci-lint's `unused` cannot
## see this — it reasons one package at a time, so a function whose only caller
## lives in another package looks used). Drop -test and it answers a second,
## softer thing — what only a test keeps alive. That one wants a human, since a
## test fake is meant to have no production caller.
deadcode:
	@out="$$(go tool deadcode -test ./...)"; \
	if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

## actionlint: the workflow files, which the forge validates only by refusing to
## run them. Extra runner labels it does not know about go in .github/actionlint.yaml.
actionlint:
	go tool actionlint

## snapshot: local release dry-run into dist/ (all platforms, archives, checksums).
## Skips SBOM + cosign signing (those need cyclonedx-gomod + cosign; run in CI/release).
snapshot:
	VERSION='$(VERSION)' $(GORELEASER) release --snapshot --clean --skip=sbom,sign

## release: tagged release (needs a pushed git tag + credentials). For a full
## signed+SBOM release install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest and cosign.
release:
	$(GORELEASER) release --clean

## release-check: validate .goreleaser.yaml
release-check:
	$(GORELEASER) check

## clean: remove build output
clean:
	rm -rf bin dist $(COVERDIR)
