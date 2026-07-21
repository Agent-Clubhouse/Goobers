# Telemetry helpers

`internal/telemetry` is the shared helper API for Goobers run tracing. The
semantic model is fixed for v1:

- `StartRun` opens the root span for a workflow run and uses `RunID` as the OTel
  trace id when it is a valid 32-character trace id.
- `StartTask`, `StartGate`, and `StartSchedulerSpan` create spans for task
  attempts, gate evaluations, and scheduler decisions. Their attributes come
  from the canonical registry in `attributes.go` (`goobers.run.id`,
  `goobers.workflow`, `goobers.stage`, `goobers.attempt.n`, and related keys).
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
