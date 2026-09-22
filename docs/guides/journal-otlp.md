# Export committed journals through OTLP Logs

Journal files remain authoritative. Native OTLP export sends a best-effort
live copy of new run and scheduler events, not a durable outbox. A stopped
collector, full queue, process crash, or shutdown deadline can lose exported
records. Recovery and compaction do not replay historical records. Keep the
files for recovery; this feature does not provide automatic catch-up or
exactly-once delivery.

## Configuration

Use a collector accepting OTLP/gRPC Logs on the existing native telemetry
endpoint:

```yaml
telemetry:
  enabled: true
  otlp:
    endpoint: http://127.0.0.1:4317
    insecure: true
    journalLogs: true
```

`journalLogs` defaults to true when native OTLP export is enabled. Set it to
false to retain trace/metric export without journal Logs, including when the
collector only supports traces. `telemetry.enabled: false`, explicit
`telemetry.otlp.enabled: false`, or no configured endpoint prevents journal
export. Files retain their existing behavior in every case.

Logs reuse the native endpoint, headers, resource attributes, and TLS settings.
The existing `GOOBERS_OTLP_ENDPOINT` and `GOOBERS_OTLP_INSECURE` overrides still
apply. No skill MCP server, model credential, or separate log receiver is
required. An unauthenticated loopback collector needs no header credentials.
Remote collectors require TLS; see [native OTLP configuration](jaeger-quickstart.md).

`goobers version --json` includes `"journal-otlp-v1"` in `capabilities` on
binaries that support this protocol. Check this capability rather than
assuming a release number supports journal export. The existing
`ExportJournalOTLP` trace replay helper exports `spans/otlp.jsonl`, not these
full journal events.

## Wire contract, version 1

Each newly committed journal event produces at most one offered LogRecord.
The instrumentation scope is `goobers.journal`, version `"1"`. `Body` is an
OTLP string containing the exact scrubbed UTF-8 JSON written to the event file,
without its terminating newline. It is not parsed and rebuilt through
floating-point values, so large integer values survive unchanged.

| Log attribute | OTLP type | Meaning |
|---|---|---|
| `goobers.journal.schema_version` | int | `1`, the export contract version, not a replacement for the body's `schema` |
| `goobers.journal.kind` | string | `run` or `scheduler` |
| `goobers.journal.id` | string | Run ID, or the persisted scheduler journal identity |
| `goobers.journal.seq` | string | Unsigned decimal sequence, equal to the JSON body's `seq` |
| `goobers.run.id` | string | Present for run journals, equal to `goobers.journal.id` |
| `goobers.instance.id` | string | Known durable instance identity; omitted when unknown |
| `goobers.gaggle` | string | Known run gaggle; omitted when unknown |

The body retains `schema: "goobers.dev/journal/event/v1"` and all event fields.
`TimeUnixNano` comes from the committed event's `time`; `ObservedTimeUnixNano`
records commit observation. A valid run ID can supply the trace ID. No span ID
is invented. Scheduler event bodies can refer to a run without turning the
scheduler journal into that run's journal.

The scheduler's `.instance-journal-id` already exists independently of export.
Compaction preserves it while advancing the physical event-file generation.
No directory or physical file path is added as export identity. Legacy
journals with unknown instance or gaggle identity remain valid.

Collectors should retain the exact body and OTLP resource, scope, and log
metadata separately. One accepted journal LogRecord is one journal record,
not one Export request. The resource continues to identify the producer
process; `service.instance.id` is not the durable journal instance identity.

## Commit boundary and lifecycle

The journal offers a copied, scrubbed event only after its file write and
fsync succeed. Failed serialization, file writes, and fsyncs do not emit.
A later failure to update the derived `state.json` does not undo the committed
file record. Initial `run.started` export waits until the staged run directory
has been published and reopened successfully.

The shared journal hook covers run creation, recovery, ordinary and compound
appends, scheduler appends, and newly written repair events. Reads, recovery
of existing records, and compaction alone do not emit. Live-journal writes use
the same commit hook, so an adopted run handle is not exported twice.
Engine history reconstruction explicitly opts out of live export, including
its staged run files and projected scheduler events. Later live appends to
those journals still export normally. This exclusion uses a constructor
option, not event timestamps or directory-name guesses.

A process registers one sink per canonical instance root. Overlapping
registrations are rejected. Unregistering stops offers from existing handles;
it cannot redirect those handles to a later owner's sink. Export only offers
to a bounded queue while a journal lock is held. Network requests and failure
reporting run outside that lock. Export failure cannot fail a journal write.

Queue rejection and export failures are counted and reported by the exporter.
The queue accepts at most 1,024 records and 8 MiB of queued data, with a
1 MiB per-record limit. Oversized records, queue overflow, and enqueue
contention are dropped rather than blocking a file append. Missing journal
identity also drops export instead of inventing an identity. The complete
record remains in the journal; the exporter does not truncate its body.

Logs SDK retries are disabled. An unavailable endpoint, deadline, or explicit
collector rejection is reported without blindly resending a possibly accepted
record. This is not an exactly-once guarantee, and a collector's queue
acceptance is not proof of final downstream storage.

Flush and shutdown use deadlines; shutdown stops intake before draining.
These counters describe this process's live export, not durable delivery
receipts from a collector or downstream storage.
`JournalExportStats.SinkPanics` reports contained panics across all registered
journal sinks in the process, not just one telemetry client. It is separate
from queue drops. Short-lived journal commands warn about a nonzero count
when releasing their telemetry client; durable journal writes still succeed.

## CI coverage

The required Windows gate runs the journal export, configuration, schema,
recovery, lifecycle, and direct CLI regression tests in its `Windows journal
OTLP export` step. It uses a named selection rather than the full CLI suite.
Workflow contract tests compare that selection with the journal regression
files, so adding a test without selecting it fails the gate.
The selection also covers `run --no-wait`: both in-process dispatch and the
detached worker must finish admitted work before closing journal telemetry.

The required Linux unit shards run these packages with the race detector.
Contract tests verify both the race-enabled workflow and package membership
in the shards. Local Windows tests do not substitute for executing those
Linux jobs; hosted execution requires publishing the changes through the
normal authorized Git workflow.
