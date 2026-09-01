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

You need the Go toolchain declared in [`go.mod`](go.mod) (currently Go 1.26.6),
Node.js 24 with npm, Git, and
[`golangci-lint`](https://golangci-lint.run) `v2.12.2` (schema-v2 config in
[`.golangci.yml`](.golangci.yml)).

```sh
make verify-fast # pre-push format, vet, and Go build tier
make tidy-check  # check that go.mod/go.sum match tidy output
make ci          # merge gate: full Go, config, and portal validation
make verify-full # ci plus integration, platform, and coverage gates
make vulncheck   # scan reachable Go code for known vulnerabilities
```

### Validation tier contract

The stable local contract is `make verify-fast` ⊂ `make ci` ⊂
`make verify-full`:

| Tier | Composition | Use |
|---|---|---|
| `make verify-fast` | Format check, `go vet`, and every `cmd/*` Go build | Fast feedback during development and before a push |
| `make ci` | The portable merge gate: fast-tier checks plus module tidiness, flake-policy enforcement, shipped-config validation, race tests with coverage, lint, and portal build/test/contract checks | Required before merge; the shipped agent workflows' `local-ci` stages invoke this tier |
| `make verify-full` | `ci` plus strict declared-dependency integration tests, walking-skeleton e2e, journal conformance, Kubernetes envtest, coverage threshold, native sandbox confinement, and Linux-node/Windows-seam validation | Nightly, on-demand, and release-candidate validation |

`make vulncheck` is a separate, network-backed static-analysis gate. It runs the
pinned `govulncheck` version used by pull-request and scheduled CI without adding
network access to the hermetic merge-tier test process.
`make tidy-check` reproduces the merge tier's `go mod tidy -diff` check with the
same configured Go command and inherited module settings.

The subset relationship is executable rather than documentary:
`verify-fast` selects checks from the same Go check list as `ci`, while
`verify-full` asks the same orchestrator to invoke `ci` and each additional
Make gate serially. Tests in `test/ci` compare the complete tier check lists and
recipes, so extra or missing commands fail the contract check. Each validation
tier prints the elapsed time for every gate it runs; CI also publishes
structured unit-test timing and soft-budget comparisons — see
[`docs/guides/test-timing.md`](docs/guides/test-timing.md).
The stress tier's fingerprint ledger, expiring quarantine helper, and
no-anonymous-retry rule are documented in
[`docs/guides/flake-management.md`](docs/guides/flake-management.md).

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

### Design-document status

Every Markdown document under `docs/design/` and `docs/adr/` must put a
`Status:` marker in its first 10 lines. The first word after the marker is one
of this controlled enum:

| Status | Meaning |
|---|---|
| `draft` | Proposed, exploratory, or under review |
| `approved` | Accepted as a design or decision, but not necessarily implemented |
| `implemented` | Reflected in the shipped system |
| `superseded` | Replaced in whole or in part by a newer source of record |
| `historical` | Retained as a completed campaign, survey, or other historical record |

Status values are case-insensitive. Free-text detail may follow the enum value,
for example `> Status: **implemented — GA in #1939**`. The merge gate rejects a
missing marker or a value outside the enum.

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
| `Go vulnerability scan` | Standalone `make vulncheck` gate for reachable standard-library and dependency vulnerabilities |
| `make ci` aggregate | Required status for the merge tier, Windows compile slice, and vulnerability scan; it runs no additional validation |
| `unit` | Standalone `make ci` gate |
| `unit behavioral suite (macos)` | Whole-tree behavioural suite, plus the `make cover-gate` coverage-threshold gate against the profile that run produces |
| `declared-dependency integration` | Full-tier `make test-integration-strict` gate with every inventoried executable provisioned, plus the envtest control-plane gate (`KUBEBUILDER_ASSETS`) |
| `sandbox confinement` | Full-tier `make sandbox-check` gate with native sandbox availability required |
| `linux node validation` | Full-tier `make linux-node-validation` platform acceptance gate for the shipped binary, daemon lifecycle, and Windows seams |

The dedicated vulnerability, integration, sandbox, and Linux-node CI jobs invoke
their corresponding Make targets. The vulnerability target also runs daily from
`.github/workflows/vulnerability-scan.yml`, so newly disclosed findings surface
without a code change.

`make test-conformance`, `make test-e2e`, `make test-envtest` and
`make cover-check` remain as local and full-tier targets, but **no longer have
dedicated CI jobs.** Each was an unsharded whole-tree run of a suite another
required job already runs, and together they cost ~48 of the ~142 runner-minutes
a pull request consumed and set both ends of its critical path. What each one
uniquely enforced moved rather than lapsed:

- **coverage** → a `make cover-gate` step on `unit-macos`, which runs the suite
  unsharded and so already emitted the whole-tree profile the gate needs.
- **envtest** → `KUBEBUILDER_ASSETS` provisioning on `integration`, which already
  selects `internal/operator` and already enforces the `-run=^TestIntegration`
  contract through a runtime AST scan. That job now asserts
  `TestIntegrationEnvtestReconcile` actually PASSED, because `testdep.RequireEnv`
  SKIPS it when the variable is empty — a shape that previously let the job exit
  0 without exercising an API server at all.
- **conformance** and **e2e** → already ran, unfiltered, inside the `unit` shards.
  `-run` filters execution but not compilation, so the conformance job was
  rebuilding 147 race-instrumented binaries to run 32 tests. The shard invocation
  carries `-count=1`, which was that job's only genuine differentiator.

`make test-conformance` still selects every Go test whose name begins with
`TestConformance`, currently covering `journal.ConformanceView`, journal sequence
determinism, and the local-runner walking-skeleton seed. That target and naming
boundary remain the landing zone for the V2 local-to-Temporal dual-runner
conformance harness. Future stress jobs follow the same
one-target-per-job pattern.
Focused targets such as
`make validate-configs`, `make portal-ci`, and `make portal-contract` remain
available when only one surface changed. `go run ./test/ci` is the
cross-platform implementation of `make ci`; it launches tools without Bash or
POSIX-shell syntax. On Windows, stock `cmd.exe` is used only for Node's
`npm.cmd` shim, and GNU Make is not required. Other convenience targets can
still use a POSIX shell.

Micro-benchmarks for journal event encoding, scrubbing, parsing, and read-model
projection are opt-in and do not run with ordinary tests:

```sh
go test -run=^$ -bench=. ./internal/journal ./internal/readmodel
```

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
`test/testsupport/testdep` and the integration CI provisioning step together.

### CI platform matrix

| Runner | Command | PR status | What it gates |
|---|---|---|---|
| `ubuntu-latest` | `go run ./test/ci` | Required via the aggregate CI check | The full Linux Go and portal gate |
| `macos-latest` | `go run ./test/ci` | Required via the aggregate CI check | The full macOS Go and portal gate |
| `windows-latest` | `go build ./...` + `go vet ./...` | Required via the aggregate CI check | Native Windows compile and vet coverage |
| `ubuntu-latest` | `make vulncheck` | Required via the aggregate CI check | Reachable Go vulnerability findings |

The required `make ci (fmt-check · vet · build · test · lint)` status keeps its
existing name for branch-protection compatibility and fails when either full
platform leg, the Windows compile slice, or the vulnerability scan fails. Go
module and build caches are scoped to each runner OS.

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
  portable CI checks, Windows compile smoke, vulnerability scan, and
  journal-conformance gate pass on the latest commit.
- **Approvals** — none. The required approval count is zero, and
  [CODEOWNER](.github/CODEOWNERS) approval is not required. CODEOWNERS are still
  requested for review, but those requests are advisory.

Required-review enforcement is the repository policy decision tracked in
[#763](https://github.com/Agent-Clubhouse/Goobers/issues/763). If that decision changes
the repository rules, update this section as part of the same settings change so the
documented and enforced policies do not drift.

Prefer small, reviewable PRs. Squash-merge is the default so `main` stays linear.

## Review rules

These are class-level rules: they exist because the same defect shape has
shipped more than once, so review checks the whole class rather than the
individual instance ([#2081](https://github.com/Agent-Clubhouse/Goobers/issues/2081)).

### Append-only growth needs a wired bound or pruner

**Rule.** A change that introduces a structure which grows without a natural
end — an append-only file, a database table, an on-disk directory of
per-run/per-item entries, or a long-lived in-memory map or slice — must
identify and **wire** its bound or pruner in the same change. "Wired" means a
running production caller reaches it: a daemon sweep, a retention loop, a
writer that trims on append, or an eviction on insert. A pruner that only an
operator can invoke, or one whose only caller is a test, does not bound
anything.

**Acceptable evidence** — a reviewer should be able to point at all three:

1. **The bound.** A named limit (row/entry count, age window, byte cap, or
   fixed capacity) with a default that applies to a stock configuration, not
   only to a configuration an operator opts into.
2. **The wiring.** The call path from a process that runs unattended to the
   code that enforces the bound. For example, the projection retention loop
   calls `PruneChangeFeed` on every pass
   (`internal/readmodel/retentionloop.go`), and the daemon's retention ticker
   calls `pruneConfiguredTelemetryRetention` and `compactSchedulerRetention`
   (`cmd/goobers/up.go`). Citing the pruner function alone is not evidence;
   cite its production caller.
3. **The test.** A test that grows the structure past its bound and asserts a
   bounded steady state — not merely that the prune function returns without
   error. `TestPruneChangeFeedBoundsGrowth`
   (`internal/readmodel/retention_test.go`) is the shape to copy.

If the bound genuinely belongs in a follow-up, say so in the PR and open the
follow-up issue in the same change; an unbounded structure landing with no
named owner for its bound is a `needs-changes`.

**Incident lineage.** This class keeps recurring in different disguises:
[#2038](https://github.com/Agent-Clubhouse/Goobers/issues/2038) — the instance
journal and the rollup scheduler tables grew at scheduler-tick rate for the
daemon's lifetime because the only compactor refused to run while a daemon was
up, so a never-restarted daemon never reclaimed anything (the referenced live
journal reached 324 MB);
[#3048](https://github.com/Agent-Clubhouse/Goobers/issues/3048) — the change
feed's designed 50,000-row bound existed but was not enforced independently of
projection retention, so the bound was documented and inoperative;
[#3049](https://github.com/Agent-Clubhouse/Goobers/issues/3049) — tombstones
accumulated with no retention policy or deleter at all; and
[#3969](https://github.com/Agent-Clubhouse/Goobers/issues/3969) — temp
directories orphaned by an OOMKill were never reclaimed because nothing swept
the ones no live process owned. In each case the growth was introduced by a
change that was correct in isolation and the bound arrived later, after the
disk or the startup cost had already become the symptom.

### Closed JSON schemas join the structural drift guard

A new **closed** JSON schema (`additionalProperties: false`) that mirrors a Go
type must join the structural Go-to-schema drift guard **in the change that adds
it** — not in a follow-up. A closed schema rejects an envelope carrying a field
it does not declare, so a field added to the Go producer later becomes a
validation failure at runtime rather than a build failure, and nothing else in
the tree notices the divergence.
[#1700](https://github.com/Agent-Clubhouse/Goobers/issues/1700)/[#1704](https://github.com/Agent-Clubhouse/Goobers/issues/1704)
and [#2042](https://github.com/Agent-Clubhouse/Goobers/issues/2042) — the last
adding `DataSchema` to `journal.Event` with no matching schema property — are
instances of that one class, which is why the rule is structural rather than a
reviewer's memory.

Registration path, both steps in the same change:

1. Register the schema file in [`api/schemas/embed.go`](api/schemas/embed.go):
   in the map naming its contract family (`Envelope`, `Journal`,
   `Notification`), or as an exported constant for a standalone artifact
   schema.
2. Add a fully populated round-trip fixture for it to the `fixtures` map in
   `TestSchemaBackedEnvelopeCompleteness`
   ([`api/validate/envelope_completeness_test.go`](api/validate/envelope_completeness_test.go)).
   The guard asserts every JSON field of the Go value is populated, marshals
   it, and validates the result against the schema, so a Go field the schema
   does not declare fails the merge gate. An entry in `schemas.Envelope` with no fixture
   fails the guard on its own; a schema outside that map — like
   `journal-event` — is covered only once its fixture is added, so add it
   explicitly.

The guard runs in `make ci` (`go test ./api/validate/...`). If a schema
genuinely has no Go producer to drift from, say so in the pull request rather
than leaving the omission unexplained.

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
pre-release baseline). This is the feature-registry's own lineage, distinct
from git release tags: it advances only on a stable tag, never on a SemVer
pre-release tag (`v1.2.3-beta.2` and similar — see `docs/guides/releases.md`
for those). The compatibility guard compares the current registry
with the feature registry executed from the latest canonical SemVer tag
advertised by `origin`. A removal is valid only when that tagged build already
marks the feature deprecated; adding deprecated and removed history in one
change does not satisfy the release window. Before the first tagged release,
the external baseline is empty and no feature may enter `removed`. Registry
validation and `TestFeatureRegistryAgainstLatestRelease` reject rewritten,
skipped, out-of-order, or too-early transitions. Local-only tags are ignored so
stale runner state cannot invent a release baseline. When changing the current
feature matrix, regenerate it with `make docs`.

Whole DSL versions have a separate support window in
`internal/supportmatrix`. After a supported DSL minor is superseded, it must
remain loadable as `supported` or `deprecated` for at least three minor
releases: a version superseded in `v1.1.0` cannot become `unsupported` before
`v1.4.0`. It must also spend at least one released minor as `deprecated`, so a
version deprecated in `v1.3.x` cannot become unsupported before `v1.4.0`;
direct `supported -> unsupported` transitions are forbidden. Keep each
`VersionSupport.History` complete and release-ordered. The support-matrix
policy guard rejects invalid histories and windows. It also compares the
current matrix with the matrix executed from the latest reachable canonical
SemVer tag: released versions and history cannot be removed or rewritten, and
a version may become `unsupported` only when that tagged matrix already marks
it `deprecated`. Adding deprecated and unsupported history in one change does
not satisfy the released-minor window; before the first tag, no version may
become unsupported.

**Current state:** DSL `2.0` is the supported authoring version; every
shipped, reference, and example workflow pins it. DSL `1.4` is `deprecated`
(replacement `2.0`, unsupported after `v0.5.0`) — a workflow pinned to `1.4`
still loads and runs, but `goobers validate` emits a `DVL020` warning. Migrate
a pinned workflow mechanically with `goobers fix --to 2.0`; the only semantic
delta the migrator pins is `automated.pollIntervalSeconds: 10` on gates fed by
a `ci-poll` task, where 2.0's input builder injects that default.

## Binary maintenance policy

The `goobers` **binary** (distinct from the DSL compatibility policy above) is
maintained **forward-only**, as a *resourcing* stance while the team is
small — not an architectural promise (`docs/design/dsl-version-lifecycle.md`
§3.5, DVL-9/#869). We do not cut backport releases of old binaries; a fix
ships in the next release, and you get it by upgrading forward. Upgrading
must never break a pinned DSL version: a newer binary still carries every
interpreter still listed in its `SupportMatrix`.

**PATCH means no author-visible contract change.** A binary release that
fixes interpreter/runtime behavior without changing any DSL version's
observable contract is a patch. If a "fix" changes what a workflow observes,
it is a new DSL minor or major, not a patch — see the DSL compatibility
policy above.

A **frozen** interpreter package (`internal/workflow/v_next`, once
`v_3_0` supersedes it in turn) may only receive contract-preserving
patches; a feature or semantic change belongs in a copied-forward
interpreter instead. This is enforced, not just documented: each frozen
package's `testdata/golden/PATCH_LOG.json` pins its `digests.json` by
sha256 and records every reviewed patch, so a compiled-digest change with
no accompanying patch entry fails CI (see that package's `README.md`).

The theoretical case of a correctness/security bug *inside* a frozen
interpreter that authors can't quickly migrate off is intentionally out of
scope for now (design doc §8.7) — with a small team and few coexisting
versions, the answer is fix-forward-and-migrate.

## Commit messages

Use a short imperative subject (`area: do the thing`), a body explaining *why* when it's
not obvious, and reference issues (`Closes #123`). Keep unrelated changes out of the commit.
