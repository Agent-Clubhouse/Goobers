# Human operations and advanced workflows: review stack

This is the temporary work ledger for the [program](design/hitl-advanced-workflows-program.md).
Stable `HAW-*` task IDs are intentional. Create numbered epics/issues and backlinks
after the related design or item merges; do not invent closing references.

## Review branches

All work is unmerged. The integration branch is `codex/hitl-advanced-workflows`,
forked from main at `04198152b63d228a9714ae2f92a7dca079ba5213`.

| Head | Base | Scope | Current evidence |
|---|---|---|---|
| `codex/haw-program-design` | integration branch | Program and stable task convention | Design/status and Markdown link checks pass |
| `codex/haw-child-design` | program design | HAW-CHD-001–009 | Draft design, indexed and link-checked |
| `codex/haw-hitl-design` | child design | HAW-HITL-001–012 | Draft design, indexed and link-checked |
| `codex/haw-events-design` | HITL design | HAW-EVT-001–009 | Draft design, indexed and link-checked |
| `codex/haw-backlog-design` | events design | HAW-BKL-001–009 | Draft design, indexed and link-checked |
| `codex/haw-child-admission` | backlog design | HAW-CHD-001 | Policy/schema/compiler checks and explicit runtime refusals; focused tests and `make verify-fast` pass |

These are local branches, not published PRs. Publication is currently blocked by
the session's remote-write approval policy. Prepared PR descriptions preserve the
intended bases. Add actual URLs here only after creation and attachment.

## Implementation status and next acceptance boundaries

### Child workflows — in progress

HAW-CHD-001 adds the optional DSL 3.1 preview `task.childWorkflows` policy, bounded
existing-Goober and registered-capability allowlists, default/max child counts,
and the upfront publication ceiling. Closed schema, CRD, DeepCopy and feature
catalog move with the Go type. Older dialects and deterministic task use refuse
it; omission preserves existing workflows.

Both execution backends still **refuse execution of opted-in workflows**. Local
start, resume, terminal resume and rerun reject before execution effects; a pinned
resume does not repair or mutate its journal. Temporal registry admission and the
direct workflow entry point also refuse. This is a deliberate incomplete-feature
boundary until HAW-CHD-002–009 connect validation, custody, child execution,
workspace reconciliation, waits and Portal intervention. It is not a claim that
an agent can start a child yet.

Focused evidence: workflow/interpreter tests, API/schema/config validation,
DeepCopy isolation, runtime refusal and resume/rerun regressions, authoring tests,
complexity gate, and `make verify-fast`. The complete `make ci` merge gate was attempted and stopped at `portal-audit`
because the environment could not resolve `registry.npmjs.org`; no full merge-gate
pass is claimed. Preceding dead-code, tidy, vet, policy, complexity, documentation,
and configuration-inventory checks passed. The complete gate remains required. The pinned controller generator produces unrelated invalid baseline
DeepCopy output under the current toolchain; only the relevant generated policy,
Task, and CRD additions are retained, with their behavior checked directly.

Next: finish and integrate proposal validation (HAW-CHD-002), same-transaction
lineage/start/cancellation custody (HAW-CHD-003), and workspace fork/reconciliation
(HAW-CHD-004). Helper tests alone do not complete those items: the agent-facing
production service and recovery path must exercise them before enabling children.

### Other streams — designs prepared, implementation pending

Human operations follows children, then events/queued starts/shared reads, then
backlog browsing/editing and visuals. Their complete acceptance tasks remain in
the respective design documents; no completion is implied by the local docs stack.

## Publication mapping

| Stable task range | Numbered issues | PR URLs |
|---|---|---|
| HAW-PGM-001 | Not created | Not published |
| HAW-CHD-001–009 | Not created | Not published |
| HAW-HITL-001–012 | Not created | Not published |
| HAW-EVT-001–009 | Not created | Not published |
| HAW-BKL-001–009 | Not created | Not published |
