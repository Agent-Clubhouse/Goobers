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
| `codex/haw-contained-recovery` | `codex/haw-contained-launcher` | Connect contained parent execution and exact child observation authority |
| `codex/haw-event-subscriptions` | `codex/haw-contained-recovery` | Match event subscriptions and persist debounce start custody |
| `codex/haw-worker-reconciliation` | `codex/haw-event-subscriptions` | Reconcile retained workers without replacement execution |
| `codex/haw-event-configuration` | `codex/haw-worker-reconciliation` | Configure gaggle-local event consumers and debounce policies |
| `codex/haw-host-recovery` | `codex/haw-event-configuration` | Recover exact parent and child worker custody before resuming retained work |
| `codex/haw-accepted-child-recovery` | `codex/haw-host-recovery` | Restore accepted child waits before parent continuation |
| `codex/haw-event-host-routing` | `codex/haw-accepted-child-recovery` | Dispatch pinned event consumers with durable input and recovery custody |
| `codex/haw-child-portal` | `codex/haw-event-host-routing` | Show child workflow custody and verified run links in the portal |
| `codex/haw-parallel-child-projection` | `codex/haw-child-portal` | Coordinate branch child waits without surrendering runnable siblings capacity |
| `codex/haw-event-causal-roots` | `codex/haw-parallel-child-projection` | Preserve original event chain limits across grouped consumers |
| `codex/haw-parallel-parent-custody` | `codex/haw-event-causal-roots` | Bind contained parent custody to owned branch journals |
| `codex/haw-discard-settlement` | `codex/haw-parallel-parent-custody` | Settle child discard without mutable parent checkout access |
| `codex/haw-child-repository-custody` | `codex/haw-discard-settlement` | Fix production child repository custody wiring |
| `codex/haw-child-host-publication` | `codex/haw-child-repository-custody` | Publish delegated child branches and PRs through durable host effects |
| `codex/haw-event-publication` | `codex/haw-child-host-publication` | Publish typed workflow events through durable outbox receipts |
| `codex/haw-parent-contribution` | `codex/haw-event-publication` | Preserve contained parent contributions across serial stages |
| `codex/haw-event-producer-retention` | `codex/haw-parent-contribution` | Fence and compact completed event publication outboxes |
| `codex/haw-parent-parallel-lanes` | `codex/haw-event-producer-retention` | Release parallel branch slots while waiting for child workflows |
| `codex/haw-child-publication-checks` | `codex/haw-parent-parallel-lanes` | Review uncertain child publication through the human portal |
| `codex/haw-child-restart-custody` | `codex/haw-child-publication-checks` | Preserve child results across bounded human restart epochs |
| `codex/haw-restart-canonical-ids` | `codex/haw-child-restart-custody` | Use canonical human restart IDs without duplicating legacy runs |
| `codex/haw-child-interactive-credentials` | `codex/haw-restart-canonical-ids` | Bind child restart effects to the gaggle interactive identity |
| `codex/haw-child-execution-history` | `codex/haw-child-interactive-credentials` | Show child execution history with attributed human restarts |
| `codex/haw-child-epoch-workspaces` | `codex/haw-child-execution-history` | Fork human child restarts from sealed prior work |
| `codex/haw-parent-parallel-workspaces` | `codex/haw-child-epoch-workspaces` | feat: isolate writable parallel parent contributions |
| `codex/haw-ordinary-start-queue` | `codex/haw-parent-parallel-workspaces` | feat: queue daemon ordinary starts with pinned targets |
| `codex/haw-child-restart-core` | `codex/haw-ordinary-start-queue` | feat: reuse common restart core for contained child epochs |
| `codex/haw-session-ledger` | `codex/haw-child-restart-core` | feat: persist shared sessions and ordered turn custody |
| `codex/haw-child-epoch-runtime` | `codex/haw-session-ledger` | feat: bind child restart runtime and retained workspace custody |
| `codex/haw-session-portal` | `codex/haw-child-epoch-runtime` | feat: add shared session portal and human command API |
| `codex/haw-scheduled-signal-starts` | `codex/haw-session-portal` | feat: durably queue scheduled, webhook and named signal starts |
| `codex/haw-child-epoch-observation` | `codex/haw-scheduled-signal-starts` | Follow child execution epochs without reviving source authority |
| `codex/haw-scoped-provider-read-integration` | `codex/haw-child-epoch-observation` | Partition shared provider reads by gaggle and credential policy |
| `codex/haw-session-provenance` | `codex/haw-scoped-provider-read-integration` | Pin shared-session context and real execution provenance |
| `codex/haw-queued-worker-occurrences` | `codex/haw-session-provenance` | Queue backlog and refill workers with shared pending capacity |
| `codex/haw-child-human-admission` | `codex/haw-queued-worker-occurrences` | Admit human child restarts through the durable common execution path |
| `codex/haw-session-coordinator` | `codex/haw-child-human-admission` | Coordinate shared-session turns and durable execution custody |
| `codex/haw-provider-read-scope-fence` | `codex/haw-session-coordinator` | Fence provider caches to trusted gaggle execution scope |
| `codex/haw-workbench-metadata` | `codex/haw-provider-read-scope-fence` | feat(workbench): define source-owned objective and relationship contracts |
| `codex/haw-session-native-runtime` | `codex/haw-workbench-metadata` | feat(sessions): execute shared turns through native human runtime |
| `codex/haw-child-epoch-publication` | `codex/haw-session-native-runtime` | feat(children): publish first PR from verified human execution epoch |
| `codex/haw-demand-schedule-starts` | `codex/haw-child-epoch-publication` | feat(queue): preserve pinned demand schedule obligations |
| `codex/haw-workbench-source-configuration` | `codex/haw-demand-schedule-starts` | feat(workbench): configure explicit planning sources and read authority |
| `codex/haw-workbench-provider-reads` | `codex/haw-workbench-source-configuration` | feat(workbench): project bounded native backlog items and relationships |
| `codex/haw-workbench-native-field-edits` | `codex/haw-workbench-provider-reads` | feat(workbench): add guarded native backlog field edits |
| `codex/haw-workbench-authorized-reads` | `codex/haw-workbench-native-field-edits` | feat(workbench): share authorized portal and session source reads |
| `codex/haw-session-backlog-tools` | `codex/haw-workbench-authorized-reads` | feat(sessions): add authorized and attributed backlog read tools |
| `codex/haw-workbench-browser` | `codex/haw-session-backlog-tools` | Browse configured backlog sources and share authorized readers with native sessions |
| `codex/haw-standalone-queued-starts` | `codex/haw-workbench-browser` | Queue standalone and detached workflow starts before dispatch |
| `codex/haw-repository-source-readers` | `codex/haw-standalone-queued-starts` | Read authorized repository objectives and relationship manifests at verified commits |
| `codex/haw-native-edit-custody` | `codex/haw-repository-source-readers` | Retain durable one-attempt native backlog edit custody and receipts |
| `codex/haw-direct-engine-queued-starts` | `codex/haw-native-edit-custody` | Queue exact direct engine starts and reconcile uncertain provider outcomes |
| `codex/haw-source-graph-foundation` | `codex/haw-direct-engine-queued-starts` | Project source-owned objectives and relationships with explicit conflicts and coverage |
| `codex/haw-repository-document-browser` | `codex/haw-source-graph-foundation` | Browse configured repository objectives and relationship manifests in the portal |
| `codex/haw-authorized-source-graph` | `codex/haw-repository-document-browser` | Browse authorized objective relationships with explicit coverage and conflicts |
| `codex/haw-native-backlog-editor` | `codex/haw-authorized-source-graph` | Edit native backlog fields with current authority and durable command receipts |
| `codex/haw-ado-shared-read-plans` | `codex/haw-native-backlog-editor` | Share scoped ADO query and hydration reads without caching mutations |
| `codex/haw-pending-start-fairness` | `codex/haw-ado-shared-read-plans` | Prevent held start batches from starving later eligible work |
| `codex/haw-intervention-view-scope` | `codex/haw-pending-start-fairness` | Keep human intervention actions scoped to the current run and queued restart |
| `codex/haw-session-backlog-edits` | `codex/haw-intervention-view-scope` | Enable audited backlog edits from shared agent sessions |
| `codex/haw-queued-human-restarts` | `codex/haw-session-backlog-edits` | Queue human restart epochs before execution capacity admission |
| `codex/haw-delivery-checkpoints` | `codex/haw-queued-human-restarts` | Reconcile delivery checkpoints and feature catalog snapshots |

## Implementation status and next acceptance boundaries

### Child workflows — implemented locally; live qualification remains

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
a live Kubernetes or model qualification. The following checkpoints distinguish
implemented custody from remaining qualification and unsupported shapes:

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
- Family retention and terminal disposition preserve active or unresolved child
  custody through long waits; live crash/reaping qualification remains required.
- Live qualification must exercise enclosing deadlines, revocation, human
  escalation and continuation together. Simulated waits preserve attempt allowances.
- File/directory replacement and case-only rename support are future extensions;
  those dispositions currently refuse before filesystem effects.
- Parent/child read-only portal visibility is delivered with bounded pagination
  and explicit refresh. The common HITL intervention path is installed.
  The queue now retains up to eight human execution epochs per accepted child,
  with immutable prior results and an active-execution pointer. Concurrent
  restart/cancel/disposition races, exact replay, retained-byte limits and family
  cleanup are covered. The credential broker now selects current human bindings
  for a verified child epoch, keeps model pods restricted to model credentials,
  and cancels operations during policy reload without reversing lock order.
  The portal links the current execution and shows bounded restart history with
  human attribution; publication checks remain anchored to the original emitting
  execution. History reads omit restart-plan payloads. Current-policy admission and worker lifetime custody gate each sealed-child
  restart. Their simulated/real-Git acceptance checks pass; live worker
  qualification remains outstanding.
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
conversation. The native daemon runtime is now installed with pinned model profiles, live human
leases and writer join evidence. Source operations remain separately gated. See [shared sessions](reference/shared-sessions.md).
Manual and typed-session native backlog field edits are installed with one-attempt
command receipts. Shared sessions now expose installed typed needs-human inspection,
resolution and receipt tools under actual initiating-human authority. The host checks
the current label-edit policy, inspected source revision, dependency coverage and
verified evidence, then retains the assessment and one-attempt marker-removal receipt.
The backlog portal now opens this assessment in a new or existing shared session,
retains separate creation/message retry keys, and shows actual agent responses.
Current access and exact source identity fence every browser scope change. PR repair
remains follow-up delivery. The installed human restart
adapters cover affected-stage fresh allowances, queued capacity and sealed-child
continuation; live worker qualification remains outstanding. Saved guidance alone is
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

Durable admission is installed for manual, scheduled, demand-sized, signal,
standalone/detached, direct-engine, child, shared-session and human restart starts.
Consumer debounce, workflow event emission and explicit authenticated external
CloudEvents ingress are installed. Queue inspection/cancellation/deadline policy
and broader transport parity remain distinct completion/qualification work.

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
routing is installed. Explicit machine ingress uses the existing listener and durable receipt commit;
human receipt visibility requires current gaggle authority. Queue lifecycle
controls and engine consumer event-input transport remain pending. Trusted consumer ancestry now carries
up to 32 original roots across another event generation, survives source-history
pruning, and charges every root atomically. A new consumer RunID cannot reset those
limits. Child and human-continuation ancestry still require verified adapters.
Workflow root counters remain retained until
all producers/descendants can be proven settled. See
[receipt limits](reference/gaggle-event-receipts.md).

Child inspection now reads retained source pins without acquiring execution
authority. Cancellation, drain and observation follow the current child epoch and
refuse late results from the original run. Queued epoch cancellation retains its
own result. The common human admission adapter now queues and restarts the exact affected
child stage, with saved guidance, current authority, cancellation and capacity
fences. The first publication may now originate in a human child epoch; the publication
keeps its emitting run identity. Revising an already accepted branch/PR intent
from a subsequent epoch remains a separate capability.

Shared provider reads now have an explicit gaggle/binding/generation partition,
full request-representation isolation, bounded body capture, and an ADO GET
transport adapter. Stage reads require an explicit host-supplied automation scope; human and unknown
launches bypass cache reuse. Daemon counter/open-PR callers now use gaggle and
configuration partitions, including ADO GETs. Typed ADO WIQL/batch plans now share
identical live reads and explicit evaluation snapshots without caching arbitrary
POSTs or mutations. Interactive sequential refreshes remain live. Unsupported ADO
demand counters remain unsupported; no claim of complete shared polling is made.

### Backlog — source browsing, relationships and native field edits implemented

The source metadata foundation now validates stable gaggle-qualified references,
objective frontmatter, relationship manifests and explicit ownership. Candidate
Markdown edits preserve body bytes and unrelated metadata; these parsers perform
no provider access or writes. See `reference/workbench-source-metadata.md`.
Explicit `spec.workbench` bindings now resolve only the singleton backlog or
configured project/additional repository files. Source selection and interactive
credentials share one current-policy boundary; session source reads can reuse a
live lease without blocking its revocation. Native backlog read routes and portal
browsing now show source-owned items, objective classification, associated native
items and relationship coverage. Exact-credential repository readers return bounded
commit-pinned Markdown objectives and relationship manifests. Repository read
routes and portal file browsing now expose source provenance, explicit objective
relationships and aliases. The installed native edit service and portal controls allow permitted field edits
with exact source/revision checks and retained command receipts.

The pure graph foundation now composes trusted source pages with verified target
and commit pins, explicit authored/native edges, unresolved targets and visible
ownership/identity conflicts. It retains no planning database and computes no
progress. The authorized aggregate route and portal relationship map are now installed.
Reads are bounded to one window per configured source; conflicts, omitted reads and
unresolved links remain explicit. This does not claim a complete backlog graph or
progress tracking.
Durable native edit commands now run through the installed write service and
portal editor. Each command is attempted at most once; lost responses remain
unknown and can be inspected without resending the write. Settled receipts are
pruned by bounded daemon maintenance; unresolved custody never expires by age.

Bounded metadata previews, source-bound GitHub/ADO draft-PR phase adapters and
durable custody are installed through the daemon API. Every phase checks current
interactive source authority; receipt reads and explicit observations require read
authority and cannot repeat a write. Lost replies retain the original command key
and phase history. The installed portal editor supports inert reviewed diffs,
explicit draft-PR submission, same-key recovery, separate provider observation and
explicit continuation, plus retained command lookup after a browser reload.

The versioned, bounded relationship-suggestion artifact now binds actual producer
provenance, deduplicates source evidence and holds provisional item references
until their exact creation receipt exists. Its review storage/portal/command
installation remains outstanding; suggestions do not become graph edges.

Remaining workbench delivery includes governed repository metadata PRs, supported
native relationship edits and creation/curation relationship suggestions. Sources
may reside in any configured gaggle repository. Repository mutations use policy-governed PRs;
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


### Native shared session delivery

The daemon now installs the pinned model-only session runtime and shared turn
coordinator, restores all unsettled session custody before admission, and retains
journals/configuration while writers remain uncertain. Actual HTTP-to-Runner
adapter tests cover accepted turns, publication refusal and unknown writers.
Native backlog read, field-edit and needs-human resolution tools now use the same
installed human-authorized services as browser operations. Availability requires
the configured source policy and installed session runtime. Source read/view requests
also include policy-lock contention in their request deadline. PR repair tools and
live model/worker qualification remain follow-up.


### Durable demand-sized schedule delivery

Due schedule observations now persist a pinned demand obligation and advance the
cursor before polling. The first count is sealed once; bounded worker starts then
transfer transactionally through shared queued/live capacity without recounting
or rebinding after a restart. Capacity release wakes the remaining obligation.
Existing priority, quota and transient fallback behavior is preserved. Unresolved
obligations retain source generations; disabling a source does not silently delete
them. Standalone/manual/detached starts now use the pinned ordinary queue and
preserve request identity through detached execution. Direct-engine admission also
retains the exact canonical input in the same queue database before attempting a
start. Its separate daemon recovery cursor verifies the actual first history event
with the configured codec and target; missing history never permits resending an
uncertain effect. Live Temporal qualification remains outstanding.

### Ordinary human restart queue normalization

HAW-EVT-002 now covers ordinary human affected-stage continuations as well as
existing child epochs. The typed shared receipt retains exact context before
capacity, source-occurrence uniqueness, current-policy dispatch, and source/config
retention. Exact command replay precedes mutable source preparation. A failed
publication barrier starts no runner; uncertain post-barrier absence cannot
silently authorize another launch. The composed HTTP acceptance test drives the
queued receipt through the actual dedicated human Runner.Resume and verifies
fresh stage allowance, selected scrubbed guidance, pinned identity and recovery.
Uncertain no-journal repair still requires explicit writer-ownership proof.
