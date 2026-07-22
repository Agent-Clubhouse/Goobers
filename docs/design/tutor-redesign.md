# Design: Tutor v2 — version-aware, instance-scoped process self-improvement

> Status: **Draft for review — not implemented** · Area prefix: `TUT` · Milestone: _proposed_ **Tutor v2**
> Scope: the tutor edits a gaggle's **instance config** (`goobers-instances/<name>/`), never product code (§1.1). Its loop closes through **Workflow CD** (§4.6, M15).
> Related issues: #36 (tutor epic), #102 (cross-run detection queries), #104 (config-only write-boundary), #453 (Workflow CD / GitOps — the promotion half), #460 (WCD-6 `configrepo:read` — the tutor needs a write-sibling, §4.8), #507 (who owns test-suite quality), #150 (`Goober.spec.model`), #417 (first-class agent signal), #776 (usage in envelopes/spans), #769 (journal/telemetry schema migration).
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)

## 1. Why this exists

The **tutor** is Goobers' offline self-improvement workflow: between live tasks it mines the run
journal + telemetry rollup and opens PRs that make the *next* runs better. It already ships and works
on a narrow slice. The PO asked for two changes that, together, amount to a redesign:

1. **Grounded self-improvement, not just persona tuning.** The tutor should be able to improve the
   **workflow itself** — add/adjust stages, gates, **skills**, and workflow-level **test / validation
   stages** — not only edit a goober's persona prompt. (These are all *process* changes; see §1.1 for what
   that scope explicitly excludes.)
2. **Version-aware diagnosis.** The tutor must know *which version of which thing* produced an outcome,
   so it does not conflate behaviourally-different versions when it compares "before vs after."

Both are backed by findings from an audit of our own code + run history (§2, §6) and by external prior
art on offline agent self-improvement (§3). This document proposes the target shape, the governance it
requires, and a staged backlog.

### 1.1 Scope — the tutor edits the *instance config*, never the product code

The tutor's write target is the **instance's own config namespace** — the deployed gaggle/workflow/goober
definitions the running daemon reconciles from. **This path is operator-defined, not fixed:** the daemon is
launched with an instance path, and *that* path is injected as the tutor's target (whatever the operator chose;
`~/source/goobers-instances/goobers/*` is only *our* local self-hosting example — a hand-maintained fork of the
sample config, see memory: instance-config-is-drifted-fork). It is **never** the Goobers product repo
(`selfhost/`, `internal/`, `cmd/`, `api/schemas`). For a customer it is *their* instance — the
workflows/goobers/skills unique to their code and area. The tutor must therefore resolve its config root from
the daemon's injected instance path, not any hard-coded directory. Two hard consequences:

- **The tutor is a *process*-improvement agent** (how a gaggle *works*): workflow structure, gates, goober
  instructions, skills, workflow-level validation. It is **not** a product-code agent.
- **Product-code learnings belong to a different actor.** Fixing a flaky Go test, `internal/gate.Evaluate`,
  or telemetry code is *"goobers working on goobers"* — the normal **implementation / remediation** workflows
  operating on the product repo, driven by issues. That is orthogonal to, and never performed by, the tutor.
  (This is why the loop closes through GitOps CD, §4.6 — the tutor changes config, CD promotes config; neither
  touches product code.)

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

- **The reachability gap (action-space, within the process domain).** The tutor's *stated* ambition (its
  `analyst`/`config-author` instructions, TUT-011) is broad, but its *enforced* boundary is a single directory
  (`confineToConfigRoot`, `cmd/goobers/openpr.go` → `cmd/goobers/configboundary.go`), which aborts
  **fail-closed** if any changed path is outside the config root. Even restricting attention to *process*
  learnings the tutor legitimately owns (§1.1), that single-root boundary is narrower than it needs to be: it
  cannot author **skill bodies** (skills live outside the config root) and treats the two coverage-gap finding
  kinds as second-class inputs. (The larger-looking "reaches only ~¼–⅓ of learnings" figure in early analysis
  counted *product-code* learnings too — but those are the implementation workflows' job, not the tutor's, so
  the honest gap is: *within the process domain, the tutor can't yet reach skills and full workflow-level
  test/validation structure.*)

## 2. What the tutor is today (precise)

**Mineable inputs.** `goobers telemetry-query` drives a `gather-signals` stage; the rollup detector
(`internal/telemetry/rollup/findings.go`) emits exactly **six finding kinds** in three families:
`stage-failure-rate`, `error-signature` (failure patterns); `gate-never-fails`, `gate-repass-churn`
(gate noise); `workflow-untriggered`, `stage-unreached` (coverage gaps). _Surface gap:_ the `--aggregate`
flag only names the first three families, so the two coverage-gap kinds are second-class inputs today.

**Enforced action space (corrected).** The tutor is **not** "instructions + skills only." Its real,
enforced write scope is *anything expressible as a file under its config root*: workflow YAML (stages, gates,
`next:`/branch routing, capability lists, readiness caps), goober `instructions.md`, goober `goober.yaml`
(skills-list; `model` once #150 lands), and deterministic shell "test stages." Within the *process* domain
(§1.1) the notable thing it **cannot** author is **skill *body* content** — skills live in a separate skills
tree outside the config root, so it can edit a goober's skills-*list* but not a *new skill's* content. Product
code (`internal/`, `cmd/`, `api/schemas`, Go tests, CI) is out of scope **by design**, not by accident (§1.1).

_Sample-vs-deployed caveat._ The shipped sample `tutor.yaml` sets `configRoot: selfhost`, i.e. it edits the
Goobers product repo's *own* sample config. That is a self-hosting artifact. A **deployed** tutor's config root
is the instance directory (`goobers-instances/<name>/`), a repo distinct from the product code — which is why
the general model has no "product golden" coupling: instance-config edits are validated by the instance's own
`goobers validate`, not by a product Go golden like `internal/workflow/merge_review_test.go`. (The golden
coupling only bites the *sample* arrangement; §4.5 keeps a guard for that case.)

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

### 4.2 Action space (expanded, per-target sub-boundaries — all within the instance config)

Replace the single config-root with **per-action-class allow-roots, each fail-closed** like today's
`configboundary.Confine`. Every root below is **inside the instance config namespace** (§1.1); nothing here is
product code.

| Action class | Allowed root (in the instance config) | Notes |
|---|---|---|
| Persona / prompt | goober `instructions.md` | today's home turf; keep light-touch |
| Workflow topology | `**/workflows/*.yaml` (stages, gates, routing, capabilities, caps) | already in-reach |
| Gate calibration | gate `check`/threshold/`next` in workflow YAML | subject to the metric-gaming guard (§5) |
| **Skill body** | the instance's skills tree (see open decision D3) | new — lets the tutor *author* skills, not just list them |
| **Workflow-level test / validation** | test/validation **stages** + `goobers validate`-gated config | new — process regression guards *for the workflows*, expressed as config; **fail-first** required (§5). NOT Go unit tests. |
| Product code (`internal/`, `cmd/`, `api/schemas`, Go tests, CI) | **none** | out of scope **by design** (§1.1) — the implementation workflows' domain, not the tutor's |

Each action is confined to its own sub-root; a skill-authoring action cannot also rewrite a workflow, a
validation-stage action cannot reach outside the config namespace. This is the disciplined version of "the
tutor can touch the workflow, skills, and validation" — bounded per target, all inside the instance config.

### 4.3 Topology — per-workflow → per-gaggle, hard-siloed at the gaggle

**The gaggle is the tutor's outer boundary, inside a single instance. There is no global/cross-gaggle tutor.**
Gaggles are intentionally namespaced and siloed — the daemon already builds a Manager + Runner **per gaggle**,
run-branch namespace is per-gaggle (#965/#1010), and telemetry is already `--gaggle`-filterable. Cross-gaggle
analysis is **rare-or-impossible by design** (a privileged, explicit exception at most, never the default). A
consumer gaggle (`acme-web`) is bounded to *its* namespace and *its* instance config; it never sees another
gaggle's runs — and, per §1.1, it never edits product code either, only its own process config.

Our learnings split cleanly *within a gaggle* (§6, part D):

- **Per-workflow, cheap, frequent, low-blast:** persona gaps, gate calibration, stage wiring — these need
  *local* context (one workflow's own runs and reviewer verdicts).
- **Cross-workflow within the gaggle, higher-blast:** the gaggle's shared skills, its workflow-level test /
  validation stages, workflow structure, capability declarations, and the gaggle's shared gate calibration.

⇒ **A two-tier hybrid, both tiers confined to one gaggle's instance config:**

1. a **per-workflow tutor** confined to that workflow's own config subtree (persona / gate / wiring), and
2. a **per-gaggle tutor** that owns the gaggle's cross-workflow *process* learnings (the gaggle's shared
   skills, workflow-level tests, workflow structure). This is the tier that crosses the single-workflow subtree
   and therefore carries the stronger governance (§5).

A per-gaggle top tier (not global) respects the silo; a per-workflow-only design would be blind to the
shared-within-gaggle skill/validation/structure class. (Recommendation — see open decision D1.)

### 4.4 Learning method (adopting the Self-Harness loop)

For each tutor tier, one offline pass:

1. **Mine + cluster.** Pull version-segmented findings (§4.1) and **cluster failures into recurring patterns**
   within a cohort (ExpeL/Self-Harness), not isolated incidents. Contrast successful vs failed cohorts.
2. **Propose a bounded edit** to exactly one action-class target (§4.2), with a stated hypothesis and the
   provenance links (run-ids + journal pointers) it is grounded in.
3. **Validate before proposing** — held-in/held-out: the change must improve one split without degrading the
   other, evals repeated to defeat single-lucky-run promotion (Self-Harness).
4. **Human review gate**, differentiated by target (§5).
5. **Promote** — the merged change reaches the *running* daemon via GitOps CD (§4.6), not by hand.
6. **Verify live** — after promotion, a mandatory holdout re-run on *post-change* telemetry (segmented by the
   new `EffectiveVersion`) confirms the predicted improvement materialised (closes the
   propose→validate→promote→verify loop; open decision D4). The stronger, future form of this step is shadow /
   A-B evaluation (§4.7).

### 4.5 Validity guard (and the self-hosting-sample golden caveat)

In the general model the tutor edits an instance config repo distinct from the product code, so its edits are
gated by the instance's own **`goobers validate`** — the required post-edit step is simply "the changed config
still validates," with no product-code coupling.

The one exception is the **self-hosting sample**, where `configRoot: selfhost` means the tutor is editing the
product repo's *own* sample config. There, a topology edit to `merge-review.yaml` breaks the product golden
`internal/workflow/merge_review_test.go` (and the `acme-web` copy; see memory: shipped-workflow-yaml-dual-assert,
crd-manifests-drifted-not-gated), which the tutor cannot touch — so a topology-writing sample tutor ships CI-red.
Fix for the sample case only: either grant a **scoped, audited golden-regeneration** step, or (cleaner) point
the sample tutor at a throwaway instance dir instead of `selfhost/`, matching the deployed model. Deployed
instances do not hit this.

### 4.6 Delivery — the tutor loop is closed by GitOps CD

The tutor emits **PRs against the gaggle's own instance config repo** (§1.1) — the exact repo a deployed daemon
should be tracking. A merged PR changes nothing on the *running* daemon unless that config is reconciled to the
live instance — and today it is not: the live instance's workflow YAML is a hand-maintained drifted fork, not
updated by a merge (see memory: instance-config-is-drifted-fork). So the tutor's loop is **structurally open**
without continuous delivery — and this is the *same* fork problem WCD exists to solve, which is why the two are
one system.

**Workflow CD (#453 / WCD-1..7, milestone 15) is the promotion mechanism that closes it.** With CD, a merged
tutor PR on the instance repo's tracked `main` is reconciled into the live daemon (poll/watch/hook +
last-known-good, #458), so the *next* runs actually execute the improved config and the tutor can measure the
effect. The two efforts compose into one loop:

```
   mine (§4.1 version-segmented)  →  propose PR (§4.2 bounded)  →  validate held-in/out (§4.4.3)
        ↑                                                                      │
   verify live / shadow (§4.7) ← reconcile to live (Workflow CD #458) ← human gate (§5)
```

Implication: **CD is a soft prerequisite for the tutor's verify-live step and a hard prerequisite for shadow
evaluation (§4.7).** The provenance foundation (§4.1) is what makes each arrow *measurable*; CD is what makes
the loop *actuate*. This is the concrete reason the GitOps milestone and the tutor milestone should be
sequenced together, not independently.

### 4.7 Shadow / A-B evaluation (future milestone)

The verify-live step above is retrospective (measure *after* promoting). The stronger, safer form — and a
natural future milestone — is to evaluate a proposed change **before** it replaces the live one:

- **Versioned / variant workflows.** A gaggle can hold more than one `EffectiveVersion` of a workflow
  simultaneously — the *live* (authoritative) version A and a tutor-proposed *candidate* version B.
- **Shadow execution.** Version B runs **non-authoritatively** alongside A against real work — it produces no
  merges, comments, or side effects that reach the world (a dry-run / sandboxed sink), but its runs are fully
  journalled under B's `EffectiveVersion`.
- **Compare, then promote or discard.** Because A and B are distinguished by the provenance key (§4.1), their
  cohorts are compared *without conflation*. B is promoted (via CD, §4.6) only if it improves the target metric
  **and** introduces no new failure/error signatures A didn't have — catching *unforeseen* regressions the
  offline held-in/held-out split (§4.4.3) can miss. Otherwise it is discarded and the finding re-opened.

This is the "propose → run as shadow → prove desired outcome and no new issues → then promote/replace" loop the
PO described. It depends on: the provenance key (§4.1, this milestone), CD promotion (§4.6, milestone 15), and
a new **shadow-run / non-authoritative-sink capability** plus variant-version routing (future). It is
explicitly **out of scope for Tutor v2's first cut** and tracked as a follow-on milestone (§8).

### 4.8 Credentials — a separate, isolated grant for the instance-config repo

Opening PRs against the instance config repo requires **write** credentials to *that* repo. These must be a
**separate, isolated credential from the gaggle's target-repo creds** — exactly as a gaggle's implementation
workflows already hold creds scoped to the *product/target* code repo (to open PRs there), the tutor holds a
distinct credential scoped to the *instance config* repo (to open PRs there). The two must not be
interchangeable: the tutor's grant must not reach the product/target repo, and the code workflows' grant must
not reach the instance-config repo.

This is a **write-scoped sibling of WCD-6's `configrepo:read`** (#460): WCD reads the config repo to reconcile
it; the tutor writes to it (via PR) to improve it. Both route through the per-capability Grant / Injector seam
(`internal/credentials/capability.go`; see memory: multi-token-credentials, WCD-6/#460), fail-closed on an
undeclared capability. Concretely the tutor needs a `configrepo:write` (or `tutorconfig:write`) capability,
granted only for the daemon's injected instance path, minted as its own scoped PAT — never `Repos[0]` and never
the target-repo token. (Provisioning that scoped PAT is an operator action, in the same family as the WCD
adversarial-isolation pen-test #461 and the throwaway-test-repo creds.)

## 5. Governance & guardrails

Expanding beyond prose edits introduces failure modes the single path-check does not cover. These are hard
requirements, each mapped to prior art (§3):

1. **Fail-first validation.** Every tutor-authored workflow-level test / validation stage must be demonstrated
   **red against the pre-fix config** (reproduces the process regression) before green against the fix. A
   vacuously-passing check that "closes" a finding games the loop. (Analog of #909's "validate every config.")
2. **Metric-gaming guard (sharpest new risk).** The tutor mines `gate-never-fails` / `gate-repass-churn`. If it
   can also *edit gates*, the cheapest way to improve those metrics is to loosen/delete the noisy gate —
   improving its own score while stripping real coverage. A "noisy" gate can be *miscalibrated, not useless*
   (see shared-gate-evaluator-blast-radius / #415). **Rule:** the tutor may never remove/loosen the very gate
   whose noise flagged the finding without independent proof the gate is dead; gate-removal PRs get stricter
   review than gate-tuning ones. (Self-evolving-safety attack surface, arXiv:2606.23075.)
3. **Never self-judge efficacy unaided.** Before/after scoring uses the deterministic version-segmented stats
   (§4.1) plus, where a judgment is needed, **multiple judges + human oversight** — never the tutor grading
   its own change (measurable self-preference bias, §3).
4. **Validity gate mandatory.** Every workflow-structure change must pass the instance's own `goobers validate`
   and the `grep -rl "evaluator: agentic"` audit before landing (§4.5). (The product-golden dual-assert applies
   only to the self-hosting *sample*, §4.5 — deployed instances gate on `validate`.)
5. **Differentiated human gate.** CODEOWNERS-differentiated review: prompt/instruction changes stay
   light-touch; `workflows/**` topology, gate removals, skill bodies, and validation-stage additions require
   explicit human sign-off. Autonomy pressure (sustained-flow-creation-without-drainage) will push to auto-merge
   tutor PRs — **resist that specifically for structure/skill/validation changes.**
6. **Provenance discipline scales up.** Tutor PRs already cite run-ids + journal pointers (TUT-007). Extend:
   validation additions cite the specific failing run(s) *and* the fail-first reproduction; structure changes
   cite the aggregate that flagged them. **Meta-risk:** if telemetry integrity is blind (#710/#530), the tutor
   mines noise and "improves" against phantom signal. Those observability fixes are *product-code* work (not the
   tutor's job, §1.1), but they are a **prerequisite** for trusting the tutor's inputs — a sequencing
   dependency, not a tutor action.

## 6. Empirical learning taxonomy (why the above)

From an audit of our operational-memory corpus + closed run-quality issues, our learnings fall into 8 recurring
classes. The decisive column is **Domain**: is the fix *process config* (the tutor's job, §1.1) or *product
code* (the implementation workflows' job)? This split is what the PO's "edit the instance, not the main repo"
directive draws.

| # | Class | Example refs | Domain | Tutor-reachable? |
|---|---|---|---|---|
| 1 | Flaky-test / non-determinism | #745, #827, #1128, wave-705 | **product** (code/test fix) | — not tutor's job |
| 2 | Gate over/under-sensitivity | `gate-never-fails`/`repass-churn`, #608 | **process** (gate config) | ✓ core tutor |
| 3 | Missing regression test | #707, #415 guard, #909 | **product** (Go test) — but the *workflow-level* analog (a missing validation stage) is **process** | ◑ process analog only |
| 4 | Workflow wiring / ordering | #929, #496, #1052, #912 | **process** where it's YAML wiring; **product** where it's `applyverdict`/`claim` logic | ✓ for the config slice |
| 5 | Goober instruction / contract gap | #297/#299/#301/#310, #314 | **process** (instructions) | ✓ home turf |
| 6 | Capability / preflight gap | #735, #238, #284 | **process** (declarations) + **product** (preflight logic) | ◑ declarations |
| 7 | Provenance / observability gap | #710, #530, #230, #849 | **product** (telemetry code) | — not tutor's job, but a prerequisite (§5.6) |
| 8 | Flow / throughput ceiling | openprcount, claim.go liveness | **product** (scheduler code) + **process** (readiness caps) | ◑ caps only |

**Reachability, honestly stated:** the *process* classes (2, 4-YAML, 5, 6-decls, 8-caps) are squarely the
tutor's domain and mostly reachable today; the expansion adds **skill bodies** and **workflow-level
validation stages** (Class 3's process analog) to that domain. The *product* classes (1, 3-Go, 7, and the code
halves of 4/6/8) are **deliberately not** the tutor's job — they are "goobers on goobers" implementation work.
So the redesign is not about reaching into code; it is about making the tutor **complete within the process
domain** and **version-aware** (§4.1) so its process changes are measured without conflation.

**Topology signal:** Classes 2/4/5 cluster per-workflow (local context); Classes 1/3/6/7/8 + shared-evaluator +
shared-schema cut across the **whole gaggle** (per-gaggle, *not* cross-gaggle — the silo holds). ⇒ the
per-workflow → per-gaggle split of §4.3.

## 7. Open decisions (for review)

- **D1 — Topology.** Adopt the per-workflow → **per-gaggle** two-tier, with **cross-gaggle rare-or-impossible
  by design** (the gaggle is the silo)? _Recommended: yes._ Confirms there is **no** global/cross-gaggle tutor.
- **D2 — Target & write-authority ceiling.** Confirm the tutor writes only to the **instance config namespace**
  (`goobers-instances/<name>/`, §1.1) — workflow YAML + skill bodies + workflow-level validation stages — and
  **never** product code (`internal/`, `cmd/`, `api/schemas`, Go tests, CI), which stays with the implementation
  workflows. _Recommended: yes_ (this is the PO's "edit the instance, not the main repo" directive).
- **D3 — Skill location.** The instance's skills currently sit outside the config root. To let the tutor author
  skill bodies, either (a) add the skills tree as a per-target allow-root, or (b) relocate skills under the
  instance config root. _Recommended: (a)_ — smaller blast radius, no move.
- **D4 — Live-verification gate.** Require a post-promotion holdout re-run confirming the predicted improvement
  before a finding is closed? _Recommended: yes_ for structure/skill/validation changes; optional for persona
  tweaks. (The stronger shadow/A-B form is a future milestone, §4.7.)
- **D5 — Auto-merge stance.** Structure/skill/validation tutor PRs are **never** auto-merged (§5.5);
  persona/gate-tune PRs may follow the normal review path. Confirm.
- **D6 — Milestone sequencing.** Sequence Tutor v2 *with* Workflow CD (§4.6): the tutor's verify-live step is
  only meaningful once CD reconciles merged config to the live instance. _Recommended: yes_ — treat CD (M15) as
  a soft prerequisite for Tutor v2 and a hard prerequisite for the shadow/A-B milestone (§4.7).

## 8. Proposed backlog (staged)

**Tier 0 — provenance foundation (gates everything; file now):**
`TUT-P1` GooberDigest · `TUT-P2` model+harness version provenance · `TUT-P3` version-segmented efficacy in
`telemetry-query` (+ cohort rules). (`TUT-P4` journal migration tracked under #769.)

**Tier 1 — action-space + safety, all within the instance config (after D1–D6):**
`TUT-A1` resolve the config root from the daemon's **injected instance path** (not a hard-coded dir) +
per-target sub-boundaries · `TUT-A2` fail-first validation-authorship contract · `TUT-A3` metric-gaming guard ·
`TUT-A4` per-workflow → per-gaggle topology (no cross-gaggle) · `TUT-A5` skill-body authoring root · `TUT-A6`
differentiated CODEOWNERS review gates · `TUT-A7` post-promotion live-verification holdout · `TUT-A8`
**isolated `configrepo:write` capability + scoped PAT** for the instance-config repo (§4.8; write-sibling of
#460), routed through the Grant/Injector seam, never the target-repo token.

**Tier 2 — closes the loop (depends on Workflow CD, M15):** the tutor targets the gaggle's **instance config
repo** as the CD source (§4.6), so merged tutor PRs reconcile to the live daemon.

**Future milestone — shadow / A-B evaluation (§4.7):** variant-version routing + a non-authoritative shadow-run
sink, so a candidate version proves itself against live runs before promotion.

**Prerequisite (observability — *product* work, already filed):** #710/#530/#230/#849 land *before* trusting an
expanded tutor's inputs (§5.6) — a sequencing dependency, not a tutor task.

## 9. References

Peer-reviewed: ExpeL (arXiv:2308.10144, AAAI 2024) · Reflexion (arXiv:2303.11366) · Voyager (arXiv:2305.16291)
· Generative Agents (arXiv:2304.03442) · CER (arXiv:2506.06698, ACL 2025).
2026 preprints (arXiv-only, treat as directional): Self-Harness (2606.09498) · Auto-Dreamer (2605.20616) ·
freshness-aware PER (2604.16918) · Safety in Self-Evolving LLM Agent Systems (2606.23075).
Provenance: W3C-PROV workflow provenance (arXiv:2509.13978).
Internal: `internal/journal/identity.go`, `internal/telemetry/rollup/{schema,efficacy,findings}.go`,
`internal/workflow/compile.go`, `cmd/goobers/{openpr,configboundary,telemetryquery}.go`,
`selfhost/gaggles/goobers/workflows/tutor.yaml`, `internal/workflow/merge_review_test.go`.
