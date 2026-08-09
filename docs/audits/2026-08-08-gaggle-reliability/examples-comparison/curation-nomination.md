## Family: backlog-curation + nomination/fan-out flows

Scope note: C has **two independent lineages** for this family — `acme-web/workflows/{backlog-curation,work-nomination,backlog-assignment}.yaml` (= the embedded `goobers examples` set verbatim, confirmed via `config-examples/embed.go`'s `//go:embed` list) at **dslVersion 1.4**, and `reference-workflows/gaggles/goobers/workflows/{backlog-curation,quality-sprint,work-nomination}.yaml` at **dslVersion 2.0**. `python/dotnet/java-service` have no files in this family. `reference-workflows/backlog-curation.yaml` is structurally identical to `acme-web`'s (only gaggle name/cadence/wording differ — confirmed by diff) and both track **upstream main directly**, not A's live tuning — its own header says so ("INTENTIONAL LIVE DIVERGENCE... Operator-tuned live cadence and budget changes are deliberately not mirrored here"). `reference-workflows/quality-sprint.yaml`, by contrast, is **not** a copy of A's `quality-sprint.yaml` at all — it's upstream's own deliberately-minimal FO-10 demo (own file: "this is an example demonstrating the primitive, not a production quality-tracking framework"). Treat these as two separate comparisons below.

---

### 1. backlog-curation.yaml — stage inventory

| Stage | A (`goobers`) | B (`goobers-site`) | C acme-web / embedded / reference-workflows (identical to each other) |
|---|---|---|---|
| reconcile-backlog (label/state drift repair) | **absent** | absent | **present** — CURE-2 (#1267) |
| implementation-feedback (chronic-failure→re-curate loop) | **absent** | absent | **present** — (#1807) |
| sample-ready-pool (ready-pool health snapshot) | **absent** | absent | **present** — CURE-7 (#1377) |
| query-backlog (claim) | present | present-but-simpler | present + bounded resweep of `blocked-on-sibling` items (#1803/#2375) |
| surface-duplicates (dedupe pre-pass) | present, identical role | **absent** | present, identical role |
| curate (agentic) | present, +milestones, +`goobers:critical` urgency routing | present-but-simpler: no milestones, no critical-labeling, 2-way disposition only | present, +milestones, but **missing `agent:model` capability grant** on its own agentic stage; re-checks resweep items for drift instead of critical-labeling |
| release-claim | identical role | identical role | identical role |

**Similarity verdict:** A user who copies C's `backlog-curation.yaml` reaches roughly A's *stage count* for query→dedupe→curate→release, but is missing the label/state-drift repair, chronic-failure feedback, and health-observability passes A itself never adopted either (see below) — those are real production lessons, encoded in the commit messages: reconcile fixes drift so goobers:ready→not-ready transitions are caught before the health snapshot; implementation-feedback prevents an item silently stuck failing implementation forever without returning to curation; resweep prevents a `blocked-on-sibling` item from being permanently excluded once its blocker actually clears. A copier also gets a curator that (per acme-web) never declares `agent:model` — per `internal/capability/capability.go`'s own comment, this capability "is deliberately NOT in the runner's auto-granted set — it must be sourced explicitly," so on a Copilot-style harness the curate stage's model auth may not be wired at all. This gap is present across most of acme-web's agentic stages (`backlog-curation`, `implementation`, `default-implement`, `work-nomination` all omit it; only `docs-updater` declares it) — a systemic omission in this example gaggle, not a one-off typo.

**Divergence direction:** **Bidirectional, not simply "C is behind."** Reference-workflows/acme-web (C) is *ahead* of the live instance A on generic backlog hygiene — A's own file was hand-synced through main commit `5f96c0c9` (2026-07-24) and never picked up CURE-7 (07-24 later same day), the resweep feature (#1803, 07-28), the feedback loop (#1807, 07-28), tracking-parent auto-close (#1878, 07-29), or the needs-human semantics split (#2343, 08-03) / sibling-dependency reclassification (#2375, 08-03) — A's own `excludeLabels` comment text is still the **pre-#2375** wording verbatim. A *did* cherry-pick the later `curation: "true"` compiler-required fix (#2500, 08-06) as a standalone backport without the surrounding resweep machinery it shipped alongside — evidence of a **targeted patch, not a resync**. Conversely, A is ahead of C on the `goobers:critical` urgency-routing logic tying curation to `test-instability-nomination`/`quality-sprint` — an instance-specific feature C's generic example has no reason to carry. So: **STALE in one direction (generic hardening), PRINCIPLED-but-cut in the other (instance-specific routing not upstreamed)**. B is a deliberate, principled subset of A (own header: "No reconcile/health-sample/dedupe-surface stages... those are real, available enhancements... not needed for a fresh, low-volume backlog yet") — B's simplification is honest about what it's foregoing.

**Parameter deltas:**

| | A | B | C |
|---|---|---|---|
| schedule | `*/10 * * * *` | `*/15 * * * *` | `17 */6 * * *` (acme) / `22 3,11,19 * * *` (ref) |
| maxRunsPerHour | 900 | 20 | 1 |
| maxItems | 20 | **10** | 20 |
| staleAfterDays | 90 | 90 | 90 |
| staleAutoClose | `"false"` explicit | **not set** (implicit default) | `"false"` explicit |
| curate capabilities | issues:write, milestones:write, agent:model | issues:write, agent:model (**no milestones**) | issues:write, milestones:write (**no agent:model**) |
| dslVersion | 2.0 | 2.0 | **1.4** |

**One-line:** *Divergent in both directions — the gap is that C ships main's newer hygiene stages (reconcile/health/feedback/resweep) A hasn't synced in ~2 weeks, while A carries a `goobers:critical` routing feature and milestone/dedupe depth B lacks; a straight C→instance copy would be functionally closer to "current main" than A's own live copy is.*

---

### 2. Nomination / fan-out flows — mapping

A runs **two** current workflows here (`quality-sprint.yaml` self-review, `test-instability-nomination.yaml` CI-flake triage — A's own header: "not superseded, distinct signal"); the design doc for a *general* instability producer exists but is unshipped (`docs/design/test-suite-quality-workflow.md`, #1489/#1490), so **no C analog exists** for `test-instability-nomination` at all. B runs **one** analog, `upstream-sync.yaml`. C carries the vestigial `work-nomination.yaml` (created #93, 2026-07-13; last touched only by a schema-lifecycle commit #1611, 2026-07-26 — **content frozen since creation**) plus reference-workflows' independent `quality-sprint.yaml` demo.

| | A `quality-sprint` | A `test-instability-nomination` | B `upstream-sync` | C `work-nomination` | C-ref `quality-sprint` |
|---|---|---|---|---|---|
| trigger | manual | schedule, 2h | manual | schedule, daily | schedule, weekly Mon |
| shape | churn → 8 lenses (**shared goober** `quality-lens-researcher`, per-branch `areaName`/`areaFocus`) → triage → nominate | gather-ci-history(`gh` CLI) → shape-instability → nominate-instability | churn → 4 lenses (**shared goober** `change-area-researcher`) → site-relevance-triage → nominate | gather-signals(`telemetry-query`) → **single nominate stage**, no fan-out | churn → 6 lenses (**shared goober** `quality-researcher`) → collate → nominate |
| terminal-stage goober | `backlog-clerk` (generic, reused across A's pipelines) | `backlog-clerk` | `site-sync-clerk` (B's own generic clerk) | `nominator` (one-off) | `nominator` (one-off) |
| **approve-issue capability** | **yes** — clerk's own per-item rubric | **yes** — forced blanket auto-approve + `goobers:critical` | **yes** — mirrors A | **no** — never approves | **no** — explicit SEC-047 comment: "a maintainer approves... this is a public repo" |
| additionalRepos / cross-repo | no (self-repo) | reads own product repo's CI history via `gh` | **yes** — `gaggle.additionalRepos` reads upstream Goobers | no | no |
| branchTimeoutSeconds | 3600 | n/a | **7200** | n/a | 2700 |
| maxConcurrentBranches | 8 | n/a | 4 | n/a | 4 |
| noise control | delegated to `backlog-clerk`'s own instructions (not in YAML) | forced auto-approve, evidence-gated | delegated to `site-sync-clerk` | inline `maxNominationsPerRun: 5`, `dedupeWindowDays: 14` | delegated to `nominator`'s instructions |

**Similarity verdict:** A user who copies C's `work-nomination.yaml` gets a materially *different, older* architecture — one shared telemetry-driven stage, hard per-run/dedupe caps at the workflow layer, and (crucially, matching SEC-047 as stated in C-ref's own comment) an agent that **never** self-approves; every filed issue waits on a human. That's the single biggest capability gap versus A: A's `backlog-clerk`/`nominate` stages hold `github:issues:approve`+`approve-issue` and can stamp `goobers:approved` themselves (blanket for CI-flake findings, per-item rubric otherwise) — a materially more permissive trust boundary that C's examples deliberately don't grant. A copier following C alone would be safer by default but strictly more human-gated at nomination volume; they would not reach A's throughput without independently deciding to grant that capability.

**Divergence direction:** B (`upstream-sync.yaml`) is the closest analog to A architecturally — same shared-goober-per-lens pattern, same `*-clerk` terminal stage generalization, same `approve-issue` grant, explicit VISION.md cross-reference to A's own design intent ("this goober exists in near-identical form for every findings-shaping pipeline this instance runs; it should be one referenceable role, not a fork"). This is **principled convergence between A and B specifically** (same operator, coordinated design), not something C reflects. C's two examples are each frozen at an earlier design generation: `work-nomination.yaml` predates the `backlog-clerk` generalization entirely (single-purpose `nominator`, no fan-out); `reference-workflows/quality-sprint.yaml` is deliberately kept as the minimal canonical FO-10 primitive demo and is not intended to track A's elaborated version — its comments say so directly. This is **STALE for `work-nomination`** (genuinely superseded — A's own `quality-sprint.yaml` header states "Supersedes work-nomination.yaml (deleted in this change)") and **PRINCIPLED for `quality-sprint`** (intentionally kept minimal as a primitive demo, not meant to converge).

**One-line:** *Divergent — C's `work-nomination.yaml` is stale/superseded lineage (pre-dates the shared-goober/backlog-clerk pattern both A and B now use) and never grants the agent self-approval that A's and B's nominate stages both hold; C's `quality-sprint.yaml` is deliberately, not accidentally, simpler (documented primitive demo) while B has converged tightly onto A's actual current architecture.*

---

### 3. backlog-assignment.yaml — orphan in C

No A or B equivalent exists at all. C-acme-web's own header: "Opt-in canonical workflow: this example is embedded for operators to copy and configure, but is not installed in the self-host gaggle." Purely deterministic (`assign-backlog`, `constant-cap`/round-robin roster), targets human-assignee load-balancing — orthogonal to A's fully-autonomous claim model, not a simplification of anything A runs.

**One-line:** *Not comparable — a feature A/B have no analog for because A/B's model has no human-roster assignment step; presence in C reflects acme-web's broader target audience, not a gap A is missing.*