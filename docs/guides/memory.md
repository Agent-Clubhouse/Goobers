# Operator guide: gaggle memory

This guide covers running the `gaggle-memory` helper and wiring the recall,
reflect, wizard-gate, and dream patterns into a gaggle's workflows. For the
architecture and threat model, see `docs/design/agent-memory.md`.

## Prerequisites

- Python 3 (stdlib only — no `pip install`).
- The helper at `tools/gaggle-memory.py`.
- A store directory. The helper creates the subtree on first use.

Every subcommand takes `--store <dir>`.

## Store layout

```
<store>/
  MEMORY.md            index of active/ (generated; do not edit by hand)
  active/              trusted, recall-eligible memories. Only `promote` writes here.
  proposed/            proposals from runs and syncs. Never recalled.
  archive/             pruned/rejected/superseded memories.
  dream/               wizard decision files (decisions-YYYYMMDD-HHMM.yaml)
  inbox/claude/        raw Claude-project memory files, input to sync-claude
  journal.log          append-only audit log
```

Pick a durable location for the store — a mounted volume or a committed config
directory — so it survives across runs. A throwaway path defeats the purpose.

## Subcommands

### recall — read trusted memory

```
gaggle-memory recall --store <dir> --workflow <wf> --title <str> \
    [--labels csv] [--areas csv] [--text-file <path>] [--max N]
```

Reads `active/` only. Hard-filters to memories whose `scope.workflows` is empty
or contains `<wf>`, scores the rest, and prints the top `N` (default 8) full
memory files, each wrapped in `==== MEMORY ... ====` / `==== END MEMORY ====`,
followed by `RECALLED k OF n ACTIVE MEMORIES` (or `NO RELEVANT MEMORIES`).

```
gaggle-memory recall --store ./mem --workflow implementation \
    --title "fix flaky integration test" --labels ci,test \
    --areas "src/integration/foo" --max 5
```

Scoring: `3×label overlap + 2×area overlap (with X/** prefix match) +
1×keyword overlap + type-prior (+1 for known-failure/fragility)`. Deterministic;
ties break by name.

### propose — nominate a memory

```
gaggle-memory propose --store <dir> --source <str> --proposed-by <str> \
    --file <path> [--file <path> ...]
```

Validates each file's frontmatter, requires non-empty `type`, `description`, and
a non-empty `## Evidence` section, and writes each into `proposed/` as
`prop-<UTCdate>-<name>.md`. Sets `provenance.source`/`proposedBy` from the flags.
Downgrades a `confidence: proven` claim from a non-`human`/non-`claude-sync`
source to `observed-once`. At most 3 files per call, 10 KB each. Never writes
`active/` or `MEMORY.md`.

```
gaggle-memory propose --store ./mem --source run:2f9c1a \
    --proposed-by reviewer --file /tmp/candidate.md
```

### promote — apply a decisions file

```
gaggle-memory promote --store <dir> --decisions latest|<path>
```

Loads the newest (or named) decisions file from `dream/` and applies it under
the hard rules. See "Decisions file format" below.

### audit — detect tampering

```
gaggle-memory audit --store <dir>
```

Recomputes the `active/` tree hash, compares it to the last journal entry, and
quarantines any `active/` file the journal does not explain (moves it to
`proposed/quarantine-<name>` and journals the move). Prints "AUDIT CLEAN" or an
"AUDIT FINDING" report. Always exits 0 — it is a detective control; the report
carries the finding. Run it first in the nightly dream.

### index — regenerate MEMORY.md

```
gaggle-memory index --store <dir>
```

Rebuilds `MEMORY.md` from `active/`, one line per memory. `promote` does this
automatically; run it by hand only to repair the index.

### sync-claude — translate Claude memory into proposals

```
gaggle-memory sync-claude --store <dir>
```

Reads `inbox/claude/*.md`, tracks a per-file content sha in
`inbox/.sync-state.json`, and for new or changed files writes a proposal into
`proposed/claude-<name>.md` with `provenance.source: claude-sync`. Type mapping:
Claude `project` → `procedure`/`fragility`/`decision` (keyword heuristic,
`procedure` fallback), `feedback` → `known-failure`, `reference` → `reference`,
`user` → dropped. Never writes `active/`.

### init-from-claude — seed a new store

```
gaggle-memory init-from-claude --claude-dir <dir> --out <dir> [--shared]
```

Reads a Claude project's `CLAUDE.md`, `memory/*.md`, and `MEMORY.md`, translates
each memory into `<out>/active/*.md` (`provenance.source: seed`, `confidence:
proven`), synthesizes a `scope` from content keywords, and regenerates
`<out>/MEMORY.md`. With `--shared`, drops any memory containing a URL, hostname,
absolute path, or `private: true` marker.

This produces a **seeds directory for a human to review** — it does not deploy
anything. Edit the synthesized `scope` on each seed, then move the reviewed
files into your live store's `active/`.

## Decisions file format

A decisions file lives in `dream/` as `decisions-YYYYMMDD-HHMM.yaml` and is
written by the wizard-dreamer, never by hand in normal operation.

```yaml
timestamp: 2026-01-14T05:00:00Z   # optional; falls back to file mtime
decisions:
  - op: promote
    file: prop-20260113-flaky-integration-suite.md
    edits:
      labels: ["ci", "test"]
    rationale: "Confirmed in run:2f9c1a and run:7b03de."
  - op: merge
    file: prop-20260113-shared-fixture-race.md
    into: flaky-integration-suite
    rationale: "Same root cause; fold evidence in."
  - op: reject
    file: prop-20260112-speculative.md
    rationale: "Single hypothesis, no cross-run evidence."
  - op: prune
    file: stale-decision.md
    rationale: "Superseded; area was removed."
report: "Promoted 1, merged 1, rejected 1, pruned 1."
```

Ops: `promote` (proposed → active), `merge` (fold a proposal's Evidence into a
named active file, keep the stronger confidence, archive the proposal), `reject`
(proposed → archive), `prune` (active → archive).

Hard rules, enforced in code — the **whole file** is rejected on any violation:

- ≤ 5 promotions and ≤ 2 merges per run.
- Promoted files pass full schema validation with non-empty Evidence.
- `confidence: proven` only if source is `human`/`claude-sync`, or the rationale
  cites ≥ 2 distinct run ids.
- `proposed/quarantine-*` files are untouchable without `humanApproved: true`.
- A decisions file older than 24h is refused (no replay).
- Unknown op rejects the file.

## Wiring memory into a gaggle's workflows

The four patterns map onto DSL stages. Copyable templates live in
`config-examples/memory/`.

### Recall at the top of an implement workflow

Add a cheap agentic `recall` task (the **scribe** goober) that runs
`gaggle-memory recall` and emits the output as a MEMORY BRIEF, then add `recall`
to the downstream implement task's `contextFrom`. Set `continueOnError: true` so
a recall miss never blocks the run — memory is advisory.

See `config-examples/memory/workflows/recall-snippet.yaml`.

### Reflect at the end

Add an agentic `reflect` task (scribe) that writes proposals via
`gaggle-memory propose`. It writes only to `proposed/`. Include the parked-run
variant so a blocked run records *why* it parked.

See `config-examples/memory/workflows/reflect-snippet.yaml`.

### Wizard gate before other reviews

Add an agentic `wizard-review` gate (the **wizard** goober) ahead of your other
review gates. Gate evaluators take no `goal` or `contextFrom`, so the wizard's
full prompt lives in its `instructions.md`.

See `config-examples/memory/workflows/wizard-gate-snippet.yaml`.

### The nightly dream

Add a schedule-triggered `dream` workflow: `audit-store` (deterministic) →
`sync-claude` (deterministic, `continueOnError`) → `consolidate` (agentic, the
**wizard-dreamer** goober, writes a decisions file) → `apply-decisions`
(deterministic, `gaggle-memory promote --decisions latest`). Cron `0 5 * * *`,
`maxConcurrentRuns: 1`.

See `config-examples/memory/workflows/dream.yaml`.

## Seeding a new gaggle

1. Run `init-from-claude --claude-dir <project> --out <seeds>` (add `--shared`
   for a store others will read).
2. Review every seed. Edit the synthesized `scope`; the tool cannot infer it
   reliably. Delete anything that is not durable, cross-run truth.
3. Move the reviewed files into your live store's `active/` and run
   `gaggle-memory index --store <store>`.
4. Wire the recall, reflect, wizard-gate, and dream stages into the gaggle's
   workflows using the templates above.

## The Claude → gaggle sync flow

To keep a Claude project's memory flowing into the gaggle:

1. Land the Claude project's memory files under `<store>/inbox/claude/`
   (however you mirror them — a scheduled copy, a checkout, a mount).
2. Run `gaggle-memory sync-claude --store <store>` — typically as the
   `continueOnError` step in the nightly dream. New or changed files become
   proposals under `proposed/claude-*.md`.
3. The wizard-dreamer judges those proposals like any other during
   `consolidate`, and `promote` applies the decision.

Synced memory is never trusted directly. It lands in `proposed/` and clears the
same promotion bar as everything else — with the one concession that its
`claude-sync` source clears a `proven` claim without the two-run-id requirement.
