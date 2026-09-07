# Trigger primitives

Triggers are declared under `spec.triggers`. A trigger firing is necessary but
not sufficient to start a run: readiness limits and run budgets must also admit
it.

```yaml
spec:
  triggers:
    - type: schedule
      schedule: "@every 30m"
      enabled: true
```

## Common fields

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `type` | string | required | One of the trigger types below. |
| `enabled` | boolean | `true` | Disables scheduler handling without deleting the declaration. It has no effect on `manual`. |
| `priority` | integer | `0` | Preserves higher-priority provider-backed polls first when a quota window cannot cover all due work. |

Trigger-specific fields must not be copied to unrelated trigger types.

## `manual`

Starts only through an explicit operator action such as `goobers run`.

**Parameters:** none beyond the common fields.

**Placement rule:** a manual trigger must be the workflow's only trigger.

```yaml
triggers:
  - type: manual
```

## `schedule`

Starts when a cron or supported interval expression becomes due.

| Parameter | Required | Description |
| --- | --- | --- |
| `schedule` | yes | Cron expression or interval such as `"@every 1h"`. Quote cron expressions. |
| `idleBackoff.enabled` | no | Enables adaptive delay after consecutive no-work runs; defaults to `true`. |
| `idleBackoff.floor` | no | Positive Go duration; defaults to `1m`. |
| `idleBackoff.ceiling` | no | Positive Go duration not below `floor`; defaults to `15m`. |

Multiple schedule triggers are allowed. The scheduler fires when any is due.

```yaml
triggers:
  - type: schedule
    schedule: "0 * * * *"
    idleBackoff:
      floor: "2m"
      ceiling: "30m"
```

## `backlog-item`

Starts from provider backlog eligibility. This trigger does not claim an item;
an autonomous consumer normally begins with a deterministic
`goobers backlog-query --claim` task.

| Parameter | Required | Description |
| --- | --- | --- |
| `selector` | no | Map whose keys are required backlog labels. Values are reserved and currently ignored for matching. |
| `trustLabel` | no | Maintainer-applied label that classifies directly triggered content as maintainer integrity. Valid only here. |
| `labelPredicate` | no | CEL expression over `labels`, ANDed with `selector`. |
| `fieldPredicate` | no | CEL expression over provider-native scalar `fields`, ANDed with the other filters. |

All backlog-item triggers in one workflow must use the same `trustLabel`.

There is no free-form provider query parameter here or on `gaggle.spec.backlog`
(#1677): `selector`/`labels`, `labelPredicate` and `fieldPredicate` are the
whole selection surface, and they mean the same thing on every backlog
provider. Declaring `query:` is a validation error, not an ignored field.

```yaml
triggers:
  - type: backlog-item
    selector:
      goobers:ready: "true"
    trustLabel: "goobers:approved"
    labelPredicate: '!("tracking" in labels)'
    fieldPredicate: 'fields["number"] > 100'
```

## `signal`

Starts when the scheduler receives the named external or workflow-produced
signal.

| Parameter | Required | Description |
| --- | --- | --- |
| `signal` | yes | Non-empty signal name. |

```yaml
triggers:
  - type: signal
    signal: implementation-completed
```

Signal-triggered chains remain subject to `readiness.maxChainDepth`.

## `webhook`

Starts when the daemon receives a verified GitHub webhook with one of the
declared event names.

| Parameter | Required | Description |
| --- | --- | --- |
| `events` | yes | Non-empty list of event names such as `pull_request`, `issues`, or `check_suite`. |

```yaml
triggers:
  - type: webhook
    events:
      - pull_request
      - check_suite
```

The API shape reserves `idleBackoff` for schedule and webhook triggers. Check
the target release with `goobers validate`: releases whose compiler has not
enabled webhook backoff reject that combination rather than silently ignoring
it.
