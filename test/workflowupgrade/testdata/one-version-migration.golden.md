# Upgrade advisory: one-version-migration

Target release: `v2.0.0` (`2222222222222222222222222222222222222222`)

Source provenance: target binary v2.0.0 (2222222222222222222222222222222222222222) fix dry-run and feature output

Compatibility confidence: `high`

## Workflow `default-implement` (`1.4` -> `2.0`)

Feature inventory:
- `gate.automated.pollIntervalSeconds`: `ga` - DSL 2.0 injects a 10-second ci-poll default, so pin 10 to preserve DSL 1.4 behavior. (source: v2.0.0 goobers fix --to 2.0 dry-run from commit 2222222222222222222222222222222222222222; confidence: high)

State graph: unchanged: `poll -> ci; ci(pass -> done, fail -> abort, timeout -> escalate)`

Recommendations:
- **required compatibility change**: feature `gate.automated.pollIntervalSeconds` changed; DSL 2.0 injects a 10-second ci-poll default, so pin 10 to preserve DSL 1.4 behavior. (source: v2.0.0 goobers fix --to 2.0 dry-run from commit 2222222222222222222222222222222222222222; confidence: high)

## Ordered upgrade plan

1. `default-implement`: in an isolated scratch copy, run `goobers fix --to 2.0` for `1.4 -> 2.0`; dependency: baseline and target provenance approved; review: generated dslVersion bump and poll interval pin. Apply that edge only after review, then validate it with the target interpreter.
2. Run target `goobers validate`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation is clean and approved tuning is unchanged.
