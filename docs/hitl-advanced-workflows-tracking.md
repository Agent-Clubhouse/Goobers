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
| `codex/haw-child-proposals` | child admission | HAW-CHD-002 foundation | Strict proposal parsing, pinned policy checks, normal compilation and runner placement; validator race tests pass |
| `codex/haw-child-authority` | child proposals | HAW-CHD-003 authority foundation | Separate signed stage credentials and exact route confinement; focused auth race tests and package lint pass |
| `codex/haw-child-lineage` | child authority | HAW-CHD-003 custody | Atomic lineage/start receipt/cancellation fence, slot and count limits, production retention; queue race tests pass |
| `codex/haw-child-workspace` | child lineage | HAW-CHD-004 mechanics | Filtered snapshots, isolated forks and merge/replace/discard preparation; Git integration tests pass |
| `codex/haw-child-validation-cli` | child workspace | HAW-CHD-002 author surface | Real `workflow validate-child` command, trusted configured parent selection, diagnostics and read-only command tests |
| `codex/haw-child-attempt-custody` | child validation CLI | HAW-CHD-003 revocation | Durable current-attempt binding, transactional acceptance fence, bounded retention; race tests and lint pass |
| `codex/haw-child-submission` | child attempt custody | HAW-CHD-002/003 service | Live trusted authority, repeated validation, atomic exact-source custody and status; integrated race tests pass |
| `codex/haw-child-http` | child submission | HAW-CHD-003 transport | Closed bounded request bodies, exact signed-origin routes and unavailable-service refusal; focused HTTP tests pass |
| `codex/haw-child-origin` | child HTTP | HAW-CHD-003 attempt identity | Atomic stage-start sequence binds occurrence and attempt across retry/recovery; journal and runner tests pass |
| `codex/haw-child-service-adapter` | child origin | HAW-CHD-003 service adapter | Real signed credential, router and SQLite submission path; in-process HTTP race tests pass |
| `codex/haw-child-run-identity` | child service adapter | HAW-CHD-003 execution identity | Closed journal lineage contract, stable child run identity and resume propagation; journal/runner tests pass |
| `codex/haw-child-mcp` | child run identity | HAW-CHD-002/003 agent tools | Conditional validate/start/status tools with safe file reads and private runtime credentials; MCP/harness tests and lint pass |
| `codex/haw-child-stage-grants` | child MCP | HAW-CHD-003 credential lifetime | Launcher grant acquisition, durable ownership/revocation and harness cleanup; focused race tests and lint pass |
| `codex/haw-child-dispatch` | child stage grants | HAW-CHD-003 execution custody | Typed dispatch, retained-source recompilation and journal reconciliation; queue recovery tests pass; launcher installation remains pending |

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

Proposal validation now checks exact source and canonical digests, the pinned
gaggle/catalog/policy, existing Goober and capability subsets, one plain manual
trigger, no generated-child recursion, normal compilation, provider requirements,
and executable placement. The durable submission path repeats those checks.
The author-facing CLI and its real command-path tests are implemented.

Stage authority uses a separate signed credential bound to gaggle, parent run,
stage occurrence, current attempt, and config/policy digests. It has no general
pod or human permissions and cannot fall back to human authentication. Enabling
its verifier is explicit. The service must still check current attempt/policy
authority before every operation; signatures alone do not admit execution.
Its 24-hour credential lifetime does not set a child-wait deadline: the launcher
must renew/rebind active work after a longer durable wait. Harness acquisition now
binds the grant and revokes it on session exit, including failure. Expired active
attempts can renew; revoked attempts cannot. Older cleanup cannot revoke a newer
attempt. Daemon service wiring is still pending.

Focused authorization tests cover cross-key and cross-token-domain use,
tampering, expiry, wrong parent, missing origin claims, and every existing API
route. The complete podauth/httpapi suites also contain socket-listener tests;
the sandbox refuses those listeners, so those complete suites have not passed.
Package lint found and fixed three initial policy formatting/comment issues;
the affected policy, engine, podauth and httpapi packages now report zero issues.

The submission service now requires a durable current-attempt binding as well as
the trusted runtime resolver. Supersession and revocation cannot race acceptance.
Exact source bytes are committed in the queue database with lineage/start custody;
retries verify retained bytes and cannot repair missing or tampered accepted
evidence. Its production pruner bounds maintenance and preserves unresolved work.
The ordinary trigger dispatcher recognizes child envelopes and leaves them queued
while child launch support is absent, preventing accidental named-catalog dispatch.

The authoring command validates proposals against a selected configured parent
without creating runs or writing proposal state. Its report explicitly identifies
current-config advisory validation. Runtime submission instead requires the
parent's immutable execution archive and current authority.

Workspace support captures filtered working state, carries a bounded delta from
the exact base commit, preserves child edits on retries, and prepares verified
merge/replace/discard trees. It does **not** yet apply those trees to the live
parent; durable application intent, exclusive custody and recovery remain required.

The HTTP adapter and scoped MCP tools now connect agent-authored source to signed,
bounded submission/status operations. Requests cannot supply another run, stage,
endpoint, credential or policy. MCP runtime credentials are private and omitted
from prompts and command arguments. Run identities preserve the accepted parent
occurrence and source/envelope pins. The dispatcher distinguishes known refusal
from uncertain handoff and reconciles durable journal evidence after reopening
the queue. Cancellation delivery alone does not imply terminal cancellation.

Next: install the journal-backed authority and pinned-generation loaders, daemon
services and child launcher; complete workspace custody/application, durable
waits, wait-aware timeouts/capacity, family cancellation and portal intervention.
Execution remains refused until those boundaries work together. Repository child
adoption and dynamic admission are in progress; parallel child workspace isolation
and branch-aware parent capacity remain required acceptance work.

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
