# Upgrade advisory: pre-stable-breaking-change

## Scope and provenance

- Current binary: /opt/goobers-v0.7.0/bin/goobers version v0.7.0 commit 0000000000000000000000000000000000000000
- Target binary: /opt/goobers-v0.9.0-beta.3/bin/goobers version v0.9.0-beta.3 commit 1111111111111111111111111111111111111111
- Exact target ref: `refs/tags/v0.9.0-beta.3` at commit `1111111111111111111111111111111111111111`
- Contract source: github:Agent-Clubhouse/Goobers@refs/tags/v0.9.0-beta.3 (commit 1111111111111111111111111111111111111111)
- Config source: local-dir:/workspace/config at analyzed digest pre-stable-current
- Canonical root: /opt/goobers-v0.9.0-beta.3/config-examples
- Source provenance: github:Agent-Clubhouse/Goobers@1111111111111111111111111111111111111111 release notes and feature registry
- Compatibility confidence: `low`

## Workflow `example/implementation` (`0.7` -> `2.0`)

Feature inventory:
- `task.inputs`: `ga` - replace the incompatible legacy claim input shape with a backlog-query task and a resultFile artifact. (source: v0.9.0-beta.3 release notes at commit 1111111111111111111111111111111111111111; confidence: low)
- `gate.branches`: `ga` - the needs-changes outcome now requires an explicit branch. (source: v0.9.0-beta.3 release notes at commit 1111111111111111111111111111111111111111; confidence: low)

Validation diagnostics:
- target baseline: dslVersion 0.7 is unsupported and the legacy claim input shape is incompatible
- proposed target validation: clean (0 warnings, 0 errors)

Target canonical state-graph diff:
- current: `start: claim; claim[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort)`
- target: `start: query-backlog; query-backlog[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort, needs-changes -> implement)`
Proposed state-graph diff:
- current: `start: claim; claim[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort)`
- proposed: `start: query-backlog; query-backlog[deterministic] -> implement; implement[deterministic] -> review; review[gate](pass -> done, fail -> abort, needs-changes -> implement)`

Recommendations:
- **required compatibility change**: dslVersion `0.7` is unsupported; move to `2.0`. (source: github:Agent-Clubhouse/Goobers@1111111111111111111111111111111111111111 release notes and feature registry; target refs/tags/v0.9.0-beta.3 at commit 1111111111111111111111111111111111111111; confidence: low)
- **required compatibility change**: feature `task.inputs` changed; replace the incompatible legacy claim input shape with a backlog-query task and a resultFile artifact. (source: v0.9.0-beta.3 release notes at commit 1111111111111111111111111111111111111111; target refs/tags/v0.9.0-beta.3 at commit 1111111111111111111111111111111111111111; confidence: low)
- **required compatibility change**: feature `gate.branches` changed; the needs-changes outcome now requires an explicit branch. (source: v0.9.0-beta.3 release notes at commit 1111111111111111111111111111111111111111; target refs/tags/v0.9.0-beta.3 at commit 1111111111111111111111111111111111111111; confidence: low)

## Ordered upgrade plan

1. `example/implementation`: in an isolated scratch copy, run `manual compatibility patch from the pinned 1.0 contract` for `0.7 -> 1.0`; dependency: pre-stable release provenance approved; expected file diff: gaggles/example/workflows/implementation.yaml replaces claim with query-backlog and bumps dslVersion to 1.0; review: legacy claim replacement and graph edge changes; validation command: `/opt/goobers-v0.9.0-beta.3/bin/goobers validate --strict --source-tree <scratch-1.0>`.
2. `example/implementation`: in an isolated scratch copy, run `goobers fix --to 2.0` for `1.0 -> 2.0`; dependency: the 0.7 -> 1.0 result validates; expected file diff: gaggles/example/workflows/implementation.yaml adds the needs-changes branch and bumps dslVersion to 2.0; review: mechanical diff and explicit needs-changes branch; validation command: `/opt/goobers-v0.9.0-beta.3/bin/goobers validate --strict --source-tree <scratch-2.0>`.
3. Run target `goobers validate --strict`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation has no warnings and approved tuning is unchanged.

## Write readiness

`blocked`: pre-stable release evidence is low confidence and requires provenance approval before either adjacent edge
