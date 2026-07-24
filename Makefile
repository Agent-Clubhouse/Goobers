# Goobers control-plane Makefile.
# Owns the Go build/test/lint toolchain for the monorepo (module github.com/goobers/goobers).

# ---- Portability posture (#630) ---------------------------------------------
# The fast and merge tiers (`make verify-fast` / `make ci` -> `go run
# ./test/ci`), the coverage gate (`make cover-check` -> `go run
# ./test/coveragegate`), and the stress tier (`make stress` -> `go run
# ./test/stress`) are pure Go. They spawn only real toolchain binaries
# (go, gofmt, git, npm, golangci-lint) and the freshly built goobers validator,
# never bash/sh or a project shell script, so CI reproduces on any OS with a Go
# toolchain — Windows included: test/ci handles the .exe suffix, the cgo race
# detector, and wrapping npm through cmd.exe.
# There are no build/CI shell scripts in the tree, and a guard test enforces
# that (test/ci: no shell on the toolchain path; the gates stay Go-delegated).
#
# The Unix-hosted full tier and convenience recipes below (build, clean, help,
# test-envtest) use a POSIX shell for environment assignments, `$(shell …)`,
# `rm`, `grep`/`sed`/`expand`, etc. On a shell-less platform (e.g. Windows cmd
# without git-bash), build a binary directly with `go build ./cmd/<name>` or
# reproduce the merge gate with `go run ./test/ci`.

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
NPM           ?= npm

# Minimum testable-logic coverage enforced by `make cover-check` (ratchet up over
# time). Overridable: `make cover-check COVERAGE_THRESHOLD=75`.
COVERAGE_THRESHOLD ?= 70
STRESS_OUTPUT_DIR   ?= stress-results
STRESS_SEED         ?= 0

# Pinned codegen + test tooling (run via `go run`, no global installs).
CONTROLLER_GEN_VERSION ?= v0.16.5
SETUP_ENVTEST_VERSION  ?= release-0.19
GOVULNCHECK_VERSION    ?= v1.6.0
ENVTEST_K8S_VERSION    ?= 1.31.0
CONTROLLER_GEN := $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
SETUP_ENVTEST  := $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
GOVULNCHECK    := $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

## help: Show this help.
.PHONY: help
help:
	@echo "Goobers — make targets:"
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed -E 's/^## ([a-z-]+): /  \1\t/' | expand -t20
	@echo ""
	@echo "Note: 'make build' also builds quarantined/superseded binaries (kept"
	@echo "compiling, not on the V0 path) — operator, scheduler are tier-3 (V2),"
	@echo "goober-runtime is superseded by the goobers binary. See docs/ARCHITECTURE.md §11."

## tidy: Sync go.mod/go.sum.
.PHONY: tidy
tidy:
	$(GO) mod tidy

## tidy-check: Fail if go.mod/go.sum differ from tidy output (CI gate).
.PHONY: tidy-check
tidy-check:
	$(GO) mod tidy -diff

## generate: Regenerate CRD DeepCopy methods and the portal API contract.
.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths=./api/v1alpha1/...
	$(GO) generate ./internal/apicontract

## manifests: Regenerate CRD YAML manifests from the CRD types.
.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd:allowDangerousTypes=true paths=./api/v1alpha1/... output:crd:dir=config/crd/bases

## docs: Regenerate the committed CLI reference (docs/cli) + man pages (docs/man)
## from the command registry, and the feature matrix (docs/feature-matrix.md)
## from the workflow feature registry + DSL SupportMatrix. CI's TestCLIDocsUpToDate and
## TestFeatureMatrixDocUpToDate fail the build if the committed output drifts
## from this, so run it after any CLI help or DSL-feature change.
.PHONY: docs
docs:
	UPDATE_GOLDEN=1 $(GO) test ./cmd/goobers -run 'TestCLIDocsUpToDate|TestFeatureMatrixDocUpToDate'

## test-integration: Run declared-dependency integration tests (missing tools skip locally).
.PHONY: test-integration
test-integration:
	$(GO) run ./test/integration -go $(GO)

## test-integration-strict: Run integration tests with missing-tool skips forbidden.
.PHONY: test-integration-strict
test-integration-strict:
	TESTDEP_STRICT=1 $(GO) run ./test/integration -go $(GO)

## test-envtest: Run tests with envtest binaries provisioned (operator integration).
.PHONY: test-envtest
test-envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		$(GO) test -tags=integration -race -covermode=atomic -coverprofile=coverage.out ./...

## test-e2e: Run the walking-skeleton E2E harness scaffold.
.PHONY: test-e2e
test-e2e:
	$(GO) test ./test/e2e -count=1

## test-conformance: Run journal-contract conformance assertions in isolation.
.PHONY: test-conformance
test-conformance:
	$(GIT_TEST_FSYNC_OFF) $(JOURNAL_TEST_FSYNC_OFF) $(GO_TEST_NETWORK_OFF) $(GO) run ./test/hermetic --go-command "$(GO)" -- -race -run '^TestConformance' -count=1 ./...

## sandbox-check: Require and exercise native sandbox confinement.
.PHONY: sandbox-check
sandbox-check:
	GOOBERS_REQUIRE_SANDBOX_TEST=1 $(GO) test -race ./internal/sandbox

## linux-node-validation: Validate the daemon lifecycle and Windows platform seams.
.PHONY: linux-node-validation
linux-node-validation:
	$(GO) build -o $(BIN)/goobers ./cmd/goobers
	$(GO) run ./test/linuxvalidate -bin $(BIN)/goobers -out $(BIN)/linux-validation-evidence
	GOOS=windows $(GO) build ./internal/winsvc/...
	GOOS=windows $(GO) vet ./internal/winsvc/...
	GOOS=windows $(GO) build ./internal/platform/safeopen/... ./internal/gooberassets/...
	GOOS=windows $(GO) vet ./internal/platform/safeopen/... ./internal/gooberassets/...

## test-shipped-workflows: Run every shipped workflow through the local runner contract harness.
.PHONY: test-shipped-workflows
test-shipped-workflows:
	$(GIT_TEST_FSYNC_OFF) $(JOURNAL_TEST_FSYNC_OFF) $(GO) test ./test/shippedworkflows -count=1

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

## vulncheck: Scan reachable Go code for known vulnerabilities.
.PHONY: vulncheck
vulncheck:
	$(GOVULNCHECK) ./...

## deadcode: Report unreviewed unreachable production functions.
.PHONY: deadcode
deadcode:
	$(GO) run ./test/deadcode -go $(GO)

## build: Build all cmd/* binaries into bin/.
.PHONY: build
build: $(addprefix build-,$(CMDS))

build-%:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/$* ./cmd/$*

build-goobers: portal-build

## image: Build the goobers container image (packaging/docker/Dockerfile) via docker.
# Optional path — not part of `ci`, `build`, or `go run ./release`; requires a
# local docker. Override the tag with IMAGE=<repo>:<tag>. CI publishing on
# tagged releases is a follow-up.
IMAGE ?= goobers:$(VERSION)
.PHONY: image
image:
	docker build -f packaging/docker/Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE) .

## deploy-validate: Build the k8s reference manifests (deploy/reference) with kubectl kustomize.
# Optional local validation for the #663 reference tree; requires kubectl.
# Schema validation (kubeconform) + helm-template rendering are follow-ups for
# the Validation & CI milestone.
.PHONY: deploy-validate
deploy-validate:
	kubectl kustomize deploy/reference/goobers-system >/dev/null
	kubectl kustomize deploy/reference/gaggle-namespace/examples/gaggle-a >/dev/null
	kubectl kustomize deploy/reference/gaggle-namespace/examples/gaggle-b >/dev/null
	@echo "deploy/reference kustomize builds OK"

## validate-configs: Build the validator, strictly check selfhost, and check other shipped config trees.
.PHONY: validate-configs
validate-configs:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/goobers ./cmd/goobers
	$(GO) run ./test/configvalidate $(BIN)/goobers

# Disable git fsync for the whole test run (#811). Every git subprocess the
# suite spawns (throwaway fixtures + the runner's real worktree clones/commits)
# operates on ephemeral scratch repos with zero durability needs, and fsync is
# the one git syscall that blocks in uninterruptible I/O sleep under disk
# saturation. When several `local-ci` stages run at once on the self-host box
# (each a full cold `make ci`), that flush contention wedged a single
# `git init/commit/clone` for the entire 10-minute stage limit — cmd/goobers
# never finished and the overnight run opened 0 PRs. core.fsync=none (git 2.36+)
# keeps git writes in the page cache so they return promptly under load; the
# deprecated core.fsyncObjectFiles is avoided (it prints a warning that pollutes
# git's combined output). Unknown to older git, the key is simply ignored.
GIT_TEST_FSYNC_OFF := GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.fsync GIT_CONFIG_VALUE_0=none

# Disable the goobers journal's OWN fsync for the whole test run — the journal
# twin of GIT_TEST_FSYNC_OFF, for the identical #811 reason. The cmd/goobers
# suite spins up real in-process `goobers run`/`signal`/`up` executions that
# fsync every journal event, checkpoint, and artifact; under the disk saturation
# of several concurrent cold `make ci` a single such fsync (and even the atomic
# rename that follows it, once the disk queue backs up) wedges in uninterruptible
# I/O for the entire 10-minute stage, so waitForRunTerminal polls a run that
# never reaches a terminal phase and the stage times out having opened 0 PRs.
# It MUST be set test-wide, not just in cmd/goobers: the shared disk queue stays
# saturated by every package's test fsync, so a per-package opt-out still stalls.
# Test instances are ephemeral t.TempDir scratch with zero durability needs, so
# nothing observable changes; production leaves the env unset and keeps fsync on.
JOURNAL_TEST_FSYNC_OFF := GOOBERS_DISABLE_FSYNC=1

# Prevent the outer `go run` that compiles the hermetic wrapper from resolving
# modules or a newer Go toolchain before the wrapper applies the same guards.
GO_TEST_NETWORK_OFF := GOENV=off GOFLAGS=-mod=readonly GONOPROXY=none GONOSUMDB=none GOPRIVATE= GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOVCS=*:off

## test: Run unit tests with race detector and coverage.
.PHONY: test
test:
	$(GIT_TEST_FSYNC_OFF) $(JOURNAL_TEST_FSYNC_OFF) $(GO_TEST_NETWORK_OFF) $(GO) run ./test/hermetic --go-command "$(GO)" -- -race -covermode=atomic -coverprofile=coverage.out ./...

## portal-ci: Install, type-check, build, test, and verify the Go wire contract.
.PHONY: portal-install portal-typecheck portal-build portal-test portal-contract portal-ci
portal-install:
	$(NPM) --prefix portal ci --no-audit --no-fund

## portal-typecheck: Install and type-check the portal.
portal-typecheck: portal-install
	$(NPM) --prefix portal run typecheck

portal-build: portal-install
	$(NPM) --prefix portal run build

portal-test: portal-install
	$(NPM) --prefix portal test

## portal-contract: Regenerate, diff, type-check, and test the Go/TypeScript wire contract.
portal-contract: portal-install
	$(GO) generate ./internal/apicontract
	git diff --exit-code -- portal/src/api/contract.generated.ts portal/src/api/wire.generated.ts
	$(NPM) --prefix portal run typecheck
	$(NPM) --prefix portal run test:contract

portal-ci: portal-build portal-test portal-contract

## cover: Show total test coverage.
.PHONY: cover
cover: test
	$(GO) tool cover -func=coverage.out | tail -1

## cover-check: Fail if testable-logic coverage is below COVERAGE_THRESHOLD.
# Standalone gate (intentionally NOT part of `ci`): a coverage dip should surface
# as its own visible job, not break every `make ci` reproduction mid-development.
# Excludes generated/cmd-mains/embed from the denominator (see test/coveragegate).
.PHONY: cover-check
cover-check: test
	COVERAGE_PROFILE=coverage.out $(GO) run ./test/coveragegate $(COVERAGE_THRESHOLD)

## verify-fast: Run the pre-push format, vet, and Go build tier.
.PHONY: verify-fast
verify-fast:
	$(GO) run ./test/ci fast

## ci: Run the portable full Go, config, and portal gate (matches the pipeline).
.PHONY: ci
ci: deadcode
	$(GO) run ./test/ci

## stress: Repeat timing-sensitive packages under the race detector.
.PHONY: stress
stress:
	$(GIT_TEST_FSYNC_OFF) $(JOURNAL_TEST_FSYNC_OFF) $(GO) run ./test/stress \
		-go "$(GO)" -packages test/stress/packages.txt \
		-output "$(STRESS_OUTPUT_DIR)" -seed "$(STRESS_SEED)"

## verify-full: Run all merge, integration, platform, coverage, shipped-workflow, and stress gates.
.PHONY: verify-full
verify-full:
	$(GO) run ./test/ci full "$(MAKE)"

## clean: Remove build artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out stress-results
