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

| `codex/haw-accepted-child-recovery` | `codex/haw-host-recovery` | Restore a committed child acceptance before dispatching parent continuation |

| `codex/haw-event-host-routing` | `codex/haw-accepted-child-recovery` | Dispatch and recover pinned event consumers through ordinary scheduler ownership |

| `codex/haw-child-portal` | `codex/haw-event-host-routing` | Browse authorized child state, result acknowledgement and verified run links |

| `codex/haw-parallel-child-projection` | `codex/haw-child-portal` | Project independent branch waits and coordinate whole-run scheduler capacity |

| `codex/haw-event-causal-roots` | `codex/haw-parallel-child-projection` | Preserve bounded original event roots across chained consumers |

| `codex/haw-parallel-parent-custody` | `codex/haw-event-causal-roots` | Bind parent contracts and recovered writers to owned branch journals |

| `codex/haw-discard-settlement` | `codex/haw-parallel-parent-custody` | Settle verified child discard without mutable parent filesystem access |

| `codex/haw-child-repository-custody` | `codex/haw-discard-settlement` | Resolve production child repository URLs and verify both custody identities |

| `codex/haw-child-host-publication` | `codex/haw-child-repository-custody` | Publish delegated immutable child branches and PRs through host-owned effects |

| `codex/haw-event-publication` | `codex/haw-child-host-publication` | Publish typed workflow events through retained outbox and receipt custody |

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
Launcher, workspace adoption, recovery and terminal capture now share production
repository URL resolution when no test override exists. Custody compares the
provider's qualified repository key and the clone-URL digest independently;
neither field is accepted in place of the other. Focused command race tests and
the command growth/lint gates pass for this correction.

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
The acceptance-before-wait crash window now restores the original attempt
accounting and accepted child before any continuation worker. Terminal parent
recovery still requires its existing authorized continuation.

**The contained parent lane supports seeded writable parallel branches.**
Supported parents consist of opted-in repository agents with explicit Linux
image placement and a configured worker transport. A serial repository seed can
fan out into isolated branch workspaces and continue through a declared join. Actual runner-to-worker
composition passes with real Git workspaces and a simulated worker. This is not
a live Kubernetes or model qualification. Remaining delivery gates:

Serial stage handoff now preserves tracked edits and new files in the held
checkout. Successful completion archives the verified final contribution before
retiring that checkout, and the artifact remains materializable after cleanup.
Failed, escalated and aborted runs retain their contribution; family settlement
and journal pruning refuse missing archives or unresolved held workspaces.
Real Git runner, retirement and retention race tests pass. Parallel branches now
fork the seed contribution, retain separate outputs, and apply child results only
to their owning branch. Join `repoFrom` selects the latest executed eligible
declared producer; it does not merge every branch. All completed branch outputs
remain archived for explicit consumption. Starting directly with parallel
execution remains refused until a common base pin is defined.

- Exercise exact recovery against a live worker deployment. Parent and child host
  recovery now rejoin the retained worker, import its verified output and preserve
  the original checkout before allowing continuation. Child resume ownership is
  fenced against parent cancellation and terminal result capture. Combined race
  and real Git tests pass with simulated worker transport. See the
  [contained attempt contract](reference/contained-workflow-attempts.md).
- Canonical typed child push/PR stages now use host-only delegated credentials;
  child model pods receive no provider token. Immutable intents precede effects,
  concurrent PR admission uses compare-and-set, and uncertain create replies only
  reconcile the exact branch/base. Pending external effects pin the family after
  acknowledgement and cancellation. Real Git production runner tests, native
  GitHub/ADO fake HTTP tests, queue/credential races and lint pass. Complete the
  live provider/worker validation. The portal now shows publication states and
  checks uncertain effects using the gaggle’s explicit interactive identity.
  Checks require current run intervention and repository read access, exclusive
  stopped-run journal custody, exact retained provenance and bounded provider
  reads. Attributed audit records replay the same result after a lost response;
  confirmation neither restarts the child nor changes its immutable result.
- Owned discard now survives narrower workspace policy without importing or
  recapturing parent files. An unplanned conflict can use a revision-bound changed
  choice; already published plans remain immutable and require reconciliation.
- Branch-aware wait projection, execution clocks and the aggregate capacity
  coordinator now pass real scheduler race tests. Per-branch scheduling releases
  the branch lane during a child wait, keeps queued siblings runnable, and
  reacquires capacity before continuation. Recovery preserves the occurrence and
  retry allowance; a simulated 24-hour wait is excluded from the branch timeout.
  Contained factories and writable fork/join now pass real Git runner tests at
  concurrency one and two. Live worker qualification remains outstanding; gates
  and other unsupported contained shapes remain refused.
- Complete child workspace family holds through crash reaping and terminal
  disposition; active or unresolved families must survive unbounded waits.
- Check enclosing run/stage deadlines, revocation, human escalation and continuation
  together. A wait must not consume retry/repass allowance.
- Extend the current safe application boundary for file/directory replacements and
  case-only path renames, currently refused before filesystem effects.
- Parent/child read-only portal visibility is delivered with bounded pagination
  and explicit refresh. Complete the common HITL intervention path.
  The queue now retains up to eight human execution epochs per accepted child,
  with immutable prior results and an active-execution pointer. Concurrent
  restart/cancel/disposition races, exact replay, retained-byte limits and family
  cleanup are covered. The credential broker now selects current human bindings
  for a verified child epoch, keeps model pods restricted to model credentials,
  and cancels operations during policy reload without reversing lock order.
  The portal links the current execution and shows bounded restart history with
  human attribution; publication checks remain anchored to the original emitting
  execution. History reads omit restart-plan payloads. Runtime admission and full
  worker lifetime leases still gate enabling sealed-child restart.
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
simulated. The shared-session ledger and portal/API adapter are now implemented:
accepted messages retain exact human attribution, durable ordering and a queued
turn; the UI distinguishes queue acceptance, verified run links and confirmed
closure. Current gaggle permissions gate creation and messaging. Unknown command
outcomes retry the same key and content; access failures clear the visible
conversation. Session runtime installation remains outstanding and availability
stays false until that adapter is connected. See [shared sessions](reference/shared-sessions.md).
Backlog edits, PR repair, paused-stage fresh allowances, parallel restarts and
sealed-child continuation remain acceptance work. Saved guidance alone is
explicitly labeled as saved; it is not described as delivered or resumed.

### Events — pinned local consumers and typed publication implemented

Typed DSL 3.1 `publish-event` stages now publish through the local host with an
explicit `event:publish` capability and workflow/type publisher allowlist. The
host binds journal occurrence, run/config identity and original causal roots;
a bounded outbox retains the original envelope/plan before receipt admission.
Retry after a lost reply reuses that occurrence and receipt. Matched consumers
run through the existing durable router; no-match publication also completes.
Actual runner/outbox/consumer race tests, current revocation and failed-reload
checks pass. Verified completed/aborted producers now settle under exclusive registry and
journal custody. After the replay window, bounded maintenance compacts outboxes
into exact-request tombstones and frees capacity without re-executing effects.
Failed/escalated/interrupted producers retain their source groups and inputs for
continuation, including failures before the first event publication. Saturation,
migration, lost-acknowledgement and actual terminal-resume tests pass. Engine, remote,
child/human-continuation producers and self-subscriptions are explicitly refused
until their corresponding ancestry/transport contract exists.

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
rematching current configuration. Gaggle YAML now declares bounded same-gaggle
subscriptions and debounce policy. Host routing sweeps dispatch exact archived
local-runner consumers through ordinary scheduler budgets, claims, capacity and
shutdown ownership. Input manifests and journal lineage reconcile uncertain
starts without duplication; only verified terminal journals settle groups.
Dependency inventories protect source journals and archived configuration before
routing is installed. Public ingress, workflow emission, all-source normalization
and engine consumer transport remain pending. Trusted consumer ancestry now carries
up to 32 original roots across another event generation, survives source-history
pruning, and charges every root atomically. A new consumer RunID cannot reset those
limits. Child and human-continuation ancestry still require verified adapters.
Workflow root counters remain retained until
all producers/descendants can be proven settled. See
[receipt limits](reference/gaggle-event-receipts.md).

Child inspection now reads retained source pins without acquiring execution
authority. Cancellation, drain and observation follow the current child epoch and
refuse late results from the original run. Queued epoch cancellation retains its
own result. The final common human admission adapter and publication from later
child epochs remain explicit follow-up work.

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
