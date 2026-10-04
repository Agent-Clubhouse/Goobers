# Design: Source-owned backlog and objective workbench

> Status: **draft** — accepted product direction, proposed contracts and delivery tasks.
> Program: [Human interaction and advanced workflows](hitl-advanced-workflows-program.md).
> Verified: 04198152b63d228a9714ae2f92a7dca079ba5213 (2026-10-03)
> Area: providers, source metadata, portal
> Delivery order: after child workflows, interactive operations, and durable start queues.
> Task IDs: `HAW-BKL-001` through `HAW-BKL-009`; issue creation follows the merged design.
> Related: [Interactive factory operations](interactive-factory-operations.md),
> [Gaggle events and durable start queues](gaggle-events-and-durable-start-queues.md),
> [Agent-authored child workflows](agent-authored-child-workflows.md).

## 1. Product contract

The portal provides a workbench for browsing and editing a gaggle's backlog,
objectives, and their explicit relationships. People can organize work, inspect its
context, connect it to objectives, and navigate to the documents, PRs, and runs that
explain it. Browse and edit are both part of the first version.

Backlogs and repositories remain authoritative. Goobers maintains a rebuildable
index for navigation, search, and joins. An accepted relationship must exist in a
configured source; losing the index must not lose the relationship.

An objective may be an ADO Epic/Feature, a GitHub issue or milestone, or a Markdown
document in a configured code, wiki, workflow, or strategy repository. These sources
may span repositories and providers inside **one gaggle**. Cross-gaggle objectives,
links, searches, and mutations are out of scope.

Humans create explicit links. Agents may suggest links during item creation or
curation; a suggestion becomes accepted only through an authorized source mutation.
V1 supplies organization and navigation. It does not calculate objective progress,
infer achievement from closed issues, score outcomes, or set roadmap priorities.

Interactive writes require both the gaggle's credential policy and the human's
per-gaggle operator authority. View authority permits only the configured read
scope. A gaggle without the interactive policy remains read/monitor only within
its existing authorized read surface; possession of a daemon credential grants
neither a human role nor a new browsing scope.
New provider-backed workbench reads require the explicit interactive binding and
viewer grant; existing monitoring does not supply an automation-token fallback.

## 2. Current implementation and gaps

The baseline is the source revision above, not unchecked parent-issue checklists.

| Existing seam | Current behavior | Use in this design |
|---|---|---|
| `internal/readservice/workitems.go` | Lists recorded provider mutations and related runs/PRs; an item without recorded actions is absent | Preserve as execution history; add source browsing rather than treating history as a complete backlog |
| `internal/httpapi/telemetry.go`, `portal/src/pages/WorkItemsPage.tsx` | `/api/v1/work-items` and detail routes expose that recorded-action view | Add a gaggle-scoped workbench and link the existing history |
| `providers/workitem_ancestry.go` (#6125, closed) | Bounded native-parent traversal with cycle, scope, omission, and completeness reporting | Reuse native ancestry semantics and bounds |
| `providers/github_workitem_ancestry.go` | Reads the GitHub parent-issue relation | Source for issue hierarchy |
| `providers/github.go` | Legacy `WorkItem.Parent` maps a milestone | Keep milestone membership distinct from parent-issue hierarchy |
| `providers/github_backlog.go` | Reads children/blockers and attaches native edges after revision checks | Reuse behind declared provider operations; inspect uncertain outcomes |
| `providers/ado_workitem_ancestry.go`, `ado_blockers.go` | Reads native hierarchy and predecessor relations | Read foundation; matching graph-write coverage is incomplete |
| `providers/model.go` | General edits carry `ExpectedRevision`; mutation idempotency differs by operation/provider | Extend through explicit contracts and conformance tests |
| `providers/workitemgraphfields.go` | Creation rejects `Parent`/`Links` before mutation on every provider | Do not assume create-item also publishes its requested graph |

The provider interface has no general unlink contract. ADO graph publication and
node/edge reconciliation remain related to #5245. UI availability must follow the
actual operation capability, including removal, rather than the provider name.
See [Provider contract and conformance](provider-contract-conformance.md).

## 3. Sources and identity

### 3.1 Gaggle configuration

Add a versioned, validated workbench source configuration to the gaggle contract.
It names existing permitted repository/backlog bindings, source type, read scope,
objective selection, writable field/edge policy, and any manifest owner. Schema
spelling is finalized with `HAW-BKL-001`; unknown fields are rejected.

- Backlog sources bound project/repository, item types, and optional area/label scope.
- Document sources bound repository, tracked branch, and path/include patterns.
- A wiki must expose a supported, configured repository or adapter; an arbitrary URL
  is a reference, not permission to crawl a site or write to it.
- A manifest owner is an explicit repository/path in this gaggle. There is no
  implicit central repository and no automatic migration of native relationships.
- Admission checks configured sources against credential audience and resource
  grants. It rejects ambiguous owners and cross-gaggle references before scanning.
- Existing configurations remain unchanged when the feature is absent.

### 3.2 Stable identity versus current location

Every projected node carries `gaggleId`, `sourceBindingId`, `kind`, stable source
identity, current locator, source revision, observed time, and integrity/provenance.
Do not identify items with a title, URL, or unqualified issue number.

Provider objects use the configured endpoint and immutable provider identity where
available. Keep human numbers, repository/project, URL, and provider move/redirect
information as locators. Resolve a provider move only with source evidence; an
inaccessible old location must not be guessed to be a new object of the same name.

Markdown objectives have a persistent `objectiveId` in lightweight frontmatter:

```yaml
---
goobers:
  schemaVersion: "objectives/v1"
  objectiveId: "obj-8e1bc91c-94cb-4c5c-9412-d7e07e86d5da"
  title: "Reliable payment processing"
---
```

The ID is generated once, unrelated to a file path, and retained on rename/move.
The enclosing gaggle scopes resolution. Two live documents declaring the same ID
are a visible conflict, not last-writer-wins. Display ordinary Markdown without
frontmatter as reference material; adding objective identity is a reviewed edit.
Preserve unrelated frontmatter and document content on metadata-only changes.

For a native objective, preserve its provider identity rather than requiring a new
frontmatter-equivalent field. A configured manifest may give it a portable alias;
the alias maps to one source object and must not create a second objective record.

## 4. Relationship semantics and source ownership

### 4.1 Distinct edges

| Edge kind | Meaning | Direction and ownership |
|---|---|---|
| `parent-of` | Work decomposition/hierarchy | Native provider relationship when available; one hierarchy parent per child under that provider's contract |
| `blocked-by` | Scheduling dependency | Dependent points to blocker; does not imply hierarchy or contribution |
| `contributes-to` | Item explicitly contributes to an objective | Item points to objective; multiple objectives are allowed |
| `references` | Supporting document/context | Node points to reference; no claim of completion or implementation |
| `milestone-member` | Provider-native grouping | Item points to milestone; separate from native parent issue |
| `implemented-by` | Source-declared PR association | Item points to PR; preserve provider semantics, including closure versus ordinary reference |
| `observed-in-run` | Execution provenance | Derived from durable run/journal records; never promoted to an authored planning edge |

An objective represented by a milestone may have both membership and explicit
contribution edges; neither silently manufactures the other. A document link or
mention alone does not imply `contributes-to`. Render inverse navigation from the
canonical edge instead of storing a second inverse relationship.

### 4.2 One source location owns each accepted edge

Resolve ownership by this preference, once, for an edge's semantic kind and scope:

1. Use the provider-native field/relation when it faithfully represents the kind.
2. For a document-owned explicit relationship, use its versioned frontmatter.
3. Use an explicitly configured source manifest for cross-provider or unsupported
   native representation; record the edge once in that manifest.

This is representation preference, not permission fallback. If a native owner is
selected but its write API is unsupported or credentials deny it, report that fact;
do not quietly create a second relationship in a manifest or an issue comment.
Changing an owner is an explicit, reviewable migration with duplicate detection.

An optional repository manifest contains a schema version, source/object aliases,
and edges with stable `edgeId`, `kind`, qualified `from`/`to`, and optional rationale.
Aliases and edges are data, never executable instructions. Frontmatter-owned
edges use the same edge shape with their document as `from`. An index record stores
the owner binding/path or native field, source revision, and edge ID when present.

For example, a strategy repository manifest can own an ADO item → GitHub objective
contribution. ADO native parentage stays in ADO; its inverse display in the portal
is computed. A document's children are not written both in its frontmatter and in
the manifest. Conflicting authorities are surfaced and write-blocked until resolved.

## 5. Shared reads and rebuildable projections

Use the provider read layer from [durable start queues](gaggle-events-and-durable-start-queues.md).
The workbench must not add independent unbounded provider polling or a second cache.

- Cache keys include endpoint, source binding, credential identity/scope generation,
  gaggle, and query/field projection. Authorization is checked before returning data.
  There is no cross-gaggle reuse, even when the underlying credentials match.
- A broad daemon read must not become a narrower human's cached result. Revocation
  invalidates or hides affected projections immediately, including search/counts.
- Scans have item/page/byte/time budgets and resumable cursors. Parallel reads share
  provider quota accounting, retry/backoff, conditional requests, and coalescing.
- Prefer changed-object/event refresh with bounded reconciliation. Expose `asOf`,
  completeness, continuation cursor, source errors, and retry timing in the read API.
- An empty authorized result, unindexed source, partial scan, access denial, and
  unavailable provider are distinct. A truncated child list is never a complete tree.
- Pin document reads to a source revision. Publish a coherent projection generation;
  partial failed rebuilds must not tombstone unseen objects or erase their edges.
- Native deletions and document moves are confirmed by successful authoritative
  reads. Access loss hides content without inventing source deletion.

The index stores node/edge projections and search aids; it owns no planning truth.
Rebuild from sources restores accepted organization. Audit receipts and transient
commands may be durable Goobers records, but cannot be the only copy of an accepted
edge. Proposed repository edits remain pending until the source branch is merged.

Initial configurable bounds are 32 source bindings per gaggle, 1 MiB per parsed
document/manifest, 100 records per API page (maximum 200), and 500 items/20 provider
pages/30 seconds per scan slice, whichever occurs first. Cursors retain unfinished
scan work; reaching a bound reports partial coverage rather than a complete graph.
The workbench projection is capped at 256 MiB per gaggle across visibility
partitions, with 50,000 nodes and 200,000 edges. Body/blob caching uses the shared
read layer's separate byte limits. These are proposed operational defaults.

Unaccepted suggestions are capped at 10,000 records and 16 MiB per gaggle and expire
after 30 days without action. Suggestions referenced by unsettled commands remain
pinned; new suggestions receive a capacity refusal when pins exhaust the bound.
The production maintenance loop runs each minute, deleting expired/unreferenced
projections and suggestions in batches of 250 rows or 50 milliseconds. Eviction
marks source coverage incomplete and never deletes source content. It must not
remove command/reconciliation evidence or accepted source edits awaiting refresh.
Add metrics and saturation/rebuild/pruner tests; a TTL declaration alone is not
an implementation of bounded storage. Operational receipts follow the interactive
service's retention contract, not suggestion expiry.

## 6. Read API and portal

Define additive gaggle-scoped route IDs in `internal/apicontract`, with generated
portal clients, discovery metadata, and bounded query schemas. Proposed resources:

- `GET /api/v1/gaggles/{gaggle}/workbench/sources`: availability, scope, freshness.
- `GET /api/v1/gaggles/{gaggle}/workbench/nodes`: source/kind/state/objective filters,
  bounded text query, opaque pagination cursor, and projection generation.
- `GET /api/v1/gaggles/{gaggle}/workbench/nodes/{nodeKey}`: source fields, owner and
  revision, available actions, explicit edges, and paged execution-history links.
- `GET /api/v1/gaggles/{gaggle}/workbench/edges`: bounded neighborhood by node/kind;
  no graph-wide transitive fetch on opening a page.

Route spellings are proposed; versioned response semantics are required. `nodeKey`
is an opaque qualified identifier, not arbitrary URL input. Existing `/work-items`
history routes retain compatibility and do not gain hidden provider-write behavior.

The portal starts with list, objective outline, and detail views. Show original
provider type/state, source location, freshness/completeness, typed relationships,
and linked documents, PRs, runs, and action history. Expand hierarchy on demand.
Keep hierarchy, dependencies, contribution, and references visually distinguishable.
Search and filters work for items never processed by Goobers.

An item detail supports permitted field edits and explicit link add/remove. The
action explains its source destination: direct backlog update or proposed repo PR.
Keep a pending source edit visibly separate from accepted metadata. A source-native
link remains available for fields/actions the configured adapter cannot support.
No progress percentage or inferred objective completion appears in V1.

## 7. Mutation contract

Use the authenticated human command/receipt mechanism from
[interactive operations](interactive-factory-operations.md). Each command binds the
human identity, gaggle, source, target, intent, expected revision(s), and idempotency
key. Action discovery and server admission use the same policy; hiding a button is
not authorization. Recheck the policy at execution, not only when rendering a form.

Native edits use provider contracts for supported fields and relationship kinds.
Document/frontmatter/manifest edits create or update a source-repository PR under
configured branch, review, and publication policy. No direct default-branch write,
automatic merge, or out-of-band wiki edit is implied by workbench edit permission.

Immediately before mutation, read the authoritative revision through the command's
write path; a cached projection alone cannot authorize replacement. On conflict,
return a typed conflict plus permitted current fields for review. Do not overwrite
a concurrent human/agent edit. Distinguish provider atomic compare-and-set from
best-effort revision checking; do not claim stronger guarantees than the adapter.

Each logical node/edge operation has its own identity. A lost response triggers
bounded reconciliation against the source before retrying. Receipts distinguish
accepted, applied, conflict, denied, unsupported, pending-PR, partial, and unknown.
Record source before/after revisions, confirmed effects, actor, policy decision,
and PR/operation links without secrets or unrelated source content.

Creating an item and adding edges is a governed multi-step publication. Preflight
all targets and required capabilities before the first mutation; preserve the
created node ID if a later edge fails, report partial completion, and retry only
missing effects. Never pass ignored graph fields to `CreateWorkItem` or recreate
the node to repair an edge. General unlink/reparent require declared contracts and
tests for both GitHub and ADO, or an explicit unsupported operation.

Ordinary metadata editing does not unblock an escalation, authorize a deployment,
or start a workflow. Those remain named operations in the sibling designs. Normal
source events may subsequently make work eligible under existing scheduler policy;
show that consequence where the edit changes eligibility.

## 8. Agent suggestions

During creation/curation an agent may emit bounded suggestions with qualified
endpoints, edge kind, rationale, source evidence revision, and proposing run/stage.
Suggestions are not accepted edges and are excluded from authoritative navigation.
Expose accept/reject controls to an authorized operator. Acceptance goes through
the same owner resolution, revision checks, and command receipt as a manual link.

Deduplicate suggestions by normalized relationship and source evidence. Revalidate
stale targets at acceptance. Rejecting a suggestion does not delete an existing
source edge. If an item has not yet been created, suggestions address its provisional
creation identity and resolve only after the durable creation receipt names it.
The initial workflow integration must not start recursive graph expansion or scan
outside the gaggle's configured sources to find a more attractive objective.

## 9. Delivery tasks

These IDs are stable planning references. They are not claims of implementation or
existing GitHub issues. Every task updates API discovery/schema/client generation
when applicable, and documents explicit provider capability differences.

### HAW-BKL-001 — Source, identity, and edge contracts

Add gaggle source configuration, closed frontmatter/manifest schemas, qualified
node identity, edge kinds, and deterministic owner resolution from §§3–4.
Acceptance: validate positive GitHub/ADO/document examples; reject cross-gaggle
targets, duplicate objective IDs, duplicate owners, unknown kinds, and scope escapes.
Test a document rename preserving its ID and unrelated frontmatter. Pin GitHub
milestone membership and parent-issue ancestry as different semantic edges.

### HAW-BKL-002 — Scoped source ingestion on the shared read layer

Depends on HAW-BKL-001 and the queue design's scoped provider-read foundation.
Add bounded native backlog/objective reads and revision-pinned Markdown ingestion.
Acceptance: a never-run item is indexed; pagination resumes without loss/duplicates;
quota errors preserve partial status; revoked credentials cannot reuse broad cache.
Test native-parent omissions, denied project boundaries, document moves, and failed
scans without false deletion. Verify dashboard refreshes do not create N+1 scans.

### HAW-BKL-003 — Rebuildable graph and bounded read API

Depends on HAW-BKL-002. Project source-owned nodes/edges into the existing read
architecture; add scoped resource routes, cursor semantics, and generated clients.
Acceptance: deleting and rebuilding the projection preserves all accepted links;
each edge resolves to one source owner; inverse views derive from that same edge.
API tests cover stable pages, mixed source failures, safe Markdown, unprocessed
items, authorized counts, opaque key scope checks, and no raw provider payload leak.
Exercise byte/count saturation and the production pruner without false completeness.

### HAW-BKL-004 — Native relationship mutation contracts

Depends on HAW-BKL-001; coordinate with #5245 rather than duplicating its publisher.
Declare supported field edits, attach/remove/reparent, and milestone operations as
separate provider capabilities with shared GitHub/ADO conformance fixtures.
Acceptance: race guards and post-write verification are exercised; unknown outcomes
reconcile without duplicate edges; unsupported removal is refused before mutation.
Test create-then-link partial failure retaining the created item and independent
node/edge operation IDs. Existing create-with-graph-fields refusal stays truthful.

### HAW-BKL-005 — Human metadata commands and receipts

Depends on HAW-BKL-003/004 and the interactive design's per-gaggle policy/command seam.
Add source-bound edit/link intents with expected revision and replay-safe receipts.
Acceptance: direct HTTP calls fail for view-only, missing-policy, wrong-gaggle,
revoked-policy, and wrong-credential-scope cases; operator commands apply only the
permitted fields. Tests cover conflict, partial effect, denied action, and crash
after source success before receipt finalization without blind duplicate mutation.

### HAW-BKL-006 — Backlog/objective browsing and navigation

Depends on HAW-BKL-003. Add the portal workbench list, objective outline, detail,
source filters, typed edge sections, and existing run/PR/history navigation.
Acceptance: browser fixtures include ADO Epic → Feature → item, GitHub native
parent plus milestone, and Markdown objective with cross-provider contribution.
Tests verify no semantic conflation, bounded expansion, loading/partial/stale/denied
states, keyboard navigation, and useful details for an item with no Goobers run.

### HAW-BKL-007 — Edit UI and PR-backed repository metadata

Depends on HAW-BKL-005/006. Implement field/link editors and the repository PR path
for frontmatter/manifest changes; expose exact owner and pending versus applied state.
Acceptance: browser tests edit an authorized native item, resolve a revision conflict,
add/remove permitted edges, and create a document-metadata PR with expected base SHA.
The accepted graph changes only after source confirmation/merge; denied or unsupported
operations have no side effects. Test a concurrent file move/edit without overwriting it.

### HAW-BKL-008 — Creation/curation relationship suggestions

Depends on HAW-BKL-005/007. Add a versioned bounded suggestion artifact and the
workflow-to-workbench projection with operator accept/reject actions.
Acceptance: suggestions alone never create graph edges; acceptance uses the ordinary
source command, is idempotent, and rejects changed/out-of-scope targets. Tests cover
provisional new-item identity, repeated suggestions, rejection, source PR pending,
and expiry with an unsettled-command pin and bounded capacity refusal.

### HAW-BKL-009 — Integration, recovery, and operator guide

Depends on HAW-BKL-001–008. Exercise the integrated flows with recording provider
fixtures and real daemon/portal tests, without requiring live provider credentials.
Acceptance: rebuild after index loss, credential revocation, quota backoff, lost
mutation response, partial graph publication, and pending-PR merge reconciliation
all preserve the source contract. Add configured-source examples and an operator
guide covering ownership, supported edits, conflict repair, and relationship meanings.

## 10. Boundaries and later work

This design does not depend on scoring progress or on a new authoritative graph DB.
Later work may add outcome evidence, objective progress semantics, bulk planning,
cross-gaggle coordination, or additional source adapters through separate decisions.
V1 must still deliver both browsing and supported edits with honest capability gaps.

Source text and agent suggestions are untrusted content. They cannot grant human
roles, expand credential scope, configure a new repository, or authorize commands.
Implementation follows the merged program's priority order and normal review gates;
this draft does not publish issues, source changes, or PRs on its own.


## Delivered source metadata foundation

The first HAW-BKL-001 slice defines a provider-neutral qualified `NodeRef` with
`gaggleId`, `sourceBindingId`, semantic `kind`, and stable `sourceId`; it is data,
not authority. Existing node titles, URLs and locations are not used as identity.
The `workbench-source` JSON Schema describes the closed `objectives/v1` Goobers
frontmatter namespace and a `relationships/v1` repository manifest. Runtime source
validation adds configured-binding, same-gaggle, edge direction and duplicate
identity/ownership checks that require context beyond the JSON shape.

The parser treats ordinary Markdown as reference material. Metadata proposals
retain an existing objective ID, preserve the exact Markdown body, and retain
unrelated YAML values and comments. YAML formatting can normalize; a proposal is
returned as bytes for a reviewed source diff, never written by the parser. Source
files are limited to 1 MiB, each file to 2,000 explicit edges, manifests to 128
aliases, and YAML to 24 levels/20,000 nodes with no aliases, duplicate keys or
multiple documents. Two live locations with one objective ID conflict.

Canonical hierarchy ownership points at the child's native parent field, while
milestone membership points at the item's milestone field. Native, frontmatter
and explicitly configured manifest ownership are selected without mutation
permissions; a denied or unsupported selected owner does not permit fallback.
Derived run observations cannot be authored in these files. Competing edge IDs
or source owners are visible conflicts. No progress rollups are introduced.

These source primitives contact no provider and write no source file. Subsequent
slices now install gaggle source configuration, exact-credential native ingestion,
read routes, portal backlog browsing, and live-session source tools. The field edit
adapter, durable command custody, manual and session editing paths are installed.
Configured repository objective ingestion, graph projection and bounded portal
relationship navigation are also installed. Governed repository metadata PRs,
native relationship mutations and suggestion acceptance remain separate active
implementation slices. See [source metadata reference](../reference/workbench-source-metadata.md).
