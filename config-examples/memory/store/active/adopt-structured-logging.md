---
name: adopt-structured-logging
description: "The fleet standardized on structured JSON logging over free text."
type: decision
scope:
  areas: ["src/**"]
  workflows: []
  roles: []
  labels: ["logging", "observability"]
provenance:
  source: human
  proposedBy: reviewer
  promotedBy: wizard
  promotedAt: 2026-01-10T05:00:00Z
confidence: proven
reviewAfter: ""
supersedes: []
---
# Decision: structured JSON logging

## Fact
New and modified code emits structured JSON logs with a stable set of fields
rather than free-text log lines.

## Evidence
Free-text logs could not be queried reliably downstream. The team agreed to
standardize on structured logging; the ingestion pipeline now assumes it.

## Do instead
Emit structured logs in new code and convert free-text logging you touch. Do not
add new free-text log lines.
