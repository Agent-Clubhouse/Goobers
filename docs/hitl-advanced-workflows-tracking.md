# Human operations and advanced workflows: review stack

This is the temporary work ledger for the [program](design/hitl-advanced-workflows-program.md).
Stable `HAW-*` task IDs are intentional. Create numbered epics/issues and backlinks
after the related design or item merges; do not invent closing references.

## Review branches

All work is unmerged. The integration branch is `codex/hitl-advanced-workflows`,
forked from main at `04198152b63d228a9714ae2f92a7dca079ba5213`.

| Head | Base | Slice |
|---|---|---|
| `codex/haw-program-design` | `codex/hitl-advanced-workflows` | Scope human operations and advanced workflows |
| `codex/haw-child-design` | `codex/haw-program-design` | Design agent-authored child workflows |
| `codex/haw-hitl-design` | `codex/haw-child-design` | Design interactive factory operations |
| `codex/haw-events-design` | `codex/haw-hitl-design` | Design gaggle events and durable start queues |
| `codex/haw-backlog-design` | `codex/haw-events-design` | Design source-owned backlog and objective workbench |
| `codex/haw-child-admission` | `codex/haw-backlog-design` | docs: track the local design and child admission stack |
| `codex/haw-child-proposals` | `codex/haw-child-admission` | feat(childworkflow): validate generated proposals against pinned parent policy |
| `codex/haw-child-authority` | `codex/haw-child-proposals` | feat(auth): isolate stage grants for child workflow operations |
| `codex/haw-child-lineage` | `codex/haw-child-authority` | refactor(triggerqueue): simplify child custody transactions |
| `codex/haw-child-workspace` | `codex/haw-child-lineage` | Add isolated child workspace snapshots and disposition preparation |
| `codex/haw-child-validation-cli` | `codex/haw-child-workspace` | feat(cli): validate child proposals against configured parent policy |
| `codex/haw-child-attempt-custody` | `codex/haw-child-validation-cli` | feat(triggerqueue): fence child acceptance by active stage grant |
| `codex/haw-child-submission` | `codex/haw-child-attempt-custody` | docs: record child custody and implementation review stack |
| `codex/haw-child-http` | `codex/haw-child-submission` | refactor(recovery): remove obsolete snapshot wrappers |
| `codex/haw-child-origin` | `codex/haw-child-http` | Bind child workflow origins to durable stage attempts |
| `codex/haw-child-service-adapter` | `codex/haw-child-origin` | feat(childworkflow): connect signed HTTP operations to durable custody |
| `codex/haw-child-run-identity` | `codex/haw-child-service-adapter` | feat(journal): pin generated child invocation lineage |
| `codex/haw-child-mcp` | `codex/haw-child-run-identity` | feat(mcpio): expose scoped child workflow tools to opted-in stages |
| `codex/haw-child-stage-grants` | `codex/haw-child-mcp` | feat(childworkflow): bind stage grants to harness invocation lifetime |
| `codex/haw-child-dispatch` | `codex/haw-child-stage-grants` | feat(queue): add typed child dispatch and journal reconciliation |
| `codex/haw-child-parent-budget` | `codex/haw-child-dispatch` | feat(childworkflow): pin parent workflow admission identity |
| `codex/haw-child-workspace-adoption` | `codex/haw-child-parent-budget` | runner: adopt trusted child forks without resetting workspace state |
| `codex/haw-child-journal-authority` | `codex/haw-child-workspace-adoption` | feat(childworkflow): expose pinned effective policy digest |
| `codex/haw-child-pinned-admission` | `codex/haw-child-journal-authority` | feat(childworkflow): load pinned stage authority from applied configuration |
| `codex/haw-child-daemon` | `codex/haw-child-pinned-admission` | feat(childworkflow): wire daemon authority and policy revocation |
| `codex/haw-child-capacity` | `codex/haw-child-daemon` | Reserve generated children against parent budgets |
| `codex/haw-child-snapshot-custody` | `codex/haw-child-capacity` | Retain bounded parent snapshots for isolated launch |
| `codex/haw-child-runtime-acceptance` | `codex/haw-child-snapshot-custody` | Exercise actual harness authority and reload fencing |
| `codex/haw-child-results` | `codex/haw-child-runtime-acceptance` | Retain verified terminal workspace results |
| `codex/haw-child-parent-suspension` | `codex/haw-child-results` | Suspend waiting parent concurrency without refunding budgets |
| `codex/haw-child-writer-custody` | `codex/haw-child-parent-suspension` | Require writer termination evidence before handoff |
| `codex/haw-child-disposition-apply` | `codex/haw-child-writer-custody` | Persist and recover parent workspace dispositions |
| `codex/haw-child-disposition-tools` | `codex/haw-child-disposition-apply` | Expose durable child disposition requests |
| `codex/haw-child-launcher` | `codex/haw-child-disposition-tools` | Launch queued children through pinned runtime and shared admission |
| `codex/haw-child-lifecycle` | `codex/haw-child-launcher` | Reconcile family custody, retained recovery and terminal results |
| `codex/haw-interactive-policy` | `codex/haw-child-lifecycle` | Add explicit gaggle human policy and credential selection |
| `codex/haw-child-parent-continuation` | `codex/haw-interactive-policy` | Retain durable child waits and resume exact parent custody |
| `codex/haw-stack-review-status` | `codex/haw-child-parent-continuation` | docs: record child continuation and interactive policy review progress |
| `codex/haw-child-storage-reservations` | `codex/haw-stack-review-status` | Reserve completion storage when accepting a generated child |
| `codex/haw-child-startup-suspension` | `codex/haw-child-storage-reservations` | Restore parked parents without holding their child execution slot |
| `codex/haw-child-recovery-deferral` | `codex/haw-child-startup-suspension` | Keep refused generated recovery actionable without blocking daemon startup |
| `codex/haw-interactive-interventions` | `codex/haw-child-recovery-deferral` | Add authorized run decisions and shared guidance in the portal |
| `codex/haw-event-receipts` | `codex/haw-interactive-interventions` | Retain scoped event receipts with replay and bounded maintenance |
| `codex/haw-child-handoff-adapter` | `codex/haw-event-receipts` | Wire child handoff to daemon workspace custody and capacity |
| `codex/haw-child-shared-wait-projection` | `codex/haw-child-handoff-adapter` | Share durable child wait projection without scheduler runner import cycle |
| `codex/haw-child-process-evidence` | `codex/haw-child-shared-wait-projection` | Retain process join evidence for generated workspace writers |
| `codex/haw-integrated-progress` | `codex/haw-child-process-evidence` | Track integrated human actions and event receipt progress |
| `codex/haw-restart-source-access` | `codex/haw-integrated-progress` | Fence restart source checks under explicit interactive identity |
| `codex/haw-child-credential-ceiling` | `codex/haw-restart-source-access` | Bind child credential delegation to accepted source and current policy |
| `codex/haw-child-disposition-recovery` | `codex/haw-child-credential-ceiling` | Recover child disposition requests with exact revision checks |
| `codex/haw-stage-restart` | `codex/haw-child-disposition-recovery` | Restore affected stages in immutable human restart epochs |
| `codex/haw-child-production-launcher` | `codex/haw-stage-restart` | Install daemon child launcher and isolated execution driver |
| `codex/haw-restart-authority` | `codex/haw-child-production-launcher` | Retain authenticated human identity with restart epochs |
| `codex/haw-restart-admission` | `codex/haw-restart-authority` | Admit human restarts with current source policy |
| `codex/haw-child-scoped-blobs` | `codex/haw-restart-admission` | Bound generated child artifact custody |
| `codex/haw-child-isolated-pod` | `codex/haw-child-scoped-blobs` | Execute isolated child stages and return verified trees |
| `codex/haw-child-retained-kit` | `codex/haw-child-isolated-pod` | Compose generated execution kits from retained source |
| `codex/haw-child-blob-plane` | `codex/haw-child-retained-kit` | Scope pod artifact access to authenticated child lineage |
| `codex/haw-child-cancellation` | `codex/haw-child-blob-plane` | Preserve isolated child workspace custody through cancellation |
| `codex/haw-restart-recovery` | `codex/haw-child-cancellation` | Recover human restart epochs with current interactive authority |
| `codex/haw-contained-pod-tokens` | `codex/haw-restart-recovery` | Separate contained pod authority from ordinary run tokens |
| `codex/haw-interactive-execution` | `codex/haw-contained-pod-tokens` | Execute and recover human restarts with interactive credentials |
| `codex/haw-parent-authority` | `codex/haw-interactive-execution` | Bind contained parent authority and execution custody |
| `codex/haw-contained-launcher` | `codex/haw-parent-authority` | Install contained child factories and recover parent secret delivery |
| `codex/haw-contained-recovery` | `codex/haw-contained-launcher` | Contained serial parent routing, exact child attempt authority, and durable replacement refusal |
| `codex/haw-event-subscriptions` | `codex/haw-contained-recovery` | Pin bounded subscriptions and transactionally route debounce groups into durable starts |
| `codex/haw-worker-reconciliation` | `codex/haw-event-subscriptions` | Retain exact worker attempts, rejoin without launch, and replay the original workspace application plan |
| `codex/haw-event-configuration` | `codex/haw-worker-reconciliation` | Declare gaggle subscriptions and compile immutable same-gaggle target revisions |

| `codex/haw-host-recovery` | `codex/haw-event-configuration` | Recover exact parent and child worker custody before resuming retained work |

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

**The contained serial parent lane is connected; unsupported shapes remain refused.**
The first supported parent consists of opted-in repository agents with explicit
Linux image placement and a configured worker transport. Actual runner-to-worker
composition passes with real Git workspaces and a simulated worker. This is not
a live Kubernetes or model qualification. Remaining delivery gates:

- Exercise exact recovery against a live worker deployment. Parent and child host
  recovery now rejoin the retained worker, import its verified output and preserve
  the original checkout before allowing continuation. Child resume ownership is
  fenced against parent cancellation and terminal result capture. Combined race
  and real Git tests pass with simulated worker transport. See the
  [contained attempt contract](reference/contained-workflow-attempts.md).
- Verify explicit PR publication delegation against the actual credential surface,
  including ambient credentials and model credentials that also authorize GitHub.
- Allow safe disposal after execution policy narrows and changing a conflicted
  merge request before application effects begin; preserve immutable applied plans.
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

### HITL — authorized run decisions and shared guidance implemented

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
handlers exist. The shared portal now exposes sequence-bound approval, override
and denial decisions plus saved shared guidance, attributed to the authenticated
human. Real daemon-to-runner and portal-client tests cover those actions.
The daemon now installs a dedicated interactive restart driver for supported local
DSL 3.1 agent/reviewer stages. A restart uses the selected saved guidance, a new
linked execution, fresh affected-stage retry/repass allowances and independently
selected model/code/backlog credentials. Recovery reuses that epoch and rechecks
current policy; ordinary automation runners cannot execute it. Composed HTTP-to-
runner acceptance covers retries, recovery, duplicate requests, redaction and
source-journal preservation with only provider transport and model execution
simulated. Transient sessions, backlog edits, PR repair, paused-stage fresh
allowances, parallel restarts and sealed-child continuation remain acceptance work. Saved guidance alone is explicitly labeled
as saved; it is not described as delivered or resumed.

### Events — receipt and routing store implemented; host integration pending

The agreed event stream still requires all workflow starts to enter durable queues,
gaggle-local authenticated ingress/emit, configurable consumer debounce, causal
history and centralized bounded GH/ADO polling/reads. Existing child receipts are
one input to that broader queue work, not its completion.

Internal event receipt custody now uses bounded CloudEvents JSON, explicit gaggle
and authenticated producer binding, immutable routing snapshots, no-match success,
shared byte reservations and daemon retention. Restart retries, conflicting
payloads/actors, concurrent duplicates, scope isolation, corrupt custody and steady
state cleanup are covered. The routing store now atomically transfers reservations
into independent `all`/`latest` groups and typed pinned starts, with crash/concurrent
timer tests, immutable membership, bounded dependency inventories and a persistent
100-start root-chain budget. Migration refuses missing historical pins rather than
rematching current configuration. Host ingress, routing sweeps, pinned execution,
terminal settlement and dependency-aware host pruning remain pending; these APIs
do not yet enable consumer execution. Workflow root counters remain retained until
all producers/descendants can be proven settled. See
[receipt limits](reference/gaggle-event-receipts.md).

### Backlog — design prepared, implementation pending

The backlog workbench still requires browse/edit surfaces and consistent explicit
relations to items, PRs, dependencies and source-owned Markdown objectives in any
configured gaggle repository. Repository mutations use policy-governed PRs;
provider/repository truth stays external to Goobers. Organization precedes progress
tracking. See the respective designs for the complete stable task list.

### Validation and review preparation

Integrated fast validation (formatting, no-phone-home, vet and all command builds)
passes through the earlier child continuation and interactive policy stack. Later
integrated child wait/process/daemon and event receipt race checks also pass. Targeted Go
race suites, Git recovery/application tests, Go API contracts, TypeScript typecheck
and portal contract tests pass for the implemented slices. Full CI is not claimed:
package-audit DNS and existing socket-listener tests are restricted in this session.
Darwin process inventory is also restricted, so that integration fixture verifies
refusal and writer cleanup; successful native capture needs an unrestricted host.

The earlier branches have been restacked with unique command-growth declarations;
the first 58 review gates passed. New slices include their own declarations and
are checked against their intended bases. The complexity gate remains at the
existing 185-function budget; its baseline has not been widened. Local draft descriptions and recovery
bundles are maintained separately; no remote PR exists yet.

## Publication mapping

| Stable task range | Numbered issues | PR URLs |
|---|---|---|
| HAW-PGM-001 | Not created | Not published |
| HAW-CHD-001–009 | Not created | Not published |
| HAW-HITL-001–012 | Not created | Not published |
| HAW-EVT-001–009 | Not created | Not published |
| HAW-BKL-001–009 | Not created | Not published |
