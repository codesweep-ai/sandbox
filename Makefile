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

.PHONY: build build-go install uninstall test test-integration vet fmt fmt-check check lint snapshot release release-check clean

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

## test-integration: live tests (real podman/firecracker on a Linux/KVM host);
## each skips gracefully when podman or the sandbox image is unavailable.
## -p 1 serializes packages: they share one network fabric + host SSH port pool
## (each uses its own temp state dir), so parallel packages would collide.
## -v streams each test's start/result as it happens: these tests boot real
## containers and microVMs, so without it a package prints nothing for minutes.
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
check: fmt-check vet test
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
