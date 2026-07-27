# Upgrade advisory: custom-drift

Target release: `v2.0.0` (`2222222222222222222222222222222222222222`)

Source provenance: verified v2.0.0 config examples and target config diff at commit 2222222222222222222222222222222222222222

Compatibility confidence: `high`

## Workflow `implementation` (`2.0` -> `2.0`)

Feature inventory:
- `workflow.spec.triggers`: `ga` - unchanged at the target. (source: v2.0.0 features --used and --dsl-version 2.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- `task.inputs.legacyContext`: `deprecated` - prefer content-digested context pointers before the next removal edge. (source: v2.0.0 feature registry at commit 2222222222222222222222222222222222222222; confidence: high)

State-graph diff:
- current: `query -> implement; implement -> review`
- target: `query -> gather-context; gather-context -> implement; implement -> review`

Recommendations:
- **recommended canonical workflow improvement**: feature `task.inputs.legacyContext` is deprecated; prefer content-digested context pointers before the next removal edge. (source: v2.0.0 feature registry at commit 2222222222222222222222222222222222222222; confidence: high)
- **local operational tuning**: `spec.triggers[0].schedule`: config diff reports INFO; preserve the local 17-minute cadence. (source: v2.0.0 config diff INFO at commit 2222222222222222222222222222222222222222; confidence: high)
- **local operational tuning**: `spec.readiness.maxConcurrentRuns`: config diff reports INFO; preserve the local concurrency limit. (source: v2.0.0 config diff INFO at commit 2222222222222222222222222222222222222222; confidence: high)
- **recommended canonical workflow improvement**: `spec.tasks[gather-context]`: the same-name canonical workflow adds context before implementation. (source: v2.0.0 canonical implementation workflow at commit 2222222222222222222222222222222222222222; confidence: high)

## Workflow `nightly-release` (`2.0` -> `2.0`)

Feature inventory:
- `task.run.command`: `ga` - unchanged at the target. (source: v2.0.0 features --used at commit 2222222222222222222222222222222222222222; confidence: high)

State graph: unchanged: `build -> deploy; deploy -> done`

Recommendations:
- **user customization requiring human judgment**: `spec.tasks[deploy].run.command`: no same-name canonical workflow exists; retain the command until its owner decides. (source: v2.0.0 canonical workflow inventory at commit 2222222222222222222222222222222222222222; confidence: high)

## Ordered upgrade plan

1. `implementation`: add only the gather-context task and its two adjacent edges; dependency: owner accepts the optional canonical improvement; review: task inputs, capabilities, and state-graph edges.
2. Run target `goobers validate`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation is clean and approved tuning is unchanged.
