# Contributing to Goobers

Thanks for your interest in Goobers — an open, self-hosted agent-workforce platform.
This guide covers the GitHub-based contribution flow. For what the project is and where
it's going, start with [`README.md`](README.md), [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
and [`docs/VISION.md`](docs/VISION.md).

## Ground rules

- Be respectful — see the [Code of Conduct](CODE_OF_CONDUCT.md).
- Contributions are accepted under the repository's [MIT License](LICENSE); by opening a
  pull request you agree your contribution is licensed under it.
- Found a security issue? **Do not open a public issue** — follow [SECURITY.md](SECURITY.md).

## Development setup

You need the Go toolchain declared in [`go.mod`](go.mod) (currently Go 1.26),
Node.js 24 with npm, Git, and
[`golangci-lint`](https://golangci-lint.run) `v2.12.2` (schema-v2 config in
[`.golangci.yml`](.golangci.yml)).

```sh
make verify-fast # pre-push format, vet, and Go build tier
make ci          # merge gate: full Go, config, and portal validation
make verify-full # ci plus integration, platform, and coverage gates
```

### Validation tier contract

The stable local contract is `make verify-fast` ⊂ `make ci` ⊂
`make verify-full`:

| Tier | Composition | Use |
|---|---|---|
| `make verify-fast` | Format check, `go vet`, and every `cmd/*` Go build | Fast feedback during development and before a push |
| `make ci` | The unchanged portable merge gate: fast-tier checks plus shipped-config validation, race tests with coverage, lint, and portal build/test/contract checks | Required before merge; the shipped agent workflows' `local-ci` stages invoke this tier |
| `make verify-full` | `ci` plus strict declared-dependency integration tests, walking-skeleton e2e, Kubernetes envtest, coverage threshold, native sandbox confinement, and Linux-node/Windows-seam validation | Nightly, on-demand, and release-candidate validation |

The subset relationship is executable rather than documentary:
`verify-fast` selects checks from the same Go check list as `ci`, while
`verify-full` has `ci` and the additional gates as serialized Make
prerequisites. Tests in `test/ci` compare the complete tier recipes and
prerequisite graph, so extra or missing commands fail the contract check.

Tests that intentionally execute tools outside the Go test process belong in
`//go:build integration` files and must declare each executable with
`testdep.Require`; their names use the `TestIntegration*` prefix so the tier
runs no ordinary package tests. `make test-integration` is the
developer-friendly entrypoint: a missing tool produces a visible, uniform skip
with an install hint.
`make test-integration-strict` sets `TESTDEP_STRICT=1`, so the same absence is a
test failure; `verify-full` and CI always use this strict target. The integration
runner prints the dependency inventory, runs only packages containing tagged
tests, and rejects direct `exec.LookPath`, raw skips, or inventory drift.
Ordinary unit tests should use in-process fakes; integration tests are for real
local executables, not network services, cloud credentials, or heavyweight
infrastructure.

**Humans:** use `verify-fast` for the short edit/push loop, `ci` for the merge
gate, and `verify-full` on a Unix-like host with the pinned envtest and native
sandbox prerequisites available. **Agent workflow authors:** a Goobers
`local-ci` stage for this repository must call `make ci`; the subprocess may
assume only the tools listed below are on the daemon's `PATH` and otherwise
inherits the daemon environment. This contract does not make stage execution
hermetic. For another repository, configure its real non-interactive merge-gate
command instead. **CI:** each validation job maps to the same contract:

| GitHub Actions job | Tier correspondence |
|---|---|
| `platform gate` (Ubuntu/macOS) | `make ci` (`go run ./test/ci` is its portable implementation) |
| `windows compile smoke` | The Windows `go vet` + build slice of `verify-fast` |
| `make ci` aggregate | Required status for the merge tier and Windows compile slice; it runs no additional validation |
| `declared-dependency integration` | Full-tier `make test-integration-strict` gate with every inventoried executable provisioned |
| `sandbox confinement` | Full-tier `make sandbox-check` gate with native sandbox availability required |
| `linux node validation` | Full-tier `make linux-node-validation` platform acceptance gate for the shipped binary, daemon lifecycle, and Windows seams |

The dedicated integration, sandbox, and Linux-node CI jobs invoke their
corresponding Make targets. E2e, envtest, and coverage are local `verify-full` gates pending CI
promotion in [#628](https://github.com/Agent-Clubhouse/Goobers/issues/628);
future conformance or stress jobs follow the same one-target-per-job pattern.
Focused targets such as
`make validate-configs`, `make portal-ci`, and `make portal-contract` remain
available when only one surface changed. `go run ./test/ci` is the
cross-platform implementation of `make ci`; it launches tools without Bash or
POSIX-shell syntax. On Windows, stock `cmd.exe` is used only for Node's
`npm.cmd` shim, and GNU Make is not required. Other convenience targets can
still use a POSIX shell.

### Platform prerequisites

| Platform | Required tools | Merge-tier invocation |
|---|---|---|
| Linux | Go from `go.mod`, Node.js 24 with npm, Git, `golangci-lint` v2.12.2 | `go run ./test/ci` (`make ci` also works with GNU Make and a POSIX shell) |
| macOS | Go from `go.mod`, Node.js 24 with npm, Git, `golangci-lint` v2.12.2 | `go run ./test/ci` (`make ci` also works with GNU Make and a POSIX shell) |
| Windows | Go from `go.mod`, Node.js 24 with npm, Git for Windows, `golangci-lint` v2.12.2, 64-bit MinGW-w64 GCC | `go run ./test/ci` from PowerShell or Command Prompt; Bash and GNU Make are not required |

The Windows compiler is required by Go's race detector, not by the CI task
runner. Install a 64-bit MinGW-w64 GCC with runtime version 8 or newer, put its
`bin` directory on `PATH`, and set `CC` when the compiler executable is not
named `gcc`. The portable runner sets `CGO_ENABLED=1` for the race-test step.
Verify the runtime with `gcc --print-file-name libsynchronization.a`: a
compatible installation prints the full path to that library rather than the
bare filename. See Go's
[race-detector requirements](https://go.dev/doc/articles/race_detector#Requirements).
No Bash or MSYS shell is required by the gate; tests that specifically
exercise Unix process and shell semantics are platform-gated on Windows.
`verify-full` is Unix-hosted because its envtest, native-sandbox, and node
validation targets use POSIX host facilities; Linux additionally requires
`bubblewrap` with unprivileged user namespaces available.
The strict integration target additionally provisions the executable inventory
reported by `make test-integration`; when adding a dependency, update
`internal/testdep` and the integration CI provisioning step together.

### CI platform matrix

| Runner | Command | PR status | What it gates |
|---|---|---|---|
| `ubuntu-latest` | `go run ./test/ci` | Required via the aggregate CI check | The full Linux Go and portal gate |
| `macos-latest` | `go run ./test/ci` | Required via the aggregate CI check | The full macOS Go and portal gate |
| `windows-latest` | `go build ./...` + `go vet ./...` | Required via the aggregate CI check | Native Windows compile and vet coverage |

The required `make ci (fmt-check · vet · build · test · lint)` status keeps its
existing name for branch-protection compatibility and fails when either full
platform leg or the Windows compile slice fails. Go module and build caches are
scoped to each runner OS.

## Workflow

1. **Fork** the repo (external contributors) or **branch** from `main` (maintainers).
2. Create a topic branch: `git checkout -b <area>/<short-description>`.
3. Make your change. Keep the diff scoped to one logical concern.
4. **Add tests** for new behavior and error paths — untested new behavior will be sent back.
5. Run the `make ci` merge tier locally.
6. Open a **pull request against `main`**, filling in the
   [PR template](.github/PULL_REQUEST_TEMPLATE.md).
7. The required Ubuntu, macOS, and Windows CI checks must pass. Address review
   feedback; keep the branch up to date with `main`.

## Merge requirements

`main` is protected. The active repository rules require:

- **CI is green** — the required aggregate confirms the Ubuntu and macOS
  portable CI checks and the Windows compile smoke pass on the latest commit.
- **Approvals** — none. The required approval count is zero, and
  [CODEOWNER](.github/CODEOWNERS) approval is not required. CODEOWNERS are still
  requested for review, but those requests are advisory.

Required-review enforcement is the repository policy decision tracked in
[#763](https://github.com/Agent-Clubhouse/Goobers/issues/763). If that decision changes
the repository rules, update this section as part of the same settings change so the
documented and enforced policies do not drift.

Prefer small, reviewable PRs. Squash-merge is the default so `main` stays linear.

## DSL compatibility policy

The `apiVersion` on configuration resources defines a compatibility line. Within
one `apiVersion`, the following changes are non-breaking and may ship in a minor
release:

- adding optional fields or enum values;
- adding stage or gate kinds;
- relaxing constraints; and
- promoting a DSL feature from `preview` to `ga`.

Removing or renaming a field, tightening a constraint, changing a default, or
changing existing semantics is breaking. A breaking change requires either a new
`apiVersion` or a `deprecated -> removed` lifecycle in
`internal/workflow/features.go` (`ga` features must first transition to
`deprecated`). A feature must remain usable as `deprecated` for at least one
released minor: if it is deprecated in `v1.2.x`, the earliest removal is
`v1.3.0`. Direct `ga -> removed` and `preview -> removed` transitions are
forbidden.

Registry entries retain every lifecycle transition in `Feature.History`; the
current `Level` and `SinceVersion` must match the final transition. Use
`vMAJOR.MINOR.PATCH` release versions (`dev` is reserved for the initial
pre-release baseline). The compatibility guard compares the current registry
with the feature registry executed from the latest reachable canonical SemVer
tag. A removal is valid only when that tagged build already marks the feature
deprecated; adding deprecated and removed history in one change does not
satisfy the release window. Before the first tagged release, the external
baseline is empty and no feature may enter `removed`. Registry validation and
`TestFeatureRegistryAgainstLatestRelease` reject rewritten, skipped,
out-of-order, or too-early transitions. CI checks out complete tag history so
the release baseline cannot silently disappear. When changing the current
feature matrix, regenerate it with `make docs`.

## Commit messages

Use a short imperative subject (`area: do the thing`), a body explaining *why* when it's
not obvious, and reference issues (`Closes #123`). Keep unrelated changes out of the commit.
