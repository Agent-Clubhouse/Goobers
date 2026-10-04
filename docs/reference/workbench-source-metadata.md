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
The next slices bind them to the configured source and interactive credentials.
