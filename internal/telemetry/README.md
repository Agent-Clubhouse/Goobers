# Telemetry helpers

`internal/telemetry` is the shared helper API for Goobers run tracing. The
semantic model is fixed for v1:

- `StartRun` opens the root span for a workflow run and uses `RunID` as the OTel
  trace id when it is a valid 32-character trace id.
- `StartTask`, `StartGate`, and `StartSchedulerSpan` create spans for task
  attempts, gate evaluations, and scheduler decisions. Their attributes come
  from the canonical registry in `attributes.go` (`goobers.run.id`,
  `goobers.workflow`, `goobers.stage`, `goobers.attempt.n`, and related keys).
  Agentic task and reviewer spans also carry `goobers.model` and
  `goobers.harness.version`; the SQLite rollup indexes both in
  `agent_invocations`, and `goobers telemetry stats` filters/groups on them.
- `NewMemoryExporter` is for unit tests. `ExporterStdout` is the local default.
- Managed worktree lifecycle measurements are emitted as
  `goobers.worktree.disk.usage` and `goobers.workcopy.disk.usage` span events.
  Measurement gaps emit `goobers.workcopy.disk.measurement_failed`.
  `ExporterOTLP` sends spans to an OTLP collector.
- `JournalSpanExporter` appends run spans to both the legacy
  `spans/spans.jsonl` projection and `spans/otlp.jsonl`.
  `NewPerGaggleJournalSpanExporter` routes scheduler spans to the
  instance-level `scheduler/spans/spans.jsonl` instead of creating run
  directories. The OTLP file uses
  `application/x-ndjson` framing; each line is the OTLP/JSON encoding of
  one `opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest`.
  `goobers telemetry export --since ...` re-emits an inclusive/exclusive
  span-start-time window from these files.

The workflow engine should call `StartRun` once per workflow run, then use the
returned context for task-attempt and gate-evaluation spans. Within-stage
happenings are span events rather than peer stage spans.

## Load/scale harness for the read path

`test/scale` is the committed load/scale harness (issue #1416, epic #1410). It
synthesizes a Goobers instance at a parameterizable scale — N run directories
written through the production `internal/journal` API plus a large
`scheduler/events.jsonl` — and benchmarks the telemetry read/ingest/reconcile
paths (`rollup.Rebuild`, indexed `readservice.ListRuns`, the Overview fan-out,
and the full status scan) so we can prove the dashboard stays responsive at
10–100× the current dogfood instance (~13.6k runs behind a ~290 MB scheduler
journal) and guard against regressions.

Scale is a multiplier over that baseline. A quick local smoke, then the target
scales:

```sh
go run ./test/scale -scale=0.01 -measure          # a few hundred runs, scratch dir
go run ./test/scale -scale=1  -out=/tmp/1x  -measure
go run ./test/scale -scale=10 -out=/tmp/10x -measure
```

To generate 100k+ runs, drive the counts directly (each run is a real journal
directory with per-append fsync, so generation is I/O-bound — minutes to tens of
minutes, and gigabytes of disk):

```sh
go run ./test/scale -runs=100000 -scheduler-events=3000000 -out=/data/100k -measure
```

The default `go test ./test/scale/...` runs a fast, merge-safe correctness check
(generate → rebuild → bounded/correct `ListRuns`, with injected orphan dirs and
oversized records proving resilience) and asserts no wall-clock threshold. The
target-scale latency measurement is opt-in: set `GOOBERS_SCALE_LARGE=<mult>`
(e.g. `1`, `10`, `100`) to run `TestMeasureLargeScale`. See the `test/scale`
package doc for the full flag reference.

## OTLP collector to ADX

Production instances send OTLP traces to an OpenTelemetry Collector running in
the cluster. The collector uses the contrib `azuredataexplorer` exporter to
write the goober-run store provisioned by `infra/bicep/modules/adx.bicep`.

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317

processors:
  batch: {}

exporters:
  azuredataexplorer:
    cluster_uri: "${env:GOOBERS_ADX_CLUSTER_URI}"
    db_name: "gooberrun"
    managed_identity_id: "system"
    traces_table_name: "OTELTraces"
    ingestion_type: "queued"

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [azuredataexplorer]
```

The ADX exporter expects the target database and tables to exist before ingest.
Use the provisioned ADX database output (`gooberrun` by default) rather than any
project telemetry database. For v1, partition queries by
`TraceAttributes.goobers.gaggle` to preserve gaggle isolation.
