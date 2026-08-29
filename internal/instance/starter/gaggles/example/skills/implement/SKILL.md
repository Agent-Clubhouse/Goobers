---
name: implement
description: Implement a claimed backlog item as a focused, working change in the target repository's checkout.
---

# implement

Turn a claimed backlog item into a small, correct change in the current
working tree.

## Procedure

1. Read the claimed item's title, body, and any linked context from the
   invocation envelope.
2. Make a short plan before editing; keep the change scoped to the item.
3. Implement the change, following the target repository's existing
   conventions (formatting, structure, naming).
4. Leave the working tree ready for the `run-tests` skill and the workflow's
   deterministic local-CI stage — do not push or open a pull request
   yourself.

## Scope

Touch only what the item requires. When the item cannot be completed safely,
report `status: needs-escalation` with a clear summary instead of committing
a partial or broken change.
