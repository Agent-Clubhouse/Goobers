# The stage contract (V0)

> The interface every stage executor and the runner speak. Substrate-neutral:
> identical at every tier (ARCHITECTURE.md §5, §2 invariant 4). Current implemented
> version: `v1alpha9` (`api/v1alpha1.StageContractVersion`).

A **stage** (this doc's "stage" is the workflow/task types' "task" — the terms
are equivalent, ARCHITECTURE.md §5) is a unit the runner executes: a
deterministic command or an agentic harness invocation. Gates are a machine
state whose evaluators run with stage-execution semantics.

Three JSON envelopes carry everything, defined in Go (`api/v1alpha1/envelope.go`,
`artifact.go`) and mirrored by closed JSON Schemas (`api/schemas/*.schema.json`):

| Envelope | Schema | Direction |
|---|---|---|
| `InvocationEnvelope` | `invocation.schema.json` | runner → stage |
| `ResultEnvelope` | `result.schema.json` | stage → runner |
| `Verdict` | `verdict.schema.json` | gate evaluator → runner |
| `ArtifactPointer` | `artifact-pointer.schema.json` | shared; how stages exchange bytes |

## The load-bearing invariant

**No stage reaches into another stage's state** (§2.4). Stages exchange
**envelopes and artifact pointers only**. This is enforced *by construction*, not
by convention:

- The invocation envelope has **no field carrying an upstream stage's result
  body**. A stage consumes prior work only through `contextPointers` — read-only
  pointers into the run journal. (`envelope_test.go` fails the build if a
  `ResultEnvelope`-typed field is ever added to the invocation.)
- The schemas are **closed** (`additionalProperties: false`): an envelope
  carrying an undeclared field — e.g. a legacy `upstreamOutputs` — is a
  validation error (`testdata/envelopes/invalid/invocation-upstream-reachthrough.json`).
- `outputs` on the result envelope accepts **scalars only**; anything larger is
  an artifact, referenced by pointer. State cannot be smuggled through `outputs`.

## Well-known outputs

Most `outputs` keys mean whatever the consuming gate or downstream stage's
`inputsFrom` says they mean. One key is interpreted by the **runner itself**:

| Output | Effect |
|---|---|
| `workspaceBranch` | Rebinds the branch every LATER stage's worktree is provisioned on, for the rest of the run. Empty/absent is a no-op, not a reset. |

This exists for workflows that re-enter on work which **already has a branch**,
rather than producing a new one. `pr-remediation` is the case it was added for
(issue #392, `docs/design/v0/pr-lifecycle-loop.md` §5): it rebases and reworks
an existing PR, so its `implement`/`review`/`local-ci` stages — reused verbatim
from `implementation` — must operate on the PR's own head branch. An agentic
stage and a gate evaluator cannot check anything out for themselves, so the
rebinding is the only way to reach them.

Constraints:

- The branch must already exist on the remote, and must live in the run-branch
  namespace (`goobers/`) that the worktree manager excludes from its prune
  fetch — otherwise the next stage's working-copy refresh deletes it.
- The rebinding is **sticky and durable**: it applies from the stage *after*
  the emitting one onward, and is recovered from the journal on resume, so a
  crash mid-chain does not silently revert the rest of the run to the default
  branch.

## How a stage gets its input

The runner hands the stage an `InvocationEnvelope`:

- `triggerRef` — optional bounded scheduler metadata identifying the event or item
  that caused the run; it never carries the provider's raw trigger payload.
- `goal` — what to achieve.
- `workspace` — absolute path to the fresh, isolated, disposable workspace the
  stage runs in. Repo-backed stages receive a git worktree at tiers 1–2; a
  deterministic task with `run.workspace: scratch` receives an empty directory
  and does not resolve a repository.

  A run's stages share one **branch**, not one tree: the first repo-backed
  stage creates the run branch (`goobers/<workflow>/<run-id>`) off
  `repoRef.branch`, and every later stage checks that same branch out — now
  carrying the earlier stages' commits — in its own fresh worktree. That is
  what makes a `local-ci` stage and a reviewer gate evaluate the run's real
  diff rather than a pristine base. Only **committed** work crosses a stage
  boundary; an uncommitted working tree does not.

  A stage may **rebind** that branch for the remainder of the run by emitting
  the well-known `workspaceBranch` output (see below).
- `contextPointers[]` — the read-only inputs. Each is exactly one of:
  - an `artifact` (`ArtifactPointer`: journal-relative `path` + `sha256` digest) —
    upstream outputs and input snapshots; or
  - an `external` ref (`kind` + `uri`) — e.g. the issue/PR URL. Content outside the
    journal is untrusted; fetching and trusting it is the stage's job.
  - on a **repass**, also the gate's most-recent `Verdict` artifact — see
    "Repass context obligation" below.
  Artifacts fanned into a parallel join also carry their declaration-order
  `branch` id and `branchName`; both fields are paired and valid only on artifact
  pointers. The union is ordered first by branch declaration, then by each
  branch's original pointer order.
- `minimumIntegrity` — the task definition's lowest accepted provenance grade.
  Backlog items, context pointers, and artifacts carry one of `trusted`
  (operator/config), `maintainer` (SEC-047 trust-labeled task text), `unapproved`
  (arbitrary provider content), or `derived` (workflow/agent output). The compiler
  rejects unknown declarations. Before dispatch, the runner refuses missing,
  contradictory, or lower-graded inputs and appends an `error` event with code
  `input_integrity_below_minimum`; no workspace or credentials are provisioned.
  `derived` remains distinguishable in the envelope and journal and is admitted at
  the maintainer tier, while only `trusted` satisfies a trusted minimum.
- `contextFrom[]` — optional task/gate names that explicitly route those
  producers' artifact and verdict pointers into this task's invocation. When
  omitted, the task receives every accumulated pointer for compatibility. The
  compiler rejects unknown sources; routing never changes a pointer's integrity.
  System-generated pointers have no producing task or gate and are therefore
  outside the filter: the `learning.episode[<seq>]` correction a repassing gate
  injects for the stage it re-enters survives `contextFrom` unconditionally,
  because no source name could ever select it. It is still graded like any
  other pointer against `minimumIntegrity`.
- `capabilities[]` — the capability grants the stage's definition declares (e.g.
  `github:issues:write`). **Capability admission fails closed**: credentials for a
  capability not listed here are never materialized (§5).
- `instructionAddendum` — an optional operator-supplied instruction appended to
  the agent's configured instructions for one explicit rerun invocation. It is
  journaled with the intervention and never written to the workflow definition.
- `inputs` — the stage's own static config from its definition. Agentic harnesses
  render this map into the invocation prompt as data.
  A parallel join additionally receives `inputs.branchCompleteness`, with one
  terminal status and artifact count per declared branch in declaration order.
- `nestedAgentPolicy` — optional versioned policy for a mechanically launched
  child agent. When present, the runner also supplies `attempt`,
  `ownershipBoundary`, `policyActions`, and a runner-authored
  `parentPlatformPolicy`. Admission intersects that authority with the stage
  policy, selected profile/model, and adapter capability. Missing parent
  authority or an adapter without a policy-enforcing child-launch path fails
  before any harness process starts. Fresh context drops optional parent item,
  inputs, addenda, and context pointers; inherited context retains them;
  explicit context carries only named pointers and selected envelope sections.
  The immutable child execution policy is delivered in every mode.
- `item`, `repoRef`, `limits` — the triggering backlog item, target repo, and
  execution bounds. `repoRef` carries repository identity and connection
  fields only: config-side declarations such as `project.checkout` (B2, #649)
  are consumed by the runner before a stage runs and never ride the envelope
  (`RepoRef.EnvelopeRef`).
- `checkoutCones` — present only when the runner honored a
  `project.checkout.sparse` declaration (#649): the repo-relative path cones
  actually materialized, keyed by workspace identity (`""` for the primary
  `workspace`, else the matching `additionalWorkspaces[i].name`). Absent (the
  common case) means every workspace has a full checkout. This is how a
  partial checkout is declared to a stage — deliberately a sibling of
  `repoRef` rather than a field on it, so `repoRef`'s own shape never changes
  regardless of checkout config.

## Where a stage writes its output

The stage returns a `ResultEnvelope`:

- `status` — one of `success`, `failure`, `blocked`, `no-work`.
- `artifacts[]` — its produced outputs. The stage writes bytes into the run
  journal (`api/v1alpha1.WriteArtifact`) and returns an `ArtifactPointer` for
  each. Every pointer carries its integrity grade; downstream stages receive the
  same grade on the enclosing `contextPointer`.
- `transcript` — an optional runner-authored `ArtifactPointer` to the scrubbed,
  digested transcript captured for this agentic attempt. It points at the
  existing journal span and is diagnostic only; it is not added to
  `artifacts[]` or passed to downstream stages. Legacy results omit it.
- `outputs` — small declared **scalar** values only.
  A deterministic bandit assignment may publish the reserved fact
  `randomizedIntervention=true`,
  `randomizedInterventionSource=bandit-assignment`, and `arm=control|treatment`.
  The read model requires all three values before treating an observation as
  randomized. An `arm` (or generic `randomized`) output by itself remains
  observational and is never promotion-eligible.
- `error` — structured failure detail (`code`, `message`, `retryable`); **required
  when `status == failure`**.
- `summary`, `metrics` — human and telemetry detail. Agentic usage uses
  `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
  `goobers.usage.copilot_premium_requests`, and `goobers.usage.cost_usd`.
  A measure the harness does not expose is omitted; an observed zero remains
  present.

Every stage process also receives `GOOBERS_TELEMETRY_DIR`, a writable directory
scoped to that stage attempt. A stage may append one JSON object per line to:

- `metrics.jsonl`: `{"name":"items","value":42,"unit":"count","attrs":{"source":"scan"}}`
- `events.jsonl`: `{"ts":"2026-07-18T18:00:00Z","name":"scan.complete","attrs":{"files":42}}`

The runner ingests both files when the stage exits. Emitted metrics are merged
into `ResultEnvelope.metrics` without replacing executor-computed values; metrics
and events are attached to the stage span and flow through the journal rollup.
Agentic reviewer gates receive the same emission surface on their gate span.
Each `attrs` object may contain at most 125 entries. Malformed or oversized
lines are counted and dropped without changing the stage or gate outcome.

## Artifact passing (the A → B hand-off)

Non-scalar data moves **only** by pointer:

1. Stage A: `ptr, _ := v1alpha1.WriteArtifact(journalRoot, "artifacts/a/out.txt", data, "text/plain")`
   → returns a pointer whose `digest` commits to the exact bytes.
2. Runner: puts `ptr` into stage B's invocation as a `contextPointer`. B never sees
   A's `ResultEnvelope`; the pointer and artifact carry the same integrity grade.
3. Stage B: `bytes, err := ptr.Resolve(journalRoot)` — reads the artifact
   **read-only** and **verifies the digest**; a mismatch is `ErrDigestMismatch`.
   Paths that escape the journal root (absolute or `..`) are refused
   (`ErrPathEscape`). Redaction runs journal-side before digesting, so digests
   commit to scrubbed bytes (§4).

Integrity is persisted in both contract and journal surfaces: invocation/item/
context/artifact fields make admission portable across harnesses, while `run.yaml`
input refs and conformance-normative `artifact.recorded` and typed refusal events
make the same decision replayable across runners. It never
lives only in the conformance-excluded `runner.*` namespace.

See `artifact_test.go:TestTwoStagePipelineByPointerOnly` for the end-to-end toy
pipeline.

## TBH-1 mutation proposals (design target; not implemented)

TBH-1 separates untrusted mutation intent from credentialed execution. An agentic
stage may propose an operation, but for a capability migrated to this boundary it
never receives that capability's writer credential. A built-in deterministic
executor is the only stage that receives the credential, validates the proposal,
and invokes the provider.

This is additive to the existing pointer-only contract:

1. The producer writes one closed, typed proposal-set document and, on success,
   returns exactly that one ordinary `ArtifactPointer` in
   `ResultEnvelope.artifacts`.
2. The executor's static `proposalInput.from` edge names that producer. On each
   graph entry to the executor, the runner selects the successful producer attempt
   for that edge and requires its result to contain exactly one artifact. It wraps
   that pointer in the executor's `ContextPointer` named `proposals`; neither
   artifact path nor media type selects the input.
3. Before dispatch or writer-credential materialization, the runner appends and
   fsyncs `proposal.input_bound`, pinning the producer stage/attempt and exact
   artifact path/digest to this executor visit. Resume and infrastructure retry
   reconstruct the same pointer from that event rather than selecting a newer
   result. Re-running the producer creates a new attempt and a new graph entry, so
   only then may a new binding be recorded.
4. The executor resolves and digest-verifies the bound pointer before parsing it.
   There is no `ResultEnvelope.proposals` field, side channel, shared directory, or
   embedded proposal body in `outputs`.

The media type is
`application/vnd.goobers.mutation-proposals+json;version=1`. The artifact path and
media type are classification aids; containment, digest verification, the closed
proposal schema, and executor policy are authoritative.

### Proposal-set schema

The first proposal schema is `goobers.dev/mutation-proposals/v1alpha1`. It is a
closed tagged union: unknown top-level fields, common fields, proposal kinds,
actions, and kind-specific fields are validation errors. Its wire shape is:

```json
{
  "schema": "goobers.dev/mutation-proposals/v1alpha1",
  "runId": "8f7c...",
  "producer": {"stage": "prepare-merge-proposal", "attempt": 1},
  "proposals": [
    {
      "id": "merge-42",
      "kind": "repo:merge",
      "target": {
        "repository": "github.com/example/project",
        "pullRequest": "42"
      },
      "preconditions": {
        "headSha": "0123456789abcdef...",
        "baseSha": "fedcba9876543210..."
      },
      "arguments": {
        "method": "squash",
        "commitTitle": "Harden mutation proposal authorization",
        "commitMessage": "Apply the reviewed trust-boundary contract."
      },
      "reason": "The pinned review verdict passed.",
      "evidence": [
        {
          "path": "artifacts/review/verdict.json",
          "digest": "sha256:..."
        }
      ]
    }
  ]
}
```

`runId`, `producer.stage`, and `producer.attempt` must equal the journaled stage
attempt that returned the artifact. They prevent a valid proposal from another run
or attempt being replayed as current intent. `proposals[].id` is unique within that
producer attempt and, together with those producer fields, is the executor's
idempotency namespace. A proposal that requires multiple provider calls derives one
stable call key from that namespace plus the zero-based operation index. IDs are
opaque printable ASCII, 1–128 bytes; they are not provider request IDs. `proposals`
must contain at least one entry.

Every proposal has:

| Field | Contract |
|---|---|
| `id` | Producer-attempt-local idempotency key. |
| `kind` | One of the four kinds below. It selects the closed target, precondition, and argument shapes. |
| `target.repository` | Canonical provider repository identity. It must normalize exactly to `InvocationEnvelope.repoRef`; a proposal cannot redirect execution to another repository. |
| `preconditions` | Kind-specific freshness pins. The object is always required; only issue creation may use an empty object because it has no existing target to pin. |
| `arguments` | Kind-specific mutation arguments. No provider command, URL, shell fragment, or free-form request body is accepted. |
| `reason` | Human-facing intent, 1–4,096 UTF-8 bytes. It is evidence, never authorization. |
| `evidence[]` | Optional in-journal `ArtifactPointer`s. Every pointer is contained and digest-verified; external URLs are not evidence pointers. |

Provider item IDs are non-empty printable strings of at most 128 bytes. Git SHAs are
full lowercase hexadecimal object IDs, not abbreviated revisions. `updatedAt` is a
UTC RFC 3339 timestamp copied from the provider read that informed the proposal.
Each proposal may cite at most 16 unique evidence pointers, all recorded earlier in
the same run.

The initial kinds deliberately name mutation intent, while capability admission
continues to use the canonical registry in `internal/capability`. Do not add aliases
such as `repo:merge` or `github:pr:close` to that registry merely because they are
proposal kinds:

| Proposal kind | Required canonical capability | Closed kind-specific shape |
|---|---|---|
| `repo:merge` | `github:pr:merge` | `target.pullRequest`; `preconditions.headSha` and `baseSha`; `arguments.method` in `merge`, `squash`, `rebase`, required bounded `commitMessage`, and optional bounded `commitTitle`. |
| `github:pr:close` | `github:pr:write` | `target.pullRequest`; `preconditions.headSha`; required bounded `arguments.comment`. |
| `repo:push` | `repo:push` | `target.ref` as a full `refs/heads/...` ref; `preconditions.sourceSha` and `remoteSha` (omitted only when creating the ref); `arguments.mode` in `fast-forward`, `force-with-lease`. |
| `github:issues:write` | `github:issues:write` | `arguments.action: create` selects bounded creation; otherwise `arguments.operations` is a bounded ordered list of existing-target issue-write operations. Existing targets carry `target.issue` and `preconditions.updatedAt`; creation omits both and uses empty preconditions. |

`prepare-merge-proposal` is the only initial producer for `repo:merge`. It
copies a workflow-pinned commit message when one was declared; otherwise it
deterministically synthesizes `commitTitle` and `commitMessage` from the current
PR title, closing references, and the trusted merge-review status comment pinned
to `headSha` and `baseSha`. `commitMessage` contains 1 byte to 16 KiB;
`commitTitle`, when present, contains 1–512 UTF-8 bytes. An agent-supplied scalar
output or free-form provider payload cannot populate either field.

The merge proposal means "land this reviewed head", not "call one particular
GitHub endpoint." Under the target's landing lease, `apply-proposals` detects the
live repository policy exactly as the current merge adapter does. For
`direct-merge`, it sends the proposal's method and the commit fields the
provider supports with the atomic expected-head guard. GitHub ignores commit
title/message for direct `rebase`, so the executor retains them in the proposal
audit record but does not forward them for that method. For
`merge-queue-enqueue`, it sends the same atomic expected head OID; the repository
ruleset selects the merge method and GitHub accepts no commit-title/message
override, so those proposal fields are likewise not sent. A successful direct
call records `outcome: merged`; a successful enqueue records `outcome: enqueued`
and advances to the existing queue watcher. Queue merge, eviction, timeout,
branch cleanup, and remediation routing remain downstream typed adapter
outcomes; enqueue is never reported as an inline merge.

`prepare-pr-close-proposal` derives its required comment from the same objective
moot/duplicate/superseded fact and reviewer rationale used by the current close
adapter. The comment is at most 16 KiB. Execution is a two-operation proposal:
post the explanation first (operation 0), then close the PR (operation 1). This
preserves the mandatory audit trail if close succeeds. If close is rejected
after the comment is confirmed, the proposal terminates
`proposal.partially_applied`; the executor never represents that outcome as a
clean refusal.

The `github:issues:write` kind has two closed shapes. Creation uses
`arguments.action: create` with required `title` and `body` and optional `labels`.
An existing-target proposal instead uses `arguments.operations`, containing one to
eight operation objects in execution order. Creation cannot appear in that list,
and the operation variants are:

| Operation action | Required fields | Optional fields |
|---|---|---|
| `edit` | At least one of `title`, `body` | `title`, `body` |
| `comment` | `body` | none |
| `add-labels` / `remove-labels` | non-empty `labels` | none |
| `close` / `reopen` | none | none |

Each operation object contains only `action` and the fields admitted by its selected
variant. For `arguments.operations`, `target.issue` and `preconditions.updatedAt`
are required. Issue titles are at most 512 UTF-8 bytes, issue bodies and issue/PR
comments are at most 16 KiB, and each `labels` list contains at most 32 unique
values of at most 100 UTF-8 bytes each. Empty edits, empty comments, duplicate
labels, unknown actions, and fields irrelevant to the selected action are refused.
Provider-specific limits may be stricter but never broaden these bounds.

Issue labels that carry a trust or eligibility decision are reserved. The executor
derives the reserved set from the canonical `goobers:approved` label plus every
provider trust label pinned for the target repository in the compiled gaggle
configuration. Neither creation labels nor `add-labels`/`remove-labels` operations
under `github:issues:write` may contain a reserved label. Comparison uses the
provider's canonical label-identity rules, including case folding; an alias cannot
bypass the reservation. The executor refuses an intersection with `reserved-label`.
The separate canonical `github:issues:approve` capability is the only authority that
may apply the pinned approval label, and it is not implied by an issue-write proposal
or executor grant. It remains on its separately admitted route until a future
proposal kind and validator migrate it; that kind must require
`github:issues:approve` on both producer and executor. Reserved-label removal
likewise requires a separately designed trust-revocation route. The initial schema
refuses both directions rather than treating revocation as an ordinary label write.

`repo:push` never permits an unconditional force. `force-with-lease` requires
`preconditions.remoteSha`, passes that exact SHA to the provider's compare-and-swap
mechanism, and is the required mode for a remediation rebase. `fast-forward` still
requires `remoteSha` when the destination exists; only `remoteSha` is absent when
creating the run branch.

### Declared authority and credential routing

For a migrated mutation route, an agentic producer still declares the canonical
capability in `Task.capabilities`; that declaration bounds what it may propose. It
does **not** cause credential injection. Routing is pinned in the compiled workflow
as `direct`, `proposal`, or `split`. `proposal` moves the complete canonical
capability behind typed proposals. `split` is permitted only when a canonical
capability covers multiple provider operations: every operation must be assigned to
one runner-owned adapter, no task process receives the broad credential, and an
unassigned operation is refused. The compiler and credential resolver apply these
fail-closed rules:

- An agentic stage proposing a migrated capability receives no writer credential
  for it. A stage cannot hold direct and proposal-routed authority for the same
  capability.
- A proposal kind whose required canonical capability is absent from the producer's
  pinned compiled task and invocation is refused.
- Proposal authority remains subject to the same workflow-level isolation as the
  corresponding direct capability. In the initial migration, the compiler admits
  `repo:merge` only from a deterministic stage whose runner-owned `run.kind` is
  `prepare-merge-proposal`; that adapter consumes merge-review's SHA-pinned verdict.
  The corresponding close adapter is `prepare-pr-close-proposal`. Both kinds are
  mutually exclusive with `run.command`, and an arbitrary deterministic command
  cannot impersonate them. The stage's user-chosen `name` is not its adapter
  identity. Implementation and pr-remediation stages may neither hold
  `github:pr:merge` nor propose a merge.
- The downstream executor declares every capability it may apply. It receives only
  those writer credentials, and only when it is the built-in `apply-proposals`
  executor; an arbitrary deterministic shell command cannot impersonate this kind.
- Capabilities not yet migrated retain current direct-injection behavior. Migration
  is pinned with the workflow definition, so a run never changes routing mode after
  it starts.

`github:pr:write` uses the narrowly defined `split` route when PR close migrates.
The close operation is assigned exclusively to `github:pr:close` proposals and the
`apply-proposals` executor. Open/update and read-only list/poll operations are
assigned to their existing runner-owned provider adapters. Those adapters receive
method-scoped provider handles from the credential broker, not a token in the stage
environment or workspace, and their interfaces do not expose close. The compiler
rejects `github:pr:write` on an agentic stage or `run.command` in this routing mode;
a declared policy action or built-in kind must select one of the closed adapters.
The underlying provider grant may have broader PR-write permission, but only the
runner credential broker holds it, and only `apply-proposals` receives a
close-capable handle. Compatibility entrypoints with direct credential injection
remain valid solely for workflow versions pinned before this route migrated; a
compiled workflow version cannot mix them with split routing. Migrating the route
therefore includes converting every current PR command entrypoint to one of the
closed built-in adapters; it cannot leave an old command path credentialed.

`github:issues:write` likewise uses `split`, because the canonical capability
currently backs both agent-authored issue intent and runner bookkeeping whose
ordering is part of the scheduler or PR-lifecycle contract. Migration assigns
every existing operation to exactly one of these routes:

| Exclusive owner | Closed method surface and preserved contract |
|---|---|
| `github:issues:write` proposal executor | Agentic curator and nominator create/edit/comment/label/state intents. These are the only initial agent-produced issue mutations; reserved trust labels remain excluded. |
| `backlog-query` adapter | Claim, release, expired/terminal claim cleanup, orphaned-claim reconciliation, and ready/stale/tracking/status drift repair performed while selecting work, including policy-authorized tracking-item auto-close. It owns the claim-ledger lock and provider claim epoch as one runner operation: reserve in the ledger, publish the provider breadcrumb, settle the server-assigned winner, roll back a losing provisional ledger reservation, and only then expose the claimed item. Release posts its breadcrumb and removes the provider marker before freeing the ledger entry, preserving retry ownership if provider cleanup fails. It cannot perform caller-supplied issue edits or comments. |
| `backlog-health` adapter | Ready-pool health sampling and implementation-failure feedback: remove the ready marker and append the bounded run/error evidence only after the configured threshold is met. Its inputs are the pinned trust/ready labels, threshold, telemetry evidence, and provider issue state; it cannot claim, release, close, or make free-form edits. |
| `issue-close-out` adapter | Implementation close-out and parking only: publish the fixed status comment/labels, remove the ready marker when required, and release the claim in the command's fixed order. Its status and outcome vocabularies are closed; it cannot select another issue or create/edit arbitrary content. |
| Backlog-assignment adapter | Assign or clear the configured assignee for selector-admitted backlog items. It cannot alter issue content, labels, state, or claims. |
| Issue-disposition adapter | Scheduler/gate escalation notification, post-merge issue closure, queue outcome routing, self-update rollback escalation issue creation, and other run-terminal status/claim-marker bookkeeping not owned by `issue-close-out`. It preserves each entrypoint's fixed operation ordering, comments, and status vocabulary. |
| PR-lifecycle bookkeeping adapter | Merge-review verdict labels/status comments, queue eviction/timeout remediation, merge-refusal demotion, post-merge fan-out/unparking/healing, remediation checkpoint/escalation, update-behind/rebase/push-remediated cleanup, and finding responses. It is target-scoped to the selected PR and any trusted selector outputs already admitted by the run. |

Read-only issue/PR consumers use broker-held read handles and receive no writer
token. Milestone assignment remains on its separately admitted canonical
capability. The compiler recognizes the closed runner-owned adapter kind and
method set for each row; a workflow-authored command cannot select a raw
`UpdateWorkItem`-style handle. The implementation issue for this migration must
inventory shipped workflows, examples, scheduler/gate hooks, terminal cleanup,
and compatibility entrypoints and reject the new routing version if any
`github:issues:write` provider mutation is unassigned. Older workflow versions
remain pinned to direct injection until that inventory and all adapters land
together; curation/nomination alone are not a completed capability migration.

#### Shipped and example issue-write route audit

The route inventory below was checked on 2026-08-17 against every workflow
under `reference-workflows/` and `config-examples/`. A listed entrypoint maps to
one owner in the table above; braces denote identical workflow paths in the two
ACME examples. Entries that currently declare the broad capability but only read issue state
are called out separately and do not acquire an issue-write method in the
migrated route; any PR mutation they perform belongs to the PR route.

| Exclusive owner | Shipped/example workflow paths |
|---|---|
| Proposal executor (curator) | `reference-workflows`: `backlog-curation/curate`; `config-examples/{acme-web,acme-web-claude}`: `backlog-curation/curate` |
| Proposal executor (nominator) | `reference-workflows`: `work-nomination/nominate`, `quality-sprint/nominate`; `config-examples/{acme-web,acme-web-claude}`: `work-nomination/nominate` |
| `backlog-query` | `reference-workflows`: `backlog-curation/{reconcile-backlog,query-backlog,release-claim}`, `implementation/query-backlog`; `config-examples/{acme-web,acme-web-claude}`: `backlog-curation/{reconcile-backlog,query-backlog,release-claim}`, `{default-implement,implementation}/query-backlog` |
| `backlog-health` | `reference-workflows` and `config-examples/{acme-web,acme-web-claude}`: `backlog-curation/{implementation-feedback,sample-ready-pool}` |
| `issue-close-out` | `reference-workflows` and `config-examples/{acme-web,acme-web-claude}`: `implementation/{close-out,park-escalated,park-needs-human}` |
| Backlog-assignment adapter | `config-examples/{acme-web,acme-web-claude}`: `backlog-assignment/assign-backlog` |
| Issue-disposition adapter | `reference-workflows`: `self-update/stage-update`; `reference-workflows` and `config-examples/{acme-web,acme-web-claude}`: `merge-review/{queue-watch,post-merge}` |
| PR-lifecycle bookkeeping adapter | `reference-workflows` and `config-examples/{acme-web,acme-web-claude}`: `merge-review/{reconcile-post-merge,record-merge-refusal}`; `reference-workflows`: `pr-remediation/{update-behind-pr,rebase-pr,remediation-checkpoint,push-remediated,respond-to-findings,park-escalated,park-infrastructure-failure,park-invalid-finding-responses}` |
| No issue-write owner (issue access is read-only) | `reference-workflows` and `config-examples/{acme-web,acme-web-claude}`: `backlog-curation/surface-duplicates`, `merge-review/check-issue-staleness`; `reference-workflows`: `pr-remediation/{gather-pr-context,gather-issue-context,validate-finding-responses}` |

This assignment is exhaustive and disjoint for the audited trees: no
shipped/example issue mutation is unassigned or has two owners. The audit found
no implementation gap requiring a follow-up issue. Adding a workflow entry that
declares `github:issues:write` must update this inventory and select exactly one
owner before the migrated routing version can validate.

The DSL implementation adds runner-owned deterministic run kinds, mutually
exclusive with `run.command`. The initial kinds are `prepare-merge-proposal`,
`prepare-pr-close-proposal`, and the executor kind `apply-proposals`:

```yaml
- name: apply
  type: deterministic
  goal: Apply the validated mutation proposals.
  run:
    kind: apply-proposals
    workspace: repo
  proposalInput:
    from: prepare-merge-proposal
    kinds:
      - repo:merge
  capabilities:
    - github:pr:merge
  next: applied
```

`kind: apply-proposals` is runner-owned code, not a configurable executable. It uses
a repository workspace only for operations such as `repo:push` that must verify a
commit in the run branch; otherwise it may use scratch. The compiler requires
a singular `proposalInput` only on this run kind. Its closed shape is `from`, naming
one stage, plus a non-empty, duplicate-free `kinds` allowlist. The named producer
must dominate the executor in the compiled graph: every path entering the executor
must have completed that exact stage successfully. Each listed kind must map to a
canonical capability declared by both producer and executor. The runner enforces
the producer result's one-artifact cardinality at runtime because artifact count is
not compiler-visible; zero or multiple artifacts is a workflow contract error, and
the executor is not dispatched or credentialed.

The runner, not the producer, assigns the bound pointer's `ContextPointer.Name` as
`proposals`. On resume it uses the exact `proposal.input_bound` record for that
executor visit. A later producer result, an artifact path under `proposals/`, or a
matching advisory media type cannot replace that binding. Dynamic source selection,
command selection, and workflow-authored validator code are invalid.

### Validation and execution

The executor treats proposal bytes as untrusted input and performs these checks in
order:

1. **Pointer and envelope:** require the sole `ContextPointer` named `proposals` to
   equal the fsynced `proposal.input_bound` path/digest and producer attempt, contain
   the path, verify the digest, enforce the media type and byte limit, parse with
   duplicate-key rejection, and validate the closed schema and producer binding.
2. **Capability:** map every proposal kind to its canonical capability and require
   the kind in `proposalInput.kinds` and its capability on both the producer attempt
   and executor stage.
3. **Run scope:** require the normalized repository to equal `repoRef`; require an
   item target to equal `InvocationEnvelope.item` or scope pins explicitly wired
   through `inputsFrom` from a preceding built-in deterministic selector; and reject
   an agentic stage as a scope source. Those typed ids/SHAs plus the runner-derived
   branch are the run's claimed execution scope. Producer-controlled prose,
   proposal fields, and undeclared outputs cannot widen it.
4. **Target allowlist:** merge and close may target only the scope-pinned pull
   request; push may target only the current run branch under the pinned branch
   namespace; issue writes may target only the scoped repository and, except for
   `create`, the scope-pinned issue. No workflow-authored glob or regex can widen
   these sets.
5. **Bounds:** enforce at most 256 KiB of resolved proposal bytes, 32 proposals per
   set, 64 KiB per serialized proposal, and the text/list limits above. A merge,
   close, or push proposal must be the set's sole proposal; only bounded issue
   proposals may be batched, with at most one existing-target proposal per issue in
   a set. That proposal may contain up to eight ordered operations; the executor
   refreshes its observational guard after each confirmed operation before
   attempting the next.
6. **Preflight guards:** acquire the runner's normalized target mutation leases in
   stable target order, then compare every required SHA and provider state with the
   proposal. For push, `sourceSha` must equal the exact runner-resolved tip of the
   current run/workspace branch pinned when `proposal.input_bound` was recorded,
   and that branch must still have the same tip. `remoteSha` must match the current
   remote ref (or the ref must be absent for creation); `fast-forward` additionally
   requires `remoteSha` to be an ancestor of `sourceSha`. For merge, `headSha` must
   match; when the live base differs from `baseSha`, the trusted adapter compares
   the base delta with the PR's changed files and refuses on intersection or an
   indeterminate comparison. Close requires the live head to match `headSha`.
   Existing-target issue writes require the provider's current `updatedAt` to
   match the proposal.

The limits and semantic validators are compiled Go code in the trusted executor.
Workflow definitions cannot replace validators or raise limits. A later declarative
constraint language requires its own design review and must only narrow these
defaults.

Guards have two explicit classes:

- An **authority guard** prevents the call from applying a different immutable
  object than the one authorized. It must be atomic with the provider mutation.
  The initial authority guards are merge/enqueue `headSha` and push `remoteSha`
  (or ref absence). The push sends immutable `sourceSha`, not a mutable branch
  name. If a provider cannot enforce a required authority guard, the executor
  refuses `conditional-mutation-unsupported`.
- An **observational freshness guard** detects stale intent but is not the source
  of mutation authority. The target allowlist, producer/executor capability
  checks, and closed typed request provide that authority. The initial
  observational guards are merge `baseSha`, close `headSha`, and issue
  `updatedAt`, because GitHub offers no compare-and-swap parameter on the
  corresponding close, issue edit/comment/label, or base-revision operation.
  The executor checks them under the shared target lease immediately before the
  narrow provider call and records that the guard mode was `observed`. A human
  or external integration can still race that read; the call therefore uses
  field-specific add/remove, state-only, or comment endpoints and never replaces
  unrelated provider state from the snapshot.

This is a provider-profiled contract, not a lowest-common-denominator fiction:

| GitHub operation | Guard profile |
|---|---|
| Direct merge | `headSha` is an atomic authority guard through the merge endpoint's expected SHA; base freshness is observed with the delta-intersection check. |
| Merge-queue enqueue | `headSha` is an atomic authority guard through `expectedHeadOid`; base freshness and live queue policy are observed under the landing lease. |
| PR close | `headSha` is observational; comment and close endpoints have no conditional resource version. |
| Existing-target issue write | `updatedAt` is observational; edit, comment, label, and state endpoints have no conditional resource version. |
| Push | remote SHA/ref absence is an atomic authority guard through the git ref update or force-with-lease primitive. |

A new provider declares the same profile per operation. It may strengthen an
observational guard to atomic, but may not weaken an authority guard. Rejection
of either a last-moment observation or an atomic provider condition is
`stale-precondition`.

Preflight is set-atomic: the executor validates **every** proposal, including one
freshness snapshot, before applying any. If preflight refuses one, none execute.
The target leases are shared by proposal execution and every runner-owned direct
adapter, so two Goobers paths cannot pass the same observation concurrently; they
do not claim to lock out provider users or other integrations.

Once preflight succeeds, proposals execute in artifact order, and a multi-operation
proposal executes in listed order. Immediately before each call, the executor
re-checks that operation's observed guards while retaining the target lease and
prepares its atomic authority condition where applicable. Under the journal writer
lock it checks the call key, then appends and fsyncs
`proposal.execution_started`. That durable barrier carries the proposal identity,
operation index, normalized target, guard mode and guard fingerprint, normalized
typed-request fingerprint, and provider idempotency key when available. The
executor releases the journal writer lock before network I/O but retains the
target/executor lease.

On success, the executor re-acquires the journal lock and appends
`proposal.operation_applied` with the confirmed external ref/effect. If another
issue operation follows, it also refreshes the item and records the returned or
newly observed `updatedAt`; that exact value is the next operation's observational
guard. This catches provider changes visible between operations and, together with
narrow field-specific endpoints, avoids stale full-resource replacement without
claiming unavailable GitHub compare-and-swap semantics. After every operation is
confirmed, the executor appends `proposal.applied`; for merge it includes the
`merged` or `enqueued` outcome. Recovery first acquires the abandoned executor
lease and target lease, then the journal lock, so it cannot race a live owner.

A changed guard before any confirmed effect records `proposal.refused`. A changed
guard or definitive provider rejection after one or more
`proposal.operation_applied` events records `proposal.partially_applied`, including
the confirmed operation count, failed operation index/code, and remaining
unattempted count. The executor records later proposals in the set as
`proposal.refused` with `set-aborted` and never attempts compensating mutations.
The close comment/close sequence and compound issue operations therefore leave a
truthful recovery trail when only a prefix completed.

Recovery inspects the complete lifecycle under the same writer lock:

- `proposal.applied` makes the proposal a verified no-op. A refused, partially
  applied, or ambiguous terminal event prevents automatic execution.
- `proposal.operation_applied` makes that operation a verified no-op and supplies
  the next observed version when another operation follows.
  `proposal.execution_started`
  without a matching operation event or terminal proposal event **never causes the
  call to be issued again blindly**, even when the provider operation appears not
  to have happened. The executor may perform read-only reconciliation. It appends
  `proposal.operation_applied` only when the provider can bind a confirmed effect
  to the exact journaled request identity. When the provider conclusively proves
  rejection or non-execution, it appends `proposal.refused` if no earlier operation
  was confirmed and `proposal.partially_applied` otherwise.
- A retry is permitted only when the provider contract enforces deduplication for
  the exact derived call key or an equivalent atomic authority precondition; it
  must reuse that key/condition and request fingerprint. `repo:push` treats the
  exact journaled `remoteSha` (or ref-absent condition for creation) this way:
  observing `sourceSha` at the ref reconciles the push as applied, observing the
  original expected state permits the same conditional push, and any other state
  is ambiguous. Without an enforceable mechanism, an outcome that cannot be proven
  either way becomes `proposal.execution_ambiguous` and escalates, carrying the
  count of earlier confirmed operations. This includes issue creation and
  commenting on providers that offer no enforceable idempotency key.
- When reconciliation reaches any refused, partially applied, or ambiguous
  terminal state, it records every later unattempted proposal in the set as
  `proposal.refused` with `set-aborted` before returning the blocked result.

Because the key belongs to the producer attempt, rerunning only the executor cannot
bypass this lifecycle. Explicit intervention must rerun the producer to create new
intent and a new key. Provider-native idempotency narrows ambiguous recovery; it
does not replace the journal barrier.

### Journal and refusal contract

Proposal artifacts continue to produce the ordinary `artifact.recorded` event. The
runner and executor additionally record these conformance-normative lifecycle
events:

| Event | Meaning |
|---|---|
| `proposal.input_bound` | Before credential materialization, pins one executor visit to the producer stage/attempt and exact proposal artifact path/digest. For push it also pins the runner-derived branch ref and exact tip SHA. |
| `proposal.execution_started` | Fsynced immediately before each provider call. Carries the proposal identity, operation index, normalized target, guard mode (`atomic`, `observed`, or `none`) and guard fingerprint, typed-request fingerprint, and provider idempotency key when available. Its absence permits a first call; its unterminated presence requires reconciliation, never blind replay. |
| `proposal.operation_applied` | The provider confirmed one call. Carries its operation index, external ref/effect digest, and, when another operation follows, the returned or newly observed version used as its freshness guard. |
| `proposal.applied` | Every operation in the proposal is confirmed. Carries producer stage/attempt, proposal artifact digest, proposal id/kind, normalized target identity, operation count, and the closed provider outcome when the kind has one (`merged` or `enqueued` for merge). |
| `proposal.refused` | Validation or provider policy rejected the mutation before a confirmed effect. Carries the same identity, the current operation index when execution had begun, and a stable refusal code. |
| `proposal.partially_applied` | One or more operations were confirmed before a later operation was definitively rejected or found stale. Carries the confirmed operation count/effect refs, failed operation index/code, and remaining unattempted count. It is terminal and never reclassified as a refusal. |
| `proposal.execution_ambiguous` | A request may have taken effect but confirmation failed. Carries its operation index and confirmed prior-operation count and is never retried automatically. |

Events carry identifiers and digests, not the proposal body or credentials. The
initial refusal-code vocabulary is:

`malformed`, `unsupported-schema`, `producer-mismatch`, `limit-exceeded`,
`capability-not-declared`, `target-not-allowed`, `outside-claimed-scope`,
`reserved-label`, `stale-precondition`, `conditional-mutation-unsupported`,
`duplicate`, `provider-rejected`, and `set-aborted`.
`proposal.partially_applied.failedCode` uses the same stable vocabulary.

These `proposal.*` events extend the cross-runner conformance surface.
Implementation must add them and their normative fields to `ARCHITECTURE.md` §3.3's
enumeration, the versioned journal schema, telemetry projection, and the shared
conformance fixtures; they must not be hidden in the conformance-excluded
`runner.*` namespace.

A refused set returns `status: blocked` with `error.code: PROPOSAL_REFUSED`; a
partial prefix returns `PROPOSAL_PARTIALLY_APPLIED`; an ambiguous provider result
returns `PROPOSAL_EXECUTION_AMBIGUOUS`. All use the existing `blocked` disposition
to journal the cause, release the claim/workspaces, and route the run to the
escalation ladder. They do not set `outputs.blockedBy`, because a proposal
execution outcome is not an issue dependency. A provider failure known to have
occurred before any mutation may use the executor's infrastructure retry path; a
definitive policy rejection, partial effect, or unknown outcome may not.

## What the runner does on each status

| Status | Runner action |
|---|---|
| `success` | advance the state machine to the next stage/gate |
| `failure` | **Non-retryable escalate disposition first (#415):** if `error.retryable == false` **and** `error.code` is a recognized escalate code (`ISSUE_OVER_SCOPE` / `NEEDS_DECOMPOSITION` / `ISSUE_NOT_APPLICABLE`), bypass the `Next` gate's evaluator and route through its optional `escalate` control branch; without one, terminate directly at `@escalate`. The stage's own `summary` posts to the driving item as the disposition's reasoning (#3363). Otherwise: if `Next` is a gate, advance — the gate branches on the failure (the reviewer-gate pattern); if not (a non-gate stage, terminal, or empty `Next`), the run ends `PhaseFailed`. Never run downstream stages on a failed result, never silently complete. |
| `blocked` | **finish the run `escalated`** (#544/#545) — never a pause. The blocked cause is journaled (`blocked_by_agent`, carrying `error`), the shared escalation notifier preserves that reason on the driving issue, normal terminal cleanup releases the claim/worktrees, and the issue is parked with its ready/claimed markers removed (#539's convention). The park label depends on whether `outputs.blockedBy` named a blocker (#2028): a named, non-cyclic blocker parks `goobers:blocked-on-sibling` (self-healing — see below); an unattributed block, or a detected circular dependency, parks `goobers:needs-human`. If `outputs.blockedBy` names blocking issue numbers, backlog selection also records the block and skips the issue if it is re-promoted before every named blocker closes (#552). |
| `no-work` | finish the run `completed` without evaluating the task's declared next state |

> **Non-retryable escalate disposition (#415, V0.7 ladder remediation L6 —
> `docs/design/v07-ladder-remediation.md` §3.4):** a `failure` result carrying
> `error.retryable == false` **and** a recognized escalate code (`ISSUE_OVER_SCOPE`
> / `NEEDS_DECOMPOSITION` / `ISSUE_NOT_APPLICABLE`) bypasses the `Next` gate evaluator and its repass
> loop after one attempt. When that gate declares an `escalate` control branch,
> the runner follows it so the workflow can perform deterministic disposition
> work before terminating; otherwise it routes straight to `@escalate`
> (terminal `escalated`). It is the signal a human, or a future decomposition
> workflow, selects on. Without it an
> un-scopeable item the stage correctly rejected on attempt 1 re-enters the repass
> loop until the budget exhausts and terminates `aborted`, not `escalated` — the
> V0.6 ladder's over-scope-probe finding. This is a business-disposition route,
> distinct from `Task.Retry` below (which is infra-only). A recognized escalate
> code with `retryable == true`, or a `failure` with an unrecognized/absent code,
> follows the ordinary failure route above.
>
> **Item judgment vs. work failure (#3363):** `ISSUE_NOT_APPLICABLE` is the
> disposition for an item whose premise no longer holds — the issue targets
> files a later change deleted, or asks for work already done. It is a verified
> conclusion ABOUT THE ITEM, not a failure of the work, so re-running the stage
> can only re-derive it. Two consequences follow from recognizing it here.
> First, the refusal is terminal on attempt 1 rather than review-failing an
> empty diff and burning the repass budget. Second, the stage's own `summary`
> is the deliverable: the runner posts it to the driving item as the
> escalation's reasoning, so a correct refusal's citation reaches a human
> instead of living only in the run journal. Emit the citation in `summary`
> (the machine-readable code goes in `error.code`, a short restatement in
> `error.message`). The code classifies as the `item-judgment` error class,
> which status rollups count separately from work failures (#3364).
>
> **Infrastructure faults never charge the work budget (#3361):** a failure of
> the substrate a stage runs ON — credential materialization, git provisioning,
> network transport, host/workspace, claims-lock contention — is not evidence
> about the work. Those failures are marked at their construction site, retried
> on the runner's bounded INFRASTRUCTURE budget (journaled `attemptClass:
> infra`, conformance-excluded), and never decrement `Task.Retry`'s attempts.
> Their terminals carry a typed `infra*` error class, which keeps them out of
> the failure-streak circuit breaker and out of the success-rate denominator
> (#3364) — at attempt budgets of 1, the prior classification converted
> transient infrastructure weather into permanent-looking work failures.
>
> **Reviewer sibling (#415):** at an agentic review gate whose subject is an
> **agentic** stage, a run branch with **no committed change (an empty diff)**
> fast-`fail`s on the first review — resolving the gate's own `fail` branch —
> rather than issuing needs-changes and looping repasses that can only re-observe
> the same emptiness. Mirrors the #316 identical-diff guard: both spare the
> repass budget a degenerate reviewer call. Scoped to an agentic subject so a
> deterministic subject that is not expected to commit to the run branch (e.g.
> merge-review's reviewer, which judges PRs from its stage outputs) still gets a
> real reviewer evaluation on an empty diff.
>
> **`blocked` contract (#544/#545, dependency-not-met — never punish the
> producer for using a documented status):** never repass, never pause — a
> `blocked` result finishes the run `escalated` after one attempt, exactly
> like the non-retryable escalate disposition above. Use `error.code:
> DEPENDENCY_NOT_MET` (or another descriptive code — unlike `failure`'s
> escalate codes, `blocked`'s code is not runner-matched, it's for a human
> reading the journal) and `error.message` naming what's unmet. **To name the
> specific blocking issue(s) so selection can retain a dependency guard (#552),**
> set `outputs.blockedBy` to a **comma-separated string of issue numbers**
> (e.g. `"441,442"` or `"#441, #442"`) — `outputs` is scalar-only by schema
> (§"Where a stage writes its output" above), so do **not** attempt an array
> or object here; a prior live occurrence tried exactly that and was
> schema-rejected, burning a whole attempt for nothing. Omit `outputs.blockedBy`
> when the block isn't attributable to specific open issues — that case parks
> `goobers:needs-human` (nothing to reason about but a human). Naming a
> blocker instead parks `goobers:blocked-on-sibling` (#2028: a self-healing
> dependency wait, not a decision), and `blockedBy` additionally prevents
> premature re-selection if the item is re-promoted while a named dependency
> remains open. **Never name the driving issue itself (#2961)** — an issue
> cannot be its own dependency. A self-reference is normalized away before the
> block is recorded (`#441`, `owner/repo#441` and `441` all match item 441), a
> `runner.annotation` of kind `blocked_by.self_reference_dropped` records the
> run, stage and item, and if it was the only entry the block is treated as
> unattributed and parks `goobers:needs-human`. Persisted-graph self-loop
> handling is unchanged, so legacy or corrupt records still surface as cycles.
> See `docs/design/needs-human-taxonomy.md` for the full model,
> including the circular-dependency exception (still `goobers:needs-human` —
> it can't self-heal).

> **A tool you could not call is not an organization policy (#2962).** Do not
> report `blocked` on organization content exclusion because a tool call was
> refused. Content exclusion is a policy fact the runtime states explicitly;
> a bare permission refusal (e.g. `Permission denied and could not request
> permission from user`) is an infrastructure fault an operator can fix, and
> reporting it as a policy block parks the driving issue for a human who has
> nothing to decide. The executor enforces this rather than trusting the
> classification: a `blocked` result whose prose claims content exclusion is
> rejected unless the captured transcript or stderr carries an explicit
> runtime content-exclusion signal, and becomes a `failure` carrying
>
> | Observed | `error.code` | Meaning |
> |---|---|---|
> | a runtime tool-permission refusal | `HARNESS_TOOL_PERMISSION_DENIED` | grant the tool to the goober, or fix the harness invocation; `outputs.toolPermissionDenied` is `true` and the refusal lines are quoted in `error.message` |
> | no runtime signal at all | `UNSUBSTANTIATED_CONTENT_EXCLUSION` | the classification was inferred, not observed |
>
> Both are non-retryable (the identical invocation reproduces the identical
> refusal), both set `outputs.contentExclusionClaimRejected: true`, and both
> preserve your original summary and error detail inside `error.message` — no
> cause is ever invented or discarded. Blocks that do not mention content
> exclusion are untouched. The effective CLI version and tool/permission
> arguments for every Copilot session are recorded at
> `.goobers/copilot-invocation.json` in the workspace, so a refusal can be
> attributed to the invocation after the fact.

`Task.Retry` (declared retry policy, attempt budget, backoff) governs only
**dispatch/infra errors** — a Go error returned by the executor, not a
business `failure`/`blocked` `ResultEnvelope`. Each policy-driven retry
attempt is a new journal entry, never overwritten history (§5). A business
`failure`/`blocked` result is never retried by `Task.Retry`; it is handled
per the table above.

**Agentic session timeout & `Task.OnTimeout` (#724).** An agentic stage's
harness session is bounded by a wall-clock timeout (currently a flat 30m,
`internal/harness.DefaultTimeout`; not yet per-stage configurable — #151 is
the natural home for a DSL-expressed limit). A timeout is a dispatch error
(marked `invoke.IsTimeout`), so by default it consumes `Task.Retry` budget
and, when exhausted, discards the run — historically throwing away real,
committed, in-progress work whose only unfinished step was CI verification.
`Task.OnTimeout` selects that behavior:

- `""` / `fail` (default) — discard the timed-out attempt and let `Task.Retry`
  run; fail the run once the budget is exhausted.
- `salvage` — on a session timeout, if the run branch carries a **viable
  committed diff** (`git diff base...HEAD` is non-empty), complete the stage
  with that diff and advance to `Next` (normally the reviewer gate, then the
  deterministic `local-ci` stage that owns `make ci`) instead of discarding
  the run. A pre-commit timeout (empty diff) has nothing to salvage and falls
  back to the `fail` path. Only valid on an **agentic** stage (the compiler
  rejects it on a deterministic one), whose deliverable is its committed diff.
  A salvaged completion records a `salvage-on-timeout.json` provenance marker
  and sets `outputs.salvagedOnTimeout = true`.

Salvage is the complement to bounding the implement session to *think-time*:
the implementer is instructed to run only fast, targeted verification and let
the deterministic `local-ci` stage own the full `-race` suite, so the session
should not spend its budget on test wall-clock in the first place — and if it
still times out mid-flight, the committed work is not lost.

For gates, the evaluator returns a `Verdict` (`decision` ∈ `pass` / `fail` /
`needs-changes`, plus `rationale`, `evidence[]` artifact pointers, and
`findings[]`); the gate maps the decision to a branch. A gate outcome with no
defined branch is an error, never a silent pass.

**Repass context obligation (#412).** When a gate's branch routes back to a
stage the run already dispatched (a repass — most commonly `needs-changes` →
`implement`), the runner attaches that gate's just-recorded `Verdict` as a
`contextPointer` on the repass invocation, named `<gate>.verdict`, via the
same pointer-only mechanism "Artifact passing" above describes for any other
upstream artifact — never the raw `ResultEnvelope`, never a schema change. A
repassing stage that reads the reviewer's actual rationale/findings can
address them directly, rather than re-inferring "something needs to change"
from the diff alone.

## Versioning & unknown-field policy

- The contract version is `v1alpha9` (`StageContractVersion`). The Go types retain
  the stable `api/v1alpha1` import path; the constant and `api/schemas` set identify
  the current wire contract. Version `v1alpha2` added the optional `triggerRef`
  invocation field for bounded scheduler trigger provenance; `v1alpha3` adds the
  optional `additionalWorkspaces` invocation field — read-only reference-repo
  checkouts for a gaggle's `additionalRepos` (MGV-11 #1286); `v1alpha4` admits
  `no-work` through the closed result schema for both deterministic and agentic
  stage producers; `v1alpha5` adds paired branch attribution to artifact context
  pointers used at parallel joins; `v1alpha6` admits the optional `repoRef.project`
  invocation field for Azure DevOps repository identity; `v1alpha7` adds
  input-integrity grades to invocation items, context pointers, and artifact
  pointers, plus the stage's declared minimum; `v1alpha8` adds the optional
  `checkoutCones` invocation field declaring a stage's sparse-checkout cones
  (project.checkout.sparse, #649); `v1alpha9` adds attempt, ownership,
  policy-action, nested-policy, and runner-authored parent-authority fields for
  mechanically enforced nested agents.
- Schemas are **closed**: unknown fields are a validation error. This is
  deliberate — it is what makes reach-through impossible and keeps the seam tight.
- Additive or breaking changes bump the contract version rather than loosening a
  schema. Validate an envelope with `api/validate.(*Validator).ValidateEnvelope`.
