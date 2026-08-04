---
name: release-checklist
description: "The steps a release change must complete before it can merge."
type: procedure
scope:
  areas: []
  workflows: ["merge-review"]
  roles: ["reviewer"]
  labels: ["release"]
provenance:
  source: human
  proposedBy: reviewer
  promotedBy: wizard
  promotedAt: 2026-01-16T05:00:07Z
confidence: proven
reviewAfter: ""
supersedes: []
---
# Release change checklist

## Fact
A change tagged for release must bump the version, update the changelog, and pass
the full CI suite before it is eligible to merge.

## Evidence
Every release that skipped the changelog bump required a follow-up fix PR. The
three steps together have been the standing convention.

## Do instead
Confirm all three are present before approving a release-tagged change. Route a
release change missing any of them to remediation rather than approving it.
