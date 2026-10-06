# Human operations and advanced workflows: incremental landing plan

Status: proposed execution plan, 2026-10-06. Design approval is separate from
implementation acceptance. Every implementation slice below needs its own review
and green required CI before landing on main.

## 1. Sources and disposition of the current PRs

| Review artifact | Intended disposition |
| --- | --- |
| [Child workflows design #6803](https://github.com/Agent-Clubhouse/Goobers/pull/6803) | Land the approved design/program first. |
| [HITL design #6804](https://github.com/Agent-Clubhouse/Goobers/pull/6804) | Land the approved design after #6803. |
| [Events/queues design #6805](https://github.com/Agent-Clubhouse/Goobers/pull/6805) | Land the approved design after #6804. |
| [Backlog design #6806](https://github.com/Agent-Clubhouse/Goobers/pull/6806) | Land the approved design after #6805. |
| [Implementation review #6807](https://github.com/Agent-Clubhouse/Goobers/pull/6807) | Keep as a draft reference snapshot. Extract reviewed slices; do not merge wholesale. Close as superseded only when all intended behavior is accounted for by landed work or explicit deferrals. |
| [Fleet authentication design #6865](https://github.com/Agent-Clubhouse/Goobers/pull/6865) | Remains a separate draft requiring design acceptance. Rebase its documentation onto main after the four designs, preserving the supplied outbound-only Agent contract. Its approval is not implied by merging the four workstream designs. |

The 159 local checkpoint branches preserve development history, not a promised
159-PR delivery sequence. Use their commits and tests as evidence; reorganize the
changes into independently verifiable delivery slices. Keep a permanent reference
to source commit `fa34a754148ea3076bfd04fe4976a5a461b063f2` for #6807 and
`fdda5e21426264e7f423ef2a4cedb01cbfdf1eee` for the Fleet design correction.

## 2. Inventory and integration risk

Compared with original base `04198152b63d228a9714ae2f92a7dca079ba5213`, the
implementation reference changes 1,319 files. As of main
`00d7ef3dfcd2f94c3549a305c770d17e5226f477`, main independently changed 585 files;
71 paths overlap the implementation reference. Overlap identifies required review,
not necessarily a textual conflict or evidence of a bug.

Largest implementation areas by changed file count:

| Area | Files | Landing consideration |
| --- | ---: | --- |
| `cmd/goobers` | 244 | Reconcile daemon wiring and existing refactors; extract reusable packages and justify growth per actual slice. |
| `internal/triggerqueue` | 138 | Separate shared storage primitives from child, session, event, restart and workbench records. Each record type owns migration/retention/recovery. |
| `portal/src` | 69 | Port onto Jeff's merged shared components and current main's locality/filter fixes. |
| `internal/runner` | 62 | Preserve main's execution fixes; qualify stage restart and child custody separately. |
| `internal/httpapi` | 54 | Attach each route to its owner slice, auth checks and production service adapter. |
| `internal/apicontract` | 54 | Regenerate contracts for each actual API slice, never copy the final generated snapshot wholesale. |
| `internal/workbenchservice` | 46 | Split reads, native edits, PR proposals and relationship curation. |
| `internal/childworkflow` | 35 | Separate authoring/admission, execution, workspace results and lifecycle qualification. |
| `internal/workbench` | 32 | Keep source ownership and portable relationships together with provider support. |
| `internal/interactiveaccess` | 31 | Land exact credential and human authorization boundaries before interactive effects. |

An initial integration inventory must classify every source change as retained,
adapted to current main, already supplied by main, superseded, or intentionally
deferred. Record that disposition per path/hunk when extracting each PR. Snapshot
file counts include tests and generated files and are not an estimate of production
code volume. A path may be changed by several slices; package ownership alone is
not sufficient to extract working PRs.

Do not replace current main's versions of shared files with snapshot copies. The
highest-risk overlaps are daemon/runner wiring, provider interfaces, auth routing,
recovery/worktrees, DSL feature registration, schemas and generated Portal contracts.

## 3. Branch and review strategy

1. Start each delivery train at current main, using `codex/haw-<slice>` branches.
2. Prefer independent PRs directly against main. Use a short stack only for a real
   code dependency; limit active depth to roughly two or three reviewable slices.
3. Each PR description names its stable task IDs, predecessor, included behavior,
   unavailable behavior, production callers, tests and rollback/recovery impact.
4. Keep adjacent changes together only when separating them would leave a broken
   build, unused production code, an unwired recovery path or misleading capability.
   Large rows below must split further; they are delivery units, not a quota of one PR.
5. After a parent lands, retarget/restack dependents onto the landed main commit and
   rerun required checks. The repository uses squash merges, so preserve source
   mappings independently of commit ancestry.
6. Regenerate schema/CRD/deepcopy/API/Portal/CLI outputs from the extracted source
   in each PR. Preserve current main's features and do not carry checkpoint-specific
   generated outputs or growth justifications blindly into a new merge base.

A typical code PR should fit one behavior and one failure/recovery story. Aim for
hundreds of handwritten production lines where practical; separate generated/test
volume in the review summary. Exceeding that guideline requires an explicit
cohesion reason and may produce additional PRs. No four giant workstream merges.

## 4. Delivery order and proposed PR boundaries

Priorities stay: child workflows, HITL, broader event/queue systems, backlog UI.
Shared prerequisites land only as far as the earlier feature needs them. The table
is an initial decomposition into about thirty delivery units; exact PR count is
set during extraction and may grow. Stable design tasks can span several PRs.

### Wave 0 — reconcile foundations

| ID | Boundary / likely files | Dependencies and acceptance |
| --- | --- | --- |
| LAND-F01 | Current-main integration inventory; adopt Jeff's shared Portal primitives in subsequent UI slices | Compare the 71 overlapping paths, identify equivalent main work, and record extraction ownership. No bulk implementation import. |
| LAND-F02 | Minimum durable admission/storage needed by children: `internal/triggerqueue`, `internal/startintent`, owned journal/API fields | Existing starts keep their behavior; admitted identities are idempotent, migrations and bounded retention have real callers. Include the first necessary consumer, not unused general framework code. |
| LAND-F03 | Shared authority/identity and scoped credential primitives required by the next slice | Preserve existing auth planes; reject cross-gaggle/target access and secret exposure. Introduce only the fields/routes consumed in that slice. |

### Wave 1 — child workflows

| ID | Boundary / likely files | Dependencies and acceptance |
| --- | --- | --- |
| LAND-C01 | Opt-in DSL, authoring/proposal validation and CLI/tool contracts: `internal/childworkflow`, workflow/API schemas, `internal/mcpio` | F01–F03 as needed; allow only existing Goobers/capabilities. Invalid proposals, recursion and unauthorized publication are refused. Feature lifecycle/version rules pass. |
| LAND-C02 | Stage-scoped admission and durable lineage: child queue records, stage grants, HTTP/MCP production handlers | C01; one unfinished child per stage, independent parallel-stage children, parent cancellation/admission race and reload narrowing tests. |
| LAND-C03 | Isolated workspace and retained artifact/result custody: worktree/recovery/child storage | C02; fork the intended parent state, enforce quotas, retain unresolved writers, and recover after crashes without overwriting the parent. |
| LAND-C04 | First supported local child execution, durable parent wait and cancellation: launcher/runner/scheduler wiring | C02–C03; actual parent→child→result path, restart recovery, stopped-writer evidence and confirmed cancellation. Include registered reconcilers in the same slice. |
| LAND-C05 | Parent result disposition and delegated PR publication: `internal/childpublication`, workspace handoff | C04; merge/replace/discard and uncertain publication recover safely. Publication requires the parent's upfront grant. |
| LAND-C06 | Contained/pod child execution and remote authority: `internal/childpod`, podauth/dispatcher/blob plane | C03–C05; qualify the configured pod capability shape, token isolation, reconnect and worker-loss recovery. This may need several PRs. |
| LAND-C07 | Parent/child Portal projection and end-to-end acceptance | C04–C06; use current Portal components. Show durable wait, child links, blocked/uncertain state and cancellation outcome. Only advertise qualified execution shapes. |

Human intervention on a child uses the common HITL restart work in H03; do not
copy a separate child-only human auth/session implementation into Wave 1. Child
normal execution and result handoff can land first while unsupported human restart
is explicitly unavailable. Recovery required for each enabled execution shape
must land with that shape, not wait for C07.

### Wave 2 — human operations

| ID | Boundary / likely files | Dependencies and acceptance |
| --- | --- | --- |
| LAND-H01 | Explicit interactive credentials, human grants and exact-target source access: `internal/interactiveaccess`, instance/config and API capabilities | F03; missing config stays non-interactive, no credential fallback, permission narrowing and human/service distinction tested. |
| LAND-H02 | Direct-mode browser authentication, if delivering direct Portal access | H01; new work missing from #6807. Login/callback/logout/token lifecycle and browser→daemon authorization must be exercised. Fleet-only deployments instead require the A-series below. |
| LAND-H03 | Run intervention and affected-stage restart with fresh allowance: `internal/intervention`, `internal/restartintent`, runner/queue/HTTP wiring | H01 and selected auth mode; immutable history, exact stage occurrence, prior context, fresh retry/repass budget, queued dispatch, cancellation and recovery. Include child restart epochs as a separately reviewable dependent PR if needed. |
| LAND-H04 | Attributed shared agent sessions and bounded durable turns: `internal/sessioning`, `internal/interactivesession`, session records and runtime | H01; two users, per-turn authority, disconnect, duplicate input, worker loss and retained uncertainty. API, actual runner and minimal usable Portal controls travel together. |
| LAND-H05 | Backlog field edits and needs-human resolution through typed source tools | H01/H04; bring forward only the provider operations needed for this journey. Verify the blocker is resolved before clearance; do not require the full backlog workbench. |
| LAND-H06 | Selected-PR repair, receipt inspection and publication reconciliation: sessionops/provider/queue/host adapters | H04; exact selected repo/PR/head, policy checks, isolated workspace, no silent retarget, uncertain effect handling. Split provider adapters from UI only when the exposed surface remains honest. |
| LAND-H07 | Integrated attention and shared-session Portal experience; local/Temporal qualification by supported shape | H03–H06; gate unsupported backends explicitly. Exercise human input→actual agent action→observed recovery using Jeff's shared UI foundation. |

### Fleet authentication track within HITL

These are additional implementation requirements from draft #6865, not code
already present in #6807 and not an instruction to reimplement a production Fleet
service in this repository.

| ID | Boundary | Dependencies and acceptance |
| --- | --- | --- |
| LAND-A01 | Review/land #6865 and align security/instance/portal/deployment requirements | Separate design acceptance; preserve the owner's supplied production Agent v2 contract. |
| LAND-A02 | Typed delegated principals and protected Agent→daemon loopback provenance | A01/H01; outbound-only Agent, same host/pod network namespace, literal `127.0.0.1:8085`, no public Fleet port. Production Agent compatibility fixtures required. |
| LAND-A03 | Exact connection/registration/request delegation and durable replay/command admission | A02/F02; production Entra delegated identity, negotiate/challenge/ready, reliable resume, 30-second presence/90-second expiry, 401/403/404 termination and 409 supersession. Do not replace deployed signing/wire formats. |
| LAND-A04 | Fleet permission leases, executor enforcement, revocation and confirmed cancellation | A03/H03–H04; preserve initiating user and service; stale connection/lease cannot create effects. Socket closure never proves work stopped. Supported executor tests and live qualification required. |
| LAND-A05 | Explicit workload-identity enrollment/runtime for a headless cloud service | Separate HAW-AUTH-010 dependency on supported Fleet behavior. Agree the external service work and ownership first; no arbitrary app credential substitution. Fleet-only headless readiness stays blocked until qualified. |

Direct browser login is not a prerequisite for fleet-only deployment. Conversely,
shipping direct login does not establish Agent/workload enrollment support. Select
the deployment being qualified in each PR and in release acceptance.

### Wave 3 — broader durable starts, events and shared reads

| ID | Boundary / likely files | Dependencies and acceptance |
| --- | --- | --- |
| LAND-E01 | Normalize remaining start sources: manual/detached, schedules including demand-sized starts, signals, direct engine, sessions/restarts | F02 and source-specific owners; no launch bypasses custody, exact replay, capacity handling, configuration retention and uncertain launch recovery. Split by start source; do not bundle all adapters into one large PR. |
| LAND-E02 | Gaggle-scoped event receipts and workflow outbox: `internal/eventing`, `internal/eventpublication` | E01 as needed; no-match success, bounded payload/retention, producer authority, durable emit identity and causal limits. |
| LAND-E03 | Subscriptions, matching and configurable debounce: eventexecution/localscheduler/queue records | E02; all/latest semantics, crash recovery, scoped fan-out, loop budgets and no cross-gaggle delivery. |
| LAND-E04 | Authenticated external ingress and queue controls: `internal/eventingress`, `internal/startcontrol`, HTTP/Portal | E01–E03; admission denial, visibility, cancellation/deadlines and verified terminal outcomes. Service ingress auth remains distinct from human Fleet identity. |
| LAND-E05 | Shared provider reads and throttling: `internal/apireadcache`, typed ADO plans and GH/ADO adapters | H01; cache by exact visibility, bound storage and request rates, share only safe reads, invalidate confirmed writes, test partial/paged responses and quota backoff. |

### Wave 4 — backlog browsing and visuals

| ID | Boundary / likely files | Dependencies and acceptance |
| --- | --- | --- |
| LAND-B01 | Source-owned item/objective/relationship model and reads: workbench/workbenchprovider/workbenchgraph | H01/E05; configured external repositories, explicit link authority and rebuildable projections. No private authoritative objective store. |
| LAND-B02 | Portal browsing and organization visuals with explicit edit capabilities | B01; item links, related items, objectives, empty/partial/unavailable states, keyboard/mobile behavior; current Portal component conventions. |
| LAND-B03 | Native edits and selected supported provider relationships | B01/H05; exact target and revision, source confirmation, partial effects and refresh. Reuse earlier HITL typed edit services. |
| LAND-B04 | PR-governed Markdown/manifest proposals and curation suggestions | B01/B03; separate code/wiki/workflow repos, explicit human acceptance, no direct protected-branch writes, pending PR reconciliation. Split provider adapters as needed. |
| LAND-B05 | End-to-end workbench acceptance and operator docs | B02–B04; browser→authorized mutation→provider receipt→rebuilt graph. Keep unsupported provider shapes explicit; no implied progress scoring. |

## 5. Gates for every implementation PR

- Build and use current main's abstractions; preserve Jeff's Portal foundation and
  all independently landed fixes. No blanket snapshot merge to resolve conflicts.
- Include a real production caller for new operations, recovery exits and pruning.
  If wiring cannot land yet, keep the unsupported capability unavailable and do
  not claim that journey delivered.
- Exercise the meaningful failure path: crash/retry/race/revocation/uncertain
  external effect appropriate to the slice. Avoid tests that only repeat structs.
- Run focused tests during extraction, then `make ci` and the repository's required
  hosted gate against the actual PR head. Do not reuse snapshot test results as
  evidence for a rebased or reshaped implementation.
- Honor DSL compatibility and schema structural guards. Document storage versions,
  replay/retention limits, downgrade refusal and operational rollback consequences.
- Regenerate documentation/contracts and update the accepted design's delivery
  ledger with actual merged PRs. Keep design status approved until the entire
  claimed scope is delivered; do not mark implemented from partial unit tests.
- Keep live provider, Kubernetes, Temporal, Entra and external Fleet qualification
  distinct from fake-adapter and unit coverage. Unqualified modes remain disabled
  or clearly unavailable.

Required CI for the design merges is also enforced; approved prose does not bypass
the repository's `make ci (fmt-check · vet · build · test · lint)` merge rule.

## 6. First extraction and next decisions

Start with F01 and a narrowly scoped F02+C01/C02 dependency boundary: compare the
current queue/runtime seams, identify the minimum child admission path, and open
the first small implementation PR only after its source mapping and tests are
concrete. Do not start by importing all 138 changed triggerqueue files.

During extraction, record for each PR: stable task IDs, snapshot commits/hunks,
current-main equivalents, prerequisites, excluded follow-ups and acceptance proof.
Create numbered backlog items and backlink the accepted designs as the units
become dispatchable; identifiers here are planning references, not invented issue
numbers or assertions that issues have been filed.

The remaining product decision is which HITL deployment to qualify first: direct
browser sign-in, production Fleet with supported delegated enrollment, or a fully
headless Fleet service that also requires A05. Child workflow delivery can proceed
while that is decided. External Fleet workload identity and local provenance need
an agreed compatibility contract; no estimated completion date is credible before
that boundary is confirmed.

Completion means each accepted capability is implemented, integrated, tested and
documented on main, with qualified deployment shapes and every snapshot item
accounted for. It does not mean merging #6807 or preserving its original branch
boundaries.
