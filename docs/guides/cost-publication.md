# Configure cost publication

Cost publication is enabled by default. It adds measured run-cost receipts to
provider attribution and publishes PR summaries and linked-issue cost shares at
merge close-out. GitHub comments and Azure DevOps PR threads/work-item comments
use the same configuration resolution.

Disable the instance default in `instance.yaml`:

```yaml
cost:
  enabled: false
```

Override it for an individual gaggle by adding this block alongside that gaggle's
existing `spec.project`, `spec.backlog`, and `spec.isolation`:

```yaml
spec:
  cost:
    enabled: true
```

The precedence is gaggle `spec.cost.enabled`, then instance `cost.enabled`, then
the built-in `true`. An omitted block, empty block, or null `enabled` inherits
the next default. Explicit `false` disables publication; the string `"false"`
and unknown properties are invalid. No workflow stage or workflow YAML change
is required, including for workflows pinned to DSL 2.0.

Disabling publication does not disable local usage measurement, journal records,
telemetry rollups, or the `goobers cost` query. It does not remove cost comments
already published. Normal merge close-out and issue closure still run, without
the optional cost summary and cost fields in attribution.

Publication reads the current validated configuration. An unreadable or invalid
configuration suppresses optional cost publication with a warning; it does not
abort merge close-out. New delayed-reconciliation records retain the originating
gaggle, so a sweep does not apply its own gaggle's override. For older records
without that identity, a disabled gaggle targeting the same repository suppresses
publication rather than guessing which gaggle originated the PR. An explicitly
identified gaggle that no longer exists also suppresses publication.
