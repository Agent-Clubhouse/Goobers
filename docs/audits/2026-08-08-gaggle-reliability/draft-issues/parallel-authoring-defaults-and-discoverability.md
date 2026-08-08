# The parallel construct is unsupported end-to-end at author time: the stable DSL silently drops `parallels:`, `workflow show` omits it, and its concurrency default contradicts its name

Suggested labels: `area:contracts`, `area:cli`, `type:bug`, `type:feature`, `goobers:needs-human`

## Problem

Fan-out is the hardest construct in the DSL to author and the one with the least authoring support. Four defects compound.

**1. A `parallels:` block pinned to the stable DSL version is silently discarded, and the resulting errors name neither the construct nor the version.** The stable interpreter builds its state machine with no parallels at all, so the declared parallel is not a state and the author gets dangling-reference errors instead:

```
INVALID workflow: compile workflow "fanout-review": task "implement" next state "dual-review" is not defined;
task "review-a" next state "@join" is not defined; task "review-b" next state "@join" is not defined
```

Nothing says `parallels` requires a higher `dslVersion`, and nothing points at the one-step migrator. The sibling version-gated field on the same interpreter does exactly the right thing — `task "x": run.script is not supported in DSL 1.4; use run.command or DSL 2.0` — so the correct behaviour already exists next door. Worse, a code comment at the drop site asserts the block "is rejected as an unknown field by this version's schema"; the schema is shared across versions and contains `parallels`, so the assertion is false and the block is simply dropped.

**2. `goobers workflow show` — the one command for eyeballing a workflow — omits the parallel entirely.** The default text view prints an edge into a state it never lists, shows `@join` as a literal dead end, and reports nothing about branches, the join, the failure policy, the failure route, the branch timeout, or the concurrency bound. `--dot` prints all of it. Same workflow, two projections that disagree:

```
=== TEXT ===                          === DOT ===
implement (kind: agentic) -> dual-review   "implement" -> "dual-review";
review-a (kind: agentic) -> @join          "review-a" -> "collate";
review-b (kind: agentic) -> @join          "review-b" -> "collate";
collate (kind: deterministic) -> <complete>  "dual-review" -> "review-a";
                                             "dual-review" -> "review-b";
                                             "dual-review" -> "implement";
```

The text view is also not version-aware: it renders the raw spec rather than the compiled machine, so it prints a confident DAG at exit 0 for the very workflow `goobers validate` rejects at exit 1 — including `@join` targets that do not exist at the pinned version.

**3. The concurrency default contradicts the construct's name, and the diagnostic that enforces the branch workspace rule states a reason that is false at the default.** `maxConcurrentBranches` unset means 1: a construct called `parallel` runs sequentially, and nothing at author time says so — not the text projection, not validate, not the scaffold. Separately, a writable repo workspace is illegal inside *any* branch regardless of the concurrency setting, so the built-in default workspace must be overridden on every branch stage — but the error explains itself with a concurrency rationale that does not apply when branches are sequential:

```
parallel "dual-review" branch "correctness": task "review-a" resolves to a writable repo workspace;
branch stages must use scratch or repo-readonly (concurrent repo-backed branches collide on the run branch)
```

The key is also asymmetric: `workspace:` is a task-level key for agentic stages and `run.workspace:` for deterministic ones.

**4. There is no user-facing example or guide.** `parallels:` appears in exactly two files in the tree: an implementer-facing design doc citing Go file paths, and one reference workflow whose branches contain no gates — so nothing demonstrates a review fan-out. The binary's embedded example set is four entries, all linear. A user who does not think to grep a design directory will not discover that branches exist; one who does is reading an implementation spec to learn a language feature.

## Evidence

- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-dotnet.md` ledger **#1** — `dslVersion` changed from 1.4 to 2.0 so `spec.parallels` would be accepted; `goobers schema`, `goobers schema workflow`, `goobers init`, and every shipped gaggle report 1.4, and nothing in the quickstart, the onboarding guide, the config-examples README, or `--help` mentions that a higher version exists.
- Same file, ledger **#12** — `goobers workflow show` text projection "lists every task and gate but omits `dual-review` entirely… `--dot` does emit the fan-out edges, so the two projections disagree."
- Same file, ledger **#6** — the default workspace is illegal inside a branch; the diagnostic repeated once per task per branch (16 clauses on the first run); the `workspace:` vs `run.workspace:` asymmetry.
- Same file, **"Docs notes"** — `spec.parallels` "is not documented for users anywhere"; the three places it appears; "no guide page, no `goobers examples` entry (the 4 embedded examples are all linear)"; the DSL-version discoverability compounding.
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/README.md` §"The big five systemic findings" item 5 and §"Cross-cutting smaller findings".
- `docs/design/static-fan-out-fan-in.md` §5.1 — "it is a state like any other: it is named, it is a valid `next`/`branches` target, and it appears in the graph projection." The text projection does not honour this.
- `docs/design/static-fan-out-fan-in.md` §5.4 and §14 OQ-2 — the sequential ceiling formula, and the recorded recommendation to default `maxConcurrentBranches` to 1.
- `cmd/goobers/workflow.go:100-114` — `printWorkflowDAG` iterates `Spec.Tasks` and `Spec.Gates` only; `Spec.Parallels` is never read, and the function receives the raw `apiv1.Workflow`, never a compiled machine. `:78-90` — the `--dot` path compiles first and renders `machine.Graph()`.
- `internal/workflow/v_current/compile.go:242-244` — the stable interpreter passes `nil` parallels into the machine, with the comment asserting schema rejection. `api/schemas/workflow.schema.json:83` defines `parallels` with no version condition, so nothing rejects it.
- `internal/workflow/v_current/compile.go:218-228` — `runScriptProblems`, the version-gated-field diagnostic that gets it right, for contrast.
- `internal/workflow/v_next/features.go:380, 514, 749` — `featureWorkflowParallels` exists only in the next interpreter's registry; the stable one has no counterpart to report against.
- `api/v1alpha1/workflow_types.go:656-663` — `MaxConcurrentBranches`, "Unset means 1". `internal/runner/run.go:1100, 1227` — the runtime branch on `> 1`.
- `internal/workflow/v_next/parallel.go:504-527` — rule 9, enforced unconditionally on the effective workspace, with the concurrency-worded rationale.
- First-party reproduction (binary `93f4098e`, scratch instance from `goobers init`): the same two-branch workflow validates clean at 2.0 and produces the three dangling-reference errors at 1.4; `goobers workflow show` renders it at exit 0 while `goobers validate` exits 1; removing one branch stage's `workspace: repo-readonly` with `maxConcurrentBranches` unset still triggers rule 9; `goobers examples list` returns `backlog-assignment`, `backlog-curation`, `implementation`, `work-nomination`; `goobers schema workflow` reports `"dslVersion":"1.4"` and the field's own description carries `examples: ["1.4"]` with no mention of a higher version.

## Proposed direction

Scoped against the in-flight DSL-2.0 epic, which already owns migrating the shipped references and examples to 2.0, deprecating the stable version, and deciding what an absent `dslVersion` means. Once those land, the "everything says 1.4" half of the discoverability problem largely evaporates. **Four changes remain, none of them covered there.**

**1. A version-gated field must diagnose itself, and must never be dropped.** Generalize the existing `run.script` rule into one contract: any spec field a pinned interpreter does not implement is reported as `<field> requires dslVersion <v>; migrate with 'goobers fix --to <v>'`, emitted before structural checks so it is not buried under the dangling references it causes. Concretely, the stable interpreter must reject a non-empty `parallels:` rather than silently building a machine without it, and the false comment at the drop site goes away with the behaviour it describes. Silently discarding a declared section of the state machine is the worst available failure mode and is what turns a one-line fix into a hunt.

*Zero-config:* purely a diagnostic and a fail-closed correction; no valid workflow changes meaning. It also survives the deprecation work — a workflow still pinned to the older version keeps getting this message until the version is removed.

**2. `goobers workflow show` renders one projection.** Delete the raw-spec text renderer and render the text view from the same compiled graph `--dot` already consumes. Text then lists the parallel as a stage, names its branches, resolves `@join` to the join stage, and prints the failure policy, failure route, branch timeout, and effective concurrency. Rendering from the compiled machine also makes `show` version-aware, so it can no longer print a confident DAG for a workflow that cannot compile.

*Zero-config:* the default output stays text and a linear workflow renders the same stage list it does today. Existing golden output changes only where the workflow contains a parallel — plus the resolved `@join` target, which is a correctness fix.

**3. Keep the concurrency default at 1, and make it legible rather than surprising.** Sequential-by-default is the right call: it keeps journals deterministic, keeps the ceiling a simple multiple, and makes opting into concurrency an explicit decision about agent cost and provider call rate. What is missing is the signal. Three small pieces: `workflow show` prints the effective `maxConcurrentBranches` and the resulting worst-case ceiling; `validate` emits one informational line per parallel naming that ceiling; and the branch-workspace diagnostic states the actual rule — a writable repo workspace is never legal inside a branch, because branch worktrees would share the run branch — instead of a concurrency rationale that does not apply at the default that ships.

*Zero-config:* behaviour unchanged; this is output only, plus one reworded message.

**4. Ship the fan-out review pattern as a runnable example and one guide page.** Add a review fan-out to the binary's embedded example set — the only shipped parallel today has no gates in its branches, so nothing demonstrates the construct anyone actually reaches for — and one user-facing authoring page that teaches the branch vocabulary, the branch-terminal rule, the failure-policy table, and the workspace constraint. The design document should stop being the only place a user can learn the language.

*Zero-config:* additive. The example is discoverable from `goobers examples list` with no repo checkout, which is the surface the cold-start evidence identifies as the single biggest success factor.

## Alternatives considered

- **Default `maxConcurrentBranches` to the branch count.** Silently multiplies harness invocations and provider call rate on a construct whose first use is agentic fan-out, and turns the run's worst-case duration into a function of a value the author never wrote. Rejected in favour of making the default visible.
- **Fix the text projection by special-casing parallels in the existing renderer.** Leaves two traversal models that will diverge again on the next state kind, and keeps `show` rendering uncompiled specs. Rendering from the compiled graph is the smaller long-term surface.
- **Document the version requirement in the guides and leave the diagnostic alone.** The failure is silent discard, not a documentation gap; prose cannot reach an author who is staring at "next state is not defined".
- **Reject `parallels:` at the JSON-Schema layer with a version conditional.** The schema is shared across interpreters and version-conditional schema branches are how the four-place field-addition tax gets worse. The interpreter already owns version gating; keep it there.
- **Wait for the older DSL version to be removed, which makes the mis-diagnosis moot.** Removal is at least two decisions away and the mis-diagnosis is live now for every author who copies a 1.4 example and adds a branch.

## Duplicate search

**Date:** 2026-08-08. **Repo:** Agent-Clubhouse/Goobers, read-only.

**Method.** `gh search issues` (open and closed) with: `maxConcurrentBranches`, `maxConcurrentBranches default`, `parallel default sequential`, `workflow show`, `workflow show parallel`, `dslVersion`, `dslVersion discoverability`, `dslVersion 2.0 discoverable`, `DSL 2.0`, `DSL 2.0 epic`, `parallels`, `parallels documentation guide`, `progressive disclosure DSL`, `feature matrix`, `examples parallel`, `graph projection parallel`, `text projection`. Search API rate-limited partway; the sweep was completed against a full inventory — `gh issue list --state all --limit 3000`, all 1,501 issues (#1–#2705) — grepped locally for `parallel|fan-?out|@join|dslVersion|DSL 1\.4|DSL 2\.0|feature.matrix|workflow show|projection|DAG|graph|examples|cookbook|authoring|guide|quickstart`.

**Nearest existing issues.**

- **#2695** `[EPIC] Move the DSL to 2.0 and deprecate 1.4` (open, filed today) with children **#2696** (pin-only `fix` fast path), **#2697** (DVL020 provenance leak), **#2698** (migrate reference-workflows and config-examples to 2.0), **#2699** (what an absent `dslVersion` means), **#2700** (flip 1.4 to deprecated and start warning). **This shrinks the draft.** The "every shipped example says 1.4" half of the discoverability finding is squarely inside #2698/#2700 and should not be re-filed. **Delta retained:** the stable interpreter *silently dropping* a `parallels:` block and mis-diagnosing it as dangling references is a correctness bug that survives the whole migration — it persists for any workflow still pinned to the old version until that version is removed, and #2700's scope is the support matrix and the docs that name 1.4, not interpreter diagnostics.
- **#1560** `FO-2: DSL surface … parallels schema, @join, and compile-time validation` (closed) — introduced the version gate and the compile rules. **Delta:** shipped the gate; did not give the older interpreter a diagnostic for it.
- **#334** `goobers workflow show <name> text DAG` (closed) and **#398** `--dot` (closed) — the two projections, added separately. **#442** `API-2: canonical workflow graph projection integrating DOT output` (closed) — created the shared graph projection and explicitly held "default text output is unchanged" as an acceptance criterion, which is precisely how the text path was left on the raw spec. **Delta:** this proposal is the follow-through #442 deliberately deferred; no open issue tracks it.
- **#1567** `FO-9: portal rendering of parallel states and branch lanes` (closed), **#2404** `Portal graph view: render Parallel join as a convergence point` (closed), **#2693** `Portal workflow graph: gates and deterministic-vs-agentic stages need stronger visual differentiation` (open) — all portal rendering. **Delta:** none touches the CLI text projection.
- **#1694** `Enable maxConcurrentBranches > 1 for repo-readonly parallel branches` (closed) — wired concurrent dispatch. **Delta:** it made the knob work; it did not make the default legible, and it did not touch the workspace diagnostic's wording.
- **#2021** `[TRACKING] Re-true hand-written narrative documentation` (open) with **#2211** `docs: reconcile static fan-out/fan-in design status with the GA implementation` (closed) — #2211 updated the design doc's *status* to GA. **Delta:** no child covers a user-facing authoring page for the construct, and none adds an example.
- **#1541** `goobers examples: serve canonical workflow YAML + usage notes from the binary` (closed) and **#2414** `Adopt goobers-io auto-wiring in shipped example workflows` (closed) — built the example surface and fixed the one shipped fan-out's artifact wiring. **Delta:** neither adds a fan-out entry to the example set; the set is still four linear workflows.
- **#2025** `validate: schema errors lack line numbers and did-you-mean suggestions` (closed) — adjacent diagnostic-quality work at the schema layer. **Delta:** the parallels case never reaches schema validation; it is dropped by the interpreter.
- **#435** `[EPIC] Onboarding & Authoring` (open), **#2633/#2638/#2640** (open quickstart gaps) — first-run onboarding, none of which reaches the parallel construct.

Nothing open or closed covers the silent drop, the text-projection omission, the concurrency-default signal, or a fan-out example/guide. **The draft is narrowed to those four; the version-pin migration half is dropped as covered.**

## Size and risk

**Overall M; file as two issues — a defect pair and an enablement pair.**

**Defect pair (file first).**
- *Version-gated-field diagnostic + stop dropping `parallels:` — S.* One check in the stable interpreter plus its ordering relative to structural checks. *Blast radius:* small and fail-closed, but any workflow that currently pins the old version while carrying a `parallels:` block flips from a confusing error to a clear one — behaviour that was already broken, now correctly reported. *Guard:* a compile test per interpreter asserting the message names the field and the required version.
- *`workflow show` renders from the compiled graph — S/M.* Removes one of two traversal models. *Blast radius:* CLI golden output, the man page, and `docs/cli/README.md`; text output changes for parallel-containing workflows and for the resolved `@join` target. `show` gains a compile step, so it now fails where it previously printed — intended, and worth calling out in the change description because it is a user-visible exit-code change for invalid configs.

**Enablement pair.**
- *Concurrency and workspace legibility — S.* Output plus one reworded diagnostic; no semantics. *Blast radius:* golden test updates only.
- *Fan-out example + authoring guide — M, mostly writing.* *Blast radius:* the embedded example set is served from the binary, so an added example ships with the release and must validate clean in CI like the others; the guide must be pinned to whichever DSL version the migration work settles on, so sequence it after that decision rather than racing it.

**Migration:** none. No config edits are required by any of the four; all valid workflows keep their meaning.
