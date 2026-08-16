# cs-sandbox — build/test/release.
# `make build` produces bin/cs-sandbox via goreleaser (single host target,
# version-stamped, CGO_ENABLED=0). Falls back to plain `go build` if goreleaser
# is absent. See .goreleaser.yaml.

GORELEASER ?= goreleaser
BIN        := bin/cs-sandbox
DOC        := image/rootfs/home/.local/bin/CS_SANDBOX.md
PKG        := ./cmd/cs-sandbox
PREFIX     ?= $(HOME)/.local
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X github.com/codesweep-ai/sandbox/internal/cli.Version=$(VERSION)
GO_FILES   := $(shell git ls-files '*.go')

.PHONY: build build-go build-ci-image build-ci-assets build-ci-fc install uninstall test test-smoke test-integration vet fmt fmt-check check docs lint snapshot release release-check clean

## build: host binary at bin/cs-sandbox via goreleaser (single target)
build:
	@mkdir -p $(dir $(BIN))
	@if command -v $(GORELEASER) >/dev/null 2>&1; then \
		$(GORELEASER) build --single-target --snapshot --clean --output $(BIN); \
	else \
		echo "goreleaser not found; using go build (run 'make build-go' explicitly to force)"; \
		$(MAKE) build-go; \
	fi

## build-go: host binary via plain go build (no goreleaser needed)
build-go:
	@mkdir -p $(dir $(BIN))
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

## build-ci-image: the slimmed sandbox image the smoke profile's live tests run
## against in CI — 693 MB and ~70 seconds, against 9.3 GB and tens of minutes for
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
	go test ./...

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
## seeding the 9 GB image into a store disk for the microVM member.
test-smoke:
	CS_SANDBOX_IMAGE=$${CS_SANDBOX_IMAGE:-$(CI_IMAGE)} \
	  go test -tags smoke -count=1 -p 1 -v -timeout 900s -run '$(SMOKE_RUN)' ./...

## test-integration: live tests (real podman/firecracker on a Linux/KVM host);
## each skips gracefully when podman or the sandbox image is unavailable.
## -p 1 serializes packages: they share one network fabric + host SSH port pool
## (each uses its own temp state dir), so parallel packages would collide.
## -v streams each test's start/result as it happens: these tests boot real
## containers and microVMs, so without it a package prints nothing for minutes.
## The smoke suite is tagged for this run too, so a full local pass covers it
## first — its failures are cheap and point at the host, not the engine.
test-integration:
	go test -tags integration -p 1 -v -timeout 900s ./...

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
docs:
	python3 scripts/lint-docs.py
check: fmt-check vet test docs
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed; see https://golangci-lint.run/welcome/install/" >&2; \
		exit 2; \
	}
	golangci-lint run

## snapshot: local release dry-run into dist/ (all platforms, archives, checksums).
## Skips SBOM + cosign signing (those need cyclonedx-gomod + cosign; run in CI/release).
snapshot:
	$(GORELEASER) release --snapshot --clean --skip=sbom,sign

## release: tagged release (needs a pushed git tag + credentials). For a full
## signed+SBOM release install: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest and cosign.
release:
	$(GORELEASER) release --clean

## release-check: validate .goreleaser.yaml
release-check:
	$(GORELEASER) check

## clean: remove build output
clean:
	rm -rf bin dist
