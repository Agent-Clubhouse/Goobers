# Upgrade advisory: pre-stable-breaking-change

Target release: `v0.9.0-beta.3` (`1111111111111111111111111111111111111111`)

Source provenance: github:Agent-Clubhouse/Goobers@1111111111111111111111111111111111111111 release notes and feature registry

Compatibility confidence: `low`

## Workflow `implementation` (`0.7` -> `1.4`)

Feature inventory:
- `task.inputs.claimQuery`: `removed` - replace the legacy claim input with a backlog-query task and a resultFile artifact. (source: v0.9.0-beta.3 feature registry at commit 1111111111111111111111111111111111111111; confidence: low)
- `gate.evaluator.agent`: `ga` - the needs-changes outcome now requires an explicit branch. (source: v0.9.0-beta.3 release notes at commit 1111111111111111111111111111111111111111; confidence: low)

State-graph diff:
- current: `claim -> implement; implement -> review; review(pass -> done, fail -> abort)`
- target: `query-backlog -> implement; implement -> review; review(pass -> done, needs-changes -> implement, fail -> abort)`

Recommendations:
- **required compatibility change**: dslVersion `0.7` is unsupported; move to `1.4`. (source: github:Agent-Clubhouse/Goobers@1111111111111111111111111111111111111111 release notes and feature registry; confidence: low)
- **required compatibility change**: feature `task.inputs.claimQuery` is removed; replace the legacy claim input with a backlog-query task and a resultFile artifact. (source: v0.9.0-beta.3 feature registry at commit 1111111111111111111111111111111111111111; confidence: low)
- **required compatibility change**: feature `gate.evaluator.agent` changed; the needs-changes outcome now requires an explicit branch. (source: v0.9.0-beta.3 release notes at commit 1111111111111111111111111111111111111111; confidence: low)

## Ordered upgrade plan

1. `implementation`: in an isolated scratch copy, run `manual compatibility patch from the pinned 1.0 contract` for `0.7 -> 1.0`; dependency: pre-stable release provenance approved; review: legacy claim replacement and graph edge changes. Apply that edge only after review, then validate it with the target interpreter.
2. `implementation`: in an isolated scratch copy, run `goobers fix --to 1.4` for `1.0 -> 1.4`; dependency: the 0.7 -> 1.0 result validates; review: mechanical diff and explicit needs-changes branch. Apply that edge only after review, then validate it with the target interpreter.
3. Run target `goobers validate`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation is clean and approved tuning is unchanged.
