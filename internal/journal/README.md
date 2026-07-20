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

## Forward compatibility (seeds #33)

A reader tolerates events written by a newer schema version: unknown fields and
unknown event types parse into the shared envelope without error.
`Event.KnownSchema()` reports whether the current build owns the event's schema
version, so a reader can decide whether to trust type-specific fields. Minimal
policy for V0: **read leniently, write strictly** (writers always emit the
current version; the schema validates that exact shape).

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
