# Goobers control-plane Makefile.
# Owns the Go build/test/lint toolchain for the monorepo (module github.com/goobers/goobers).

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# ---- Build metadata (injected into internal/version via -ldflags) -----------
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/goobers/goobers/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

# Discover command binaries from cmd/*.
CMDS := $(notdir $(wildcard cmd/*))
BIN  := bin

GO            ?= go
GOLANGCI_LINT ?= golangci-lint

# Minimum testable-logic coverage enforced by `make cover-check` (ratchet up over
# time). Overridable: `make cover-check COVERAGE_THRESHOLD=75`.
COVERAGE_THRESHOLD ?= 70

# Pinned codegen + test tooling (run via `go run`, no global installs).
CONTROLLER_GEN_VERSION ?= v0.16.5
SETUP_ENVTEST_VERSION  ?= release-0.19
ENVTEST_K8S_VERSION    ?= 1.31.0
CONTROLLER_GEN := $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
SETUP_ENVTEST  := $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

## help: Show this help.
.PHONY: help
help:
	@echo "Goobers — make targets:"
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed -E 's/^## ([a-z-]+): /  \1\t/' | expand -t20

## tidy: Sync go.mod/go.sum.
.PHONY: tidy
tidy:
	$(GO) mod tidy

## generate: Regenerate DeepCopy methods (controller-gen object) for the CRD types.
.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths=./api/v1alpha1/...

## manifests: Regenerate CRD YAML manifests from the CRD types.
.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd paths=./api/v1alpha1/... output:crd:dir=config/crd/bases

## test-envtest: Run tests with envtest binaries provisioned (operator integration).
.PHONY: test-envtest
test-envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...

## fmt: Format all Go source.
.PHONY: fmt
fmt:
	$(GO) fmt ./...

## fmt-check: Fail if any Go source is not gofmt-clean (CI gate).
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

## vet: Run go vet.
.PHONY: vet
vet:
	$(GO) vet ./...

## lint: Run golangci-lint (install: see .golangci.yml header).
.PHONY: lint
lint:
	$(GOLANGCI_LINT) run

## build: Build all cmd/* binaries into bin/.
.PHONY: build
build: $(addprefix build-,$(CMDS))

build-%:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/$* ./cmd/$*

## test: Run unit tests with race detector and coverage.
.PHONY: test
test:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...

## cover: Show total test coverage.
.PHONY: cover
cover: test
	$(GO) tool cover -func=coverage.out | tail -1

## cover-check: Fail if testable-logic coverage is below COVERAGE_THRESHOLD.
# Standalone gate (intentionally NOT part of `ci`): a coverage dip should surface
# as its own visible job, not break every `make ci` reproduction mid-development.
# Excludes generated/cmd-mains/embed from the denominator (see test/coverage_gate.sh).
.PHONY: cover-check
cover-check: test
	COVERAGE_PROFILE=coverage.out bash test/coverage_gate.sh $(COVERAGE_THRESHOLD)

## ci: Full gate run locally (matches the pipeline).
.PHONY: ci
ci: fmt-check vet build test lint

## clean: Remove build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out
