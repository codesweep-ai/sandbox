# cs-sandbox — build/test/release.
# `make build` produces bin/cs-sandbox via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
CS_LINT    ?= go tool cs-lint
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

.PHONY: help build build-go build-ci-image build-ci-assets build-ci-fc install uninstall test test-race tools setup-smoke test-smoke test-integration coverage coverage-check coverage-baseline vet fmt fmt-check check ci prose refs oss surface ledger lint deadcode snapshot release release-check clean

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

## build-ci-image: the slimmed sandbox image the smoke profile's live tests run
## against in CI — 474 MB and ~70 seconds, against 6.04 GB and tens of minutes for
## the real one, which is what makes booting real sandboxes in CI affordable.
## Derived from the real Containerfile, never a second copy of it: see
## image/ci-slim.sh. Use it locally the same way test-smoke does:
##   make build-ci-image && make test-smoke
##
## Named for what it holds rather than for the package it is slimmed from. It is
## not a sandbox — ci-slim.sh strips every toolchain, the cs- tools included — so
## a tag reading `sandbox` would be the same trap the published packages were
## split up to avoid (internal/cli/root.go). The two variants get two names for
## the same reason and one more: CI_SLIM_KEEP_AGENTS=1 keeps the three agent
## CLIs, 1.38 GB against 474 MB, and it is the only one of the two that can run
## an agent — so a test that got the wrong one under a shared tag would fail at
## `command -v claude`, nowhere near the setting that chose it.
##
## Exported so the setting reaches ci-slim.sh whether it came from the
## environment or from this make command line.
CI_SLIM_KEEP_AGENTS ?=
export CI_SLIM_KEEP_AGENTS
CI_AGENTS_IMAGE ?= localhost/sandbox-slim-agents:ci
ifeq ($(filter 1 true yes on,$(CI_SLIM_KEEP_AGENTS)),)
CI_IMAGE ?= localhost/sandbox-slim:ci
else
CI_IMAGE ?= $(CI_AGENTS_IMAGE)
endif
build-ci-image:
	@mkdir -p $(dir $(BIN))
	./image/ci-slim.sh > bin/Containerfile.ci
	podman build -q -t $(CI_IMAGE) -f bin/Containerfile.ci image/rootfs

## build-ci-assets: an asset tree identical to image/ except that its
## Containerfile is the slimmed one. Pointing CS_SANDBOX_ASSETS_DIR at it lets
## the SHIPPED `cs-sandbox build` produce the CI image and, below, the microVM
## artifacts — so CI exercises the real command rather than a bespoke path that
## could drift from it.
CI_ASSETS := bin/ci-assets
build-ci-assets:
	rm -rf $(CI_ASSETS)
	@mkdir -p $(CI_ASSETS)
	cp -r image $(CI_ASSETS)/
	./image/ci-slim.sh > $(CI_ASSETS)/image/Containerfile

## build-ci-fc: the Firecracker artifacts the microVM smoke test needs — the
## pinned firecracker binary, a guest kernel extracted from Fedora's kernel-core,
## and a base rootfs built from the CI image. Measured at ~1m35s cold and ~676 MB
## on disk (337 MB packed), which is what makes caching them in CI worthwhile.
## `du --apparent-size` reports 33 GB instead: base-rootfs.ext4 is a sparse file,
## which is why the CI job packs the cache with `tar --sparse`.
## Needs /dev/kvm writable and the FC host packages (see `cs-sandbox doctor
## --engine firecracker`). Set CS_SANDBOX_FC_CACHE to keep them out of the
## developer's real cache:
##   CS_SANDBOX_FC_CACHE=/tmp/fc make build-ci-fc
build-ci-fc: build-go build-ci-assets
	CS_SANDBOX_ASSETS_DIR=$(CI_ASSETS) CS_SANDBOX_IMAGE=$(CI_IMAGE) \
	  ./$(BIN) build --engine firecracker

## versions: what this build is made of — this repo's binary, every pinned tool,
## the Go toolchain, and whether a workspace is overriding the go.mod pins. Each
## line is read by asking that binary its own version. It deliberately depends on
## nothing and runs from source: reporting a version must not trigger a build.
## -buildvcs=true because `go run` leaves out the VCS stamp by default, and that
## stamp is the version now that nothing injects one with -X.
.PHONY: versions
versions:
	@if out="$$(go run -buildvcs=true -ldflags '$(LDFLAGS)' $(PKG) version 2>&1)"; then \
		printf '%-12s %-42s %s\n' '$(notdir $(BIN))' "$$(printf '%s\n' "$$out" | awk 'NR==1{print $$2}')" 'this repo'; \
	else \
		printf '%-12s %s\n' '$(notdir $(BIN))' "FAILED — $$(printf '%s\n' "$$out" | head -1)"; \
	fi
	@for t in $$(go list tool 2>/dev/null); do \
		if out="$$(go tool $$t version 2>&1)"; then \
			printf '%-12s %s\n' "$$(basename $$t)" "$$(printf '%s\n' "$$out" | awk 'NR==1{print $$2}')"; \
		else \
			printf '%-12s %s\n' "$$(basename $$t)" "FAILED — $$(printf '%s\n' "$$out" | head -1)"; \
		fi; \
	done
	@printf '%-12s %s\n' 'go' "$$(go env GOVERSION)"
	@w="$$(go env GOWORK)"; \
	case "$$w" in \
		''|off) printf '%-12s %s\n' 'workspace' 'off — versions above are go.mod pins' ;; \
		*)      printf '%-12s %s\n' 'workspace' "$$w — local checkouts override the go.mod pins" ;; \
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
## CS_SANDBOX_IMAGE, so the probe below finds it there and builds nothing.
##
## Cheap when the host is already set up: podman is asked whether the image is
## there, and only its absence builds one. The question goes to podman rather
## than to this repository's own `doctor`, which is the obvious place for it and
## the wrong one — doctor reports an unbuilt image as a warning and still exits
## 0, so a probe reading its exit code would call every fresh machine ready and
## build nothing, which is the one case this target exists for.
##
## The image asked about is the one the run will boot — CS_SANDBOX_IMAGE where it
## is set, $(CI_IMAGE) otherwise — which is the same expression test-smoke below
## passes down. Probing the default while the run boots an override is how a
## target builds an image nobody asked for and still leaves the run without one.
##
## The image and nothing else. build-ci-fc would be the tempting second half —
## it produces the microVM artifacts the heaviest member wants — but it builds
## them by writing the base rootfs into the developer's real Firecracker cache,
## from the CI image, and that cache holds one rootfs stamped with one image id.
## A campaign or a sandbox booting the full image would find it replaced and
## rebuild it back, 32 GiB at a time, every time the two were used in turn.
## build-ci-fc's own note says to redirect CS_SANDBOX_FC_CACHE for exactly this
## reason, so it stays a deliberate step rather than something a test target
## does to a machine unasked. Without those artifacts the microVM member skips
## itself, which is what it already does on the macOS and WSL2 legs.
##
## Nothing here fails. The live members of this profile skip themselves on a host
## with no engine and the engine-free members still run, which is what makes the
## same `make test-smoke` correct on every leg — a setup step that turned that
## skip into a failure would take the profile away from the hosts it was written
## for. A build that was actually attempted and then failed does fail, because a
## host that got that far has a fault rather than a limitation.
setup-smoke: tools
	@if ! command -v podman >/dev/null 2>&1; then \
		echo "setup-smoke: no podman on this host — the live members of the smoke profile will skip themselves"; \
	else \
		image=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)}; \
		if podman image exists "$$image"; then \
			echo "setup-smoke: $$image is built"; \
		else \
			$(MAKE) --no-print-directory build-ci-image; \
		fi; \
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
test-smoke: setup-smoke
	@scripts/coverage.sh reset smoke
	$(WITH_TOOLS) CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} CS_COVERDIR=$(COVER_ABS)/smoke \
	  go test -tags smoke $(COVERFLAGS) -count=1 -p 1 -v -timeout 1200s -run '$(SMOKE_RUN)' ./... \
	  -args -test.gocoverdir=$(COVER_ABS)/smoke

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
	CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_AGENTS_IMAGE)} \
	  go test -tags live_agents -count=1 -p 1 -v -timeout 3600s ./internal/cli/ -run 'LiveAgent'

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
check: fmt-check vet lint deadcode test coverage-check prose refs oss surface

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
## The smoke tiers are left out: each boots a real guest from an image another
## job builds, and one of them needs /dev/kvm. Run them with
## `make build-ci-image && make test-smoke`.
ci:
	$(call say,the gate a contributor runs before pushing)
	@$(MAKE) --no-print-directory check
	$(call say,module verification)
	go mod verify
	$(call say,vet behind the build tags)
	go vet -tags integration ./...
	go vet -tags smoke ./...
	$(call say,actionlint)
	@command -v actionlint >/dev/null 2>&1 || { \
		echo "actionlint is not installed; go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12" >&2; \
		exit 1; \
	}
	actionlint
	$(call say,compile every package)
	go build ./...
	$(call say,build)
	@$(MAKE) --no-print-directory build-go
	$(call say,release manifest)
	@$(MAKE) --no-print-directory release-check
	$(call say,ledger)
	@$(MAKE) --no-print-directory ledger
	@printf '\nci: every gate ran. Not reproduced here: build-test on macOS and\n'
	@printf 'WSL, the smoke tiers, and the coverage job that merges them.\n'

## lint: the Go rules from .golangci.yml (see that file for what is on and why).
## Three passes for the same reason vet takes three: a build tag hides a file
## from the linter as surely as from the compiler, and the live tests are where
## an unread helper survives longest.
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed; see https://golangci-lint.run/welcome/install/" >&2; \
		exit 2; \
	}
	golangci-lint run
	golangci-lint run --build-tags=integration
	golangci-lint run --build-tags=smoke

## deadcode: functions no entry point reaches (golangci-lint's `unused` cannot
## see this — it reasons one package at a time, so a function whose only caller
## lives in another package looks used). Drop -test and it answers a second,
## softer thing — what only a test keeps alive. That one wants a human, since a
## test fake is meant to have no production caller.
deadcode:
	@command -v deadcode >/dev/null 2>&1 || { \
		echo "deadcode is not installed: go install golang.org/x/tools/cmd/deadcode@latest" >&2; \
		exit 2; \
	}
	@out="$$(deadcode -test ./...)"; \
	if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

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
