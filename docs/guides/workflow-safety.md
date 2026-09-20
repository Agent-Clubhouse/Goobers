# Advisory workflow safety lint

`goobers validate` and `goobers lint` analyze the compiled DSL 2.0 and 3.0
workflow graph for evidence, publication, feedback and recovery risks. The
daemon uses the same pass at startup and accepted configuration reload.
No flag, credentials, schema migration, agent, shell, or provider call is
required by this analysis. Rehearsal is a separate feature, not this pass.

All `SAF001` through `SAF007` findings are **strict-neutral**: even
`--strict` leaves them advisory. Existing validation errors and other
strict-promoted warnings retain their behavior. Findings never change claims,
scheduling, permissions, readiness, retry limits, run outcomes, or a compiled
workflow's execution identity.

The checked-in configuration CI gate also keeps these seven codes nonfatal
while printing them. Its existing warning allowlist still rejects other
unexpected warnings, including unrecognized safety codes.

| Code | Meaning |
| --- | --- |
| SAF001 | A recognized code review has no verified subject-patch route. |
| SAF002 | A PR rejection can terminate or park without a recognized publisher for that gate's verdict. |
| SAF003 | Rework's `contextFrom` excludes the rejecting gate's full verdict. |
| SAF004 | Terminal no-work bypasses an explicitly declared recovery obligation. |
| SAF005 | A known rejection cycle has no subject-changing effect. |
| SAF006 | Custom behavior, an invalid advisory annotation, parallel joins, or an analysis bound prevents complete coverage. |
| SAF007 | An advisory was suppressed, with its original code and reason retained for inspection. |

Findings include the gaggle, workflow, stage, YAML location where available,
a witness path, consequence, repair, confidence and coverage limits.
`--json` adds a `safety` object to the existing
`goobers.dev/diagnostics/v1` finding without changing existing field types;
`--github-annotations` emits the same message at the source location.
Instance, gaggle and workflow warnings retain the findings for the dashboard.
Reload health distinguishes rejected candidate warnings from applied
definitions through `definitionReload.candidateWarnings` and
`rejectionReason`. Accepted reload replaces the warning set, clearing repairs.
Default text status keeps manual-only workflows collapsed, including their
safety details. Use `status --all`, `status --workflow <name>`, JSON status,
or the dashboard to inspect those advisories.

## What the pass knows

The versioned command-effect catalog recognizes exact argv forms, not stage
names, comments, prompts or capability grants. It recognizes the runner's
implicit diff in a writable subject workspace, explicit
`git diff <base>...HEAD` artifacts, empty successful `git diff --check`,
selected-PR context, `apply-verdict [--gate <name>]`, terminal no-work
selectors and explicit `remediation-checkpoint --escalate`.

A publisher on another branch or for another gate does not satisfy a
rejection. `continueOnError` on publication includes a failed-publication
path. Terminal no-work also checks any pending publication obligation.
Budget-exhaustion paths use the production repass budget helper,
including the separate infrastructure budget and configured escalation
branch. Known non-changing cycles are not called infinite loops.
Unknown agents and scripts are not assumed unable to make progress.
Empty queues and intentional human parks do not imply a recovery defect.

Read-only evidence is deliberately qualified: a detached base checkout is
not the changed run branch, while local pinned-workspace behavior can differ.
An explicit subject patch satisfies either shape. Static analysis cannot
prove provider delivery, artifact contents, custom script behavior, or live
candidate eligibility. It does not inspect instruction prose.
Writable agents alone do not establish a code-review role: recognized code
operations, a selected PR, `commitsRepo`, or a review profile must establish it.
Sibling context only rebinds managed PR branches; evidence that depends on
that conditional binding is qualified rather than certified.
An explicit Git patch must also come from the subject workspace, not an
unrelated run branch or a base-only checkout. Self-comparisons such as
`git diff HEAD...HEAD` are known empty producers.

## Optional author assertions and suppression

Existing `metadata.annotations` can carry a small JSON object:

```yaml
metadata:
  annotations:
    goobers.dev/safety: >-
      {"version":1,"stages":{
        "review":{"review":"pr"},
        "capture-change":{"evidence":"patch"},
        "post-findings":{"publishes":"review"},
        "rewrite":{"changesSubject":true},
        "park":{"parks":true}
      }}
```

Stage names must exist. `review` applies to agentic gates and accepts
`code`, `pr`, or `internal` (intentionally non-code/internal-only review).
`evidence` accepts `patch` or `none`; it describes a custom producer,
not the contents of a current result. `publishes` names the gate whose
verdict is published. Gate-side publication must be explicitly asserted:
inherited write capabilities alone are not evidence of publication.

For a selector that really owes recovery on a no-work result, declare
`"recoveryOnNoWork":"handler-stage"`. Do not add that assertion to an
ordinary idle queue or deliberately excluded human-parked PR.

Assertions are trusted author statements, not verified execution or new
runtime behavior. Unknown fields, nonexistent stages, unsupported versions,
malformed JSON, and assertions contradicting a known command produce coverage
warnings, never new load errors. Assertions cover only their named effect;
a custom evidence assertion does not prove that the script cannot change
the subject. Gate-side publication can assert only that gate's own verdict.

Suppressions are scoped to this workflow and optionally one stage. Each
requires a rule and a nonempty reason; suppression does not hide its existence:

```yaml
metadata:
  annotations:
    goobers.dev/safety: >-
      {"version":1,"suppressions":[
        {"code":"SAF001","stage":"review",
         "reason":"The external review adapter supplies the pinned patch."}
      ]}
```

## Bounds and identity

The pass allows at most 2,048 abstract-state expansions, 128 path hops and
128 findings per workflow. Parallel joins stop that path with explicit
incomplete coverage rather than inventing successful branch outputs.
Reaching any bound emits `SAF006`; valid workflows still run.

Findings deduplicate by rule, scope and evidence and have identities including
the compiled configuration digest, inherited gaggle controls, catalog version
and binary identity.
The daemon's existing content-digest reload guard avoids repeating work on
unchanged polls. There is no cross-binary analysis cache. A changed binary
reanalyzes the configuration on boot. Default retry-bound diagnostics
explicitly state when an instance override is unavailable to config-tree
analysis rather than reporting the default as a guaranteed effective limit.

`BenchmarkSafetyFiveGaggleTwentyThreeWorkflows` measures the added pass on
a synthetic five-gaggle/23-workflow fixture (11 tasks and a review gate per
workflow, mixed evidence routes and bounded rework), with compile time excluded.
`TestSafetyFiveGaggleTwentyThreeWorkflowBudget` enforces the two-second target.
Reference measurement on September 19, 2026: Windows/amd64, AMD EPYC 7763
64-Core Processor, benchmark concurrency 32; approximately 2.2 milliseconds
and 828 KB allocated per complete 23-workflow pass. This synthetic fixture
is a regression target, not a guarantee for arbitrary graphs.
No clean result claims that arbitrary workflows cannot fail.
