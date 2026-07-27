# External telemetry connectors

External telemetry connectors let deterministic workflow stages query a target
product's operational telemetry without exposing provider credentials to the
stage process or embedding provider SDK behavior in workflow YAML. This is
separate from Goobers' own run telemetry in `telemetry.db`.

The connector API is `telemetryconnector/v1alpha1`; normalized artifacts use
`goobers.dev/external-telemetry-query-result/v1alpha1`. The built-in adapters are
ADX/KQL over REST (`adx-kql-rest@v1`) and a hermetic fixture adapter (`fake@v1`).

## Configure a named ADX connector

Connector definitions live in `instance.yaml`, not in a workflow. Provider
fields stay under `config`, authentication contains references or identity
selection only, and host policy bounds every invocation:

```yaml
externalTelemetry:
  connectors:
    - name: production-metrics
      kind: adx-kql-rest
      version: v1
      auth:
        mode: workload-identity
      policy:
        timeout: 30s
        maxAttempts: 3
        retryBackoff: 1s
        maxRows: 1000
        maxBytes: 1048576
      network:
        allowedHosts:
          - acme-metrics.westus.kusto.windows.net
      config:
        cluster: https://acme-metrics.westus.kusto.windows.net
        database: production
        columnUnits:
          latency_ms: ms
          availability: percent
        watermarkColumn: dataAsOf
```

`name`, `kind`, `version`, and `config` are required. Connector names match
`[a-z][a-z0-9-]{0,62}`. `timeout`, `maxAttempts`, `retryBackoff`, `maxRows`, and
`maxBytes` are host maxima; a workflow may only tighten them. A workflow may
lengthen, but not shorten, `retryBackoff` to avoid increasing source load.
Retries use fixed backoff with no jitter, and the result artifact records
attempts, truncation, rows dropped, sampling, and effective row/byte bounds.

`network.allowedHosts` accepts hostnames only. ADX must use HTTPS. Plain HTTP is
available only when `allowHTTP: true` and every allowed host is loopback, for
local fixtures. The host-supplied HTTP client checks every request and redirect.

### Authentication

ADX supports these reference-only modes:

| Mode | Configuration | Intended use |
|---|---|---|
| `azure-cli` | Optional `tenant` | Local development with `az login` |
| `workload-identity` | Standard `AZURE_*` workload identity environment | Federated workload identity |
| `managed-identity` | Optional `clientId` | Azure system/user-assigned managed identity |
| `default-azure` | Optional `tenant` | Azure SDK default credential chain |
| `bearer-token` | Exactly one of `token.env` or `token.file` | External token provisioning |
| `none` | No credential | Loopback HTTP fixtures only |

For an env-backed token:

```yaml
auth:
  mode: bearer-token
  token:
    env: ADX_QUERY_TOKEN
```

For a file-backed token:

```yaml
auth:
  mode: bearer-token
  token:
    file: /run/secrets/adx-query-token
```

Inline values are unknown fields and fail strict instance decoding. Token files
must satisfy Goobers' private-file checks. Resolved bearer and Azure tokens are
registered with the run journal scrubber and never appear in connector
artifacts, query provenance, or error bodies.

## Author a query stage

The generic deterministic stage selects only a connector name and
provider-neutral query fields:

```yaml
- name: query-service-health
  type: deterministic
  goal: Read current service health from operational telemetry.
  run:
    # Placeholder: kind=external-telemetry dispatches in-process.
    command: ["goobers", "external-telemetry"]
  inputs:
    kind: external-telemetry
    connector: production-metrics
    queryRef: queries/service-health.kql
    parameters: '{"environment":"production"}'
    window: 15m
    freshness: 5m
    shape: table
    expectedColumns: '[{"name":"service","type":"string"},{"name":"availability","type":"number","unit":"percent"},{"name":"dataAsOf","type":"datetime"}]'
    queryTimeout: 20s
    queryMaxAttempts: "2"
    queryRetryBackoff: 1s
    maxRows: "100"
    maxBytes: "65536"
  capabilities:
    - telemetry:read
  expectedOutputs:
    - dataState
    - queryDigest
  next: interpret-health
```

Exactly one of `query` or `queryRef` is required. `queryRef` is resolved inside
the stage workspace with the same path and symlink containment checks used for
other declared stage files; it is limited to 1 MiB. `parameters` is a JSON
object. `shape` is `point`, `table` (default), or `time-series`; the host
rejects a shape that the selected connector did not declare before executing
the query.
`expectedColumns` is an exact ordered list of normalized names, types, and
optional units. A mismatch is a failed query, never an empty result.

`window` is a relative duration ending when the query starts. Alternatively,
set RFC 3339 `windowStart` and/or `windowEnd`. ADX injects declared endpoints as
`_goobers_window_start` and `_goobers_window_end` query parameters. They are
reserved names. A checked-in KQL query can consume them directly:

```kusto
declare query_parameters(
  _goobers_window_start:datetime,
  _goobers_window_end:datetime,
  environment:string
);
ServiceHealth
| where Timestamp between (_goobers_window_start .. _goobers_window_end)
| where Environment == environment
| summarize availability=avg(Availability) by Service
| project service=Service, availability, dataAsOf=now()
```

The ADX adapter always calls `/v2/rest/query`, rejects KQL control commands,
sends parameters through ADX request properties, and applies server-side
hardline read-only, callout, external-data, sandbox, impersonation, and
remote-entity restrictions. It normalizes KQL scalar, table, and time-series
rows to the common column vocabulary: `string`, `integer`, `number`, `boolean`,
`datetime`, `duration`, and `json`.

## Result and failure semantics

Every attempt records `external-telemetry-query.json`. It contains:

- connector name, kind, version, and source identity;
- query and parameter digests, parameter names, and checked-in query reference;
- requested window, query start/end, and source `dataAsOf` when available;
- shape, typed columns, bounded rows, units, and labels;
- attempts, row/byte bounds, truncation, dropped-row, and sampling metadata;
- one explicit state: `present`, `empty`, `stale`, or `failed`.

`freshness` compares source `dataAsOf` with query completion. For ADX, set
`config.watermarkColumn` to a projected `datetime` column; the adapter uses the
latest non-null value. It falls back to a source freshness header when a proxy
supplies one. Present rows are `stale` when the watermark is absent or too old.
A valid zero-row response is `empty`. Successful point queries additionally
expose the single scalar as `telemetryValue`; all successful queries expose
`dataState` and `queryDigest`.

Authentication, transport, throttling exhaustion, timeout, cancellation,
query rejection, schema mismatch, plugin failure, and limit failures produce a
failed stage plus a `failed` artifact with a stable code and retryability flag.
They never become `empty`, `present`, or a healthy fallback. The failed artifact
does not persist provider response bodies or error strings.

## Hermetic fixtures

Use `fake@v1` for workflow and connector contract tests:

```yaml
externalTelemetry:
  connectors:
    - name: fixture-metrics
      kind: fake
      version: v1
      config:
        source: checked-in/service-health
        responses:
          service-health:
            columns:
              - name: availability
                type: number
                unit: percent
            rows:
              - [99.95]
```

Set the workflow's `query: service-health`. The fake performs no network or
credential access and returns a cloned fixture so tests cannot mutate shared
configuration.

## Compiled connector extensions

Provider implementations use the versioned
`github.com/goobers/goobers/telemetryconnector/v1alpha1` API. A factory declares
its stable kind/version, JSON configuration schema, authentication modes, query
language, and supported result shapes; validates adapter config; and builds a
read-only connector from host services:

```go
package acmetelemetry

import connector "github.com/goobers/goobers/telemetryconnector/v1alpha1"

func init() {
    connector.MustRegisterFactory(Factory{})
}
```

A custom distribution links the extension with a blank import from a file in
its `cmd/goobers` package. The runner discovers registered factories before it
configures named connectors. Workflow YAML remains unchanged: it still uses
`kind: external-telemetry`, a connector name, and generic query fields.

Factories must use `BuildOptions.HTTPClient` for network access, honor context
cancellation, return typed rows, and never return credentials or secret-bearing
headers. Return `NewQueryError` with a stable code, kind, and retryability for
classified failures. The host independently enforces timeout, retry count,
row/byte bounds, schema validation, and normalized artifact construction.

## Troubleshooting

| Symptom/code | Action |
|---|---|
| `connector_not_found` | Match workflow `connector` to an `instance.yaml` connector name. |
| `no plugin registered` at startup | Link/register the declared `kind@version`, or correct it. |
| Adapter config error at startup | Compare `config` with the factory's declared JSON schema. Unknown fields fail closed. |
| `authentication_failed` | Check identity assignment, tenant/client ID, or the referenced env/file source. Do not put a token in YAML. |
| Network policy denial | Add only the ADX cluster hostname to `allowedHosts`; keep the cluster URL HTTPS. |
| `schema_mismatch` | Update the KQL projection or exact `expectedColumns` names/types/units. |
| `stale` | Inspect `dataAsOf`, source ingestion lag, and the workflow's `freshness` bound. |
| `response_too_large` / truncation | Tighten the query and projection; increase instance policy only after reviewing evidence size. |
| `timeout` / `service_unavailable` | Check ADX health and throttling, then tune bounded timeout/retry policy. |
