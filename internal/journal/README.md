# `internal/journal` — the run journal (provenance contract)

Every run — on any runner, at any tier — produces the same inspectable,
append-only record. The portal, telemetry rollup, Tutor, and humans debug from
the **journal**, never from runner internals. This package owns the format and
the Go API that writes and reads it.

Authoritative spec: `docs/ARCHITECTURE.md` §4 (the journal) and §3.3
(conformance). Issue #8.

## Layout

```
runs/<run-id>/
  schema.json    # directory schema version and earliest compatible binary
  run.yaml       # pinned identity: workflow name+version+digest, gaggle, trigger, input refs
  state.json     # current machine state; atomically replaced checkpoint (derived)
  events.jsonl   # append-only event journal; every event carries a monotonic seq
  inputs/        # immutable, content-digested snapshots of run inputs
  artifacts/     # stage outputs, stored by content digest (sha256/<ab>/<rest>)
  spans/         # per-stage trace spans (telemetry, not conformance)
```

`<run-id>` is the run's OpenTelemetry trace id.

## Debugging with `cat` / `jq` / `grep`

The journal is **human-readable first**. `events.jsonl` is one JSON object per
line, so the standard tools just work:

```sh
# The whole story of a run, in order:
cat runs/<id>/events.jsonl | jq -c '{seq, type, stage, status}'

# What did each stage do, including retries (attempt N)?
jq -c 'select(.type | startswith("stage.")) | {seq, stage, attempt, attemptClass, status}' \
  runs/<id>/events.jsonl

# Every gate verdict and the branch it selected:
jq -c 'select(.type=="gate.evaluated") | {seq, gate, verdict, target}' runs/<id>/events.jsonl

# Which external issues/PRs did this run touch?
jq -c 'select(.type=="ref.touched") | .externalRef' runs/<id>/events.jsonl

# Find the first error:
grep -m1 '"type":"error"' runs/<id>/events.jsonl | jq .

# Pin: what workflow definition did this run commit to?
jq '{workflow, workflowVersion, workflowDigest}' runs/<id>/run.yaml   # yq for raw yaml

# Where is the run right now (derived checkpoint)?
jq '{phase, machineState, lastSeq}' runs/<id>/state.json

# Verify an artifact's content address by hand:
shasum -a 256 runs/<id>/artifacts/sha256/ab/cdef...    # matches the digest in the event
```

## Event envelope

One versioned JSON object per line. See `api/schemas/journal-event.schema.json`
for the machine-readable contract; every field there is tagged
`x-conformance: normative | excluded`.

| field | notes |
|---|---|
| `schema` | envelope version (`goobers.dev/journal/event/v1`) |
| `seq` | monotonic per-run sequence from 1 — the ordering key |
| `type` | `run.started` · `run.finished` · `stage.started` · `stage.heartbeat` · `stage.finished` · `gate.evaluated` · `artifact.recorded` · `span.recorded` · `input.snapshot` · `ref.touched` · `error` · `redaction` · `repaired` |
| `branch` | 0 at tiers 1–2; reserved for tier-3 parallel branches |
| `time` | timestamp — **excluded** from conformance |
| `stage`/`attempt`/`attemptClass` | stage identity; `attemptClass` is `policy` or `infra` |
| `gate`/`verdict`/`target` | gate evaluation |
| `status` | terminal status for `run.finished`/`stage.finished` |
| `ref` | in-journal content pointer (`{path, digest, size, mediaType?}`) |
| `externalRef` | `{provider, kind, id, url?}` — identity is the first three |
| `error` | `{code, message?}` |
| `redaction` | `{target, oldDigest, newDigest, reason?}` |
| `runner` | runner-specific annotations — the ONLY sanctioned divergence, always **excluded** |

### Conformance (§3.3) — normative vs excluded

The same workflow with fixed stage effects must produce **equivalent** journals
on either runner. "Equivalent" compares only the **normative** fields, in
`(branch, seq)` order:

- **Normative:** `seq`, `type`, `branch`, stage/gate identity and outcome,
  external-ref identity `(provider, kind, id)`, artifact **digest**, error
  `code`, redaction digests. Retry attempts count **only when
  `attemptClass != "infra"`**.
- **Excluded:** `time` and any duration, `stage.heartbeat`, `spans/` (and its
  `span.recorded` events — harness/LLM output, structural only), `state.json`
  (derived), the entire `runner.*` namespace, `ref.path`/`size`, `url`, human
  `message`.

`Event.IsConformanceNormative()` and the per-field `x-conformance` markers in the
schema drive #29's determinism assertion and the V2 conformance harness (#40).

## Durability & crash recovery

- **Append + fsync per event.** `Append` writes one line and fsyncs before
  returning, so a completed event is never lost to a crash.
- **Atomic checkpoints.** `state.json` is written via temp-file + rename; a
  reader never sees a half-written checkpoint. It is *derived* — always
  reconstructable from the event log.
- **Torn-write repair.** A crash can only leave a partial final line. `Recover`
  discards it, appends a corrective `repaired` event (so even the repair leaves a
  trace), and reopens the run for appending. `state.json` is never trusted over
  the log: the run phase is reconstructed from the events themselves.

## Redaction — secrets never land at rest (`SEC-041`, `TEL-013`)

Every event, input snapshot, and artifact passes through a `Scrubber` **before
write and before digesting**, so content addresses commit to the scrubbed bytes.
The default scrubber chains a **registry** (fed every resolver-issued credential,
exact-match redaction) before a **pattern net** (secret-shaped material that
never went through the resolver).

The one sanctioned edit to the append-only journal is remediation of a miss:
`Run.Redact` (backing `goobers journal redact`) replaces a leaked blob with a
scrubbed copy, removes the leaked bytes, and appends a `redaction` event
recording the old→new digests.

## Schema compatibility

`schema.json` is the inspectable run-directory schema authority. `OpenRead`
migrates the legacy pre-manifest layout after creating a quiescent `.bak` copy
under the runs root's sibling `.journal-backups/` directory.
The `schema` fields in `run.yaml`, `state.json`, and each event remain their
payload-envelope versions. A directory or payload version newer than this build
fails closed with the found and supported versions plus the required binary.
See [`docs/guides/schema-migrations.md`](../../docs/guides/schema-migrations.md)
for the compatibility, backup, and rollback policy.

## The instance journal (`scheduler/events.jsonl`)

Alongside per-run journals, the instance root has its own long-lived log at
`<instance-root>/scheduler/events.jsonl` (§4/§6): scheduler decisions
(`trigger.fired`, `tick.skipped`, an instance-level `run.started`/`run.finished`
echo) and claim-ledger transitions (`claim.acquired`, `claim.released`,
`claim.force_released`). It uses
the **same envelope, same append+fsync durability, and the same torn-tail
crash-recovery** as a run's `events.jsonl` — `InstanceLog` shares its core with
`Run` (`appendEvent`, `truncateTornTail`) rather than duplicating it. Unlike a
`Run`, it is opened once for the daemon's lifetime (`OpenInstanceLog`), not once
per run, and carries no `run.yaml`/`state.json`/artifacts. Instance-only event
types add two informational fields not used in a run's own
log: `workflow` (which workflow the decision concerns) and `runId` (which run a
claim/dispatch pertains to) — a run's own events don't need either since both
are implicit from the run directory.

### In-daemon compaction and the generation pointer

Unlike a run's `events.jsonl` (write-once for the run's lifetime, then
retired), the instance journal accumulates for as long as the daemon runs and
needs periodic compaction (`(*InstanceLog).Compact`, or `goobers telemetry
compact`'s offline `CompactInstanceEvents` for a stopped daemon) to drop aged
scheduler/claim history without unbounded growth.

Compaction never rewrites `events.jsonl` in place. On Windows, no
rename/replace/delete API — not `MoveFileEx`, not even the dedicated
`ReplaceFile` API built for exactly this kind of swap — can act on a path that
some handle has open without `FILE_SHARE_DELETE`, and an ordinary reader (the
portal, `goobers status`, another independently-opened `InstanceLog`, or
anything else that might `open()` the file) has no reason to request it.
POSIX has no such restriction, which is why this class of bug only ever
surfaced on Windows CI.

Instead, each compaction writes its output to a new **generation**:
`events.jsonl.gen-NNNNNN`, alongside a small pointer file, `events.jsonl.current`
(just the generation number, written via the ordinary durable-write+rename
primitive — safe on Windows because nothing holds a lasting handle on the
pointer itself, unlike the events file). `resolveInstanceEventsPath`
(`instancegen.go`) is the single place that turns "an instance directory" into
"the current events file": `OpenInstanceLog`, `Append` (via
`ensureActiveFile`, which already knew how to detect and reopen a rotated
file — the same mechanism now also catches a generation bump), `ReadInstanceLog`,
and `Compact` itself all go through it. Generation 0 keeps the legacy bare
`events.jsonl` name and has no pointer file, so a directory that predates this
scheme, or has never compacted, needs no migration.

A reader that resolved a path before a compaction advances the pointer keeps
reading that exact file, undisturbed, forever — Windows has nothing to object
to, since nobody ever touches that path again. Stale generations are cleaned
up after the pointer advances: each compaction sweeps *every* generation
present on disk that is older than the one behind current
(`cleanupStaleInstanceEventsGenerations`), keeping at most the current and
immediately-previous generation — enough for a reader that resolved the
pointer moments before it advanced to still find what it expected. Sweeping
the whole directory, rather than only the single generation that just fell
out of the window, means a transient removal failure (sharing violation,
permission denied) no longer strands that generation forever: the next
compaction comes back for it. Removal failures are reported on
`InstanceEventsCompaction.StaleGenerationCleanupErr` (alongside
`StaleGenerationsRemoved`) instead of being swallowed, and are deliberately
not fatal — the compaction that recorded new data still succeeds, since a
stranded generation only wastes disk. A caller outside this package that needs the current file's own
path (e.g. a freshness/dead-man-switch health check reading its mtime) uses
the exported `InstanceEventsPath`, never a hardcoded `events.jsonl` join.

## Go API

```go
run, err := journal.Create(runsDir, journal.RunIdentity{
    RunID: traceID, Workflow: "nominate-and-fix", WorkflowVersion: 3,
    WorkflowDigest: machine.Digest(), Gaggle: "web",
    Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "issue-8"},
}, inputs, journal.WithScrubber(scrub))

run.SetMachineState("implement")
run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
ref, _ := run.RecordArtifact("plan.txt", planBytes)       // content-addressed, scrubbed
run.RecordSpan("implement", "copilot-cli.transcript", transcriptBytes) // spans/, scrubbed
run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
run.Close()

// read-only:
rd, _  := journal.OpenRead(runDir)
id, _  := rd.Identity()
evs, _ := rd.Events()
data, _ := rd.ArtifactBytes(ref)                          // digest-verified on read

// after a crash:
run, report, _ := journal.Recover(runDir, journal.WithScrubber(scrub))
```

`journal.Ref` is the on-disk production form of the stage contract's wire
`api/v1alpha1.ArtifactPointer` (#10) — same fields — so the runner maps
journal→wire 1:1.
