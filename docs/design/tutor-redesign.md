# Design: Tutor v2 — version-aware, workflow-scoped self-improvement

> Status: **Draft for review — not implemented** · Area prefix: `TUT` · Milestone: _proposed_ **Tutor v2**
> Supersedes the config-only framing of the shipped tutor (`selfhost/gaggles/goobers/workflows/tutor.yaml`).
> Related issues: #36 (tutor epic), #102 (cross-run detection queries), #104 (config-only write-boundary), #507 (who owns test-suite quality), #150 (`Goober.spec.model`), #417 (first-class agent signal), #776 (usage in envelopes/spans), #769 (journal/telemetry schema migration).
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)

## 1. Why this exists

The **tutor** is Goobers' offline self-improvement workflow: between live tasks it mines the run
journal + telemetry rollup and opens PRs that make the *next* runs better. It already ships and works
on a narrow slice. The PO asked for two changes that, together, amount to a redesign:

1. **Grounded self-improvement, not just config tuning.** The tutor should be able to improve the
   **workflow itself** — add/adjust stages, gates, **skills**, and **tests** — not only edit a goober's
   persona prompt.
2. **Version-aware diagnosis.** The tutor must know *which version of which thing* produced an outcome,
   so it does not conflate behaviourally-different versions when it compares "before vs after."

Both are backed by findings from an audit of our own code + run history (§2, §6) and by external prior
art on offline agent self-improvement (§3). This document proposes the target shape, the governance it
requires, and a staged backlog.

### The two load-bearing findings

- **The conflation trap (provenance gap).** Every run records a stable id, an author-declared
  `WorkflowVersion`, and a tamper-evident `WorkflowDigest = sha256(compiled definition)`, echoed on every
  span and stored in the rollup (`internal/journal/identity.go`, `internal/telemetry/rollup/schema.go`).
  The rollup even ships purpose-built version-aware analysis (`internal/telemetry/rollup/efficacy.go`:
  `DigestHistory`, `AssessEfficacy`). **But `WorkflowDigest` covers only workflow *structure*.** It
  references goobers **by name**; `GooberSpec.Instructions` is a **file path, not content**; the digest
  **excludes goober specs entirely**; the **model id** survives only inside transcript blobs (not indexed);
  the **harness/CLI version** is checked at preflight and then discarded. So a change to a goober's persona
  prompt or model produces **no new digest**, and `AssessEfficacy`'s before/after comparison **silently
  mixes runs whose agent behaviour changed**. That is exactly the version-conflation the PO flagged.

- **The reachability gap (action-space).** The tutor's *stated* ambition (its `analyst`/`config-author`
  instructions, TUT-011) is broad, but the *enforced* boundary is a single directory: `confineToConfigRoot`
  with `configRoot: selfhost` (`cmd/goobers/openpr.go` → `cmd/goobers/configboundary.go`), which aborts
  **fail-closed** before opening a PR if any changed path is outside `selfhost/`. Against our real learnings
  (§6) that boundary reaches only **~¼–⅓** of what we actually learn — and even some of that only by
  prompt-patching around a root cause that lives in code.

## 2. What the tutor is today (precise)

**Mineable inputs.** `goobers telemetry-query` drives a `gather-signals` stage; the rollup detector
(`internal/telemetry/rollup/findings.go`) emits exactly **six finding kinds** in three families:
`stage-failure-rate`, `error-signature` (failure patterns); `gate-never-fails`, `gate-repass-churn`
(gate noise); `workflow-untriggered`, `stage-unreached` (coverage gaps). _Surface gap:_ the `--aggregate`
flag only names the first three families, so the two coverage-gap kinds are second-class inputs today.

**Enforced action space (corrected).** The tutor is **not** "instructions + skills only." Its real,
enforced write scope is *anything expressible as a file under `selfhost/`*: workflow YAML (stages, gates,
`next:`/branch routing, capability lists, readiness caps), goober `instructions.md`, goober `goober.yaml`
(skills-list; `model` once #150 lands), and deterministic shell "test stages." What it **structurally
cannot touch**: Go tests (`cmd/`, `internal/`), production code, `.github/` CI, `api/schemas/`, CRD
manifests, and **skill *body* content** (the only skill tree is repo-root `./skills/`, outside `selfhost/`
— so it can edit a goober's skills-*list* but cannot author a *new skill's* content).

**The self-defeat.** Even the tutor's *in-reach* YAML topology edits cannot land cleanly: editing
`merge-review.yaml` turns the structural golden `internal/workflow/merge_review_test.go` red, and the tutor
cannot touch that test to regenerate it. A topology-writing tutor that cannot regenerate goldens ships
CI-red by construction.

## 3. Prior art (what the field already knows)

The pattern the PO is describing — **an offline pass that mines a system's own execution traces and
proposes bounded changes to its configuration, without weight updates** — is well-established. Key anchors,
with the peer-reviewed ones first:

- **ExpeL** (arXiv:2308.10144, AAAI 2024) — the closest *mechanistic* analogue: a train/inference split
  where an agent gathers experiences across tasks and extracts natural-language insights by contrasting
  **successful vs failed** trajectories (not raw traces), maintained by add/edit/importance-voting, with no
  parametric updates. Directly validates "mine success *and* failure, distil, apply."
- **Reflexion** (arXiv:2303.11366) — verbal self-reflection into an episodic buffer improves later trials
  without weight updates. _Caveat:_ same-task multi-trial, so it is a structural analogy for retrospection,
  **not** evidence that cross-version segmentation is solved.
- **Voyager** (arXiv:2305.16291) — a compounding, executable **skill library** improved by an iterative
  loop. Precedent for a persistent reusable-skill store (relevant to letting the tutor author skills).
- **Generative Agents** (arXiv:2304.03442) — memory-stream + periodic reflection consolidation. _Caveat:_
  within-a-single-run cross-day continuity, not version-segmented cross-run learning.
- **CER** (arXiv:2506.06698, ACL 2025) — training-free in-context self-improvement from a synthesised
  trajectory buffer. _Caveat:_ injects into the context window rather than emitting PRs.
- **Self-Harness** (arXiv:2606.09498, 2026 preprint) — the closest analogue to *our specific goal*: a
  three-stage offline loop (weakness-mining → harness-edit proposal → proposal validation) that **clusters
  failed traces into model-specific failure patterns**, proposes **bounded edits to the harness/config/prompt
  surface (not weights)**, keeps an **auditable harness lineage**, and **accepts an edit only if it improves a
  held-in/held-out split without degrading the other**, with stochastic evals repeated. This is close enough
  to a template that §4.4 adopts it.
- **Auto-Dreamer** (arXiv:2605.20616, 2026 preprint) — frames offline "dreaming" as a learned consolidator
  (complementary-learning-systems / sleep theory) and makes **provenance a first-class structural
  requirement**: every learned entry stores a provenance pointer to its source trajectory that the offline
  consolidator dereferences, under a read-only / region-rewrite write boundary. Strong external validation of
  §4.1. (The "dreaming" term also appears in 2026 industry coverage of Anthropic's Managed-Agents work; that
  coverage is secondary/blog-grade, so we borrow the *term* but anchor the *mechanism* to Auto-Dreamer.)
- **Provenance:** W3C-PROV workflow-provenance work (arXiv:2509.13978) formalises **"by whom" (agent/model)
  and "how" (method)** as first-class provenance dimensions — precisely the axes our digest omits.
- **Why segmentation is mandatory (quantitative):** freshness-aware experience-replay theory
  (arXiv:2604.16918) shows a sample's usefulness **decays with policy/version divergence** (effective sample
  size `ESS ≤ n·exp(−D_KL)`). _Caveat:_ a loose bound (tight form uses Rényi-2), so cite as directional
  motivation, not an exact discount. It is the theoretical backbone for "do not pool runs across versions."
- **Guardrails:** "Safety in Self-Evolving LLM Agent Systems" (arXiv:2606.23075) maps a Propose/Evaluate/Commit
  attack surface and finds most cells under-defended; and LLM-as-judge self-scoring shows **measurable
  self-preference even double-blind** — so a tutor must **never score its own before/after efficacy unaided**
  (multiple judges + human oversight).

**Net:** the design below is squarely on the established path. Our novel obligations are (a) closing the
provenance gap that our digest leaves open, and (b) the governance that an *expanded* write surface demands.

## 4. The redesign

### 4.1 Provenance foundation (prerequisite tier — must land first)

Define the **effective-version key** of a run as the content hash of *everything that determines behaviour*,
not just workflow structure:

```
EffectiveVersion = H( WorkflowDigest
                     ⊕ GooberDigest         # sha256 over each participating goober's RESOLVED spec:
                     #                         loaded instructions *content* (not path), skills set,
                     #                         model id, harness id + options
                     ⊕ ModelId              # per agentic stage
                     ⊕ HarnessVersion )     # the preflight CLI/model version we currently discard
```

Concretely:

- **P1 — GooberDigest.** Fold the *resolved* `GooberSpec` (instructions **content**, skills, model, harness,
  options) into the run's identity — either extend `computeDigest` (`internal/workflow/compile.go`) to a full
  "effective definition" hash, or add a separate `GooberDigest` to `RunIdentity` + a rollup column. Today the
  digest sees only `{Name, Version, Spec}` and the Spec names goobers.
- **P2 — Agent-version provenance.** Add `AttrModel` + `AttrHarnessVersion` to the span registry
  (`internal/telemetry/attributes.go`), populate from the adapter model and the **preflight version that is
  currently thrown away** (`internal/harness/copilot.go`), and index them in the rollup so they are queryable
  rather than buried in transcript blobs.
- **P3 — Version-segmented efficacy.** Expose the existing `AssessEfficacy`/`DigestHistory` **keyed on
  `EffectiveVersion`** through the `telemetry-query` connector, so a tutor stage consumes version-segmented
  stats directly. Define **cohort rules**: same-`EffectiveVersion` runs pool; a change on *any* axis starts a
  new cohort; partial-overlap cohorts (same prompt, new model) are compared *across* the boundary or excluded,
  **never pooled** (§3, freshness-aware PER).
- **P4 — (lower) journal-envelope migration.** The rollup DB is already versioned/migrated; the journal
  envelope is schema-stamped but not migration-versioned. Track under #769; not on the critical path (the
  rollup is rebuildable from journals).

Without this tier the expanded tutor "improves" against a signal that silently mixes versions — so **P1–P3
gate everything else**.

### 4.2 Action space (expanded, per-target sub-boundaries)

Replace the single `selfhost/` allow-root with **per-action-class allow-roots, each fail-closed** like today's
`configboundary.Confine`:

| Action class | Allowed root | Notes |
|---|---|---|
| Persona / prompt | goober `instructions.md` under `selfhost/` | today's home turf; keep light-touch |
| Workflow topology | `selfhost/**/workflows/*.yaml` | already in-reach; **must** co-regenerate goldens (§4.5) |
| Gate calibration | gate `check`/threshold/`next` in workflow YAML | subject to the metric-gaming guard (§5) |
| **Skill body** | the skills root (`./skills/`, see open decision D3) | new — lets the tutor *author* skills, not just list them |
| **Tests** | a whitelisted `*_test.go` set for the package a finding names | new — the single biggest unlock; **fail-first** required (§5) |
| Production `.go`, CI, `api/schemas`, CRD | **none** | out of scope for the tutor; stays human/other-workflow |

A test-writing action is confined to *test files*; it must not be able to also edit production `.go`. This is
the disciplined version of "the tutor can touch the workflow, skills, and tests" — bounded per target, not a
blanket root.

### 4.3 Topology — hybrid two-tier

Our learnings split cleanly (§6, part D):

- **Per-workflow, cheap, frequent, low-blast:** persona gaps, gate calibration, stage wiring — these need
  *local* context (one workflow's own runs and reviewer verdicts).
- **Cross-cutting, expensive, rare, high-blast:** flaky shared tests, missing regression tests, telemetry
  integrity, harness/preflight, flow ceilings, shared-evaluator (`internal/gate.Evaluate` touches **every**
  agentic gate), shared `api/schemas` contracts.

⇒ **A two-tier hybrid:**

1. a **per-workflow tutor** confined to that workflow's own config subtree (persona / gate / wiring), and
2. a single **global platform tutor** that owns cross-cutting learnings (tests, shared stages, skills,
   schema) — and is precisely the tier that crosses `selfhost/` and therefore carries the strongest
   governance (§5).

A single global-only tutor drowns per-workflow nuance in cross-run aggregates; per-workflow-only tutors are
each blind to the shared-code/test/flaky class that is the *majority* of hard learnings. (This is a
recommendation — see open decision D1.)

### 4.4 Learning method (adopting the Self-Harness loop)

For each tutor tier, one offline pass:

1. **Mine + cluster.** Pull version-segmented findings (§4.1) and **cluster failures into recurring patterns**
   within a cohort (ExpeL/Self-Harness), not isolated incidents. Contrast successful vs failed cohorts.
2. **Propose a bounded edit** to exactly one action-class target (§4.2), with a stated hypothesis and the
   provenance links (run-ids + journal pointers) it is grounded in.
3. **Validate before proposing** — held-in/held-out: the change must improve one split without degrading the
   other, evals repeated to defeat single-lucky-run promotion (Self-Harness).
4. **Human review gate**, differentiated by target (§5).
5. **Verify live** — after merge, a mandatory holdout re-run on *post-change* telemetry confirms the predicted
   improvement materialised (closes the propose→validate→verify loop; open decision D4).

### 4.5 Fixing the golden self-defeat

A workflow-structure or shared-stage edit **must** run the dual-assert (`internal/workflow` + `cmd/goobers`,
across `selfhost` **and** the `config-examples/acme-web` copy) and **regenerate the affected structural
goldens/manifests** as part of the same PR, or it ships CI-red (see memory: shipped-workflow-yaml-dual-assert,
new-subcommand-needs-manpage-docsgen, crd-manifests-drifted-not-gated). Golden regeneration is itself a bounded
crossing into `internal/` test artifacts and must be a scoped, audited capability — not a blanket grant.

## 5. Governance & guardrails

Expanding beyond prose edits introduces failure modes the single path-check does not cover. These are hard
requirements, each mapped to prior art (§3):

1. **Fail-first tests.** Every tutor-authored test must be demonstrated **red against the pre-fix tree**
   (reproduces the regression) before green against the fix. A vacuously-passing test that "closes" a finding
   games the loop. (Analog of #909's "validate every config.")
2. **Metric-gaming guard (sharpest new risk).** The tutor mines `gate-never-fails` / `gate-repass-churn`. If it
   can also *edit gates*, the cheapest way to improve those metrics is to loosen/delete the noisy gate —
   improving its own score while stripping real coverage. A "noisy" gate can be *miscalibrated, not useless*
   (see shared-gate-evaluator-blast-radius / #415). **Rule:** the tutor may never remove/loosen the very gate
   whose noise flagged the finding without independent proof the gate is dead; gate-removal PRs get stricter
   review than gate-tuning ones. (Self-evolving-safety attack surface, arXiv:2606.23075.)
3. **Never self-judge efficacy unaided.** Before/after scoring uses the deterministic version-segmented stats
   (§4.1) plus, where a judgment is needed, **multiple judges + human oversight** — never the tutor grading
   its own change (measurable self-preference bias, §3).
4. **Blast-radius checks mandatory.** Workflow-structure / shared-stage changes run the dual-assert and the
   `grep -rl "evaluator: agentic"` audit before landing (§4.5).
5. **Drift/golden sync is the tutor's job.** Any workflow-YAML edit updates the `acme-web` copy and regenerates
   affected goldens/manifests in the same PR (§4.5), or the PR is instantly CI-red.
6. **Differentiated human gate.** CODEOWNERS-differentiated review: prompt/instruction changes stay
   light-touch; `workflows/**` topology, gate removals, skill bodies, and any `*_test.go` addition require
   explicit human sign-off. Autonomy pressure (sustained-flow-creation-without-drainage) will push to auto-merge
   tutor PRs — **resist that specifically for structure/test/skill changes.**
7. **Provenance discipline scales up.** Tutor PRs already cite run-ids + journal pointers (TUT-007). Extend:
   test additions cite the specific failing run(s) *and* the fail-first reproduction; structure changes cite the
   aggregate that flagged them. **Meta-risk:** if telemetry integrity is blind (#710/#530), the tutor mines
   noise and "improves" against phantom signal — so **observability fixes are a prerequisite tier, not a peer**
   (§6 Class 7).

## 6. Empirical learning taxonomy (why the above)

From an audit of our operational-memory corpus + closed run-quality issues, our learnings fall into 8 recurring
classes. Write-target legend: `instruction` | `skill` | `test`(Go) | `wf-structure`(selfhost YAML) |
`code-fix`(out of scope) | `infra/policy`.

| # | Class | Example refs | Write-target(s) | Reachable today? |
|---|---|---|---|---|
| 1 | Flaky-test / non-determinism | #745, #827, #1128, wave-705 | code-fix + test + instruction/policy | ✗ (mostly) |
| 2 | Gate over/under-sensitivity | `gate-never-fails`/`repass-churn`, #608, #415 | wf-structure, instruction, sometimes code-fix | ◑ (YAML slice) |
| 3 | **Missing regression test** | #707 (only errcheck caught), #415 guard, #909 | **test (Go)** | ✗ — **the biggest unlock** |
| 4 | Workflow wiring / ordering bug | #929, #496, #1052, #912 | wf-structure (YAML) or code-fix (applyverdict/claim) | ◑ (YAML slice) |
| 5 | Goober instruction / contract gap | #297/#299/#301/#310, #314 | **instruction** | ✓ (home turf) |
| 6 | Capability / preflight gap | #735, #238, #284 | wf-structure (decls) + code-fix/infra | ◑ (decls only) |
| 7 | Provenance / observability gap | #710, #530, #230, #849 | code-fix (telemetry) | ✗ — **caps tutor quality** |
| 8 | Flow / throughput ceiling | openprcount, claim.go liveness | code-fix + wf-structure (caps) | ◑ (caps only) |

Cross-class modifiers: **drift/dual-copy** (selfhost + acme-web, goldens, manifests) and **platform
portability** — both almost always resolve to test/CI + code outside the config root.

**Reachability:** exactly **1 class fully reachable** (Class 5) and **~4 partially** (only their YAML slice);
by volume of concrete learnings, the current config-only tutor addresses ~¼–⅓. **Class 3 (missing tests)** is
the single largest thing the expansion unlocks; **Class 7 (observability)** feeds the tutor and so is a
prerequisite, not a peer.

**Topology signal:** Classes 2/4/5 cluster per-workflow (local context); Classes 1/3/6/7/8 + shared-evaluator +
shared-schema cut across everything (global). ⇒ the two-tier split of §4.3.

## 7. Open decisions (for review)

- **D1 — Topology.** Adopt the hybrid two-tier (per-workflow + one global platform tutor)? _Recommended: yes._
  Alternatives: single global; per-workflow only.
- **D2 — Write-authority ceiling.** Confirm the tutor may author **workflow YAML + skill bodies + fail-first
  Go tests**, but **not** production `.go`, CI, or `api/schemas`. _Recommended: yes_ (matches the PO's "skills,
  tests, etc." while keeping production code human/other-workflow).
- **D3 — Skill location.** Skills live at repo-root `./skills/`, outside any tutor root. To let the tutor author
  skills, either (a) add `./skills/` as a per-target allow-root, or (b) relocate the goobers gaggle's skills
  under `selfhost/`. _Recommended: (a)_ — smaller blast radius, no move.
- **D4 — Live-verification gate.** Require a post-merge holdout re-run confirming the predicted improvement
  before the finding is considered closed? _Recommended: yes_ for structure/test/skill changes; optional for
  persona tweaks.
- **D5 — Auto-merge stance.** Structure/test/skill tutor PRs are **never** auto-merged (§5.6); persona/gate-tune
  PRs may follow the normal review path. Confirm.

## 8. Proposed backlog (staged)

**Tier 0 — provenance foundation (gates everything; file now):**
`TUT-P1` GooberDigest · `TUT-P2` model+harness version provenance · `TUT-P3` version-segmented efficacy in
`telemetry-query` (+ cohort rules). (`TUT-P4` journal migration tracked under #769.)

**Tier 1 — action-space + safety (after D1–D5):**
`TUT-A1` per-target sub-boundaries (replace single `configRoot`) · `TUT-A2` golden/dual-assert co-regeneration
capability · `TUT-A3` fail-first test-authorship contract · `TUT-A4` metric-gaming guard · `TUT-A5` two-tier
topology (per-workflow + global) · `TUT-A6` skill-body authoring root · `TUT-A7` differentiated CODEOWNERS
review gates · `TUT-A8` live-verification holdout.

**Prerequisite tier (observability — already filed):** #710/#530/#230/#849 land *before* trusting an expanded
tutor (§5.7).

## 9. References

Peer-reviewed: ExpeL (arXiv:2308.10144, AAAI 2024) · Reflexion (arXiv:2303.11366) · Voyager (arXiv:2305.16291)
· Generative Agents (arXiv:2304.03442) · CER (arXiv:2506.06698, ACL 2025).
2026 preprints (arXiv-only, treat as directional): Self-Harness (2606.09498) · Auto-Dreamer (2605.20616) ·
freshness-aware PER (2604.16918) · Safety in Self-Evolving LLM Agent Systems (2606.23075).
Provenance: W3C-PROV workflow provenance (arXiv:2509.13978).
Internal: `internal/journal/identity.go`, `internal/telemetry/rollup/{schema,efficacy,findings}.go`,
`internal/workflow/compile.go`, `cmd/goobers/{openpr,configboundary,telemetryquery}.go`,
`selfhost/gaggles/goobers/workflows/tutor.yaml`, `internal/workflow/merge_review_test.go`.
