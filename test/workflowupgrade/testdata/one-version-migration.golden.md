# Upgrade advisory: one-version-migration

## Scope and provenance

- Current binary: /opt/goobers-v1.4.0/bin/goobers version v1.4.0 commit aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
- Target binary: /opt/goobers-v2.0.0/bin/goobers version v2.0.0 commit 2222222222222222222222222222222222222222
- Exact target ref: `refs/tags/v2.0.0` at commit `2222222222222222222222222222222222222222`
- Contract source: github:Agent-Clubhouse/Goobers@refs/tags/v2.0.0 (commit 2222222222222222222222222222222222222222)
- Config source: local-dir:/workspace/config at analyzed digest one-version-current
- Canonical root: /opt/goobers-v2.0.0/config-examples
- Source provenance: target binary v2.0.0 (2222222222222222222222222222222222222222) fix dry-run and feature output
- Compatibility confidence: `high`

## Workflow `example/default-implement` (`1.4` -> `2.0`)

Feature inventory:
- `gate.evaluator.automated.pollIntervalSeconds`: `ga` - DSL 2.0 injects a 10-second ci-poll default, so pin 10 to preserve DSL 1.4 behavior. (source: v2.0.0 goobers fix --to 2.0 dry-run from commit 2222222222222222222222222222222222222222; confidence: high)

Validation diagnostics:
- target baseline: dslVersion 1.4 remains loadable but requires the migration default-preservation patch
- proposed target validation: clean (0 warnings, 0 errors)

Target canonical state graph: unchanged: `start: poll; poll[deterministic] -> ci; ci[gate](pass -> done, fail -> abort, timeout -> escalate)`
Proposed state graph: unchanged: `start: poll; poll[deterministic] -> ci; ci[gate](pass -> done, fail -> abort, timeout -> escalate)`

Recommendations:
- **required compatibility change**: feature `gate.evaluator.automated.pollIntervalSeconds` changed; DSL 2.0 injects a 10-second ci-poll default, so pin 10 to preserve DSL 1.4 behavior. (source: v2.0.0 goobers fix --to 2.0 dry-run from commit 2222222222222222222222222222222222222222; target refs/tags/v2.0.0 at commit 2222222222222222222222222222222222222222; confidence: high)

## Ordered upgrade plan

1. `example/default-implement`: in an isolated scratch copy, run `goobers fix --to 2.0` for `1.4 -> 2.0`; dependency: baseline and target provenance approved; expected file diff: gaggles/example/workflows/default-implement.yaml bumps dslVersion and pins automated pollIntervalSeconds to 10; review: generated dslVersion bump and poll interval pin; validation command: `/opt/goobers-v2.0.0/bin/goobers validate --strict --source-tree <scratch-2.0>`.
2. Run target `goobers validate --strict`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation has no warnings and approved tuning is unchanged.

## Write readiness

`ready for explicit write`: the exact target binary and contracts agree, the adjacent goobers fix edge is available, and the proposed config validates cleanly
