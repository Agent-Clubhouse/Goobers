# Export to multiple named destinations

`telemetry.exporters` sends the instance's existing telemetry signals to up to
16 operator-owned destinations. Each destination has a unique, stable name.
Local journals are written once, regardless of the number of remote destinations.
No destination is configured by default.

```yaml
telemetry:
  collectionProfile: standard
  exporters:
    - name: operations
      kind: otlp-grpc
      endpoint: https://collector.example.com:4317
      headers:
        authorization:
          env: OPERATIONS_OTLP_AUTH
      tls:
        caFile: /etc/goobers/collector-ca.pem
    - name: tenant
      kind: azuremonitor
      connection:
        file: /etc/goobers/application-insights.txt
      replay:
        maxAge: 72h
        maxBytes: 536870912
```

Names must match `[a-z][a-z0-9-]{0,63}`. Duplicate names, unknown kinds,
missing required settings, incompatible transport fields, inline credentials,
and undeclared secret stores fail configuration validation before startup.
Credentials use the existing `TokenRef` forms: `env`, `file`, `keychain`, or
`store`. OTLP authenticates through `headers`; Azure Monitor resolves its
connection string through `connection`. Secret values are registered with the
shared scrubber and never rendered in configuration, status, or local journals.

## Signals and durability

| Kind | Endpoint and authentication | Signals | Buffering |
| --- | --- | --- | --- |
| `otlp-grpc` | Explicit `endpoint`; optional `headers` containing TokenRefs; per-destination `tls` | Run/stage/scheduler traces, native metrics, and committed journal logs by default. `journalLogs: false` disables only that destination's journal logs. | Independent bounded memory queues; no durable disk replay. |
| `azuremonitor` | `connection` TokenRef selects the Application Insights resource and ingestion endpoint | Existing `collectionProfile`: `health` exports whitelisted diagnostics; `journal` adds committed journals; `standard` adds traces; `diagnostic` additionally permits host/account identity. | Independent bounded disk replay for traces, journals, and diagnostics, enabled by default. |

A named OTLP destination never enables diagnostic export. The separately
configured `telemetry.diagnostics.otlp` block remains the explicit diagnostic
OTLP destination and keeps its own endpoint and credentials. It can run alongside
named destinations. Azure Monitor keeps its existing profile-controlled diagnostic
routing. Local diagnostic history is unaffected.

OTLP/HTTP and durable OTLP replay are not supported by this list. Both are tracked
in [#6502](https://github.com/Agent-Clubhouse/Goobers/issues/6502). Unsupported kinds
are rejected; the daemon never applies only the supported portion of a list.

Each destination owns its queue, transport, TLS configuration, health observations,
and replay cursor. A slow or rejecting destination does not prevent healthy
exports or local journal writes. Remote failures remain best-effort. An unreadable
TLS file or unavailable credential degrades that destination and leaves the local
client and other destinations usable. Restore the file or credential and restart
the daemon to rebuild the destination. TLS certificate and trust-file changes are
read on restart, independently for each destination.

## Spools, restart identity, and migration

Named Azure spools live below
`telemetry-export/destinations/exporter-<name>/` in the instance root. Each name
has its own age and byte budget, shared across its three signal streams. Reordering
the list or restarting the process preserves that name's spool and record IDs.
Renaming a destination starts a different spool. Removing a destination stops its
exports without deleting retained files. Restoring the same name resumes replay
within its configured age and byte bounds. Keep the instance root on persistent
storage when replay must survive replacement of a container or machine.

Existing `telemetry.otlp` and `telemetry.azureMonitor` configurations retain their
behavior when `exporters` is absent or empty. A nonempty list replaces those two
blocks; combining the shapes is rejected to avoid duplicate delivery. The legacy
`GOOBERS_OTLP_ENDPOINT` and `GOOBERS_OTLP_INSECURE` overrides do not select or modify
a named destination. Remove those overrides when migrating; a resulting legacy
route alongside a named list is rejected.

Drain pending legacy Azure replay before migration. Copy its connection-string
reference to a named destination's `connection` field, and copy its replay limits.
The legacy spool is retained in its original location and is not automatically
moved or replayed into a differently named destination. Keep names tied to the same
logical backend: retained records are sent using that name's configured connection
on the next start.

`goobers telemetry configure` continues to manage the legacy single Azure block.
It refuses a named configuration; edit the list in `instance.yaml` instead.

## Check delivery

Use the Azure connectivity probe for a selected named destination:

```sh
goobers telemetry test --destination tenant ./instance
```

The probe sends one identity-free record directly, bypassing replay. For both
kinds, daemon status exposes independent entries under
`telemetryExporterHealth.destinations`, including trace/metric state, journal and
diagnostic queue counters, and Azure pending-record and delivery evidence. Text
status prints one line per named destination. Successful local queue admission
is separate from durable replay delivery: inspect `replay.lastSuccess`,
`replay.lastFailure`, `replay.failureClass`, and `replay.activeFailure` for Azure
acknowledgements across restarts. Configuration errors expose only a bounded
`unavailableReason`; credentials and spool paths are omitted.

See [Azure Monitor setup and KQL examples](azure-monitor.md) for connection-string
setup, collection profiles, and queries. For OTLP collectors, verify the explicit
receiver supports every enabled signal and inspect the destination's metric health
when a collector accepts traces but rejects metrics.
