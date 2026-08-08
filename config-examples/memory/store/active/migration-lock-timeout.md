---
name: migration-lock-timeout
description: "Schema migrations time out when run against a busy database."
type: known-failure
scope:
  areas: ["db/migrations/**"]
  workflows: []
  roles: []
  labels: ["database", "migration"]
provenance:
  source: human
  proposedBy: coder
  promotedBy: wizard
  promotedAt: 2026-01-15T05:00:03Z
confidence: proven
reviewAfter: ""
supersedes: []
---
# Schema migration lock timeout on a busy database

## Fact
A migration that adds a column with a default fails with a lock timeout when the
target table is under write load.

## Evidence
Reproduced deterministically: the migration acquires an exclusive lock the write
traffic will not yield. It succeeds every time against an idle table.

## Do instead
Split the change into an add-nullable-column step and a separate backfill, or run
the migration in a low-traffic window. Never add a defaulted column to a hot
table in one step.
