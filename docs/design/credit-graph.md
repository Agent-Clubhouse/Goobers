# Credit graph contract

> Status: **implemented for per-run attribution and cohort evidence.** The graph
> contract and provenance capture landed for #4077, credit propagation and
> failure-cause classification for #4078, and the read service now calls
> `creditgraph.Build` and `creditgraph.Attribute` while constructing stored
> attribution observations for EffectiveVersion/workload cohorts.
> Delivered-by: #4077, #4078, #6355
> Scope-delta: none; #6355 added the conformance tests and the compatibility contract that were the remaining reconciliation work.
> Verified: 324c31ac6 (2026-10-02)
>
> The older `internal/readmodel/credit.go` implementation remains a separate
> cross-run operational ranking. See
> [Relationship to `internal/readmodel`](#relationship-to-internalreadmodel)
> for the boundary between these paths, and
> [Conformance and compatibility](#conformance-and-compatibility) for where
> their answers must agree and how existing credit data carries over. That
> reconciliation is tracked by
> [#6355](https://github.com/Agent-Clubhouse/Goobers/issues/6355).

## Workflow enrollment

For an operator walkthrough of enabling Backprop, reading attribution and
fault-audit findings, and verifying fixes, see the
[Backprop user guide](../guides/backprop.md).

Backprop is opt-in per workflow on DSL 3.0:

```yaml
dslVersion: "3.0"
spec:
  backprop:
    enabled: true
    version: v1
```

Omitting `backprop`, or setting `enabled: false`, performs no attribution work.
For an enrolled workflow, terminalization durably appends `run.finished` and
then writes `attribution.json` beside the run journal. The record pins the workflow identity and digest, EffectiveVersion,
workload, run ID, contract version, deterministic attribution, and an explicit
`complete`, `insufficient-evidence`, or `failed` analysis status. Analysis is
read-only and best effort: a failure is reported independently and never changes
the run's phase, gate verdict, CI admission, or publication path.
EffectiveVersion includes the pinned workflow and goober digests plus the
recorded model and harness version; readers use that persisted value unchanged
for every analysis status so incompatible runs cannot enter the same cohort.
Runs containing more than one model/harness pair have no defined
EffectiveVersion and are excluded from cohort aggregation.

## Cross-workflow fault audit

The read-only Backprop fault auditor consumes only persisted records from
enrolled workflows. Its default seven-day window, sample floor, observation
cap, finding budget, evidence/run caps, and cooldown make every pass bounded;
the output is always `report-only` and never files an issue, edits a workflow,
or rolls back a run. Operators can select a narrower gaggle, workflow, or time
window through the existing telemetry query scope. The observation cap counts
enrolled records, not terminal rows: readers continue through unenrolled rows
under a separate finite scan budget so sparse enrollment cannot hide later
evidence.

One stable signature produces one deterministic finding across duplicate runs
and repeated passes. Shared daemon, worktree, scheduler, journal, claim,
admission, publication, recovery, or shared-CI failures must cross unrelated
workflow boundaries before they are assigned to Goobers product reliability.
Harness, model, provider, credential, network, filesystem, operating-system,
and similar shared failures route to the external component owner. A
workflow-definition finding must remain localized to one workflow,
EffectiveVersion, and attributed node path. Sparse samples, missing exact
provenance, contradictory evidence, mixed candidate domains, and failures that
do not cross the required boundary remain `mixed-or-unknown` with reduced
confidence.

Each finding carries exact run/journal/artifact links, affected workflows and
versions, node paths, environments, counter-evidence, rejected or retained
alternative domains, and a recommended owner/action. Product reliability,
external-component, workflow remediation, and mixed/unknown findings are
separate fields on the telemetry read surfaces and candidate-findings artifact.
A supplied fix marker moves a finding through `verification-pending`,
`recovered`, or `repeated` using post-fix observations; absence of a post-fix
cohort never counts as recovery. Stored audit passes durably record report
cooldowns and operator fix markers in `scheduler/backprop-audit/state.json`;
cooldowns are scoped by gaggle and workflow so a narrow read cannot suppress a
broader cross-workflow classification. Only the candidate-findings pass that
feeds filing applies and records the cooldown; the daemon's telemetry stats
read API (`GET /api/v1/telemetry/stats`) and the other status/read surfaces
show every current finding and record only
verification baselines, so viewing a finding never hides it from the filing
pass. (The `goobers telemetry stats` CLI reads only the local rollup and does
not include attribution cohorts or fault-audit findings.) Unfixed cooldowns and baselines older than 30 days are pruned; fix
markers and the baselines they verify against are kept. Operators record a deployed fix with
`goobers telemetry mark-fix --finding=<backprop-id>` (optionally pinning its
RFC3339 deployment time with `--applied-at`). This is auditor metadata only
and does not mutate workflows, issues, or run journals.

## Ground-truth labels

Terminal phase is not the same as correctness: a run can succeed and still be
wrong, or fail for reasons the user does not care about. Operators record their
own verdict for an enrolled run with

```sh
goobers telemetry label --run=<run-id> --outcome=correct|incorrect \
  [--reason=TEXT] [--by=NAME] [--labeled-at=RFC3339] [path]
```

Each label is appended to `labels.json` (schema
`goobers.dev/backprop/labels/v1`) beside the run's `attribution.json`, with
provenance: who recorded it (`--by`, defaulting to the OS user), its source
(`human` today), when the verdict was reached (`labeledAt`), and when it was
stored (`recordedAt`). Only runs that already carry an attribution record can
be labeled. The history is append-only and keyed by a content-derived ID, so
re-recording an identical verdict is a no-op and an earlier label is never
rewritten.

Labels may land long after the run. Aggregates are recomputed from the stored
records on every read, and each run contributes its *effective* label: the one
with the latest `labeledAt`, ties broken by ID. That rule is independent of the
order labels were recorded in, so a late label re-scores its cohort
deterministically. Cohort aggregates expose a `groundTruth` summary
(`labeledRunCount`, `correctRunCount`, `incorrectRunCount`) beside `runCount`,
omitted when no run in the cohort is labeled, so partial label coverage stays
visible rather than being presented as the whole cohort.

The effective label also weights scores. A correct verdict confirms a run's
journal-derived attribution and an incorrect verdict contradicts it:

| Effective label | Run weight | Run confidence `c` becomes |
|---|---|---|
| none | 1 | `c` |
| `correct` | 2 | `c + (1 - c) / 2` |
| `incorrect` | 0.5 | `c / 2` |

A cohort's `topContributingPaths[].share` and `.confidence` are weighted means
over the cohort's runs using the run weight and calibrated confidence, and a
fault-audit finding's `confidence` is the weighted mean of its signals'
calibrated cause confidences (before the existing sparse, provenance, and
mixed-domain caps). Unlabeled cohorts score exactly as before. Labels do not
change fault-domain classification, finding IDs, or verification state.

Labels are read-only inputs. Recording one never touches the run journal, the
attribution record, the run's phase, or any gate verdict.

Not yet implemented (follow-ups under #7121): declaring ground-truth sources in
workflow or gaggle config, deferred signal sources (reverted PR, R-SZZ traced
bug, reopened issue, main CI breakage), user-provided checker commands, and
feeding labels into per-run credit propagation (`attribution.json` itself).

## Why

Credit assignment needs one shared answer to "what produced this outcome, and
through which nested execution element?". Today the journal records the pieces
— runs, stages, nested agent lifecycles, transcript spans, artifacts, gate
verdicts — but nothing joins them into a graph a consumer can traverse, and
nothing states which joins the journal does *not* support. Without that second
half, a consumer silently guesses, and a guessed edge is indistinguishable from
a recorded one once it lands in an aggregate.

## Contract

`internal/creditgraph` defines the graph and materializes it for one run.

**Nodes** (`NodeKind`): `outcome`, `run`, `stage`, `subagent`,
`model-invocation`, `tool-call`, `tool-result`, `tool`, `evidence`,
`evaluator`, `runtime`, `environment`.

**Edges** (`EdgeKind`), always pointing from the containing or causing element
to the nested or caused one: `attributed-to` (outcome → run), `contains`,
`delegates` (subagent → subagent), `invokes` (model invocation → tool call),
`uses` (tool call → shared tool identity), `produces` (tool call → tool result,
stage → evidence), `evaluates` (evaluator → stage), `depends-on`.

The root is the run's final outcome, so `Graph.Walk` from `Graph.RootID`
traverses downward from the outcome into every nested execution element. A
node reachable from several parents — the shared `tool` identity is the normal
case — is visited once per traversal, so the graph is a DAG, not a tree.

**Provenance.** Every node and edge carries `ProvenanceRecorded` or
`ProvenanceUnknown`. Unknown means the element is referenced by something the
journal recorded but has no record of its own: a stage that never journaled
`stage.started`, an agent named as a parent that never journaled a lifecycle, a
span whose content is unavailable, a tool result whose call id matches no
recorded call *in its own span*. Each one also appends a `Gap` naming what is
absent. Per-record nodes — model invocations, tool calls, tool results — are
identified by the owning span's content digest as well as the owner and record
index, so a subagent that emits several spans (one per stage attempt, say)
keeps each span's records distinct and a call id reused across spans is never
joined to another span's call. The projection never infers a link to close a
hole, so a partially instrumented run
projects a smaller, honest graph instead of a complete-looking, fabricated one.
("Read model" in this document's earlier revisions meant this journal→graph
projection, not `internal/readmodel`; the two are unrelated, which is exactly
the confusion #4523 removed.)

## Provenance capture

The graph is built from what a run already emits: `run.started`/`run.finished`,
`stage.started`/`stage.finished`, `agent.lifecycle` (which already carries
`id`, `parentId`, `dependsOn`, stage, attempt, and resolved model),
`artifact.recorded`, `gate.evaluated`/`gate.overridden`, and `span.recorded`
transcript spans in the `goobers.dev/telemetry/genai-event/v1` shape, whose
records supply model invocations, tool calls, and tool results.
Recorded telemetry span attributes also project harness versions as `runtime`
nodes and deployment identities as `environment` nodes. Missing attributes do
not invent components; when present, each component retains the exact
`span.recorded` journal sequence and span artifact reference used as evidence.

The one link the journal did not carry is *which subagent a transcript span
belongs to*. The harness executor now appends an additive `runner.annotation`
of kind `credit-span-provenance` naming the span digest and the stage's single
root nested agent. It is runner-namespace bookkeeping, excluded from
conformance, and it is emitted only when that root is unambiguous — otherwise
the span's owner stays an explicitly unknown subagent node rather than a
guessed one.

## Consuming the graph

```go
graph, err := creditgraph.Build(creditgraph.Input{
    RunID: runID, Gaggle: gaggle, Workflow: workflow,
    Events: events, SpanData: spanContentByDigest,
})
graph.Walk(graph.RootID, func(node creditgraph.Node, depth int) bool { ... })
```

`SpanData` is optional: a caller that cannot cheaply read span content gets the
run's structure with the model and tool layer reported as gaps. Nodes, edges,
and gaps are in deterministic construction order, so two builds of the same
journal compare equal.

## Credit propagation

`creditgraph.Attribute` computes the credit assignment over a built graph
(#4078). It answers two questions, and refuses to answer either where the
journal is silent.

**Signed contribution with uncertainty.** Responsibility starts as one unit of
mass at the outcome and is split down the graph's edges, weighting an edge
higher when the child's own recorded signal agrees with the outcome's direction
and lower when it disagrees. Each `Contribution` reports the resulting `Share`
in [0,1] and a `Score` that carries the direction the node is estimated to have
pushed: a tool result that succeeded inside a failing run scores positive, the
failing stage scores negative. `Uncertainty` rises with unrecorded nodes,
unknown-provenance edges, gaps on the node, and unrecorded elements below it,
and `Confidence` is its complement. Nodes are emitted in graph order and every
number is rounded, so two attributions of one graph compare equal.

**Failure-cause classification.** For each stage the journal recorded as
failing, `Attribute` emits `CauseFinding`s drawn from a fixed taxonomy:
`bad-tool-choice`, `bad-tool-result`, `bad-interpretation`,
`weak-instructions`, `routing`, `model`, `topology`, `environment`, and
`unknown`. Each finding names the node it attributes, the assumptions the rule
rests on, and the evidence behind it. Contradictory signals — a passing
evaluator verdict on a failing stage, successful tool results beside a failing
one — lower a finding's confidence and are recorded as assumptions rather than
resolved by fiat. A repeated stage attempt whose outcome differed is recorded
as `intervention:` evidence and is the only entry that is more than
correlational.

Missing provenance never becomes blame: a stage whose own record is absent,
whose subtree is mostly unrecorded, or whose only work is behind a gap yields a
single `unknown` finding with reduced confidence, and a run that failed with no
failing stage yields an `unknown` finding on the outcome rather than one pinned
on whichever stage is present.

## Relationship to `internal/readmodel`

Two packages compute "which node contributed to bad outcomes", with distinct
production responsibilities.

| | `internal/creditgraph` | `internal/readmodel` (`credit.go`, `causal.go`) |
|---|---|---|
| Landed | #4077 (2026-08-31), #4078 (2026-09-02) | #2957 (2026-08-15), extended by #3545 |
| Scope | One run: a typed DAG projected from that run's journal, plus signed contribution, uncertainty, and a failure-cause taxonomy | Cross-run: a SQLite rollup over `run_node`/`run`, ranking nodes by routed/failure/escalation/retry-waste counts in a time window |
| Provenance model | Explicit `ProvenanceRecorded`/`ProvenanceUnknown` plus `Gap` records; refuses to infer a missing link | Counts what the projection recorded; no per-edge provenance concept |
| Production callers | `internal/readservice/telemetry_attribution.go` reconstructs stored runs with `Build`, computes `Attribute`, and feeds cohort aggregation and evidence surfaces | `goobers telemetry query --aggregate credit-assignment` (`cmd/goobers/telemetryquery.go`), the read API (`internal/readservice/telemetry.go`), and the portal through it |

So today:

- **`internal/readmodel` remains the aggregate operational ranking path.**
  Existing credit-assignment queries and portal summaries continue to use its
  SQLite rollup.
- **`internal/creditgraph` is the per-run evidence path.** The read service
  reconstructs selected stored runs, computes provenance-aware attribution,
  and aggregates those observations into the cohort and contributing-path
  surfaces.

## Conformance and compatibility

Tracked by #6355 (split from #4523, which closed the production-wiring half).

### Where the answers overlap

Both paths are projections of the same run journal, so where they answer the
same question they must agree. `TestCreditGraphConformsToReadmodelRollup`
(`internal/creditgraph/readmodel_conformance_test.go`) builds both from one
journal and pins three rules:

1. **Routed stages.** The stages `creditgraph.Build` records for a run are
   exactly the `stage` rows `readmodel.ProjectRun` writes to `run_node` for it.
2. **Cause location.** Every stage `creditgraph.Attribute` names in a
   `CauseFinding` is a stage the rollup routed that run through. When the
   rollup counts the run as a failure, it charges that failure to the stage
   creditgraph blames (the rollup also charges every other routed node; it has
   no per-node blame).
3. **Aborts.** A run whose last gate routed to `@abort` is a failure in both:
   the rollup counts it in `FailureRuns`, and creditgraph reads a failed
   outcome.

### Where they deliberately do not overlap

- **Failure outside an abort.** The rollup's failure signal is the run's last
  gate verdict and target. Creditgraph's is the `run.finished` status. They
  differ in three ways, and the conformance test pins each one so none can
  change silently:
  - A run that fails inside a stage with no failing gate is a failed outcome
    in creditgraph, and routed but not failed in the rollup.
  - A rejecting gate that routes to `@escalate` is a failure (and an
    escalation) in the rollup. Creditgraph reads an `escalated` outcome as
    neutral.
  - A rejecting gate that routes back to a stage, after which the run
    completes, is still a failure in the rollup, because the last gate verdict
    was a rejection. Creditgraph reads the completed outcome as a success.
- **Nested elements.** Subagents, model invocations, tool calls, tool results,
  runtime and environment nodes, and provenance gaps exist only in
  creditgraph. The rollup has no node below stage or gate.
- **Gates.** The rollup projects a gate as a `gate` node. Creditgraph models
  gate evaluation as an `evaluator` node that judges a stage, so gate identity
  is not compared.
- **Cross-run ranking and causal estimates.** `CreditAssignment` and
  `CausalCredit` aggregate across runs in a window. Creditgraph cohorts
  aggregate by EffectiveVersion and workload over enrolled runs only. Neither
  is computed from the other, and their numbers are not comparable.

### Compatibility and migration of existing credit data

No credit data migrates between the two stores, and none needs to:

- The rollup tables (`run_node`, `run_node_parent` in the read model) are a
  derived projection of run journals. They are rebuilt from the journals, not
  from creditgraph, and their meaning does not change.
- Per-run attribution is the `attribution.json` record written at
  terminalization for a workflow enrolled in `backprop` (DSL 3.0). The read
  service reads that record, and rebuilds the graph from the journal only for
  evidence links and environment labels. Runs with no record (unenrolled, or
  finished before enrollment) are skipped. They are not backfilled from the
  rollup, because the rollup lacks the provenance that attribution requires.
- Consumers keep their current path. The aggregate credit-assignment query,
  the read API's causal credit, and the portal summaries read the rollup.
  Cohort, contributing-path, and fault-audit surfaces, and `goobers trace`'s
  attribution view, read creditgraph records. A
  consumer that moves from one to the other must treat the non-overlapping
  cases above as different answers, not as drift.
- If a later change makes the two failure signals agree, that change belongs
  in `readmodel.ProjectRun` and must update the pinned divergence cases in the
  conformance test in the same commit.
