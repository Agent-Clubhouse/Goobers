# Source-owned workbench metadata

Status: local implementation foundation; source configuration and portal adapters
are not installed yet. These formats store planning truth in a repository.
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
Provider ingestion and browse/edit routes follow in separate slices.


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
    - name: planning-links
      kind: relationships
      repository:
        provider: github
        owner: example
        name: workflow-definitions
      paths: [planning/relationships.yaml]
      writes:
        relationships: [contributes-to, references]
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
it does not prove this command authored that state. The host must persist command
custody before calling the adapter. Manual editing routes and UI remain disabled
until that host custody and authorization path is installed. Relationships and
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
commands never expire automatically to free capacity. This custody slice does not
enable editing until the authorized host/API is installed.


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
the daemon installs the reader. Manual source edits remain a later slice.

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
the same current interactive policy boundary as native backlog reads. Repository
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
edges. Document bodies are excluded from graph output. The aggregate authorized
service, graph API and visualization are separate installation slices.

Native edit identity uses the physical source target and exact patch. Objective
classification and unrelated configuration changes do not change that identity,
so a confirmed command can be replayed without minting a token or re-reading its
obsolete pre-edit revision. Current source and field authority still apply.
