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

## Workflow `implementation` (`2.0` -> `2.0`)

Feature inventory:
- `workflow.spec.triggers`: `ga` - unchanged at the target. (source: v2.0.0 features --used and --dsl-version 2.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- `task.inputs`: `deprecated` - prefer content-digested context pointers before the next removal edge. (source: v2.0.0 feature registry at commit 2222222222222222222222222222222222222222; confidence: high)

Validation diagnostics:
- target validation: clean; canonical comparison reports only tuning, structural, and user-owned differences
- proposed target validation: clean (0 warnings, 0 errors)

Target canonical state-graph diff:
- current: `query -> implement; implement -> review; review(pass -> done, fail -> abort, timeout -> escalate)`
- target: `query -> gather-context; gather-context -> implement; implement -> review; review(pass -> done, fail -> abort, timeout -> escalate)`
Proposed state-graph diff:
- current: `query -> implement; implement -> review; review(pass -> done, fail -> abort, timeout -> escalate)`
- proposed: `query -> gather-context; gather-context -> implement; implement -> review; review(pass -> done, fail -> abort, timeout -> escalate)`

Recommendations:
- **recommended canonical workflow improvement**: feature `task.inputs` is deprecated; prefer content-digested context pointers before the next removal edge. (source: v2.0.0 feature registry at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- **local operational tuning**: `spec.triggers[0].schedule`: config diff reports INFO; preserve the local 17-minute cadence. (source: v2.0.0 config diff INFO at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- **local operational tuning**: `spec.readiness.maxConcurrentRuns`: config diff reports INFO; preserve the local concurrency limit. (source: v2.0.0 config diff INFO at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)
- **recommended canonical workflow improvement**: `spec.tasks[gather-context]`: the same-name canonical workflow adds context before implementation. (source: v2.0.0 canonical implementation workflow at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)

## Workflow `nightly-release` (`2.0` -> `2.0`)

Feature inventory:
- `stage.run.command`: `ga` - unchanged at the target. (source: v2.0.0 features --used at commit 2222222222222222222222222222222222222222; confidence: high)

Validation diagnostics:
- target validation: clean; no same-name canonical workflow exists
- proposed target validation: clean (0 warnings, 0 errors)

Current state graph: `build -> deploy; deploy -> done`
Target canonical state graph: no same-name canonical workflow exists.
Proposed state graph: unchanged: `build -> deploy; deploy -> done`

Recommendations:
- **user customization requiring human judgment**: `spec.tasks[deploy].run.command`: no same-name canonical workflow exists; retain the command until its owner decides. (source: v2.0.0 canonical workflow inventory at commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)

## Ordered upgrade plan

1. `implementation`: add only the gather-context task and its two adjacent edges; dependency: owner accepts the optional canonical improvement; expected file diff: gaggles/example/workflows/implementation.yaml adds only gather-context and rewires query -> gather-context -> implement; review: task inputs, capabilities, and state-graph edges; validation command: `/opt/goobers-v2.0.0/bin/goobers validate --strict --source-tree <scratch-canonical-improvement>`.
2. Run target `goobers validate --strict`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation has no warnings and approved tuning is unchanged.

## Write readiness

`analysis-only`: the owner must separately approve the optional gather-context change; the unmatched nightly-release workflow remains untouched
