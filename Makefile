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

## help: Show this help.
.PHONY: help
help:
	@echo "Goobers — make targets:"
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed -E 's/^## ([a-z-]+): /  \1\t/' | expand -t20

## tidy: Sync go.mod/go.sum.
.PHONY: tidy
tidy:
	$(GO) mod tidy

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

## ci: Full gate run locally (matches the pipeline).
.PHONY: ci
ci: fmt-check vet build test lint

## clean: Remove build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out
