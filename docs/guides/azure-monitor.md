# Export tenant telemetry directly to Azure Monitor

Goobers can send its OpenTelemetry run/stage traces, committed journal events,
and whitelisted service/fleet diagnostics directly to a customer-owned
Application Insights resource. No OpenTelemetry Collector is required for this
path. Export is disabled unless the instance explicitly configures a
connection-string secret reference; Goobers has no built-in or maintainer-owned
telemetry destination.

Direct export includes bounded on-disk replay. Windows, macOS, Linux, and
Kubernetes use the same configuration; the Kubernetes instance root must be on
a persistent volume if pending records must survive pod replacement.

## Configure the destination

Copy the connection string from the Application Insights resource's Overview
page. Store it outside `instance.yaml`, then reference it from the instance:

```yaml
telemetry:
  enabled: true
  collectionProfile: standard
  azureMonitor:
    connectionString:
      env: APPLICATIONINSIGHTS_CONNECTION_STRING
```

That connection-string reference is the only required destination setting.
Replay is enabled automatically with a 72-hour age limit and a 512 MiB
per-instance limit. An operator can change the bounds, or deliberately disable
replay, without changing the destination:

```yaml
telemetry:
  azureMonitor:
    connectionString:
      env: APPLICATIONINSIGHTS_CONNECTION_STRING
    replay:
      maxAge: 168h
      maxBytes: 1073741824
```

`maxAge` accepts 1h through 720h; `maxBytes` accepts 1 MiB through 10 GiB.
When either bound is reached, the oldest batches are removed first and the
pruned-record counters make that loss visible. Set `replay.enabled: false` only
when live-forward-only delivery is intentional.

`collectionProfile` is optional and defaults to `standard`. Its v1 contract is:

| Profile | Service/fleet health | Committed journals | Run/stage traces | Machine/account identity |
| --- | --- | --- | --- | --- |
| `health` | yes | no | no | no |
| `journal` | yes | yes | no | no |
| `standard` | yes | yes | yes | no |
| `diagnostic` | yes | yes | yes | yes |

Use `standard` for routine fleet visibility and execution reconstruction. Use
`diagnostic` only when the tenant explicitly consents to exporting hostname and
runtime-account fields. Unknown profile names fail configuration validation;
changing a profile never deletes or reduces the authoritative local journal,
rollup, or diagnostic history. The generated machine-readable contract is
[`telemetry-collection-profiles-v1.json`](../reference/telemetry-collection-profiles-v1.json).
Exported resources carry the effective choice as
`goobers.telemetry.profile`, so queries and audits do not have to infer it.

Records remain structured JSON. Goobers does not base64-wrap workflow bodies,
prompts, credentials, or arbitrary raw payloads as a telemetry escape hatch.

Set the environment variable in the account/environment of the Goobers daemon
and restart the service. A private file or declared secret store is also valid:

```yaml
telemetry:
  azureMonitor:
    connectionString:
      file: C:\ProgramData\Goobers\secrets\application-insights.txt
```

```yaml
telemetry:
  azureMonitor:
    connectionString:
      store: tenant-key-vault/goobers-application-insights
```

The resolved string is registered with the journal scrubber before the
telemetry client starts. It is never written back into config, logs, journals,
or span attributes. Invalid or missing secret references and malformed
connection strings fail configuration before normal daemon work begins.

`azureMonitor` can run alongside `telemetry.otlp`; traces and committed journal
Logs fan out to each explicitly configured destination. A separately configured
`telemetry.diagnostics.otlp` destination also continues to receive diagnostic
Logs while Azure Monitor receives the same whitelisted records. Profile routing
does not enable, redirect, or replace either explicit OTLP destination.

## Delivery and trust

Delivery is asynchronous and bounded by the existing trace, journal, and
diagnostic queues. A slow or unavailable Azure endpoint cannot block a workflow
stage or journal commit. The export workers first write already-scrubbed,
export-ready batches beneath
`<instance-root>/telemetry-export/azure-monitor/{traces,journal,diagnostics}` and
then send them. Failed batches retry with bounded backoff and replay on the next
process start without waiting for a new workflow. Local journals and diagnostic
history remain authoritative.

Delivery is **at least once**: a process can stop after Azure accepts a batch
but before its local acknowledgement is removed. Every envelope therefore has
a stable `goobers.telemetry.record_id` custom dimension that survives retries.
Queries which count unique events should deduplicate on that value. Service and
fleet health records expose pending record/byte counts, oldest pending age,
retry attempts, age/byte pruning, and malformed-file losses. Spool files are
private, atomically published, bounded, and contain the same scrubbed envelopes
sent to Azure—not connection strings or an unsanitized copy of the journal.

The directory is relative to the configured instance root on every operating
system. For example, an instance at `C:\goobers` uses
`C:\goobers\telemetry-export\azure-monitor`; an instance at
`/var/lib/goobers` uses `/var/lib/goobers/telemetry-export/azure-monitor`.
On Kubernetes, mount the whole instance root (or at minimum this directory) on
a persistent volume. An `emptyDir` preserves retries across a container restart
in the same pod but loses them when the pod is replaced. Budget disk capacity
per replica from `replay.maxBytes`; 150 instances at the default cap have a
worst-case configured ceiling of 75 GiB, before filesystem overhead.

An Application Insights connection string identifies the destination. When
the resource permits local authentication, ingestion uses the instrumentation
key in that string. The key is not a bearer credential or strong proof of a
tenant identity, even though it should still be handled as a secret. Resources
that disable local authentication require an Entra-authenticated ingestion
path; that is separate from this connection-string exporter.

## Confirm data in KQL

After a workflow completes, this query should show its run and stage spans in
the Application Insights `dependencies` table:

```kusto
dependencies
| where timestamp > ago(30m)
| where cloud_RoleName == "goobers"
| project timestamp, operation_Id, id, name, success,
          instanceId=tostring(customDimensions["goobers.instance.id"]),
          gaggle=tostring(customDimensions["goobers.gaggle"]),
          workflow=tostring(customDimensions["goobers.workflow"]),
          runId=tostring(customDimensions["goobers.run.id"]),
          outcome=tostring(customDimensions["goobers.outcome"])
| order by timestamp asc
```

The Application Insights operation ID is the OpenTelemetry trace ID, and the
dependency ID is the span ID, so parent/child execution can be reconstructed
with standard transaction-search tooling or KQL.

Committed journals and diagnostics appear in the `traces` table. Journal
messages retain the scrubbed JSON committed locally up to Application Insights'
32 KiB message limit. Larger messages carry
`goobers.azure_monitor.truncated=true`; the complete record remains in the local
journal (and in native OTLP Logs when configured). Diagnostic messages use their
stable event name. This query joins both streams by durable instance and run
identity:

```kusto
traces
| where timestamp > ago(30m)
| where cloud_RoleName == "goobers"
| extend stream=tostring(customDimensions["goobers.telemetry.stream"]),
         recordId=tostring(customDimensions["goobers.telemetry.record_id"]),
         instanceId=coalesce(
             tostring(customDimensions["goobers.instance.id"]),
             tostring(customDimensions["instanceId"])),
         runId=tostring(customDimensions["goobers.run.id"]),
         gaggle=tostring(customDimensions["goobers.gaggle"])
| where isnotempty(recordId)
| summarize arg_max(timestamp, *) by recordId
| project timestamp, recordId, stream, instanceId, gaggle, runId, message, customDimensions
| order by timestamp asc
```

Use the same `recordId` projection and `summarize arg_max(timestamp, *) by
recordId` pattern for `dependencies` when a query must count logical spans
rather than ingestion attempts.
