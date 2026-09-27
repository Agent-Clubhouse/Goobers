# Export tenant telemetry directly to Azure Monitor

Goobers can send its OpenTelemetry run/stage traces, committed journal events,
and whitelisted service/fleet diagnostics directly to a customer-owned
Application Insights resource. No OpenTelemetry Collector is required for this
path. Export is disabled unless the instance explicitly configures a
connection-string secret reference; Goobers has no built-in or maintainer-owned
telemetry destination.

The tenant telemetry program tracked by #5909 still adds configurable collection
profiles, durable replay, and one-step Windows/Kubernetes onboarding without
changing the signal schemas introduced here.

## Configure the destination

Copy the connection string from the Application Insights resource's Overview
page. Store it outside `instance.yaml`, then reference it from the instance:

```yaml
telemetry:
  enabled: true
  azureMonitor:
    connectionString:
      env: APPLICATIONINSIGHTS_CONNECTION_STRING
```

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
Logs while Azure Monitor receives the same whitelisted records.

## Delivery and trust

Delivery is asynchronous and bounded by the existing trace, journal, and
diagnostic queues. A slow or unavailable Azure endpoint cannot block a workflow
stage or journal commit; local journals and diagnostic history remain
authoritative. This path is live-forward only and does not replay records after
an outage. Each background export checks Azure's HTTP ingestion result; rejected
or timed-out journal and diagnostic batches increment their existing failure/loss
counters. Durable bounded replay is tracked by #5910.

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
         instanceId=coalesce(
             tostring(customDimensions["goobers.instance.id"]),
             tostring(customDimensions["instanceId"])),
         runId=tostring(customDimensions["goobers.run.id"]),
         gaggle=tostring(customDimensions["goobers.gaggle"])
| project timestamp, stream, instanceId, gaggle, runId, message, customDimensions
| order by timestamp asc
```
