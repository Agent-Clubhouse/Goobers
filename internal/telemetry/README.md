# Telemetry helpers

`internal/telemetry` is the shared helper API for Goobers run tracing. The
semantic model is fixed for v1:

- `StartRun` opens the root span for a workflow run and uses `RunID` as the OTel
  trace id when it is a valid 32-character trace id.
- `StartTask`, `StartGate`, and `StartSchedulerSpan` create spans under the run
  context and attach stable Goobers attributes (`gaggle`, `workflowId`, `runId`,
  `taskId`, `gate.decision`, and related fields).
- `NewMemoryExporter` is for unit tests. `ExporterStdout` is the local default.
  `ExporterOTLP` sends spans to an OTLP collector.

The workflow engine should call `StartRun` once per workflow run, then use the
returned context for task/gate spans. Runtime code should wrap harness/evaluator
execution with `StartTask`/`StartGate` using the canonical invocation envelope
fields.

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
`TraceAttributes.gaggle` to preserve gaggle isolation.
