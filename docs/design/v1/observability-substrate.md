# Design: Observability substrate — OTel-standard run data for arbitrary workflows and the Tutor (V1)

> Status: **Draft for review** · Area prefix: `TEL` (extends), `TUT` (feeds) · Milestone: **V1**
> Requirements: [`docs/requirements/telemetry.md`](../../requirements/telemetry.md) ·
> [`docs/requirements/tutor.md`](../../requirements/tutor.md) ·
> Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §4 (journal), §8 (telemetry)
> Peer design: [`36-tutor-workflow.md`](36-tutor-workflow.md) (PR #100) — the Tutor
> consumes what this substrate produces.

## 1. Why this exists

V1's hallmark is twofold: Goobers runs **arbitrary workflows for projects that are not
Goobers**, and the **Tutor exists**. Both stand on the same substrate: every run must
leave behind a rich, standards-shaped, queryable play-by-play — per-stage outcomes,
durations, error codes, within-stage traces, agentic token/credit usage, and the
prompts that were actually sent. This document turns the PO's observability
requirements into concrete design decisions and dispatchable missions.

PO goals (traceability anchors used throughout):

- **G1** Each run records rich per-stage data: success, duration, reasons/error codes.
- **G2** Stages can emit custom metrics/logs, including detailed within-stage traces.
- **G3** Logs follow OpenTelemetry standards — including the emerging standards for
  agentic/GenAI logging — and are consumable by standard tools locally now, in the
  cloud later (PO: "v3"; maps to the cloud-scale milestone's ADX/OTLP drop-in, §10).
- **G4** Agentic stages log usage: tokens and/or GitHub Copilot premium requests.
- **G5** The prompts sent by agentic stages are captured, or there is a standardized
  way to obtain them (adapter capture, or instructed/standardized agent self-logging).
- **G6** The sum is a detailed play-by-play of an execution, usable for monitoring
  *and* as the Tutor's raw material.
- **G7** Collected data is queryable after the fact — including from *inside*
  workflows via a connector stage — not only by humans at a CLI.

## 2. What V0 already ships (baseline)

- **Journal spans + SQLite rollup** (#22, #24): per-run `spans/` in the journal;
  `telemetry.db` rollup tables (`runs`, `stage_attempts`, `gate_verdicts`,
  `provider_mutations`, `run_errors`, `spans`, `span_events`); a `goobers telemetry
  stats|errors` CLI over them. Journals are the source of truth; the rollup is a
  rebuildable projection (TEL-032). **Caveat (2026-07-13 review):** the OTel span
  pipeline is *library-complete but unwired* — no V0 binary constructs the telemetry
  `Client`, so production runs emit journal events but zero spans, and the shipped
  work-nomination workflow invokes a `goobers telemetry-query` subcommand that does
  not exist. Both are filed as V0 remediation; O1/O6 below build on those fixes
  landing first.
- **Envelope contract**: `ResultEnvelope.Metrics` (numeric: duration, tokens, cost,
  custom) and `ErrorInfo.Code` already exist (G1/G4's contract foothold);
  `Limits.MaxTokens` / `MaxCostUSD` exist as *declared* budgets.
- **OTel dependencies** are already in `go.mod` (SDK, OTLP/gRPC + stdout exporters).
- **Built-in stage-kind pattern**: `run.command: ["goobers", "backlog-query", …]` —
  a `goobers` subcommand as a documented deterministic stage kind (§7). The connector
  stage (G7) reuses this pattern; no DSL change.
- **Redaction at ingest** (#8/#14/#66): registry + pattern scrubbing before anything
  lands at rest, scrub-before-digest. Everything this design adds (transcripts,
  custom metrics, OTLP export) flows through the same choke point.

Gaps this design closes: no canonical OTel attribute set (TEL-Q3's remainder); no
GenAI/agentic semantic conventions; adapters do not populate usage metrics; prompts
are not captured; no stage-facing custom-metric emission surface; no export path a
standard tool can consume; no first-class telemetry connector stage contract; no
retention enforcement (TEL-013's remainder).

## 3. Design decisions

### D1 — Canonical OTel span model + attribute registry (G1, G3)

Run = **trace**; each stage attempt, gate evaluation, and scheduler decision = a
**span**; within-stage happenings = **span events** (or child spans where the source
is genuinely nested, e.g. harness tool calls). One canonical attribute registry,
kept in code next to `internal/capability`'s registry pattern and drift-guarded by
test:

| Attribute | On | Notes |
|---|---|---|
| `goobers.run.id`, `goobers.gaggle`, `goobers.workflow`, `goobers.workflow.version`, `goobers.workflow.digest` | all spans | resource-level identity |
| `goobers.goober`, `goobers.stage`, `goobers.stage.type` (`deterministic\|agentic\|gate\|scheduler`) | stage/gate spans | |
| `goobers.attempt.n`, `goobers.attempt.kind` (`policy\|infra`) | stage spans | mirrors the journal's attempt tagging (§3.3) |
| `goobers.item.id`, `goobers.item.url` | spans of backlog-driven runs | |
| `goobers.outcome` (`success\|failure\|blocked`), `error.type`, `goobers.error.code` | stage/gate spans | `error.type` is standard OTel semconv; `goobers.error.code` = `ErrorInfo.Code` |
| `goobers.gate.decision`, `goobers.gate.repass.n` | gate spans | |

Standard OTel names are used wherever semconv defines one; `goobers.*` only where it
does not. Where the journal and spans describe the same fact they MUST agree
(same enums, same codes) — spans stay **excluded from the §3.3 conformance set**, but
they must not *contradict* the journal.

### D2 — Agentic-stage semantics: GenAI semconv + usage accounting (G3, G4)

Agentic stage spans adopt the **OTel GenAI semantic conventions**:
`gen_ai.operation.name`, `gen_ai.request.model`, `gen_ai.response.model`,
`gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, with tool calls as span
events. Two Goobers-specific extensions (no semconv equivalent):

- `goobers.usage.copilot_premium_requests` — Copilot's billing unit; adapters report
  the billing unit their harness actually has, rather than pretending everything is
  tokens.
- `goobers.usage.cost_usd` — populated when the adapter can derive it; never guessed.

The same numbers land in `ResultEnvelope.Metrics` under these canonical names — the
envelope remains what gates/runner act on; spans remain the observability view. The
runner **enforces** `Limits.MaxTokens`/`MaxCostUSD` once adapters report usage
(budget exceeded → stage failure with `error.code=budget-exceeded`, run branches per
policy) — a declared budget that nothing enforces is a false promise to config
authors.

Adapter reality: a harness that exposes no usage data reports nothing — missing
metrics are represented as *absent*, never zero. The Copilot CLI adapter parses
usage from its session output where available; the Claude Code adapter (future,
separate issue) has first-class usage output.

### D3 — Export: journal-first, OTLP out (G3, G6)

The journal stays the source of truth; export is a projection, in two forms:

1. **OTLP file export** (always on, zero-dependency): spans additionally serialized
   as OTLP/JSON lines to `runs/<run-id>/spans/otlp.jsonl`. Any standard pipeline
   (otel collector `filelog`/OTLP-JSON receivers, Jaeger, Grafana Tempo) can ingest
   it; `goobers telemetry export --since …` re-emits any window from journals —
   rebuildability (TEL-032) doubles as backfill.
2. **OTLP push** (opt-in): `instance.yaml` `telemetry.otlp.endpoint` streams the
   same spans over OTLP/gRPC to a local collector the user runs (Jaeger quickstart
   documented in a guide). Off by default; tiers 1–2 keep zero service dependencies.

Cloud collection (managed collector / ADX per §10) is **explicitly out of scope**
(PO: "v3") — it is the same exporter pointed at a different endpoint, which is the
entire point of D1–D3 being OTel-real.

### D4 — Prompt/transcript capture (G5)

Every agentic stage attempt SHOULD produce a **transcript artifact**
(`transcript.jsonl`, a journal artifact: digested, scrubbed, pointer in the result
envelope). Records use the versioned GenAI-events schema
`goobers.dev/telemetry/genai-event/v1`: `{schema, role, content, model, usage,
tool_call…}` per line. Unversioned records remain readable as legacy data and
are not backfilled. Three capture sources, in order of preference:

1. **Harness-native**: the adapter converts the harness's own structured
   session/transcript output (Copilot CLI session logs; Claude Code transcripts).
2. **Adapter-composed minimum** (normative floor, always feasible): the adapter
   *itself* records the invocation prompt it composed (envelope goal + context +
   instructions) and the final output. **Every agentic attempt MUST at minimum have
   this** — we never depend on the harness's cooperation to know what we asked.
3. **Instructed self-logging** (fallback, non-normative): goober instructions direct
   the agent to append key decisions to a declared output artifact — useful color,
   never trusted as the record (an agent that misbehaves is exactly the one whose
   self-report you can't trust).

Transcripts are **spans-adjacent, not conformance data**: excluded from §3.3 like
all `spans/` content. Redaction: transcripts pass the same registry+pattern scrub
before landing (they will contain repo content; they must never contain resolver
credentials). Retention: transcripts are the largest artifact class — D8's window
applies, with a per-run size cap (adapter truncates with an explicit
`truncated=true` marker rather than failing the stage).

### D5 — Custom stage metrics/logs (G2)

Stages get a well-known, zero-dependency emission surface (works for `curl`-level
shell stages and agents alike — no SDK required):

- The executor sets `GOOBERS_TELEMETRY_DIR` to a writable, stage-scoped directory.
- `metrics.jsonl`: `{"name":…,"value":…,"unit":…,"attrs":{…}}` per line → merged
  into the stage's `ResultEnvelope.Metrics` (name-collision: stage-emitted loses to
  runner-computed) and onto the stage span.
- `events.jsonl`: `{"ts":…,"name":…,"attrs":{…}}` per line → span events (the
  within-stage trace for deterministic stages; agentic stages already get harness
  events via the adapter).
- stdout/stderr remain captured as today (logs); nothing new to learn for the
  simple case.

Ingest happens at stage completion: scrub → journal `spans/` → rollup. Malformed
lines are dropped with a counted warning event, never a stage failure — telemetry
must not be able to fail a run (fail-closed applies to the journal, not to optional
enrichment).

### D6 — The telemetry connector stage (G7)

A real `goobers telemetry-query` subcommand becomes the documented **built-in stage
kind** (same pattern as `backlog-query`, §7): typed flags (`--window`,
`--aggregate …`, `--threshold k=v`, `--format candidate-findings`), a **versioned
candidate-findings artifact schema** (`api/schemas/`): each finding = `{kind,
subject, metrics, threshold, flagged_runs: [journal pointers]}` — exactly the
deterministic→agentic handoff PR #100's T2/T3 need, and equally usable by any user
workflow ("query your own gaggle's last week and post a report"). Today's `goobers
telemetry stats|errors` remains the human surface; both are veneers over one query
engine. (This also permanently closes the V0 defect where shipped workflows invoke
a `telemetry-query` command that was never implemented.)

### D7 — Detection catalog (feeds Tutor T2; extends PR #100 §T2)

The cross-run aggregates MUST cover all four families the Tutor proposes changes
from — not only failures:

| Family | Aggregates | Needs |
|---|---|---|
| **Failure patterns** | failure-rate by stage/workflow/test, error-code clustering, crash-resume frequency | V0 rollup (exists) |
| **Waste** | duration/token/cost percentiles per stage, retry waste (attempts that never changed outcome), repass loops that always eventually pass | **D2 usage data** |
| **Gate noise** | gates that never fail over N runs, reviewer-verdict repetition (same rationale text cluster), needs-changes → unchanged-diff repasses | V0 rollup + span events |
| **Coverage gaps** | workflows never triggered, items claimed-but-expired, stages never reached, error codes with no owning gate | V0 rollup (exists) |

### D8 — Retention (closes TEL-013's remainder)

`instance.yaml`: `telemetry.retention.window` (default 90d) and
`telemetry.retention.maxRuns` (default 500). Daemon housekeeping (and `goobers
telemetry prune`) deletes expired **journals and their rollup rows together**
(journal is source of truth; a rollup row whose journal is gone is undiagnosable —
prune atomically per run). Tutor/T5's before/after comparisons read only inside the
window; the design accepts that. Coordinates with #55 (worktree hygiene) for the
shared "disk is finite at tier 1" posture.

### D9 — Goober `model` selection (unblocks a Tutor proposal type)

`Goober.spec.model` (optional string, harness-scoped vocabulary) + optional
`harnessOptions` (opaque map validated by the adapter). Without this, the Tutor
proposal type "change the model an agent uses" (PO list) has no config surface to
edit. Adapters MUST fail closed on an unknown model value at validation time, not
mid-run.

## 4. What this design does NOT change

- **Two-store doctrine** (§8) — untouched; this is all the goober-run store.
- **Journal as source of truth**; rollup and OTLP forms are projections.
- **Conformance set** (§3.3) — spans/transcripts/metrics stay excluded; nothing here
  may create runner-visible behavior differences.
- **Redaction-at-ingest, scrub-before-digest** — every new at-rest form (transcripts,
  custom metrics, OTLP files) goes through the existing choke point.

## 5. Missions (dispatchable, single-PR-sized)

| # | Mission | Design | Depends on |
|---|---|---|---|
| **O1** (#143) | Canonical span model + attribute registry, applied to existing spans; drift-guard test | D1 | #126 |
| **O2** (#144) | Usage accounting: adapter → envelope metrics under canonical names; rollup columns + aggregates; runner enforcement of MaxTokens/MaxCostUSD | D2 | O1 |
| **O3** (#145) | Transcript artifact standard + Copilot adapter capture (composed-prompt floor; session-log parse when available) | D4 | O1, #117 |
| **O4** (#146) | `GOOBERS_TELEMETRY_DIR` emission surface + ingest (metrics.jsonl / events.jsonl) | D5 | O1 |
| **O5** (#147) | OTLP file export + opt-in collector push + `telemetry export` backfill; Jaeger guide | D3 | O1 |
| **O6** (#148) | Connector stage: typed `telemetry-query` + versioned candidate-findings schema; migrate work-nomination onto it | D6 | O1, #132, #127 |
| **O7** (#149) | Retention: config, daemon housekeeping, `telemetry prune` | D8 | — |
| **O8** (#150) | DSL: `Goober.spec.model` + `harnessOptions` (+ validation, docs, starter examples) | D9 | — |

(V0.1 remediation prerequisites: #126 wires the span pipeline; #132 implements the
minimal `telemetry-query` subcommand; #127 makes the rollup robust; #117 unifies
scrubbing across at-rest surfaces.)

Test plans follow the repo standard: seeded-fixture unit tests per aggregate/surface,
negative controls for redaction on every new at-rest form (per the #qa-gate standard
from #66/#81), determinism tests for artifact outputs, and one e2e extension: the
walking skeleton asserts a run's OTLP file parses with the standard OTel proto and
carries the D1 attribute set.

Tutor sequencing (PR #100): T2 consumes O6 (artifact schema) and grows the D7
catalog — failure/noise/coverage families work on V0 data **now**; the waste family
lands when O2 does. T3's evidence pointers are the O6 schema's `flagged_runs`. O3
is what makes prompt-level Tutor diagnosis ("change the prompt a stage uses")
evidence-based rather than guesswork.

## 6. Out of scope

- Cloud/managed collection (ADX, hosted collectors) — cloud-scale milestone (PO: v3).
- OTel *metrics/logs signals* as separate pipelines — spans + span events + rollup
  cover V1 needs; revisit when a real consumer appears.
- A network telemetry listener at tiers 1–2 (`GOOBERS_TELEMETRY_DIR` is
  deliberately file-based; zero ports, zero deps).
- Cross-instance/fleet aggregation — out until multi-instance exists.
