# Upgrade advisory: custom-drift

## Scope and provenance

- Current binary: /opt/goobers-v2.0.0/bin/goobers version v2.0.0 commit 2222222222222222222222222222222222222222
- Target binary: /opt/goobers-v2.0.0/bin/goobers version v2.0.0 commit 2222222222222222222222222222222222222222
- Exact target ref: `refs/tags/v2.0.0` at commit `2222222222222222222222222222222222222222`
- Contract source: github:Agent-Clubhouse/Goobers@refs/tags/v2.0.0 (commit 2222222222222222222222222222222222222222)
- Config source: local-dir:/workspace/config at analyzed digest custom-drift-current
- Canonical root: /opt/goobers-v2.0.0/config-examples
- Source provenance: verified v2.0.0 config examples and target config diff at commit 2222222222222222222222222222222222222222
- Compatibility confidence: `high`

## Workflow `example/implementation` (`2.0` -> `2.0`)

Feature inventory:
- `workflow.spec.triggers`: `ga` - unchanged at the target. (source: v2.0.0 features --used and --dsl-version 2.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- `task.inputs`: `ga` - unchanged at the target. (source: v2.0.0 feature registry at commit 2222222222222222222222222222222222222222; confidence: high)

Validation diagnostics:
- target validation: clean; canonical comparison reports only tuning, structural, and user-owned differences
- proposed target validation: clean (0 warnings, 0 errors)

Target canonical state-graph diff:
- current: `start: query; query[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort, timeout -> escalate)`
- target: `start: query; query[deterministic] -> gather-context; gather-context[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort, timeout -> escalate)`
Proposed state-graph diff:
- current: `start: query; query[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort, timeout -> escalate)`
- proposed: `start: query; query[deterministic] -> gather-context; gather-context[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort, timeout -> escalate)`

Recommendations:
- **local operational tuning**: `spec.triggers[0].schedule`: config diff reports INFO; preserve the local 17-minute cadence. (source: v2.0.0 config diff INFO at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- **local operational tuning**: `spec.readiness.maxConcurrentRuns`: config diff reports INFO; preserve the local concurrency limit. (source: v2.0.0 config diff INFO at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- **recommended canonical workflow improvement**: `spec.tasks[gather-context]`: the same-identity canonical workflow adds context before implementation. (source: v2.0.0 canonical implementation workflow at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)

## Workflow `example/nightly-release` (`2.0` -> `2.0`)

Feature inventory:
- `stage.run.command`: `ga` - unchanged at the target. (source: v2.0.0 features --used at commit 2222222222222222222222222222222222222222; confidence: high)

Validation diagnostics:
- target validation: clean; no same-identity canonical workflow exists
- proposed target validation: clean (0 warnings, 0 errors)

Current state graph: `start: build; build[deterministic] -> deploy; deploy[deterministic] -> done`
Target canonical state graph: no same-identity canonical workflow exists.
Proposed state graph: unchanged: `start: build; build[deterministic] -> deploy; deploy[deterministic] -> done`

Recommendations:
- **user customization requiring human judgment**: `spec.tasks[deploy].run.command`: no same-identity canonical workflow exists; retain the command until its owner decides. (source: v2.0.0 canonical workflow inventory at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)

## Ordered upgrade plan

1. `example/implementation`: add only the gather-context task and its two adjacent edges; dependency: owner accepts the optional canonical improvement; expected file diff: gaggles/example/workflows/implementation.yaml adds only gather-context and rewires query -> gather-context -> implement; review: task inputs, capabilities, and state-graph edges; validation command: `/opt/goobers-v2.0.0/bin/goobers validate --strict --source-tree <scratch-canonical-improvement>`.
2. Run target `goobers validate --strict`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation has no warnings and approved tuning is unchanged.

## Write readiness

`analysis-only`: the owner must separately approve the optional gather-context change; the unmatched nightly-release workflow remains untouched
