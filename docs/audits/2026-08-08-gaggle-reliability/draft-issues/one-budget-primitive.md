# One remediation-budget declaration covering every cause, present and future — instead of four required inputs that crash the stage when omitted

Suggested labels: `area:workflows`, `area:contracts`, `type:feature`, `goobers:needs-human`

## Problem

`remediation-checkpoint` requires four per-cause budget inputs — `conflictBudget`, `substantiveBudget`, `failingCIBudget`, `siblingOverlapBudget` — and hard-fails the stage if any one of them is unset, the first time any remediation cause fires. The requirement is invisible everywhere an author would look for it: `task.inputs` is an untyped string map in the workflow schema, so `goobers validate` structurally cannot see that a stage requires an input, and the only statement of the requirement anywhere is one sentence in a man page and a comment in the reference workflow.

What a user experiences: a config that validates clean, deploys clean, and runs clean for days, then crashes on a live PR with an error naming a YAML key they have never seen — at a moment chosen by which remediation cause happens to fire first. The stage does not degrade, escalate, or default; it exits with a provider error and burns the cycle.

The runtime has already conceded that hand-listing does not scale. When a fifth cause (human comment) was added, its budget was made optional with a documented default, and the code says why: requiring it "would fail every already-deployed workflow the moment it upgraded to a binary that reads it." That reasoning is correct — and it applies with exactly equal force to the other four, and to the sixth cause whenever it arrives. As things stand, the runtime can only grow its cause vocabulary by adding permanently-optional causes or by breaking every deployed workflow.

Three separate promises are broken at once: config that validates is not config that can run; a field's requirement is not discoverable from the config surface; and adding a runtime capability is a compatibility event for every workflow author.

## Evidence

- VISION wish 4, "One real budget/backpressure primitive (needs upstream)" — `goobers-instances` `VISION.md` (branch `mason/dsl-2.1-wishlist`): an author bounding a remediation loop "should set one field, once, and have it apply to every cause the runtime currently knows about *and every cause it grows later* — not hand-list N inputs by name and silently 500 the day a workflow forgets one or the runtime adds an N+1th cause nobody's workflow declared a budget for yet."
- `docs/audits/2026-08-08-gaggle-reliability/domains/audit-site-runs.md`, finding `sibling-overlap-budget-live-at-shutdown` [HIGH/confirmed]: the only failure class still live in the second gaggle's final minute. 4 of its 5 pr-remediation runs ever — `25432fafac06c9cc79b3c39d348a4a20`, `4ff82a2bc4d93e4abdb9ab7aa80c9ded`, `88f7cdf968526111c61cc0f1425e7367`, `0b9a10fe6632e2d99e3863ef54bf9004` — failed with `siblingOverlapBudget must be a positive integer, got ""`, last at 18:47:31Z.
- Same audit, `README.md` regime table: this is one of the two "final-hour config bugs" that made up the fourth and last failure regime of that gaggle's life.
- `docs/audits/2026-08-08-gaggle-reliability/domains/audit-drift-audit.md` (finding at line 30): the copied workflow dropped the sibling-overlap *classification stage* but the sibling-overlap *cause* still arrives from merge-review's post-merge fan-out — so the required input was dropped while the thing that requires it was not. The finding's own conclusion: "This is the operator's own live proof of VISION wish 4."
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/README.md`, systemic finding 2 — "Prose knows what tooling doesn't enforce": across five fresh-eyes onboardings, `validate` caught 32% of authoring friction (14 of 44 tweaks); the documented traps were the unchecked ones.
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-dotnet.md` line 89, gap (3): "`DefaultMaxRepasses = 3` appears only in `docs/design/human-in-the-loop.md`, and whether a parallel's `onFailure` route counts against a repass budget is stated nowhere, so I cannot tell whether my implement → dual-review → implement loop is bounded." Budget legibility is a first-hour problem, not only a drift problem.
- Upstream code (worktree at `86ad1f70`):
  - `cmd/goobers/remediationcheckpoint.go:1221-1238` — `declaredRemediationBudgets` reads the four legacy inputs and returns an error when any is absent, unparseable, or `<= 0`.
  - `:1206-1211` — the design concession, in the code's own words: `humanCommentBudget` "is a DEFAULT rather than a required input (unlike the four legacy budgets) … requiring it would fail every already-deployed workflow the moment it upgraded to a binary that reads it."
  - `:1239-1251` — the correct behavior, already implemented for exactly one cause: absent → documented default; present-but-invalid → hard error, so a typo cannot silently take the default.
  - `:29-53` — the cause vocabulary (`remediationCauseOrder`: conflict, substantive, failing-ci, sibling-overlap, human-comment). Adding a sixth cause today means adding a sixth input name.
  - `:632` and `:1213-1220` — `--budget` ("override every DSL-declared per-cause budget") already implements one-number-for-every-cause. The semantics exist in the binary; they are simply unreachable from the DSL.
  - `api/schemas/workflow.schema.json` — `task.inputs` is `{"type":"object","additionalProperties":{"type":"string"}}`, so no stage can publish an input contract and `validate` has nothing to check against.
  - `reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml:302-311` — the shipped example sets all five budgets to `"2"`; the only correct configuration anyone has is a copy of that block.
  - `docs/man/goobers-remediation-checkpoint.1:20` and `docs/cli/README.md:2044` — the requirement ("the five per-cause budget inputs (humanCommentBudget defaults to 2 when undeclared)") is stated exactly once, in reference documentation an author reaches after the fact.

## Proposed direction

**One field, with the per-cause names demoted to optional overrides, and no configuration ever fatal.**

```yaml
inputs:
  resultFile: "checkpoint-result.json"
  # Applies to every remediation cause the runtime knows — now and after it
  # grows new ones. Per-cause overrides below are optional.
  remediationBudget: "2"
  # failingCIBudget: "4"   # override one cause when you have a reason
```

1. **`remediationBudget` sets the allowance for every cause in `remediationCauseOrder`, including causes added after the workflow was written.** These are precisely the semantics `--budget` already implements at `remediationcheckpoint.go:1213-1220`; this promotes them from a diagnostics flag to the primary DSL surface.
2. **The five `<cause>Budget` inputs keep their names and meaning and become optional overrides.** Rich options behind a smart default: one field for everyone, five for the author with a specific reason.
3. **An absent budget is never a failure.** Resolution order: per-cause input → `remediationBudget` → a documented constant `defaultRemediationBudget = 2` (matching today's `defaultHumanCommentBudget` and the value every shipped and instance workflow already sets). A *present but invalid* value stays a hard error — a typo must not silently inherit a default. This is exactly the rule `:1239-1251` already applies to `humanCommentBudget`, generalized.
4. **Make the growth rule a test, not a convention.** A regression test that iterates `remediationCauseOrder` and asserts every cause resolves a budget with no per-cause input declared, so adding cause N+1 cannot reintroduce a required input.
5. **Close the typo hole the defaults open.** Once absent inputs default, `siblingoverlapBudget` (wrong case) stops crashing and silently gets the default instead. `goobers validate` must therefore reject an input ending in `Budget` on a `remediation-checkpoint` stage whose prefix names no known cause. Narrow and worth shipping in the same change. The general problem — that `task.inputs` is an untyped string map, so *no* stage can publish its input contract — is larger and is deliberately not proposed here.
6. **Say the effective budget out loud.** The checkpoint's stage result and sticky comment already carry `escalationOutcome: budget-exhausted`; they should also name the budget that was exhausted and where it came from (per-cause input / `remediationBudget` / default), so "why did this escalate at 2" is answerable from the run rather than from the YAML.

**Smart defaults — what zero-config does.** A pr-remediation workflow that declares no budget input at all runs, allows 2 attempts per cause, records that value in the escalation record, and never returns a 500. The shipped reference workflow drops from five budget lines to one, with a commented override showing the next rung. Nothing about the per-cause *semantics* already established changes — distinct causes still own independent allowances; only the declaration ergonomics and the omission behavior change.

**Repass interaction — stated, not folded in.** This is not the repass budget, and unifying them here would be wrong. `MaxRepasses` (`internal/runcontrol`, default 3; `internal/gate/evaluate.go:131`) bounds gate traversals inside a single run, and the audit's `repass-budget-not-composed` finding shows it is enforced per-gate, so a run alternating between the review gate and the CI gates blows straight past it — one implementation run executed 16 implement sessions over 4.75 hours. The remediation budget bounds attempts at a *cause* across successive runs against one PR. Different scopes, different persistence, different timescales; the repass fix additionally needs a run-scoped counter that does not exist yet and sits behind an open correctness bug. A separate draft in this bundle (`repass-budget-composed-per-run.md`) proposes the repass-composition fix. The two should share vocabulary — "budget", "exhausted", the `escalationOutcome` enum — and must not share a field. Filing them together would make this zero-risk change wait on that design.

## Alternatives considered

- **Just make the four legacy inputs optional and stop (a three-line change).** Why not: it removes the crash but leaves five knobs and still no way to say "two attempts per cause" once. The one-field semantics already exist behind `--budget`; shipping only half leaves the wish open. That said, if only one piece can ship, the defaults are the half that ends the production failure.
- **Teach `goobers validate` the stage's required inputs and error on omission.** Why not: turning a runtime crash into an authoring error is an improvement, but it preserves the hand-listing and still breaks every deployed workflow the day cause N+1 lands — the exact failure wish 4 names. Worth doing only in the narrow typo-check form above.
- **A workflow-level `remediation:` block with a `budget:` map.** Why not: `task.inputs` is string-valued, so a nested map needs new schema surface and a bespoke resolution path for one stage. A string input plus per-cause overrides buys the same ergonomics with no schema change and lands identically on DSL 1.4 and 2.0.
- **Fold this into the repass budget as one universal retry primitive.** Why not: see above — different scopes, and the repass side has an open critical correctness bug that would block a change that is otherwise a day's work.
- **Keep the hard-fail as a deliberate fail-closed safety property.** Why not: it does not fail closed, it fails loud and late. The crash arrives on the first PR that happens to hit that cause, days after deploy. Failing closed would mean failing at `validate`.

## Duplicate search

Searched 2026-08-08 (read-only), `gh search issues --repo Agent-Clubhouse/Goobers`, open and closed, plus `gh issue view` on each candidate. Terms: `budget`, `per-cause budget`, `per-cause`, `remediation budget`, `budget primitive`, `budget default`, `remediation cause`, `future cause`, `maxAttempts`, `retry budget`, `repass budget`, `maxRepasses`, `backpressure`, `humanCommentBudget`, `conflictBudget`, `siblingOverlapBudget`, `required input`, `unset input`, `hard-fail`, `positive integer`, `declared inputs`, `stage input`, `escalate cause`.

Nearest existing issues and the delta:

- **#953** (closed 2026-07-19) / **#1791** (merged) — *pr-remediation: budget attempts PER CAUSE, not as one flat counter*. The issue that created this surface. Its "Configurability" section asks only that budgets be "DSL-declared inputs … rather than Go constants, so an operator can tune per workflow without a rebuild"; it never considers omission, defaults, or a growing cause set. This proposal preserves its per-cause semantics and fixes its ergonomics — follow-through, not reversal.
- **#941** (closed) — *PRR-6: declare the remediation policy in the DSL instead of hardcoding it in Go*. The sibling design issue #953 references. Same lineage, same gap: declaring in the DSL, with no story for what an undeclared field means.
- **#2395** (merged 2026-08-04) — *treat a new human comment as a remediation cause*. Where the optional-with-default pattern was invented, for exactly one cause. This proposal generalizes that decision instead of repeating it per cause.
- **#1394** (closed) and **#1417** (closed) — *remediation-checkpoint hard-fails with unresolved `selectedNumber` / `attemptedHeadSha` when rebase-pr fails*. Same defect **class** (the checkpoint crashes on a value it declared required), different mechanism: those are `inputsFrom` wiring on failure paths, not static `inputs`. Both were fixed per-field, and #1417's own text argues the fix "needs to cover every `rebase-pr` failure-exit path uniformly, not just the ones exercised so far." Neither covers static budget inputs; together they are the strongest precedent that per-field patching of this stage recurs.
- **#1061** (closed) — *apply-verdict fails with 'selectedHeadSha is required'*. Same class again, different stage, fixed per-field.
- **#1973** (open, `goobers:critical`) — *trackRepass resets the repass budget on gate pass*. Different axis (gate repass within a run) and the reason repass is not folded in here.
- **#1698** (merged) / **#1671** (closed) — *run-control budgets (MaxRepasses, StalledRunTimeout) are process/instance-wide singletons — no per-workflow or per-gaggle override*. Adjacent: made run-control budgets overridable. Touches neither remediation causes nor omission behavior.
- **#390** (merged) — *durable per-PR repass budget + same-diff escalation*. Prior art for durable per-PR counters; predates per-cause budgets.
- **#1022** (open) — *Budget hierarchy: token/cost budgets at stage/run/workflow/gaggle/instance*. A different axis entirely (spend, not retries). Cross-reference so the two do not invent conflicting vocabulary; it does not cover retries-per-cause.
- **#2272** (closed) — *total-elapsed-time run timeout, independent of per-gate repass counters and the stalled-run timeout*. Confirms upstream already treats these as separate axes.
- **#2701**, **#2702**, **#2398** (all open) — pr-remediation parking, re-claiming, and human-comment responsiveness. Adjacent lane behavior; none touches budget declaration or omission.
- **#2695** (open epic) and children #2696–#2700 — DSL 2.0 / 1.4 deprecation. Version lifecycle only; no stage-input surface. This proposal needs no schema change and behaves identically under both interpreters, so it does not depend on the epic's sequencing.

No open or closed issue proposes a single budget declaration covering all causes, or defaults for the four legacy inputs. Filing whole, not narrowed.

## Size and risk

**Size: S** for the budget resolution (the validate typo-check adds a small M-shaped tail if the check is generalized; keep it narrow and it stays S).

Blast radius:
- `cmd/goobers/remediationcheckpoint.go` — `declaredRemediationBudgets` plus its tests (`remediationcheckpoint_test.go` already asserts rejection of invalid `conflictBudget`; add cases for absent-input defaulting and for the cause-set enumeration test).
- `reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml` — collapse five lines to one plus a commented override.
- Regenerate `docs/man/goobers-remediation-checkpoint.1` and `docs/cli/README.md` — a new input name requires the docs/man regeneration pass, and the current text asserts five required inputs.
- One narrow `goobers validate` check for unknown `*Budget` input names on this stage.

No schema change, no interpreter change, no journal or telemetry change.

Migration: **none required.** Existing workflows declaring four or five budgets keep working byte-for-byte — their inputs become overrides carrying the same values. The instance configs can collapse to one line whenever convenient; the reference workflow should collapse in the same PR so the thing everyone copies teaches the new shape.

Risks:
- *A workflow that omitted a budget now runs instead of crashing.* That is the intent, and the default equals the value every shipped and instance workflow already sets, so no live behavior changes except that the crash stops. Still worth calling out in the PR description: it is a silent behavior change for anyone who was relying on the crash as a smoke alarm.
- *Typos become silent* once absent inputs default. Mitigated by point 5; the two must ship together, not sequentially.
- *Vocabulary collision with the proposed token/cost budget hierarchy.* Mitigated by naming this field `remediationBudget` rather than a bare `budget`, leaving the generic term free for the spend axis.
