# Fan-out review can only be written inverted: a gate verdict cannot fail its own branch, branches cannot share a park stage, and the join cannot see verdicts

Suggested labels: `area:contracts`, `area:runner`, `type:feature`, `goobers:needs-human`

## Problem

The parallel construct executes correctly. Authoring a *review* fan-out on top of it does not compose with the way every shipped review loop is written.

A branch is a closed, disjoint subgraph whose only exits are `@join`, `@abort`, and `@escalate`. A gate's verdict targets are ordinary transition targets, so the most load-bearing edge in every review loop — `needs-changes: <implementation stage>` — is structurally illegal inside a branch. A verdict cannot leave a branch as a verdict; it can only leave as a branch *status*. Three costs follow.

1. **No soft branch failure.** The only way to turn a `needs-changes` verdict into the branch failure a failure policy can route is to invent a deterministic stage whose entire body is `exit 1` and route the verdict there.
2. **Dead edges are mandatory.** That stage still needs a branch-terminal successor to satisfy the branch-reachability rule, so it declares `next: "@join"` — an edge unreachable by construction.
3. **Shared terminal stages must be hand-cloned.** Two branches whose gates both park cannot name one park stage. Disjointness forces a byte-identical copy per branch, scaling linearly with branch count.

At the join, the completeness record carries each branch's terminal status and artifact count, never what any branch's gate decided — so the more natural `continue_on_error` plus a deciding join is unavailable. A deterministic join cannot read even the status record: the invocation's inputs are projected to the stage environment as strings only, so the completeness record reaches an agentic join (serialized into its prompt) and reaches a deterministic one as nothing at all, with no error.

Measured on a two-reviewer fan-out: 2 exit-1 stages, 2 cloned park stages, 2 dead edges, and 4 workspace declarations — roughly 55 of 90 fan-out-specific YAML lines are scaffolding. The stated intent, "both reviewers must pass," appears nowhere in the file. It is an emergent property of a failure policy plus two stages whose job is to fail, and a reader cannot recover it from the config. That is the opposite of config-as-truth.

The natural first attempt — copy a shipped `review` gate into each branch unchanged — is rejected with one unwrapped 3,372-character line of 36 `;`-separated clauses, 18 of them a disjointness cascade and one of them asserting the branch contains a nested parallel, all downstream of a single illegal edge.

## Evidence

- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-dotnet.md` ledger **#3** — two `reject-*` stages invented whose entire body is `exit 1`, "purely to turn a `needs-changes` verdict into a branch failure the parallel's `failurePolicy: all_or_nothing` + `onFailure: implement` can route".
- Same file, ledger **#4** — `next: "@join"` added to both `reject-*` stages "even though that edge is unreachable by construction".
- Same file, ledger **#5** — `park-needs-human` split into two byte-identical clones; first attempt produced `state "park-needs-human" is unreachable from start "query-backlog"`.
- Same file, ledger **#2** — removing `needs-changes: implement` from both branch gates was tweak 2 of 12; the error was "a single unwrapped ~4500-char line with `;` separators and no dedup or root-cause ordering".
- Same file, **"DSL ceremony notes"** — the ceremony tax quantified (2 exit-1 stages = 22 lines, 2 duplicated park stages = 28 lines, 2 dead edges, 4 workspace declarations; ~55 of ~90 fan-out lines), and the three asks named: `@fail-branch`, shared/auto-cloned park stages, and per-branch verdict visibility at the join.
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/README.md` §"The big five systemic findings" item 5 — the same finding reproduced as a cross-flavor headline.
- `docs/audits/2026-08-08-gaggle-reliability/domains/audit-nomination-flows.md` line 93 — the runtime half is healthy in production (8-lens and 4-lens fan-outs launch concurrently, completeness records accurate, `continue_on_error` works). This is an authoring-surface problem, not a reliability defect.
- `docs/design/static-fan-out-fan-in.md` §5.2 — "a branch that wants to fail *softly* returns a failed status and lets the parallel's failure policy decide"; no mechanism is provided for producing that status from a verdict.
- `docs/design/static-fan-out-fan-in.md` §5.5 rule 1 — predicts the clone tax verbatim: "two branches whose gates both escalate cannot share one escalation state — each needs its own. The diagnostic must say so, since the natural authoring instinct is to share it."
- `internal/workflow/internal/model/machine.go:17-55` — the reserved-target vocabulary: `@abort`, `@escalate` (terminal, `IsReservedTarget`), `@join` (non-terminal branch target, `IsReservedBranchTarget`), plus `IsReservedAnyTarget`. There is no failed-branch target.
- `internal/workflow/v_next/parallel.go:504-527` — rule 9's writable-workspace rejection, and the enclosing per-state branch checks that produce the cascade.
- `internal/runner/parallel.go:12-14, 311-328` — `BranchCompletenessInput` and `completeness()`; each `journal.BranchOutcome` carries `Branch`, `Name`, `Status`, `Artifacts`. No verdict.
- `internal/runner/run.go:3089-3091` (deterministic join) and `:3604-3609` (agentic join) — where the completeness record is attached to the invocation.
- `internal/executor/shell.go:314` → `internal/executor/env.go:203-207` — `for k, v := range inputs { if s, ok := v.(string); ok { … } }`. A non-string input is dropped silently, so a deterministic join receives no completeness record. `internal/harness/prompt.go:49-54` JSON-encodes the same inputs for agentic stages, so the two stage kinds see different inputs from the same envelope.
- First-party reproduction (binary `93f4098e`, scratch instance, DSL 2.0): copying a shipped-shape review gate into each of two branches yields a single 3,372-character `INVALID` line with 36 `;`-separated clauses, including `parallel "dual-review" branch "correctness" contains parallel "dual-review"; nested parallels are not supported` and 18 `branch subgraphs must be disjoint` clauses.

## Proposed direction

Three additive constructs, landed on the next DSL major alongside the in-flight DSL-2.0 work. Nothing in the stable DSL changes; every existing workflow — including ones already carrying the exit-1 workaround — keeps compiling and running unchanged.

**1. `@fail-branch`: a reserved branch target that settles the branch as failed.**

Legal only on a state reachable from a branch start, exactly as `@join` is. It settles the branch with status `failed`, at which point the parallel's declared failure policy owns the outcome — the same path a stage that fails terminally already takes. It must be admitted through the reserved-*branch*-target predicate, never the reserved-*terminal* one: the terminal predicate is what makes reachability treat a target as a run exit and what makes dangling-reference checks skip it. A third reserved word that is neither terminal nor join-continuing requires the same call-site audit the join target already went through.

With it, "both reviewers must pass, either rejection repasses the implementer" is written as what it means:

```yaml
gates:
  - name: review-correctness
    branches:
      pass: "@join"
      needs-changes: "@fail-branch"
      fail: park-needs-human
      escalate: "@escalate"
parallels:
  - name: dual-review
    failurePolicy: all_or_nothing
    onFailure: implement
```

Zero-config behaviour: unchanged. A workflow that never writes `@fail-branch` behaves bit-for-bit as today.

**2. Compiler-cloned shared branch sinks.**

Relax the disjointness rule for a *branch sink*: a stage reachable only from branch starts, reaching only branch-terminal targets, with no edge back into a branch body. Such a stage may be named by more than one branch; the compiler instantiates one copy per naming branch and rewrites each branch's edges to its own instance. Executed states stay disjoint, so branch attribution — the invariant that makes per-branch journal ordering, completeness, and telemetry sound — is untouched. The sharing is authoring-time only.

Zero-config behaviour: no new field, no annotation. Authors write the thing they already tried to write; the compiler performs the duplication they are currently doing by hand. Configs that already carry hand-clones keep working and can be collapsed at leisure.

**3. Verdicts in the completeness record, and a transport that carries it.**

Extend each branch outcome with the verdict decisions recorded in that branch, keyed by gate name, in branch-declaration order — the missing half of "did I actually get what I asked for?" Then fix the projection so a join can read it: write the record to a run-scoped file and export its path as a single environment variable, rather than JSON in an env var, because the record grows with branch count and environment size is bounded. Independently, an input the runner sets and the stage provably cannot see must be an error, not silence — silent omission is what makes the current gap invisible.

Zero-config behaviour: workflows without a parallel are unaffected. The transport change does alter behaviour for any deterministic stage that today declares a non-scalar input and silently receives nothing; that surface should be enumerated before the change lands, since a stage may have been written around the omission.

Deliberately out of scope here: the diagnostic *presentation* for a bad parallel (one unwrapped line, no deduplication, no root-cause ordering, consequences reported per-branch and per-task). Fixing the cause removes most of that cascade, and the presentation fix is independently worth its own issue.

## Alternatives considered

- **Permit `needs-changes` to route out of the branch to the pre-parallel stage.** Two branches racing back into one implementation stage has no defined semantics, sibling cancellation at that moment is undefined, and the parallel already provides exactly one well-defined repass route through its failure policy.
- **Relax disjointness generally so branches may share arbitrary states.** Total branch attribution is what makes per-branch ordering, completeness, and telemetry sound. Cloning at compile time keeps the authoring win without touching the runtime invariant.
- **Add a `requireAllBranchesPass` or quorum field to the parallel.** A second synchronization primitive for something the failed-branch target plus the existing all-or-nothing policy already expresses; partial and quorum joins are an explicit non-goal of the shipped design.
- **Document the exit-1 pattern in a cookbook instead of changing the DSL.** The tax is per-branch and linear, and it hides intent. Publishing the workaround makes it the contract.
- **Let the join glob branch artifacts to learn verdicts.** Reintroduces the directory inference the completeness record exists to replace, and cannot see a branch that produced no artifact.
- **Expose the completeness record only to agentic joins (status quo) and require the join be agentic.** Forces an LLM invocation to make a decision that is a pure function of recorded statuses, and makes the deciding-join pattern cost a model call per run.

## Duplicate search

**Date:** 2026-08-08. **Repo:** Agent-Clubhouse/Goobers, read-only.

**Method.** `gh search issues` (open and closed) with: `fail-branch`, `fail-branch parallel`, `@fail-branch`, `join reserved target`, `parallel branch`, `parallel branch verdict`, `gate verdict branch`, `branch verdict join`, `park stage shared branch`, `fan-out`, `fan-in`, `fan-out fan-in`, `parallels`, `branch subgraph disjoint`, `branchCompleteness`, `maxConcurrentBranches`. The search API rate-limited partway, so the sweep was completed against a full inventory — `gh issue list --state all --limit 3000` returning all 1,501 issues (#1–#2705) — grepped locally for `parallel|fan-?out|fan-?in|@join|branches|verdict|needs-changes|repass|park|boilerplate|duplicat|ceremony`.

**Nearest existing issues.**

- **#1558** `[EPIC] Static fan-out/fan-in` (open) — the parent. Its slice table (FO-1…FO-10) is entirely closed; the epic scope is delivering the construct and its conformance surface, not the ergonomics of authoring gates inside it. No slice mentions verdict routing, shared branch stages, or verdict visibility. **Delta: this proposal is the authoring surface the epic deliberately did not scope.**
- **#1560** `FO-2: DSL surface … parallels schema, @join, and compile-time validation` (closed) — shipped the ten compile rules that create the constraint, including the disjointness rule that forces per-branch clones. It is the origin of the problem, not a fix for it. **Delta: no failed-branch target was proposed or considered.**
- **#1565** `FO-7: fan-in data flow — branch-qualified inputsFrom and artifact union` (closed) — shipped the data path into the join and the completeness record's current shape (status + artifact counts). **Delta: verdict content and the deterministic-stage transport gap are both outside it.**
- **#1566** `FO-8: conformance corpus and feature GA`, **#1563** `FO-5: execute parallel branches`, **#1694** concurrency dispatch, **#1699** branch telemetry (all closed) — runtime and conformance, not authoring.
- **#1567** `FO-9: portal rendering of parallel states and branch lanes` and **#2404** `Portal graph view: render Parallel join as a convergence point` (both closed) — rendering, not semantics.
- **#1310** `DSL design: explicit parallel failure policy + bounded parallel/for_each` (open) — its remaining half is `for_each` (data-driven width). **Delta: orthogonal; width, not branch-internal routing.**
- **#155** `Tier-3 DSL extensions: parallel branches + child workflows` (open) — its parallel half is answered by the shipped design; its remaining half is child workflows. **Delta: a child workflow would be a different containment model, not a branch-failure target.**
- **#2382** `quality-sprint's triage fan-in stage silently receives empty inputsFrom.stageQualified` (closed) and **#2414** / **#2406** (closed, artifact hand-off wiring) — fan-in *artifact* plumbing bugs, already fixed. **Delta: none of them touches the completeness record or the string-only input projection.**
- **#1849** `Deterministic guard that can override (tighten) an agentic verdict` (open) — verdict authority on the root path, not branch-scoped routing. **Delta: complementary; neither subsumes the other.**

Nothing open or closed proposes a failed-branch reserved target, shared/compiler-cloned branch stages, or verdict-carrying completeness. **The proposal is uncovered in full.**

## Size and risk

**Overall M–L; file as three issues, landed in this order.**

**(1) `@fail-branch` — S/M.** Touches the reserved-target vocabulary, the next interpreter's branch-reachability and branch-exit rules, the workflow JSON Schema's gate-branch target constraints, the feature registry, and the generated feature matrix. *Blast radius:* the reserved-target predicates are shared with dangling-reference checking and `canExit`, so every call site must be re-audited to decide which predicate it wants — the same audit the join target required, with the same failure mode if missed (a target silently accepted outside a branch). *Guard:* a conformance fixture per failure policy asserting the routing decision and whether the join ran, plus a fixture asserting the stable DSL still rejects the token.

**(2) Compiler-cloned branch sinks — M.** Changes executed stage identity: a shared sink becomes one instance per branch. *Blast radius:* the journal, the read model, the portal graph, and per-stage telemetry all key on stage name, so the branch-qualified instance name is a user-visible contract decision that must be made explicitly rather than fall out of the implementation. *Guard:* a fixture asserting per-branch attribution stays total, and a decision recorded on whether the clone name appears in run reads.

**(3) Verdicts in completeness + input transport — M.** The record extension is additive. The transport fix is not parallel-specific: today *any* non-string input is silently dropped for deterministic stages, so the change alters behaviour for every deterministic stage declaring a non-scalar input. *Migration:* enumerate that surface first; if it is non-empty, land the file-based transport additively and keep the existing string projection, rather than changing what existing stages receive. This third slice is separately fileable if the transport surface turns out to be wide.

**Migration overall:** none required. All three are additive on the next DSL major; existing configs, including hand-written exit-1 and cloned-park workarounds, continue to compile and run. Collapsing a workaround is opt-in and reviewable as a shrinking diff.
