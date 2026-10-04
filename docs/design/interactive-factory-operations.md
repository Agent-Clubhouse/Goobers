# Design: Interactive factory operations

> Status: draft — implementation contract for the human operations surface
> Area: Portal, runtime, providers, authorization
> Verified: 04198152b63d228a9714ae2f92a7dca079ba5213 (2026-10-03)

Program: [HITL and advanced workflows](hitl-advanced-workflows-program.md).
Companions: [child workflows](agent-authored-child-workflows.md),
`gaggle-events-and-durable-start-queues.md`,
and `source-owned-backlog-workbench.md`.
Task identifiers below are stable local planning identifiers; publication of numbered
issues and epics follows merged designs and implementation-task documents.

## 1. Outcome and accepted decisions

An operator can inspect an issue, PR, or blocked run; understand the required decision;
edit its source, converse with an agent, provide guidance, and observe confirmed recovery.
The same operations are available to authorized agents through typed daemon operations.

- Sessions belong to a gaggle, are shared with its authorized users, and continue after
  the initiating browser disconnects. Every human message retains its own attribution.
- V1 permits multiple authorized users to contribute sequentially to the same session.
  Presence, simultaneous conversation presentation, and full group-chat UX are future work.
- An optional gaggle interactive-access block selects provider credentials, allowed
  actions/targets, and human viewer/operator grants. Omission disables interactive writes
  and session creation; existing read-only monitoring remains available under its policy.
- Provider operations use the configured gaggle interactive execution credentials.
  Goobers records the initiating human; the browser never supplies a provider token.
- A Goober profile or additional instructions may customize interactive assistance.
  Neither configuration nor conversation text implicitly grants additional authority.
- Authorized edits may update backlog items directly. Repository changes are proposed
  through PRs and follow the configured review/merge policy in the backlog-workbench design.
- Agents may assess an answered needs-human request and clear it when the recorded
  decision is satisfied. They must explain the assessment and verify the resulting state.
- Restarting an affected stage includes prior context plus new guidance and restores
  its retry/repass allowance. Earlier attempts and decisions remain immutable history.
- Child workflows use the same attention and intervention experience. V1 supports a
  parent iterating over children; recursive workflow creation is outside this release.

## 2. Original baseline and remaining gaps

The table records the original main-branch baseline above. It is historical input
to this design, not the current local delivery status. The stack now installs
per-gaggle human access, sequence-bound decisions, queued stage restarts, shared
native agent sessions, source browsing and audited manual/session field edits.
Needs-human resolution, selected-PR repair and post-turn receipt inspection are
also installed in the local daemon, with current source authority and bounded effects.
See the [delivery ledger](../hitl-advanced-workflows-tracking.md). No deployed
end-to-end qualification is claimed.

| Existing seam | What ships / what this design adds |
| --- | --- |
| [`internal/httpapi/mutations.go`](../../internal/httpapi/mutations.go) | Durable admission for approve/override/rerun, idempotency, actor stamping; add the complete portal experience and new restart semantics. |
| [`internal/engine/hitl.go`](../../internal/engine/hitl.go) | Opt-in terminal intervention, generation checks and a default 24-hour hold; add explicit long/unbounded waiting and child coordination without changing old histories. |
| [`internal/engine/registryrefusal.go`](../../internal/engine/registryrefusal.go) | Temporal admission rejects explicit human gates; occurrence-bound human decisions must be implemented before advertising that capability. |
| [`cmd/goobers/run_continue.go`](../../cmd/goobers/run_continue.go) | `run continue` creates a distinct journal and does not execute stages; executable continuation and parent rebinding are required where an old execution is settled. |
| [`api/v1alpha1/operator_message.go`](../../api/v1alpha1/operator_message.go) | Bounded durable messages, acknowledgements and outcomes; reuse for run input and add durable session conversation. |
| [`internal/engine/operator_message.go`](../../internal/engine/operator_message.go) | Temporal stores bounded receipt references; content and credentials remain outside workflow history. Preserve that boundary. |
| [`portal/src/App.tsx`](../../portal/src/App.tsx) | Approve/override/rerun UI handlers remain stubs; capability presence alone must not imply an implemented action. |
| [`internal/readservice/workitems.go`](../../internal/readservice/workitems.go) | Recorded provider-action history and related PRs; full provider-backed backlog browsing is owned by the workbench design. |
| [`internal/httpapi/router.go`](../../internal/httpapi/router.go) | Human roles are instance-scoped; add gaggle viewer/operator authorization. |
| [`api/v1alpha1/common.go`](../../api/v1alpha1/common.go) | Existing `connectionRef` does not select runtime credentials; implement selection before exposing an interactive credential reference. |

Keep compatible parts of [Human-in-the-loop](human-in-the-loop.md). Its historical
descriptions of absent backend intervention must not be copied as current deficiencies.
Relevant backlog includes #1983, #3772, #5191, #5192, #5253, #2973 and #6468;
closed foundational items are evidence to inspect, not substitutes for acceptance tests.

## 3. User journeys and product boundaries

1. **Scope work:** open an item, start/join a shared session, inspect repository evidence,
   ask questions, and apply an authorized issue edit or source-controlled document change.
2. **Unblock intake:** read the exact decision needed, edit the item or answer in the
   session, inspect the agent's resolution assessment, and see eligibility recomputed.
3. **Repair delivery:** open a PR or exhausted run, provide guidance, restart the affected
   stage, and follow new attempts through validation and provider confirmation.
4. **Resolve a child:** the parent shows “Waiting for child — human input needed”; open
   that child's evidence/decision, resolve it, then observe the original parent continue.

The portal offers the source-aware operations allowed by policy; it does not require an
interactive agent for a straightforward manual edit. Conversation can produce analysis
without a mutation. Agent proposals and applied changes have distinct visible states.
Session content is operational evidence; objective and relationship truth remains in the
repository/backlog as required by the workbench design, never in a private session index.

## 4. Interactive access and execution policy

The policy and named credential vocabulary below is implemented alongside
bounded shared sessions using an existing configured Goober and independently
authorized typed source operations. See [interactive access](../reference/interactive-access.md) for the current
permissions route, source formats and implementation boundaries.

The portal implements human-only local run inspection, occurrence-bound decisions,
saved shared guidance, queued affected-stage restart epochs and shared native
sessions through the existing runner and journal services. Saved notes alone are
not agent delivery. Engine intervention remains unsupported on this surface until
an equivalent authority and receipt path is qualified. See the reference above for
bounds, idempotency, pending outcomes and the actual routes.

```yaml
spec:
  interactiveAccess:
    credentials:
      backlog: team-backlog-operator
      repositories:
        - repository: {provider: github, owner: acme, name: web}
          credentialRef: team-code-author
    actions: [backlog.read, backlog.edit, backlog.resolve, repository.read,
              run.intervene, run.restartStage, pr.repair, source.proposeChange]
    humans:
      viewers: [{issuer: https://identity.example, group: product-readers}]
      operators: [{issuer: https://identity.example, group: product-operators}]
    sourceWrites: {mode: pull-request}
```

References resolve server-owned credential bindings backed by the existing instance
secret-reference mechanisms; no inline secrets or parallel credential vault are introduced.
Implement a concrete selector that resolves each credential reference, provider identity
and qualified target to the existing credential injector. The currently decorative
`connectionRef` field is not that selector. Reject unresolved, incompatible or ambiguous
bindings before admission; do not reuse the legacy first-binding fallback.
An explicit reference may identify the same credential used by normal gaggle execution.
Omission never silently inherits credentials or enables interaction. Cross-provider code
and backlog bindings resolve independently; missing one cannot fall back to another.
Targets resolve to configured gaggle bindings, not arbitrary URLs submitted by a client.

Human authorization and provider permission are independent checks:
`authenticated principal ∩ gaggle grant ∩ allowed action/target ∩ execution capability`.
The provider enforces its own credential permissions as the final boundary. A powerful
provider token does not enable actions omitted from the interactive policy.
An operator implies a viewer for that gaggle. Grants match issuer plus stable subject or
verified group identifier; display names are never identifiers. Instance administrators
manage policy; gaggle viewer/operator grants must be explicit, including for administrators.
An instance administrator does not automatically gain wider provider-backed visibility.
Preserving existing read-only monitoring does not grant new provider-backed workbench reads.
The established local authenticated principal may be granted explicitly in solo mode.

Policy changes affect new commands immediately. Recheck policy before each provider
effect, queued command dispatch, and credential refresh. Revocation stops new effects;
already-submitted effects are reconciled and reported, not described as rolled back.
An accepted job stores the admitted policy digest, references and capability ceiling.
Later policy may narrow that authority, never silently expand a running job's grants.
Optional persona instructions cannot bypass these checks or widen a child's authority.

## 5. Durable shared sessions

A session has a stable ID, gaggle, title, creator principal, creation time, pinned assistant
configuration digest, policy reference, bounded context references, execution budget,
and linked items/runs/PRs. Qualified targets preserve provider/project/repository identity.
Session state is `idle`, `queued`, `running`, `awaiting-human`, `cancel-requested`, or
`closed`. Last execution outcome is separate from conversational availability.
Sessions are never keyed to a browser connection, websocket ID, or process PID.

Persist accepted messages in an append-only session ledger before returning success.
Each record carries session sequence, message ID, actor kind, authenticated principal
when human, agent identity when generated, timestamp, purpose and scrubbed content/ref.
An agent action records the originating human message/command and execution identity;
it must not impersonate that human as the author of an agent-generated message.
Two humans may submit concurrently: one sequencer assigns durable order and executes
one conversational turn at a time per session. A UI can show queued input immediately.
An explicit interrupt uses the run-intervention path; a normal message never kills work.

The session service owns execution after durable acceptance. Disconnects, HTTP timeouts
and SSE reconnections neither cancel the session nor duplicate accepted turns. On restart,
recover queued/active work from durable records and the runner's authoritative state.
Use a recoverable execution lease and attempt identity to prevent two session workers.
Store the accepted command before dispatch; persist its returned execution identity so an
uncertain dispatch is reconciled before retry. Never claim exactly-once model invocation.

For a harness with native session continuation, persist only safe opaque session metadata.
If that session is unavailable, start a new invocation with bounded prior evidence and
new guidance; disclose reconstructed context. Do not claim exact engine-state recovery.
Keep full evidence in retained artifacts and a bounded context manifest for each turn.
Summaries identify their sources and are not substituted for an authoritative decision.

A session close stops admitting new turns, requests cancellation of its active work, and
settles only after execution disposition is known. Archive is a presentation/retention
operation on a closed session. Idle shared sessions do not reserve a stage execution slot.
Active shared sessions have explicit concurrency/admission limits independent of browser count.

## 6. Attention and recorded resolution

The shared read service joins provider-backed item state with run/child/session evidence.
Each attention occurrence has qualified subject, reason code, requested decision, evidence
refs, generation/revision, first-seen time, current disposition, and authorized actions.
Classify `human-decision`, `execution-stalled`, `provider-conflict`, and `warning` separately.
Show sibling dependency waits distinctly; being blocked does not always require a human.
Preserve historical failure even after a later attempt resolves the operational problem.
Personal dismissal is not a shared resolution and never unparks work.

Resolution flow: read current source/revision → collect answer or edit → assess the
specific open request → record rationale/evidence → conditionally apply the source
transition → verify provider state → recompute eligibility → publish a resolution event.
Agents may perform the assessment and clear resolved needs-human automatically when the
policy grants `backlog.resolve`. A second generic “approve agent” click is not required.
Conflicting or insufficient answers leave a concrete unresolved question and no clearance.
Record the assessor agent, originating human evidence, old/new revisions and effect receipt.

Clearing a backlog marker does not approve a gate, override provider checks, merge a PR,
increase an execution budget, or silently restart work. Those remain separate typed
operations, although one authorized user command may explicitly request a composed flow.
Requeue uses the durable-start design after current eligibility is confirmed. A resolved
decision may remain unscheduled because another claim, dependency, policy or limit blocks it.
The portal shows that reason instead of claiming resolution means execution has begun.

## 7. Stage restart, guidance and allowance epochs

Introduce a distinct `restart-stage` command rather than changing legacy rerun semantics.
Admission names run, stage, observed occurrence/attempt or terminal generation, reason,
new guidance, selected prior-context refs, expected PR head if applicable, and reset intent.
Validate graph position, live ownership, artifact integrity, provider revision and authority.
Show the affected stage, resumed path, branch and effective allowances before submission.

For an active attempt, request bounded interruption and await confirmed surrender before
starting a replacement. Siblings are untouched unless the pinned graph requires their
outputs to be invalidated; reject an unsupported graph boundary with an actionable reason.
The replacement consumes selected prior results/diff/transcript and the new guidance.
Do not discard an unpushed patch or substitute a moved PR head without explicit reconciliation.

Each accepted restart creates a new intervention epoch. Retry and repass counters in the
affected retry/repass scope start fresh against the configured allowance; lifetime counters
remain cumulative. Attempt IDs and journal sequence never reset. Record old consumption,
new allowance, reset scope, actor, guidance digest and source attempt before dispatch.
This covers the gate/repass loop that returns to the restarted stage; it does not reset
unrelated branches, whole-instance rate limits, or parent aggregate cost/concurrency limits.
When remaining aggregate budget cannot fund restart, report the exact exhausted budget;
any budget change is a separately authorized, recorded operation, not implied by chat text.

An open execution can consume the restart through its runner-owned command path.
A settled execution requires an executable linked continuation with a new run identity,
immutable source-terminal reference and equivalent provider/branch checks. Finish actual
dispatch and recovery before exposing this action. The original continuation CLI
was only a journal-preparation foundation; the local stack now installs queued
human epoch dispatch and recovery. Map parent observation to the accepted continuation
explicitly; never rewrite an old child's successful/failed result or source journal.

## 8. Child human handoffs and wait lifecycle

Use the parent/child identities and durable completion contract in the child-workflow
design. A child records `awaiting-human` separately from its execution phase; the parent
wait observes that durable state and links the actionable request into the same inbox.
The parent must not interpret the first `run.finished` escalation record as settled failure:
the existing Temporal protocol writes that record before its intervention hold.
Child resumption and completion notify the same parent wait; duplicate/out-of-order notices
are reconciled by child generation and accepted result identity.

Explicit human gates require a decision tied to run, gate, occurrence and pinned branches.
Implement local/Temporal conformance before advertising them; a gate-name-only signal is
insufficient when a loop visits the same gate twice. Late decisions fail as stale.
Terminal intervention and pending-gate approval remain distinct actions in the portal.

Support explicit bounded or unbounded human waits for the new protocol. Existing runs keep
their pinned 24-hour/default behavior; zero/unset never changes meaning during an upgrade.
Unbounded human waiting does not mean unlimited messages, model work, or child creation.
Display duration and cancellation controls; never label an intentional human wait as a stall.
A durable child wait releases the parent's logical active execution slot; its idle pod may
remain allocated. Active-work deadlines stop while the explicit wait policy governs elapsed
waiting. Resume reacquires capacity before new active work, as the child-workflow design
requires. Cancelling the parent durably cascades to its active children; the portal names
affected children and distinguishes requested cancellation from confirmed family settlement.
Expired old-protocol holds display “execution settled”; offer a valid continuation action
only when supported, rather than accepting an answer that cannot resume anything.

## 9. Commands, read API and portal composition

Add contracts to the shared API registry and generated portal fixtures. Proposed routes:

| Route | Result |
| --- | --- |
| `GET /api/v1/gaggles/{g}/interactive-capabilities` | Authorized actions/targets, policy revision, assistant and delivery modes; no secrets. |
| `GET /api/v1/gaggles/{g}/attention` | Bounded cursor page of current occurrences and freshness. |
| `GET/POST /api/v1/gaggles/{g}/sessions` | List/create a shared session. |
| `GET /api/v1/gaggles/{g}/sessions/{s}` | Session summary and linked work; messages are separately paged. |
| `GET/POST /api/v1/gaggles/{g}/sessions/{s}/messages` | Cursor history or durable ordered submission. |
| `POST /api/v1/gaggles/{g}/sessions/{s}/close` | Implemented transport for typed close, with durable idempotency. Further interrupt/linked commands use separate typed operations; never arbitrary shell text. |
| `POST /api/v1/gaggles/{g}/operations` | Typed resolve, restart-stage, repair-PR or source-edit request. |
| `GET /api/v1/gaggles/{g}/operations/{o}` | Durable accepted/running/applied/rejected/partial/unknown outcome and receipts. |

Reuse the existing SSE/change-cursor transport for invalidation and reconnect. A read
snapshot carries its cursor; missed notifications trigger bounded reload, not lost state.
Existing run operator-message endpoints remain compatible and delegate to shared services.
CLI and agent tools submit the same commands; the portal cannot invent a private write path.

Every mutation requires a stable idempotency key, expected subject revision/generation,
and declared action. Scope keys by gaggle, principal and operation; a changed payload under
the same key returns conflict. Return accepted only after durable handoff, and applied only
after authoritative effect evidence. Timeouts after dispatch yield pending/unknown with
an operation ID; clients inspect it instead of generating a fresh key and repeating effects.
Provider edits and compound effect recovery reuse the workbench's mutation service.
PR repair targets the selected PR and expected head, preserves its owned branch, and
publishes through the configured provider write path. New code invalidates prior checks
as required by normal delivery; repair authorization never implies merge authorization.
The human selects a configured source binding, exact repository identity, native repository
and PR IDs, locator and inspected head in structured session input. The accepted message
and immutable execution input bind that selection; prose and earlier turns cannot grant it.
Any same-repository PR may be selected under current `pr.repair`; forks and active
agent/worktree custody are refused. An iterative repair may advance only to the confirmed
descendant proven by this turn/actor/PR's previous command receipt or exact retained
positive observation, retaining the original selection
and ancestor command ID. Foreign or uncertain head movements require fresh human intent.
The initial native primitive supports at most 32 regular UTF-8 file additions, edits or
deletions and 1 MiB of content; executable edits, symlinks, submodules and binary changes
are explicitly unsupported. GitHub `createCommitOnBranch.expectedHeadOid` and ADO push
`refUpdates.oldObjectId` provide atomic head comparison. PR-open status is checked before
that call; neither native primitive atomically compares PR-open status with branch update.
Provider acknowledgement and a later exact commit observation remain distinct receipt
facts. The shared command ledger, current execution lease and private session tools
now connect to local daemon custody. The host holds `claims.lock`, an admission barrier
and sorted repository-manager locks through the bounded provider effect and receipt.
Topology and manager membership come from one independently published immutable
custody snapshot. Workflow-only reload waits for active repairs; changed-policy reload
cancels and joins affected sessions before taking the custody publication lock.
A matching PR-locator claim (including expired or ambiguous claims), active automation,
retained branch occupancy or an unknown historical worktree root blocks repair.
Inventory is capped at 4,096 journals/claims and 64 roots; incomplete inventory refuses.
This first host adapter supports one local daemon with per-stage worktrees. Temporal,
shared claims, pinned workspaces and custom provider endpoints require a qualified
ownership adapter and remain refused. It does not fence independent external processes.
Actual Git integration exercises local branch custody; GitHub/ADO transport tests use
controlled native HTTP responses. Live provider qualification remains outstanding.
The human portal picker supplies structured selection separately; the host tools never
infer repair authority from prose or expose provider credentials to the model.
Unsupported delivery, denied access, stale revision, expired wait and budget exhaustion
have distinct typed results and do not degrade into success-shaped responses.

## 10. Evidence, bounds, credentials and provider cache

Keep three records: immutable human/agent decisions, conversation evidence, and operational
diagnostics. An action record includes actor/principal, execution credential reference,
policy digest, target/revision, originating message, reason, attempt/epoch, result and receipt.
No provider secrets, raw authorization headers, or secret-bearing idempotency keys enter
logs, telemetry or Temporal payloads. Scrub before durable acceptance, not only rendering.
Preserve existing digest/containment checks on artifact reads and render Markdown safely.

Initial configurable defaults: 64 KiB inline message, 16 artifact refs per command,
1 MiB referenced content per message, 100 records per page (hard maximum 200),
32 open sessions and four executing turns per gaggle, 64 queued turns per session.
Reject overflow with a typed capacity result; never silently drop an accepted message.
Cap model context at the configured harness budget; retain larger evidence by digest.
Long sessions rotate sealed transcript artifacts and keep bounded in-memory indexes;
never load all messages or child history to render one page. Retention must not delete
evidence needed by an active operation, unresolved decision, or active parent wait.
Closed-session and settled-operation retention defaults to 30 days, with 30-day
idempotency tombstones after expiry. The gaggle admits at most 10,000 retained
sessions/operations and 1 GiB of conversation artifacts by default; existing tighter
instance storage limits still apply. Active references pin evidence, so saturation
backpressures new input rather than pruning an unresolved decision. The production
maintenance loop prunes at most 100 records per sweep and enforces byte/count limits
at admission. Tests must demonstrate steady-state bounds and retained active custody.
Tombstones prevent replay of expired keys from accidentally repeating a mutation;
after the advertised deduplication horizon clients must use explicit new intent.

The provider cache is a rebuildable read projection. Key by provider endpoint, qualified
target, credential visibility/identity and representation; invalidate after confirmed writes.
Do not share a broader identity's cached data with a narrower interactive identity.
Mutable authorization policy and provider permission probes have explicit freshness;
cached presentation never substitutes for revision/permission checks at mutation time.
After a partial provider update, expose per-effect progress and reconcile from source.
Do not replay already-completed field/label changes over an intervening human edit (#6468).

## 11. Rollout, compatibility and verification

Ship contracts behind absent-by-default interactive capabilities, then enable local and
Temporal implementations independently according to conformance results. Preserve current
read-only portal routes, legacy CLI verbs, no-interaction configurations and pinned histories.
New policy fields require schema/manifests/validator/generator updates together. Do not
repurpose `selfIdentity`, `isolation.identityRef`, or decorative `connectionRef` semantics.
Do not fabricate missing actors, escalation reasons or provider receipts for old data.
Update portal/runtime requirements that restrict interaction once this design is approved.

Meaningful tests cover: two humans' ordered messages; disconnect/reconnect; daemon crash
before/after admission and dispatch; duplicates with same/different payload; lease recovery;
credential revocation; explicit administrator grants, cross-gaggle denial and cached-data
isolation; partial provider writes; rejection of repository direct-write bypasses;
restart allowance epochs and moved PR heads; stale human-gate occurrences; child human wait,
resolution, slot release/reacquisition, cancellation cascade and old-hold expiry; retention
bounds and secret scrubbing.
Run identical core vectors against local and Temporal backends. Use fake adapters for
unsupported-live-message degradation and verify the actual delivered context, not only logs.
Browser tests exercise answer→assessment→verified clearance and guidance→restart→new outcome,
plus shared-session refresh, permission changes, keyboard navigation and mobile attention views.
Execute focused suites per slice; the complete stack must satisfy `make ci` and documented
integration gates. Source/API tests alone do not justify claiming a live provider worked.

## 12. Implementation sequence and acceptance

| Task | Deliverable and acceptance boundary |
| --- | --- |
| HAW-HITL-001 | Versioned policy/session/operation/attention contracts; schemas reject ambiguous credentials, unknown actions and unbounded payloads; old fixtures stay valid. |
| HAW-HITL-002 | Resolve explicit credentials and gaggle human grants; prove denied cross-gaggle reads/writes, no fallback, policy narrowing and no secret exposure. Depends on 001. |
| HAW-HITL-003 | Durable session ledger, ordering and admission; concurrent writers, idempotency, paging, restart and capacity tests reconstruct every accepted input. Depends on 001–002. |
| HAW-HITL-004 | Session execution coordinator and harness continuation/reconstruction; disconnect survives, one turn executes, uncertain dispatch reconciles, context is bounded. Depends on 003. |
| HAW-HITL-005 | Canonical attention/resolution projection; distinguish pending gates, terminal holds, settled history, provider parks and personal dismissals. Depends on 001. |
| HAW-HITL-006 | Typed item resolution and automatic agent assessment; answer/edit clears only the observed satisfied decision, records rationale and verifies provider effects. Depends on 002–005 and workbench mutation service. |
| HAW-HITL-007 | Restart-stage allowance epochs and context; local conformance proves reset scope, cumulative history, interrupt settlement and provider/head checks. Depends on 001–002. |
| HAW-HITL-008 | Temporal restart/executable continuation and occurrence-bound human gates; replay, duplicate decisions and settled execution cases pass. Depends on 007. |
| HAW-HITL-009 | Integrate child waits and attention; parent releases its active slot while waiting, reacquires it to resume, follows accepted continuation, and reports cascading cancellation and family settlement accurately. Depends on 005/008 and child-workflow contract. |
| HAW-HITL-010 | Portal shared sessions, attention and run/PR actions; role/capability-driven controls call common services and render acknowledgements/outcomes. Depends on 004–009. |
| HAW-HITL-011 | CLI/agent-tool parity, end-to-end local/Temporal and provider fixtures; cross-surface vectors prove identical audit and authority semantics. Depends on 006–010. |
| HAW-HITL-012 | Operator docs, migrations/rollback and release gates; generated contracts/indexes current, opt-in deployment exercised and unsupported adapter modes documented. Depends on 011. |

The program stages child workflows first, then this HITL work, then durable event queues,
then backlog portal breadth. Where later services are required, implement the narrow typed
interface and deterministic fixtures now; do not advertise a user journey until wired end to end.

## 13. Prior art and review notes

[Copilot dynamic workflows](https://docs.github.com/en/copilot/concepts/agents/dynamic-workflows)
demonstrate explicit review checkpoints and structured agent work. Its
[SDK implementation](https://github.com/github/copilot-sdk/blob/main/nodejs/src/session.ts)
documents at-least-once step producers, reinforcing the need to reconcile external effects.
[Factory Missions](https://docs.factory.com/missions/overview) are relevant to plan review
and supervised execution. These are product/source precedents, not dependencies or a claim
that Goobers has been live-tested against those products.

The implemented source selector uses named `instance.interactiveCredentials`, backed by
existing token/auth sources. Initial shared sessions select an existing Goober
and admit at most 32 open sessions per gaggle, four executing turns per gaggle and
64 queued turns per session. See the shared-session reference for retention and
current runtime bounds; more extensive persona configuration is later scope.
These choices do not reopen explicit per-gaggle grants, PR-only repository changes,
cascading child cancellation, shared sessions, source ownership or human attribution.
