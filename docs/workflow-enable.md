# Workflow enable/disable

Goobers workflows are declarative: each schedule, signal, webhook, or
backlog-item trigger defined on a workflow's `spec.triggers` is activated by the
daemon as soon as the workflow is applied. There are two ways to pause a
trigger without deleting it:

1. Toggle its `enabled` field in the workflow CRD source and re-apply the
   config, or
2. Ask the daemon to do that toggle atomically on your behalf via the
   `PUT /api/v1/gaggles/{gaggle}/workflows/{workflow}/enabled` route.

Both paths converge on the same declarative surface — the persisted workflow
YAML — so a subsequent `goobers apply` never regresses the state a portal edit
just published.

## The `enabled` field on `Trigger`

The `enabled` field is optional on every trigger:

| value    | behavior                                                                                   |
| -------- | ------------------------------------------------------------------------------------------ |
| omitted  | Backwards-compatible default: the trigger is considered enabled. Legacy workflows keep working unchanged. |
| `true`   | Explicitly enabled. Identical runtime behavior to omitting the field.                       |
| `false`  | Disabled. The daemon skips this trigger during reconciliation.                              |

Disabled schedule triggers are torn down when the reconciler next runs and are
not restarted on the next tick, but the trigger keeps its ordinal slot in the
workflow, so re-enabling it later restores the same schedule identity rather
than allocating a new one.

`trigger.type: manual` is a no-op for `enabled`: manual triggers only run when
a human invokes them and there is no ambient reconciliation to skip, so the
daemon ignores the field on manual entries.

## HTTP contract

`PUT /api/v1/gaggles/{gaggle}/workflows/{workflow}/enabled` accepts a single
JSON object:

```json
{ "enabled": false }
```

with `Content-Type: application/json` and a body of at most 64 KiB. The
response envelope is:

```json
{ "gaggle": "web", "workflow": "implement", "enabled": false }
```

The route is idempotent: re-sending the same value returns the current state
without touching the on-disk YAML. Unknown JSON fields, trailing bytes after
the object, other HTTP methods, and non-JSON content types are rejected before
the mutation runs.

## Atomic mutation and rollback

The daemon serializes workflow YAML edits behind a mutex shared with the
config reloader, so an in-flight `enabled` write never overlaps a periodic
reload. Each write:

1. Reads the workflow's source YAML from the currently applied config, using
   the same lookup the reloader uses when it swaps definitions in.
2. Overwrites (or inserts) the `enabled` value on every non-manual trigger,
   preserving unrelated fields, comments, and formatting via a YAML node
   traversal — a struct-round-trip is not used.
3. Writes the result to a temp file next to the workflow and atomically
   renames it into place, preserving the original file mode.
4. Drives a single on-demand reload check. If the reloader rejects the
   updated config (`workflow_edit_rejected`, HTTP 422), or fails outright
   (`workflow_edit_failed`, HTTP 422), the daemon restores the original
   bytes so the on-disk state remains identical to the last successfully
   applied config.

If the daemon is running but the mutation surface has not been wired (for
example a distribution that turned it off), the route responds with HTTP 503
and code `workflow_mutations_unavailable`, and the `portal/config`
`capabilities.workflowEnable` flag is `false` so the portal can hide the
control.
