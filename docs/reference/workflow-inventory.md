# GitHub Actions workflow inventory

Every workflow in `.github/workflows/`, with the triggers that are actually
enabled in the file and whether it is **active** (something fires it) or
**dormant**. A workflow is dormant in one of two ways:

- its only enabled trigger is `workflow_dispatch` while a `schedule:` block sits
  commented out, so it never runs on its own; or
- it carries the top-level comment `# workflow-inventory: dormant-until-provisioned`.
  Its triggers fire, but every run skips its work and ends green until the
  credentials or fixtures it needs are provisioned.

A dormant workflow is not a gate. It enforces nothing until whatever it is
blocked on lands and its schedule is uncommented or its marker removed; the
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
| `ado-live-write.yml` | pull_request, schedule, workflow_dispatch | dormant | #5727 (scratch repository, `ADO_WRITE_REPOSITORY` variable, first green run) |
| `ci.yml` | merge_group, pull_request, push | active | — |
| `config-validate-gate-selftest.yml` | pull_request, push, workflow_dispatch | active | — |
| `design-delivery.yml` | pull_request | active | — |
| `design-ledger-reconcile.yml` | push, schedule, workflow_dispatch | active | — |
| `evals-gate.yml` | workflow_dispatch | dormant | #2681 (direction superseded; #2667/#2668 closed as redirected — retire or re-scope after ratification) |
| `evals-tests.yml` | pull_request, push, workflow_dispatch | active | — |
| `flake-watch.yml` | schedule, workflow_dispatch | active | — |
| `ghcp-echo.yml` | schedule, workflow_dispatch | active | — |
| `large-repo-scale.yml` | schedule, workflow_dispatch | active | — |
| `portal-package.yml` | pull_request, workflow_dispatch | active | — |
| `provider-fixture-drift-ado.yml` | workflow_dispatch | dormant | #4602 (ADO fixture work item, PAT, first live-candidate review) |
| `provider-fixture-drift.yml` | workflow_dispatch | dormant | #1478 (designated repo, issue, PR, credential) |
| `release.yml` | push, workflow_dispatch | active | — |
| `scheduled-failure-alarm.yml` | schedule, workflow_dispatch | active | — |
| `stress.yml` | schedule, workflow_dispatch | active | — |
| `tracked-gap-references.yml` | push, schedule, workflow_dispatch | active | — |
| `vulnerability-scan.yml` | schedule, workflow_dispatch | active | — |

<!-- END GENERATED WORKFLOW INVENTORY -->

## Enabling a dormant workflow

1. Confirm the blocking issue is closed and its prerequisites exist (fixtures,
   credentials, a runner — whatever the workflow's own header names).
2. Uncomment the `schedule:` block, or, for a provisioning-gated workflow,
   delete its `# workflow-inventory: dormant-until-provisioned` line once a run
   has gone green with real credentials.
3. Re-run `go run ./test/workflowinventory -write`; the row flips to `active`
   and its `Blocked on` cell clears.
