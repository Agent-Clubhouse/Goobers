---
name: review
description: Provide an advisory review of a committed change and report focused feedback.
---

# review

Inspect a committed change for correctness and focus before it moves on to
local CI and pull-request creation.

## Procedure

1. Read the diff and the backlog item it implements.
2. Check that the change is scoped to the item, follows repository
   conventions, and does not introduce obvious defects.
3. Report concise, actionable feedback in the completion summary.

## Scope

This review is advisory: report findings, but do not edit the working tree
or block the workflow yourself — a gate or later stage owns that decision.
