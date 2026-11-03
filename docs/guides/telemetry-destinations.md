# Export to multiple named destinations

`telemetry.exporters` sends the instance's existing telemetry signals to up to
16 operator-owned destinations. Each destination has a unique, stable name.
Local journals are written once, regardless of the number of remote destinations.
No destination is configured by default.

## Separate opt-in OneCollector uploader

The compiled Go binary also supports an additive OneCollector HTTP uploader.
It has no built-in endpoint, routing key, event catalog, or organization schema.
An operator supplies a protected JSON profile and enables its declared environment
variable with the exact value `1`. A missing profile or missing opt-in sends
nothing. This uploader does not change `telemetry.enabled`, legacy destinations,
named exporters, or the release/signing pipeline.

Check support and install only the profile reference:

```text
goobers telemetry onecollector --capabilities --json
goobers telemetry configure --onecollector-profile-file <absolute-profile-path> --json <instance>
```

The second command validates without resolving credentials or sending data.
It preserves public telemetry settings, reports `changed: false` for an existing
identical reference, and refuses to replace a different profile. Restart the
daemon after changing the profile or its reference. Restrict profile and key-file
access to the account running Goobers.

This fabricated profile demonstrates journal export:

```json
{
  "profile_version": 1,
  "enable_env": "SYNTHETIC_UPLOAD",
  "endpoint": "https://collector.example.invalid/ingest",
  "routing_key": {"env": "SYNTHETIC_ROUTING_KEY"},
  "instrumentation_key": {"env": "SYNTHETIC_INSTRUMENTATION_KEY"},
  "mappings": [{
    "signal": "journal",
    "event_name": "Synthetic.Journal",
    "envelope_version": "4.0",
    "fields": {
      "body": {"source": "journal.raw_json"},
      "format": {"constant": "\"synthetic-v1\""}
    }
  }]
}
```

Keys use indirect `env`, `file`, `keychain`, or `store` references, never inline
credentials. Instance-backed startup resolves declared stores; standalone explicit
uploads use environment, file, or keychain references. Credential resolution
registers both keys with the configured secret scrubber. Diagnostics never
include destination URLs, credentials, response bodies, or input values.

Mappings select `journal`, `trace`, `metric`, or explicit `input` signals.
`match` compares every selector against its allowed strings. `fields` and
`extensions` project source facts or serialized JSON constants. Missing optional
facts are omitted. `data_source`, `event_name_source`, `timestamp_source`, and
`envelope_version_source` support validated native inputs; `timestamp_format:
"unix_nano"` converts unsigned nanoseconds to UTC. `unix_nano_millis` and
`rfc3339_millis` produce UTC timestamps with exactly three fractional digits,
truncating smaller units. `rfc3339` preserves the supplied string.
Metric mappings must explicitly
select `metric.name`, `metric.unit`, and `metric.kind`, so the operator owns the
catalog and units.

Journal `journal.raw_json` retains exact committed text, including numeric
spellings; it is not scrubbed again after the journal writer's boundary.
`trace.raw_json` uses the client's effective scrubber before conversion.
Metric scalar integers remain exact. Histogram replay keeps bounded compressed
segments, validates count/buckets/extrema/sum, then streams one synthetic row per
observation. `metric.point_id` identifies the original point and decimal-string
`metric.sample_index` distinguishes replay rows. `metric.integer_value` truncates
toward zero only within the signed 64-bit range. `nonnegative` is a per-mapping
policy, not an instrument-name rule.

Attribute `projections` select `attributes`, `resource`, or `scope`, a `prefix`,
`key_encoding: "hex"`, optional string-only `aliases`, and optional `type_prefix`.
Complex attribute values retain serialized OTLP wrappers; companion type keys
use the final projected key. Other facts include `metric.value`, `metric.unit`,
`metric.kind`, `metric.temporality`, `metric.monotonic` for sums, decimal-string
`metric.start_time_unix_nano`/`metric.time_unix_nano`, and resource/scope metadata.

The in-process queue is limited to 1,024 records and 8 MiB, including in-flight
reservations. Native trace records are limited to 2 MiB; an oversized span does
not discard neighboring valid spans. Requests contain at most 1,000 records and
8 MiB. Export has a 10-second budget, never follows redirects, and never retries
uncertain delivery. Queue drops and failures produce bounded standard diagnostics.
There is no disk replay; local journals remain authoritative.

### Explicit validation, upload, and MCP

Profiles may also declare `inputs` and `tools`. Each input owns its JSON Schema,
optional recursive camel-key normalization or collision checking, missing-only
defaults, blank-string removal, batch `items_path`, record schema, bounded CEL
rules, and object fields to serialize once. Schema format/content checks are
enabled; external schema references are forbidden. Strict JSON decoding rejects
duplicate keys, trailing input, invalid UTF-8, and nesting beyond 32 levels.
Input is capped at 16 MiB and embedded native JSON at 2 MiB.

```text
goobers telemetry onecollector upload --profile-file <profile> --input <name> --validate-only
goobers telemetry onecollector upload --profile-file <profile> --input <name>
goobers telemetry onecollector mcp --profile-file <profile>
```

Upload reads one JSON value from stdin. Validation performs the full preparation
and serialization without reading the opt-in variable, resolving keys, or sending
data. Receipts report `validated`, `uploaded`, `disabled`, `validation`, `internal`,
`network`, `timeout`, `cancelled`, or `http`; successful HTTP receipts retain the
actual response status. Disabled upload exits successfully without sending data.
Its receipt still reports `success: false`, and MCP reports `isError: true`.
Explicit inputs are deliberately not scrubbed: validated caller content, including
embedded JSON strings and exact numeric tokens, is preserved. Do not submit secrets
that should not reach the configured destination.

CEL rules see `input` and exact JSON `tokens` indexed by dotted paths. Helpers
include `jsonParse(text[, collisionCheck])`, `jsonToken(text, path)`,
`jsonStringify(value)`, `jsonDepth(value)`, `jsonObjects(value)`,
`utf16Length(text)`, `signedDecimal(text)`, `unsignedDecimal(text)`,
`canonicalBase64(text)`, and `validTimestamp(text)`. Signed/unsigned integers
remain distinct exact CEL values; decimal adaptation is only for validation,
never publication. Rules have expression-size and execution-cost limits.
Native mappings can select decoded fields through
`input_json.<record-field>.<path>` without changing the original string.

MCP reuses Goobers' bounded newline-delimited stdio transport. Only the profile's
tool names, descriptions, argument schemas, selected input, optional payload path,
and Boolean validation switch are exposed. Discovery does not resolve upload
credentials. Validation and upload results use standard text content and
`isError`; malformed tool-call parameters remain JSON-RPC errors.

## Cloud analytics scope

`goobers-cloud` deliberately does not host the full local `telemetry.db`
analytics surface. It retains only first-success milestones and the four
defect-nomination families `stage-failure-rate`, `gate-noise`,
`credit-assignment`, and `error-signature`. Other telemetry is exported only
through OTLP to an operator-configured collector; Goobers does not install or
select a collector and does not phone home by default.

Local rollup families such as general stats and trends, costs and usage,
CI-check analytics, learning episodes, coverage gaps, efficacy, labels, shadow
comparisons, and compact/prune operations are unavailable in cloud mode. They
fail closed rather than returning an empty result that could be mistaken for
“no defect found.”

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
| `azuremonitor` | `connection` TokenRef selects the Application Insights resource and ingestion endpoint | Existing `collectionProfile`: `health` exports whitelisted diagnostics; `journal` adds committed journals; `standard` adds traces; `diagnostic` additionally permits host/account identity, including `instanceRoot` and home-directory paths that other profiles withhold ([details](azure-monitor.md#account-paths)). | Independent bounded disk replay for traces, journals, and diagnostics, enabled by default. |

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
