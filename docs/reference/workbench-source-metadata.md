# Source-owned workbench metadata

Status: configured source reads, relationship navigation and native field edits
are installed locally. These formats store planning truth in a repository.
Changes must be proposed through the gaggle's source-write PR policy.

## Markdown objectives

```yaml
---
layout: page
goobers:
  schemaVersion: objectives/v1
  objectiveId: obj-8e1bc91c-94cb-4c5c-9412-d7e07e86d5da
  title: Reliable payment processing
---
```

The objective ID is generated once and retained when the file moves. Ordinary
Markdown without this namespace is a reference document. Two live files with the
same ID are a conflict. The Goobers namespace is closed; other frontmatter stays
author-owned. Metadata proposals preserve the exact Markdown body and unrelated
frontmatter values/comments. YAML formatting may normalize, so review the diff.

## Relationship manifests

```yaml
schemaVersion: relationships/v1
edges:
  - edgeId: edge-a72fdd9b-f947-449f-9dba-0fe2d9e6e587
    kind: contributes-to
    from:
      gaggleId: web
      sourceBindingId: backlog
      kind: work-item
      sourceId: provider-immutable-id
    to:
      gaggleId: web
      sourceBindingId: strategy
      kind: objective-document
      sourceId: obj-8e1bc91c-94cb-4c5c-9412-d7e07e86d5da
```

`sourceId` must come from the provider's stable identity or persistent objective
metadata. A title, URL or unqualified issue number is not enough to resolve it.
Binding names refer only to configured sources in the same gaggle. An optional
`aliases` array names existing qualified objects without creating new objectives.

Supported authored kinds are `parent-of`, `blocked-by`, `contributes-to`,
`references`, `milestone-member`, and `implemented-by`. They remain distinct.
`observed-in-run` is derived from journals and cannot be supplied by an author.
Frontmatter may contain the same edge shape in `goobers.edges`, but may own only
its document's outgoing edges. Inverse navigation is computed from one edge.

Use one owner for each relationship: its faithful native field, its document
frontmatter, or an explicitly configured manifest. Native permission denial or an
unsupported write operation is an error, not a reason to create a second copy in
a manifest. Hierarchy's native parent field belongs to the child; milestone
membership belongs to the member item. Duplicate IDs, duplicate relationships
and competing owners are conflicts.

## Bounds and schema

The embedded `workbench-source` schema describes metadata/manifest shape. The
runtime additionally validates configured scope, directions and source ownership.
Documents/manifests are at most 1 MiB, with 2,000 edges per file, 128 aliases per
manifest, and a 32-binding gaggle namespace. YAML aliases, duplicate keys, complex
keys, multiple documents, depth above 24 and more than 20,000 nodes are rejected.
The projection contract limits coherent objective sets to 50,000 nodes and edge
sets to 200,000. Incomplete scans do not prove deletion or a document move.

These parsers and proposal builders do not grant authorization, execute Markdown,
follow links, contact providers, mutate repositories or maintain planning state.
The installed read and native-field command services apply current authorization before provider access. Repository proposals additionally require the custody and publication service described below.


## Explicit gaggle sources

`spec.workbench` configures a bounded source namespace. It grants no human action
or credential. When `interactiveAccess` is absent, existing run monitoring stays
available to authorized users; these sources do not activate provider reads.

```yaml
workbench:
  schemaVersion: sources/v1
  relationshipManifest: planning-links
  sources:
    - name: backlog
      kind: backlog
      objectives:
        labels: [objective]
        types: [Epic, Feature]
      writes:
        fields: [title, description, labels]
        relationships: [parent-of, blocked-by, references]
    - name: strategy
      kind: documents
      repository:
        provider: github
        owner: example
        name: strategy-wiki
      paths: [objectives/reliability.md]
      writes:
        fields: [title, description]
        relationships: [contributes-to, references]
        metadata: [assign-objective]
    - name: planning-links
      kind: relationships
      repository:
        provider: github
        owner: example
        name: workflow-definitions
      paths: [planning/relationships.yaml]
      writes:
        relationships: [contributes-to, references]
        metadata: [aliases]
```

Every document/manifest repository must already appear in the gaggle's `project`
or `additionalRepos`; its configured branch is used. Strategy or wiki repos need
not contain code. Paths are literal files, with no traversal, glob or link crawling.
Documents allow up to 128 Markdown files per binding; a relationships binding owns
one YAML file. A file cannot have multiple source owners. Source binding names are
persistent references; renaming one requires reviewed reference migration.

The single backlog binding uses `spec.backlog`. GitHub binds exact owner/repository;
ADO binds the configured ADO code project's organization and backlog project. A
GitHub owner cannot supply an ADO organization. Objective selectors explicitly
classify matching stable native IDs, types or labels; they do not filter ordinary
backlog browsing. Omitted selectors infer no objectives.

Omitted `writes` is read-only. Declared fields and relationship kinds are additional
allowlists: each action still requires current human authorization, independently
bound interactive credentials and provider support. Repository changes require a
policy-governed PR. Denial on a native owner never falls back to a manifest. Changing
`relationshipManifest` selects the location for eligible new edges; it does not
move existing relationships.


## Native backlog projections

The native read adapter returns one caller-driven window of at most 100 candidates
with an opaque source/target cursor. GitHub issue database IDs are stable references;
issue numbers and URLs are locators. ADO work-item IDs are stable and revisions
remain a distinct field. Objective classification uses configured native IDs,
types or labels. Assignees and source tags retain their native values.

Pages report omissions, remaining candidates and relationship coverage explicitly.
GitHub milestone identity is available inline; its parent/blocker relations are
currently not loaded. ADO inline parent/dependency targets remain unresolved until
their membership in this source is verified. A page ending is not proof of a full
consistent scan or deletion. Each operation has a 15-second limit, a 2-MiB raw
reply bound, a 256-KiB item bound and a 1-MiB projected page bound.

The provider adapter itself has no credentials, cache, polling loop or write path.
The daemon installs this reader for browser requests and authorized native sessions.


## Native field edit adapter

A native edit command changes one source-allowlisted field: title, description,
state, labels, or assignees. It supplies the stable source ID, current locator and
expected revision. ADO uses an atomic revision test; GitHub's timestamp check is a
preflight check and cannot eliminate concurrent source edits. Labels and assignees
replace the full visible set; generic edits preserve service control labels.

The adapter issues a single native mutation without automatic retry, then makes
one readback. Receipts separately report provider acknowledgement and whether the
observed value matches. Matching state after a lost response remains uncertain;
it does not prove this command authored that state. The installed host persists command
custody before calling the adapter and exposes authorized manual editing routes
and portal controls. Relationships and
repository PR proposals use separate contracts.

Durable command custody now records the exact actor, source target, request,
operation digest and accepted/attempted/completed timestamps in the shared queue
database. A command can claim at most one effect attempt, including across process
restart. Provider acknowledgement and observed matching state remain separate;
uncertain effects cannot be replayed automatically. The host must recheck current
actor, source target and field permission when accepting or reading receipts.

Receipt capacity is reserved before acceptance against the shared byte budget,
with at most 1,000 live or retained commands per gaggle. Confirmed/not-applied
details remain for 30 days, followed by a compact replay tombstone for 30 more.
The resulting idempotency guarantee is 60 days. Accepted, attempting and unknown
commands never expire automatically to free capacity. The installed writer/API uses this custody for manual field edits.


## Interactive read service

The shared service selects sources and resolves explicit interactive credentials
under the same current gaggle policy. It returns bounded native projections to
both portal and live-session callers. Source metadata reads expose configured
scope and advisory write allowlists; they expose no credentials or connection
references. Every provider read rechecks authorization.

Interactive caching shares the existing gaggle/binding/generation store and is
separate from automation, even if credentials match. Refreshes conditionally
revalidate with the provider; they never reuse an hour-long scheduler snapshot.
A live session uses its already-held human lease, checks exact retained source
configuration, and cancels/joins an active read before changed policy is published.
The daemon installs the following human-authenticated, no-store routes:

- `GET /api/v1/gaggles/{gaggle}/workbench/sources`
- `GET /api/v1/gaggles/{gaggle}/workbench/sources/{source}/items`
- `GET /api/v1/gaggles/{gaggle}/workbench/sources/{source}/items/{item}`

The portal's gaggle workbench lists configured backlog sources, explicitly loads
one bounded page, and shows item details, objective classification, associated
native items and relationship coverage. Refresh replaces the visible window;
source or access changes clear old content. Item navigation supplies the expected
stable ID. Only verified targets in a configured source can load linked details.

The same service is the default source reader for native shared-session tools.
Tools bind the exact initiating human, live execution lease and retained source
configuration; providers remain in the host. A missing source or read grant leaves
the session in conversation-only mode. Read availability is advertised only after
the daemon installs the reader. Manual field edits use the installed writer described below.

## Repository document reads

The repository service resolves the configured branch to an exact commit and reads
only declared literal paths. GitHub reads verify regular-file tree entries and blob
identity; ADO reads verify the exact repository, commit, path, file type and Git
blob hash. Symlinks, LFS expansion and linked remote content are not followed.

Each page contains at most eight files, with a 1-MiB file limit, 4-MiB page limit
and 15-second operation deadline. File results distinguish available, unavailable,
invalid-source and oversized content. Ordinary Markdown has no invented objective
identity; declared objective IDs and manifest edges use the shared source parser.
Commit, blob and content digest identify the observed bytes.

Continuation cursors bind source, target and branch commit. If the branch moves,
the caller restarts its read. Exhausting a page is not deletion evidence or proof
of a coherent multi-page scan. Repository credentials and conditional caching use
the same current interactive policy boundary as native backlog reads.
The daemon installs `GET /api/v1/gaggles/{gaggle}/workbench/sources/{source}/documents`.
Workbench HTTP reads use the existing eight-second bounded request budget, which
narrows the provider's maximum and completes before the portal's client timeout.
The portal shows one bounded window and one selected file, source provenance,
objective identities, authored relationships and manifest aliases. Source text is
displayed without executing Markdown or loading linked content. Refresh discards
old content after source, branch, access or client changes. Repository edits still
require a separate policy-governed PR path.

## Rebuildable graph projection

The graph projector accepts already-authorized provider/document adapter pages
under one configured source set. It verifies target digests and coherent repository
commit/path windows, then retains explicit native relationships, objective edges
and manifest aliases. It performs no reads and accepts no client-authored graph as
source truth. Node and edge keys identify this projection; they do not create new
source-owned identities.

Nodes retain every distinct observation. Duplicate objective locations, competing
edge IDs or source owners remain visible conflicts with no selected winner.
Unresolved or unread targets remain explicit; native backlog pages retain their
non-snapshot consistency even after pagination ends. The graph reports source
coverage and omissions without inferring deletion, movement or progress.

Inputs are bounded to 512 pages and 32 MiB, with at most 10,000 nodes and 50,000
edges. Document bodies are excluded from graph output. The installed
`GET /api/v1/gaggles/{gaggle}/workbench/graph` endpoint reads the server-selected
source set under current human authority and a seven-second aggregate budget.
It observes at most 25 native items or eight literal repository files per source,
reserves source read budgets before fetching, and reports skipped sources explicitly.
Its response carries the current source generation; inconsistent repository commits
are rejected. Clients cannot submit a graph or choose arbitrary source targets.

The portal's **Load relationships** action shows coverage, objective/item selection,
incoming/outgoing directions and source ownership. Conflicting or unread links
remain visible in the list without becoming verified connections in the diagram.
Navigation is local to the loaded window and does not recursively fetch linked
content. Refresh clears old content before reading; source generation mismatches
require refreshing source configuration. No progress calculation is inferred.

Native edit identity uses the physical source target and exact patch. Objective
classification and unrelated configuration changes do not change that identity,
so a confirmed command can be replayed without minting a token or re-reading its
obsolete pre-edit revision. Current source and field authority still apply.


## Installed native field commands

The portal detail editor checks current source capabilities before offering a
field. `PATCH /api/v1/gaggles/{gaggle}/workbench/sources/{source}/items/{item}`
requires a unique `Idempotency-Key`, stable source identity and expected revision.
The service stores the verified human identity, admits the immutable request and
claims one attempt before resolving the configured interactive credential.
Duplicates and receipt reads recheck current authority without resolving tokens
or repeating provider effects. No automation credential fallback is used.

One field is edited per command. ADO uses an atomic revision test; GitHub checks
its timestamp before the mutation but cannot provide an atomic issue revision
condition. Control labels are preserved; needs-human resolution is a separate
operation. Provider acknowledgement and a matching readback are both needed for a
confirmed receipt. A lost response remains unknown even if the observed content
matches. The editor retains the command key and offers receipt inspection; a fresh
edit after a settled result first reads the current item and capabilities.

The daemon performs bounded receipt maintenance even without an active scheduler.
Confirmed/not-applied evidence becomes a compact tombstone after 30 days, retained
for another 30 days. Accepted, attempting and unknown custody does not expire.
Governed repository metadata PRs and native relationship writes are separate
capabilities; this field editor does not claim those operations.

## Shared session native edits

When the host installs native editing, a shared agent turn can use the private
`get_backlog_edit_capabilities`, `edit_backlog_item` and
`get_backlog_edit_receipt` tools. Each call checks the actual human, retained
source configuration and live turn lease. The model supplies a request ID; the
host namespaces it to the real session/turn and uses the same durable command
store as manual editing. It never supplies provider credentials to the model.

Calls serialize within the existing bounded tool budget. Actual command IDs,
operation digests and result artifacts are retained in the run journal, including
uncertain effects when policy revocation races completion. Revocation blocks
returning results to the caller, while the host finishes bounded receipt custody.
Missing editing permission leaves conversation and permitted reads available.
The backlog editor also drops the previous connection/item state before rendering
a changed scope, preserving unresolved keys only for the same source item.


## Governed repository proposal adapters

A pure preview now supports title/body changes and exact `references` or
`contributes-to` additions/removals in an existing configured file. It binds the
current commit, blob and content digest, preserves objective identity and unrelated
metadata, and enforces source field/relationship allowlists. Native relationships
cannot fall back to a manifest. Creating or moving objective files remains separate.

Source-bound adapters verify the current base revision and execute explicit,
one-attempt publication phases. GitHub creates a tree, commit, absent branch and
draft PR. ADO creates an absent branch, pushes against its pinned head and creates
a draft PR. Each operation verifies its exact repository, file, branch and command
marker; observation is separate from provider acknowledgement. An unknown response
cannot be converted to an acknowledged mutation just because matching state exists.

These adapters now serve the installed portal publication path, with durable
proposal custody, current-policy checks at each phase, reviewed submission and
separate receipt inspection. See the governed metadata API below.


## Agent relationship proposal artifact

The embedded `relationship-suggestions` schema and pure parser accept a bounded
`relationship-suggestions/v1` JSON artifact: at most 100 proposed additions and
256 KiB. Each proposal names a distinct relationship kind, qualified endpoints, a
rationale and the source target/revision evidence for each existing endpoint.
Document evidence pins the configured path, commit, blob and content digest.
These are claims to check against fresh authorized source reads before acceptance.

The host binds actual run, stage, attempt, artifact path and digest separately;
model output cannot claim that provenance. Identical relationships and evidence
deduplicate across runs independently of rationale text. A changed evidence revision
creates a new review candidate without changing the eventual relationship ID.

A provisional work item names a creation request in the producing execution
occurrence. It cannot become a source node until the host supplies a confirmed
creation receipt for that exact request and occurrence. No guessed issue number,
URL or model-supplied receipt resolves it. Rejection is a review decision and never
removes an existing edge.

This slice provides the artifact contract and pure binding/materialization helpers.
Bounded journal artifact ingestion and retained review decisions are now implemented.
Portal accept/reject, fresh source verification and command submission remain
separate installation work. Suggestions are not input
to the authoritative graph projector and grant no source permission.

## Dedicated needs-human resolution custody

Provider adapters now inspect one exact GitHub/ADO item with bounded comments and
known dependency coverage, and can remove only the canonical needs-human marker.
They retain unrelated labels and control markers. ADO uses an atomic revision
test; GitHub's revision check remains a preflight condition.

Dedicated operational custody records the actual session/human origin, immutable
observation, rationale and evidence before claiming one marker effect. It shares
the native command capacity and byte budget. Lost responses remain uncertain,
even if a later read sees the marker gone. Complete source and learned-dependency
coverage is required; unresolved or unknown blockers prevent acceptance.

This provider/ledger slice does not install a portal action by itself. The live
session resolver, host learned-record lock and portal entry point follow next.
It neither approves a gate nor restarts a run or publishes a PR. Settled receipt
retention follows the same 30-day detail plus 30-day tombstone policy; unresolved
custody never expires automatically.

The reusable live-session resolver is now implemented. It checks the actual queued
turn against journal session lineage, retained source configuration and the held
human execution lease. Inspection and resolution share the scheduler's existing
claims lock and an exact learned-record digest. The agent supplies the assessment;
the host verifies current permissions, source revision, complete known blockers,
and real message/comment/command evidence. It does not remove learned records or
infer that general workflow eligibility has been granted.

Confirmed replay reads its receipt without minting another provider credential or
re-reading the obsolete pre-edit revision. A cancellation during the effect joins
a bounded receipt save before releasing authority and claim custody. Session
tools and the portal entry point remain the next installation slice.

## Repository proposal phase receipts

The shared queue now retains repository proposal intent, exact before/after bytes,
validated plan and immutable phase receipts before provider publication. Each
phase is claimed once. An uncertain phase retains its original result; a later
exact observation is separate evidence and can only permit a subsequent explicit
submission to continue. An uncertain ADO branch creation cannot advance merely
because a matching base ref appears. A final observed PR is recorded as observed,
distinct from a provider-acknowledged PR.

Native field commands, marker resolutions and repository proposals share one
1,000-command gaggle limit and the database byte budget. The installed maintenance
pass allocates cleanup across all three kinds within its existing row/time bound.
Uncertain and partially visible proposal effects stay pinned; settled results use
the existing detail/tombstone retention policy. Custody alone does not install the
repository submission API or portal controls; those remain the next slice.

### Governed metadata proposal API

Configured document sources support title/body edits and explicit `references` or
`contributes-to` edges when their source write policy permits them. The configured
relationship-manifest owner supports those same explicit edge proposals. Other
native relationship kinds remain unsupported by this proposal editor.

The source's `proposal-preview` route reads the current configured branch and
returns inert before/after text pinned to its exact commit, blob and content digest.
An explicit `proposals` submission retains the same reviewed intent and an
idempotency key, then creates a separate draft PR using the gaggle's interactive
repository identity. Source write mode defaults to `pull-request` when omitted.
No proposal edits the configured branch directly or merges its PR.

Keep the returned command ID. Receipt reads remain available with current read
permission after write permission is revoked. `check` observes an uncertain native
phase without another mutation. `continue` is an explicit write action that
rechecks current permission and advances only an unattempted next phase for the
retained intent. A matching observation never rewrites the original uncertain
acknowledgement. Changing keys to retry an uncertain write can create another
proposal; use the original receipt instead.


### Assigning objective identities and editing aliases

With `writes.metadata: [assign-objective]`, an existing declared Markdown file can
receive an objective ID through the same preview and draft-PR flow. The portal
creates one ID for the reviewed request and keeps it through retries. Existing
objective IDs cannot be replaced. The Markdown body and unrelated frontmatter
are preserved; the file remains an ordinary reference until the PR is merged.

With `writes.metadata: [aliases]`, the configured owning relationship manifest
can add or remove a named alias for an exact gaggle-qualified target. Removal
matches both name and target; a conflicting name cannot silently retarget an
alias. An alias supplies a label for an existing reference, not a new objective.

Both operations require current repository-write authority, exact source/path and
commit/blob/content pins, and the source-specific metadata allowlist. They are
exclusive with field or edge changes in a single proposal. Omitted metadata
permission disables these controls without granting any other source write.


### Retained suggestion decisions

Operational review custody now keeps the actual producing run, stage event and
artifact digest with the human's accept/reject decision. Acceptance links to one
exact repository proposal request; an uncertain submission cannot be replaced
by another key. A link records the handoff, not PR publication or a graph edge.

Unresolved decisions retain the source journal/artifact, archived configuration
and linked proposal. Settled reviews use 30 days of detail followed by 30 days of
compact replay identity. Review custody shares the command count, byte budget and
bounded maintenance with source edits, marker resolutions and PR repairs. The
repository/provider remains the owner of planning truth.


### Reviewed suggestion service

The review service verifies the selected retained artifact and current endpoint
visibility. Preview re-reads both source identities and revisions, derives the
configured metadata owner, and returns its ordinary proposal diff. Acceptance
must carry the exact owner revision and operation digest shown by that preview;
the client cannot substitute an edge or destination. A repeated linked decision
returns the existing proposal without advancing another write phase.

Rejection requires an operator's current `source.proposeChange` grant and records
only a review decision. Receipt inspection remains available under current source
read authority. The supported acceptance shapes are `references` and
`contributes-to` between existing native work items and objective documents.
Provisional creation references, native hierarchy changes and other endpoint
kinds remain explicit unsupported cases. The service is implemented; portal/API
installation follows as a separate review slice.
