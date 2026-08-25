# cs-sandbox — build/test/release.
# `make build` produces bin/cs-sandbox via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
CS_LINT    ?= cs-lint
BIN        := bin/cs-sandbox
DOC        := image/rootfs/home/.local/bin/CS_SANDBOX.md
PKG        := ./cmd/cs-sandbox
PREFIX     ?= $(HOME)/.local
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X github.com/codesweep-ai/sandbox/internal/cli.Version=$(VERSION)
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

.PHONY: help build build-go build-ci-image build-ci-assets build-ci-fc install uninstall test test-race test-smoke test-integration coverage coverage-check coverage-baseline vet fmt fmt-check check docs oss walkthrough cs-lint-installed ledger lint deadcode snapshot release release-check clean

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
## image/ci-slim.sh. Use it locally the same way CI does:
##   make build-ci-image && CS_SANDBOX_IMAGE=localhost/cs-sandbox:ci make test-smoke
CI_IMAGE ?= localhost/cs-sandbox:ci
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
## and a base rootfs built from the CI image. Measured at ~1m35s cold and ~1 GB
## on disk (459 MB packed), which is what makes caching them in CI worthwhile.
## Needs /dev/kvm writable and the FC host packages (see `cs-sandbox doctor
## --engine firecracker`). Set CS_SANDBOX_FC_CACHE to keep them out of the
## developer's real cache:
##   CS_SANDBOX_FC_CACHE=/tmp/fc make build-ci-fc
build-ci-fc: build-go build-ci-assets
	CS_SANDBOX_ASSETS_DIR=$(CI_ASSETS) CS_SANDBOX_IMAGE=$(CI_IMAGE) \
	  ./$(BIN) build --engine firecracker

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
test-smoke:
	@scripts/coverage.sh reset smoke
	CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} CS_COVERDIR=$(COVER_ABS)/smoke \
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
test-integration:
	@scripts/coverage.sh reset integration
	CS_COVERDIR=$(COVER_ABS)/integration \
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
	CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-localhost/cs-sandbox:ci-agents} \
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
## docs: the prose rules from CONTRIBUTING.md, over every doc in the set
docs: cs-lint-installed
	$(CS_LINT) docs

## oss: the rules this repo has to satisfy as a published project
oss: cs-lint-installed
	$(CS_LINT) oss

## walkthrough: check the docs against the binary, the code and the build
walkthrough: build cs-lint-installed
	$(CS_LINT) walkthrough

# The three targets above are one shared tool: github.com/codesweep-ai/lint.
# Its knobs for this repo live in .cs-lint.yaml, and `cs-lint <linter> --explain`
# says what each rule wants.
cs-lint-installed:
	@command -v $(CS_LINT) >/dev/null 2>&1 || { \
		echo "cs-lint is not installed: go install github.com/codesweep-ai/lint/cmd/cs-lint@latest" >&2; \
		exit 2; \
	}

## ledger: validate the issue records and prove ledger.html is current
ledger:
	@command -v cs-ledger >/dev/null 2>&1 || { \
		echo "cs-ledger is not installed: go install github.com/codesweep-ai/ledger/cmd/cs-ledger@latest" >&2; \
		exit 2; \
	}
	cs-ledger check ledger

## check: the full local gate — fmt-check, vet, the linters, and unit tests
check: fmt-check vet lint deadcode test coverage-check docs oss walkthrough

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
