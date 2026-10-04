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

| `codex/haw-child-capacity` | `codex/haw-child-daemon` | HAW-CHD-003/005 | Scheduler race tests; normal budget and concurrency limits |
| `codex/haw-child-snapshot-custody` | `codex/haw-child-capacity` | HAW-CHD-004 | SQLite reopen, corruption, retention and Git integration tests |
| `codex/haw-child-runtime-acceptance` | `codex/haw-child-snapshot-custody` | HAW-CHD-003 | Configured archive-to-harness acceptance and concurrent reload race tests |
| `codex/haw-child-results` | `codex/haw-child-runtime-acceptance` | HAW-CHD-004 | Committed and dirty child changes retained against original fork; bounded family retention |
| `codex/haw-child-parent-suspension` | `codex/haw-child-results` | HAW-CHD-005 | Scheduler capacity and restart ownership race tests |
| `codex/haw-child-writer-custody` | `codex/haw-child-parent-suspension` | HAW-CHD-004/005 | Process-owner proof tests; sandbox verifies fail-closed Darwin inventory refusal |
| `codex/haw-child-disposition-apply` | `codex/haw-child-writer-custody` | HAW-CHD-004 | Git/race tests for merge, replace, discard, partial application, intervening edits and slot release |
| `codex/haw-child-disposition-tools` | `codex/haw-child-disposition-apply` | HAW-CHD-002/004 | Signed HTTP request-to-host-ack test, conditional MCP tools, Go/TS wire contract tests |
| `codex/haw-child-launcher` | `codex/haw-child-disposition-tools` | HAW-CHD-003/005 | Real queue-to-Runner publication, cancellation barrier and capacity race tests |
| `codex/haw-child-lifecycle` | `codex/haw-child-launcher` | HAW-CHD-003/004/005 | Journal-only crash recovery, cancellation, source pins, terminal result and registry custody tests |
| `codex/haw-interactive-policy` | `codex/haw-child-lifecycle` | HAW-HITL-001/002 | Per-gaggle authorization, verified groups, exact credential scope, reload fencing, daemon route and portal contract tests |
| `codex/haw-child-parent-continuation` | `codex/haw-interactive-policy` | HAW-CHD-005 | Serial runner wait/continuation and Git workspace recovery tests; adapter installation pending |

These are local branches, not published PRs. Publication is currently blocked by
the session's remote-write approval policy. Prepared PR descriptions preserve the
intended bases. Add actual URLs here only after creation and attachment.

## Implementation status and next acceptance boundaries

### Child workflows — in progress

The DSL 3.1 preview policy, strict generated-source validator and advisory CLI
are implemented. Stage-only signed grants are tied to actual journal occurrence
and attempt identities, immutable archived definitions and currently applied
permissions. The real local harness and HTTP service use that authority; reload
revokes changed-gaggle grants before publishing the new catalog. Source, start,
lineage and result custody share the bounded SQLite trigger database and existing
family retention sweep.

The typed child launcher now uses normal runner execution and scheduler capacity,
charged to the configured parent budget. A publication barrier commits exact
journal identity before stage effects; uncertain handoff is recovered from that
journal. Cancellation fences submissions and queued starts before signaling live
owners. Generated recovery uses retained source rather than a named catalog alias.

Workspace capture includes permitted staged, dirty and untracked work. Separate
child forks preserve both committed and uncommitted changes in the returned
result. Parent merge/replace/discard requests are durable and keep the occurrence
occupied. Under exclusive writer custody, the coordinator persists an application
plan, checks each path against its before/after state, applies atomic file changes,
stages only changed paths, verifies the result, and atomically acknowledges the
child. HEAD, unrelated staged entries and excluded credential/runtime paths remain
untouched. Repeated acknowledgement cannot reapply child work over later parent
progress. Discard never retracts an already-published PR.

The serial runner has an actual invocation handoff: stop/join writers, persist a
wait marker, retain the managed checkout, suspend concurrency without refunding
budget, wait, reacquire capacity and dispatch a fresh attempt in the same stage
occurrence. Previous context, transcript and lifetime usage remain available.
Recovery adopts the held checkout across a crash; watchdogs recognize child waits.

**General child opt-in execution remains refused.** Remaining delivery gates:

- Finish production daemon handoff/launcher installation and boot reconciliation.
- Verify explicit PR publication delegation against the actual credential surface.
- Support a child from each concurrent parent stage without releasing a live
  sibling's capacity; remove temporary serial/repository-only restrictions after
  tests cover branch workspaces, scratch work and join behavior.
- Complete child workspace family holds through crash reaping and terminal
  disposition; active or unresolved families must survive unbounded waits.
- Check enclosing run/stage deadlines, revocation, human escalation and continuation
  together. A wait must not consume retry/repass allowance.
- Extend the current safe application boundary for file/directory replacements and
  case-only path renames, currently refused before filesystem effects.
- Add parent/child portal visibility and the common HITL intervention path.
- Keep recursion deferred as agreed; one unresolved child per stage occurrence,
  including distinct parallel occurrences, remains the required v1 scope.

### HITL — authorization and credential slice implemented

Optional gaggle interactive policy defines explicit viewer/operator grants for
verified issuer/subject or group identities. Instance role checks still apply;
an unlisted instance administrator does not bypass gaggle policy. Named instance
credential sources bind exactly to backlog or repository targets and reuse current
GH/ADO authentication, secret stores and redaction. Automation credentials are
never an interactive fallback. Omission preserves authorized read-only monitoring.
Repository edits require PR publication policy.

The actual capabilities route distinguishes configured permission from implemented
operation availability. Current policy fences bounded authorization/provider effect
callbacks against reload. New actions are not advertised as usable until their
handlers exist. Shared portal approval/escalation/guidance actions are in progress;
fresh affected-stage allowances, settled-run continuation, transient sessions,
backlog edits and PR repair remain acceptance work. Saved guidance alone must not
be described as delivered or resumed.

### Events and backlog — designs prepared, implementation pending

The agreed event stream still requires all workflow starts to enter durable queues,
gaggle-local authenticated ingress/emit, configurable consumer debounce, causal
history and centralized bounded GH/ADO polling/reads. Existing child receipts are
one input to that broader queue work, not its completion.

The backlog workbench still requires browse/edit surfaces and consistent explicit
relations to items, PRs, dependencies and source-owned Markdown objectives in any
configured gaggle repository. Repository mutations use policy-governed PRs;
provider/repository truth stays external to Goobers. Organization precedes progress
tracking. See the respective designs for the complete stable task list.

### Validation and review preparation

Integrated fast validation (formatting, no-phone-home, vet and all command builds)
passes through the child continuation and interactive policy stack. Targeted Go
race suites, Git recovery/application tests, Go API contracts, TypeScript typecheck
and portal contract tests pass for the implemented slices. Full CI is not claimed:
package-audit DNS and existing socket-listener tests are restricted in this session.
Darwin process inventory is also restricted, so that integration fixture verifies
refusal and writer cleanup; successful native capture needs an unrestricted host.

Before publication, add the required unique command-growth justification to each
older affected review branch and recheck the stack bases. The legacy baseline file
must not be repinned for normal growth. Local draft descriptions and recovery
bundles are maintained separately; no remote PR exists yet.

## Publication mapping

| Stable task range | Numbered issues | PR URLs |
|---|---|---|
| HAW-PGM-001 | Not created | Not published |
| HAW-CHD-001–009 | Not created | Not published |
| HAW-HITL-001–012 | Not created | Not published |
| HAW-EVT-001–009 | Not created | Not published |
| HAW-BKL-001–009 | Not created | Not published |
