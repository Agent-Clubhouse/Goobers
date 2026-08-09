# Instance vs shipped examples — similarity analysis (2026-08-09)

Four-family comparison of the running instance's workflows (A = mature
goobers gaggle, B = goobers-site fork, both on `special-agent/config-2.0-reconciled`)
against every shipped example lineage (C = acme-web/embedded `goobers examples`,
reference-workflows, python/java/dotnet-service skeletons, starter/quickstart).
Full per-family tables in this directory.

## One-line answers

- **implementation**: *similar (~90%)* — acme-web/reference carry the full PR
  lifecycle, all four gates, and both parking lanes with production comments
  intact; the gap is tuning + lanes (no `agent:model`, no `local-ci`
  timeout, no critical lane, no `respectAssignee`).
- **merge-review**: *near-twin (~97%)* — acme-web and reference are
  full-fidelity copies of A's election/queue/scope machinery at the same
  vintage; B is the deliberate minimal fork. But merge-review is **not in the
  embedded `goobers examples` catalog** and absent from every polyglot
  skeleton — the product's endgame has no on-ramp example.
- **backlog-curation**: *the examples are AHEAD of live* — shipped curation
  carries four stages live never adopted (reconcile-backlog/CURE-2,
  implementation-feedback/#1807, sample-ready-pool/CURE-7, bounded
  blocked-on-sibling resweep/#1803).
- **pr-remediation**: *reference-only (~92%)* — the reference tracks A's
  PRR arc closely; acme-web folds remediation into one inline `remediate-ci`
  task; the polyglot skeletons have no remediation shape at all.

## The headline: drift is bidirectional, and the mirror is mislabeled

`reference-workflows/` declares "INTENTIONAL LIVE DIVERGENCE" only for
budgets/cadence, but the real divergence runs both directions:

**Reference ahead of live (adoption backlog for the instance):**
1. **Integrity floor + remediate-ci, as a pair** (TBH-4/#1885): reference
   `implement` sets `minimumIntegrity: maintainer` with a `contextFrom`
   allowlist excluding provider-derived evidence, *and* adds a separate
   `remediate-ci` consumer at unapproved-grade for CI evidence. Live has
   neither: no integrity floor (provider-authored content flows unfiltered
   into implementer sessions) and `ci-gate fail → implement` direct.
   ⚠️ These must be adopted **together** — the floor alone would strip CI
   evidence from repass paths and recreate identical-diff blind loops.
2. **Curation stages** (CURE-2, #1807, CURE-7, #1803 resweep) — shipped,
   tested, never synced into the live gaggle.

**Live ahead of reference (examples-staleness to fix upstream):**
- critical lanes, `respectAssignee`, `test-integration-strict`, 1800s
  local-ci (#1969's lesson — acme-web carries **no** timeout and is exposed
  to the exact SIGQUIT-as-CI-failure loop), `pollIntervalSeconds`,
  `dslVersion: "2.0"` (all C files are 1.4; #2698's migration).
- acme-web's agentic stages/goobers omit `agent:model` (the cold-start
  python agent hit this: scaffolder and guide say required, examples omit).

## Verdict on the operator's question

The examples and the instance are **converging lineages of the same
design, roughly one hardening-generation apart in each direction** — not
strangers. A user copying acme-web + reference gets ~90% of production
reliability; what they miss is concentrated, named, and mostly
parameter-level. The three structural fixes that would close the loop:
embed merge-review in the examples catalog, carry #1969's timeout into
acme-web, and declare the reference's live-divergence honestly in both
directions (or better: make the sync bidirectional and continuous — the
curation stages sitting unadopted for weeks is the same drift class the
gaggle-to-gaggle audit found, one level up).
