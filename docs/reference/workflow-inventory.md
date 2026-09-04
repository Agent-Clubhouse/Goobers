# GitHub Actions workflow inventory

Every workflow in `.github/workflows/`, with the triggers that are actually
enabled in the file and whether it is **active** (something fires it) or
**dormant** (its only enabled trigger is `workflow_dispatch` while a `schedule:`
block sits commented out, so it never runs on its own).

A dormant workflow is not a gate. It has no recorded runs and enforces nothing
until whatever it is blocked on lands and its schedule is uncommented; the
`Blocked on` column names that. Reviewers cited the provider-fixture-drift pair
as a CI strength while both had never fired once (#4224) — this table exists so
that the difference is legible.

The table below is generated and checked in merge-tier CI by
`go run ./test/workflowinventory` (the `workflow-inventory` check in the
`checks` job). Regenerate it with `go run ./test/workflowinventory -write` after
adding, removing, or retriggering a workflow, then fill in the `Blocked on` cell
for any new dormant workflow — the check fails while one reads `TODO`, because a
workflow that never runs and names no blocker is indistinguishable from an
abandoned one. Everything except `Blocked on` is derived from the workflow files
themselves, so no cell can drift from the YAML.

<!-- BEGIN GENERATED WORKFLOW INVENTORY -->

| Workflow | Enabled triggers | Status | Blocked on |
| --- | --- | --- | --- |
| `ado-live-conformance.yml` | schedule, workflow_dispatch | active | — |
| `ci.yml` | merge_group, pull_request, push | active | — |
| `config-validate-gate-selftest.yml` | pull_request, push, workflow_dispatch | active | — |
| `evals-gate.yml` | workflow_dispatch | dormant | #2667 (runner integration + committed baselines) |
| `evals-tests.yml` | pull_request, push, workflow_dispatch | active | — |
| `flake-watch.yml` | schedule, workflow_dispatch | active | — |
| `ghcp-echo.yml` | schedule, workflow_dispatch | active | — |
| `large-repo-scale.yml` | schedule, workflow_dispatch | active | — |
| `provider-fixture-drift-ado.yml` | workflow_dispatch | dormant | first live-candidate review (no tracking issue) |
| `provider-fixture-drift.yml` | workflow_dispatch | dormant | #1478 (designated repo, issue, PR, credential) |
| `release.yml` | push, workflow_dispatch | active | — |
| `scheduled-failure-alarm.yml` | schedule, workflow_dispatch | active | — |
| `stress.yml` | schedule, workflow_dispatch | active | — |
| `tracked-gap-references.yml` | schedule, workflow_dispatch | active | — |
| `vulnerability-scan.yml` | schedule, workflow_dispatch | active | — |

<!-- END GENERATED WORKFLOW INVENTORY -->

## Enabling a dormant workflow

1. Confirm the blocking issue is closed and its prerequisites exist (fixtures,
   credentials, a runner — whatever the workflow's own header names).
2. Uncomment the `schedule:` block.
3. Re-run `go run ./test/workflowinventory -write`; the row flips to `active`
   and its `Blocked on` cell clears.
