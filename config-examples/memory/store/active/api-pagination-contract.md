---
name: api-pagination-contract
description: "How the public list endpoints paginate: cursor-based, opaque tokens."
type: reference
scope:
  areas: ["src/api/**"]
  workflows: []
  roles: []
  labels: ["api", "reference"]
provenance:
  source: human
  proposedBy: docs
  promotedBy: wizard
  promotedAt: 2026-01-11T05:00:00Z
confidence: proven
reviewAfter: ""
supersedes: []
---
# Reference: list-endpoint pagination contract

## Fact
Public list endpoints paginate with an opaque cursor token returned as
`nextCursor`. Clients pass it back as `cursor`; they never construct offsets.

## Evidence
This is the established contract across the list endpoints. Offset-based paging
was removed because it skipped or repeated rows under concurrent writes.

## Do instead
Preserve the cursor contract when adding or changing a list endpoint. Do not
reintroduce offset/limit paging on public endpoints.
