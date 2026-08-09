# Draft issues — awaiting operator review (2026-08-08)

Eighteen drafts, none filed. Every draft carries its own upstream duplicate
search (run today — the landscape moves same-day) with the nearest existing
issues and the surviving delta. Notable kills/shrinks the searches produced:
conditional-GET caching and poll dedup were **already shipped** (#1053), the
credential `repos:` qualifier folds into open #1794 rather than competing,
the DSL-version discoverability half of the parallels draft is owned by the
new 2.0 epic (#2695/#2698/#2700), and repass accounting overlaps approved
#1973 (the draft narrows to the composition + projection delta).

## Recommended filing order

**Tier 1 — beta-blockers (config-as-truth / catches-mistakes-early):**
- `sibling-ordering-strategies.md` — the zero-merge deadlock's structural fix, operator-ratified "both, opt-in" (added 2026-08-09 after the live incident)
- `validate-materialize-effective-capability-grants.md` — the K4 class; also proposes the missing scoped-override DSL surface (fold into #1794)
- `structured-artifact-handoffs.md` — VISION wish 1; the incident class prompt-hints cannot reach
- `one-budget-primitive.md` — VISION wish 4; ~3-line minimum viable half flagged
- `first-class-script-stages-contract-surface.md` — the ratified "both" direction
- `stages-declare-environmental-assumptions-gaggle-preflight.md` — the general preflight framework beyond this branch's CAP003

**Tier 2 — hums / trustworthy:**
- `repass-budget-composed-per-run.md` — per-run ceiling + gate-repass projection (evidence-derived defaults)
- `no-work-verified-against-journaled-artifacts.md` — contested/unfounded no-work classification, warn-first
- `telemetry-projections-key-on-payload-not-workflow-name.md` — #2494 concrete design; retires two lint exceptions
- `branch-level-verdict-routing-for-fan-out-review.md` — `@fail-branch` + compiler-cloned branch sinks
- `parallel-authoring-defaults-and-discoverability.md` — silent-drop fix, projection parity, concurrency default

**Tier 3 — parity and DX:**
- `ado-merge-review-and-lifecycle-parity.md` — **[EPIC, customer-blocking]** the ADO half that isn't wired: merge chain, lifecycle close, assignee/identity parity (added 2026-08-09 from the live deep-dive; claim-tag leak already fixed on-branch)
- `ado-capability-vocabulary-dead-values-and-github-spelled-requirements.md` — note: the `report-pr-status` fix already exists in v_next and needs **backporting to v_current**; that slice is Tier-1 severity for any ADO user
- `ado-onboarding-path-parity.md`
- `ci-command-multi-step-and-local-ci-inheritance.md` — docs stopgap is S and independently shippable
- `init-noninteractive-standard-workflow-selection.md`
- `run-provenance-binary-version-and-daemon-lifecycle-telemetry.md`
- `agentic-github-mutations-invisible-to-provider-telemetry.md` — proxy-based capture; a gh-only shim would miss this instance's MCP-based mutations entirely
- `github-quota-efficiency-options-ladder.md` — deliberately an options ladder, not a rearchitecture
- `dx-docs-debt-batch-mutations-sidecar-and-authoring-contracts.md`

Filing notes: assign filed issues to the operator per instance convention;
DSL-surface proposals target `v_next` under epic #2695; re-run each draft's
duplicate search at filing time.
