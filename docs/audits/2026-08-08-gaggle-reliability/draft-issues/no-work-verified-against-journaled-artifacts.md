# An agent's `no-work` completes the run on its word alone, even when the evidence it was handed never arrived — classify it against journaled artifacts

Suggested labels: `area:runner`, `area:contracts`, `area:telemetry`, `type:bug`, `goobers:needs-human`

## Problem

A stage that reports `no-work` short-circuits its run straight to completed. The
runner accepts the claim unconditionally, skips every downstream stage, and
records a run that is indistinguishable — in the run list, in metrics, in every
health number an operator looks at — from a healthy empty tick.

That is correct behaviour when the agent looked and there was genuinely nothing
to act on. It is silent data loss when the reason the agent found nothing is that
the data never reached it. A fan-in stage whose eight upstream analyses all
succeeded but journaled nothing has no basis for any verdict at all; today it
completes the run and the eight analyses are gone with no record that anything
went wrong. The most consequential failure mode a nomination or analysis workflow
has — *it ran, it saw nothing, it said fine* — is the one the product cannot
report.

The runner is not missing information here. It holds, in the journal, exactly
what each upstream stage produced. It does not look.

## Evidence

- `docs/audits/2026-08-08-gaggle-reliability/domains/audit-nomination-flows.md`,
  finding `no-work-trusted-silent-loss`: run
  `313af282743c533c51f75ec9cab0b038` (2026-08-03, `status=completed`) — all eight
  `review-*` lens stages succeeded, but each `findingsRef` pointed at
  `~/.copilot/session-state/<uuid>/files/*.json` with `artifact_sizes:[]`
  (nothing journaled, and the paths were unreadable under the sandbox deny rule);
  the `triage` fan-in then returned `no-work` and the run completed with zero
  filings.
- Same file, finding `test-instability-wrong-repo`: 39 completed runs, every one
  ending in `no-work`, every one analysing the config repo instead of the target
  repo. Green, "completed", forever.
- `docs/audits/2026-08-08-gaggle-reliability/README.md`, "The nomination flows":
  "`no-work` is trusted unconditionally… The runner holds the evidence to
  cross-check (upstream journaled artifacts) and doesn't."
- Upstream contract as written to the agent, `internal/harness/prompt.go:170`:
  "Use `no-work` only when the task completed without error but found nothing to
  act on; the runner then completes the workflow without running downstream
  stages."
- Upstream enforcement, `internal/runner/run.go:2233-2245`: `ResultNoWork`
  short-circuits to `PhaseCompleted` unconditionally, never `t.Next` (#233's
  deliberate contract).
- Precedent that the runner already refuses one `no-work` on evidence:
  `internal/runner/run.go:3144-3156` inspects committed work before accepting an
  infrastructure-retry `no-work`, and `run.go:3185`
  (`preserveCommittedWorkOnInfraRetry`) rewrites it to success. Narrow case,
  right instinct (#2224).
- The artifact check that exists and why it did not fire:
  `internal/harness/executor.go:264-277` fails a stage whose declared
  `artifactFile` is missing (`declaredArtifactFailure`,
  `internal/harness/executor.go:315-322`, code `missing_declared_artifact`) — but
  only for the stage that was meant to produce it, and only if the workflow
  declared one. `artifactFile` is optional
  (`internal/mcpio/config.go:27,33`), and the silent-loss run declared none;
  `audit-nomination-flows.md` finding `artifact-handoff-contract-retrofit`
  records that the contract was retrofitted across the pipeline afterwards.
- Read-model landing zone already reserved: `internal/readmodel/schema.go:70-75`
  (`disposition` = `'produced' | 'no-work' | 'unknown'`, "reserved, not yet
  populated") and `internal/readmodel/project.go:15-27`, which sets
  `no-work` only when a run touched exactly one stage and never claims
  `produced`.
- Population shape (`telemetry.db`, gaggle `goobers`, `workflow LIKE
  'implementation%'`, `started_at >= '2026-08-01'`): 8,225 of 8,735 runs executed
  with zero stage re-entries and 8,185 of those completed. The uncontested empty
  tick is the overwhelming majority of all runs and must not change.

## Proposed direction

Make the runner choose between three honest outcomes instead of one, using
evidence it already journals, and never by asking the agent a second time. The
classification is derived entirely from what the workflow already declares —
there is no new field required to get correct behaviour.

At the point a stage reports `no-work`:

**1. Uncontested — behaves exactly as today.** The stage consumed no declared
upstream (no `inputsFrom`, no fan-in join, no input artifact), or every upstream
it consumed itself reported no work and journaled nothing. Complete the run,
`disposition: no-work`. This is the backlog-query empty tick — the dominant
population above, and a deliberate shared contract with production workflows.
Untouched, deliberately.

**2. Contested — completes, with a warning and a distinct disposition.** At least
one upstream stage this one consumed journaled a non-empty artifact, and this
stage reports nothing to act on. Both readings are real: a triage that read eight
genuine findings files and judged none worth filing is a well-calibrated triage,
and a triage that received them and dropped them looks identical from here. The
runner cannot tell and must not pretend to. Complete the run, record
`disposition: no-work-contested`, and journal a warning event naming the upstream
artifacts that were present when the claim was made. A moot outcome is a real
outcome; manufacturing a red run out of judgement teaches operators to ignore red
runs.

**3. Unfounded — the stage fails.** The stage declared upstream inputs and every
one of them journaled zero artifacts while reporting success. The stage judged
nothing because it was handed nothing; its verdict carries no information and
must not terminate a run as complete. Fail the stage with a stable code
(`no_work_without_evidence`) and let the workflow's own failure routing decide
what happens next. This is the eight-lost-lenses shape, and the only one that
should ever go red.

**Zero-config behaviour.** A workflow that declares no artifact handoffs and has
no fan-in stays in case 1 permanently — nothing changes for the cron ticks that
are most of the run population. The check bites exactly when an author declares
the handoff they should be declaring anyway, which is the same lever the
structured-artifacts wish tightens further. There is no way to opt into red runs
by writing an ordinary workflow.

**One escape hatch, off by default.** A task-level `noWork: strict | warn |
trust`, for the genuine exceptions: a stage whose upstream legitimately produces
artifacts it may ignore (`trust`), or an operator shaking down a new pipeline who
wants case 2 to fail too (`strict`). Progressive disclosure — correct behaviour
requires knowing nothing about it.

**Staging, because case 3 turns previously-green runs red.** Ship all three
classifications together with case 3 defaulting to `warn` for one release, so
operators see the warning volume on their own traffic first; flip the default to
`strict` as a follow-up gated on that observation. Landing the classification and
the enforcement in one step, with no observation window, is the failure mode this
proposal exists to argue against.

**Read model.** Add `no-work-contested` to the reserved `disposition` enum,
populated from the warning event. The point of reserving that column was that a
later value would be an additive change rather than a retrofit onto an ordering
index — this is the first value that needs it, and it should be coordinated with
the canonical terminal-outcome projection that owns the enum's definition and
with the run-list filter that will consume it. One hard constraint on that
coordination: a contested `no-work` must **not** be hidden by the "exclude
no-work runs" toggle, or the filter silently undoes the fix.

**Honest limit.** This does not catch the wrong-repo false green. Those runs
produced a well-formed artifact containing a truthful answer about the wrong
repository, so no artifact-presence check can see it; that needs deterministic-
stage repo pinning, which is a separate proposal. Stated here so the design is
not credited with more than it does.

**Version placement.** The classification is runtime behaviour and needs no DSL
change. The `noWork:` escape hatch is task-level DSL surface and should land in
2.0 only, consistent with the DSL 2.0 epic's finding that the 2.0 interpreter is
a strict superset of 1.4 and that 1.4 is heading for deprecation.

## Alternatives considered

- **Require the agent to justify its `no-work` (a mandatory reason or evidence
  field).** Rejected: the failure is that the agent's view of the world was
  wrong. A second self-report from the same view is not corroboration, and this
  codebase's repeated lesson on completion-contract defects is to fix the
  producer rather than widen the schema.
- **Hard-fail every contested `no-work`.** Rejected: the legitimate moot outcome
  is common and desirable. A rule that turns judgement into a failure trains
  operators to stop reading failures.
- **Duration or output-size heuristics ("a `no-work` under N seconds is
  suspect").** Rejected on both design and data grounds: the shortest `no-work`
  ticks are the healthiest runs in the instance, and a slow provider query can
  legitimately return nothing.
- **Solve it purely in the read model — classify after the fact, never change run
  status.** Insufficient for case 3: an unfounded `no-work` consumes a claim,
  skips every downstream stage, and reports success, so the work silently never
  happens. Cases 1 and 2 *are* read-model-only under this proposal, which is
  where the existing outcome-summary and filter work already lives.
- **Make `artifactFile` mandatory on fan-in stages.** Blunt: it breaks every
  workflow that does not declare one, and it still passes a stage that declares
  an artifact and writes an empty file. The classification proposed here treats a
  declared-but-empty artifact as no evidence, which is the property that matters.
- **Extend the existing infrastructure-retry check instead of generalising it.**
  That check is keyed to committed git work on a retried attempt; it cannot see
  artifacts, fan-in branches, or first attempts. Generalising the principle is
  cheaper than maintaining two unrelated no-work exceptions.

## Duplicate search

Searched 2026-08-08 against `Agent-Clubhouse/Goobers`, open and closed, terms:
`no-work`, `no-work verification`, `silent data loss`, `false green`, `agent
self-report`, `completion contract`, `trust the agent`, `disposition`, `verify
artifacts`, `cross-check`, `unverified`, `empty findings`.

- **#2638** (open, `goobers:approved` + `goobers:ready`) — the same symptom at
  the onboarding layer: a sample run reports success with no eligible issue. Its
  ruling of record explicitly fences the runtime: "backlog-query/runner no-work
  semantics are a deliberate shared contract (#233) with production workflows and
  MUST NOT change." This draft is written to respect that fence — case 1, the
  backlog-query tick, is untouched; only stages with declared upstream evidence
  are classified at all.
- **#1429** (open) defines the `produced | no-work | unknown` disposition
  contract for read consumers; **#1439** (open) filters on it; **#2188** (closed)
  is the narrow filter request that caused the column to be reserved. All three
  project or filter whatever the runner recorded; none verifies the claim. Delta:
  the producer-side classification and the new enum value, plus the constraint
  that #1439's toggle must not hide a contested run.
- **#2224** (closed) — infrastructure retry can discard committed work when the
  retry returns `no-work`. The same principle applied to one narrow case, and the
  code precedent this generalises. Cited as precedent, not duplicate.
- **#1849** (open) — a deterministic guard that can tighten an agentic verdict.
  Same shape one layer up (gate verdicts, not stage results); the two should
  share vocabulary. Not a duplicate.
- **#887**, **#849** (both closed) — earlier "run reports completed while the
  real work didn't happen" bugs, each fixed at its own stage. Evidence the class
  recurs; neither generalises.
- **#2522** (open) — completion-contract hint drift. Adjacent to the prompt text
  quoted above, unrelated to verification.
- No issue found proposing verification of an agent-declared `no-work` against
  journaled artifacts, under any term tried.

## Size and risk

**M.**

- Classification needs the run's upstream artifact record, which the journal
  already holds (`artifact.recorded`, and the artifact list on `stage.finished`).
  No new instrumentation, no new journal event types beyond the warning.
- Contained in `internal/runner` (the `ResultNoWork` branch and the fan-in join
  path), `internal/readmodel` (one enum value, projector change), plus the
  optional task field in `api/v1alpha1` with CRD/JSON-schema regeneration.
- Blast radius: every workflow containing an agentic stage that can report
  `no-work`. Observably small — the dominant population is case 1 — but case 3
  turns previously-green runs red, which is both the point and the risk; hence
  the warn-first release above.
- Read-model migration: a new `disposition` value, not a new column. Existing
  rows reproject. Portal must render the third value; a portal that does not know
  it should degrade to showing it as a no-work run with a warning badge, never
  hide it.
- Config migration: none required. The escape hatch is optional and DSL 2.0 only.
- Coordination cost is the main risk, not code: the disposition enum has an owner
  and a consumer already in flight, and adding a value without them lands two
  incompatible definitions of the same column.
