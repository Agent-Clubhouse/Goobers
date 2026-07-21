# Export local traces to Jaeger

Goobers' local journal and SQLite remain the primary telemetry stores.
Collector push is an additional, opt-in projection: with no
`telemetry.otlp.endpoint` and no `GOOBERS_OTLP_ENDPOINT`, Goobers opens no
collector connection and has no service dependency.

## Start Jaeger

Run Jaeger's all-in-one image with OTLP/gRPC and the query UI bound to
loopback:

```sh
docker run --rm --name jaeger \
  -e COLLECTOR_OTLP_ENABLED=true \
  -p 127.0.0.1:4317:4317 \
  -p 127.0.0.1:16686:16686 \
  jaegertracing/all-in-one:latest
```

Add the local collector to `instance.yaml`:

```yaml
telemetry:
  enabled: true
  otlp:
    endpoint: http://127.0.0.1:4317
    insecure: true
```

Restart `goobers up`, run a workflow, then open
<http://127.0.0.1:16686/> and select the `goobers` service.

`insecure: true` is required for this plain-text endpoint and is accepted only
for `localhost` or a loopback IP. Non-loopback collectors require TLS:

```yaml
telemetry:
  otlp:
    endpoint: https://collector.example.com:4317
```

An endpoint without a URL scheme, such as `collector.example.com:4317`, also
uses TLS. Goobers rejects remote insecure endpoints, `http` without explicit
insecure mode, and contradictory `https` plus `insecure: true` settings before
the daemon starts.

## Authentication

Collector credentials are never stored inline. Configure each OTLP metadata
header with exactly one environment or secret-file reference:

```yaml
telemetry:
  otlp:
    endpoint: https://collector.example.com:4317
    headers:
      authorization:
        env: GOOBERS_OTLP_AUTHORIZATION
      x-api-key:
        file: /run/secrets/otlp-api-key
```

Set the environment value to the complete header value expected by the
collector, for example `Bearer ...`. Secret files must not be accessible to
group or other users (use mode `0600`). Missing, empty, inline, or ambiguous
header credentials fail startup. Resolved values are registered with the
telemetry scrubber and are not written to journals.

## Environment overrides

The daemon resolves these process variables after reading `instance.yaml`:

| Variable | Override |
|---|---|
| `GOOBERS_OTLP_ENDPOINT` | `telemetry.otlp.endpoint` |
| `GOOBERS_OTLP_INSECURE` | `telemetry.otlp.insecure` (`true` or `false`) |

Non-empty environment values override their file fields independently, and
the final combination must satisfy the same TLS and loopback rules. This
allows a deployment to keep a secure default in `instance.yaml` while
selecting its collector at launch:

```sh
GOOBERS_OTLP_ENDPOINT=https://collector.example.com:4317 \
GOOBERS_OTLP_INSECURE=false \
goobers up /path/to/instance
```

Header entries still contain references rather than values; the referenced
environment variable or file supplies the credential. To disable collector
push, omit the file endpoint and unset both OTLP override variables.

Standard OpenTelemetry SDK variables do not enable or override Goobers
collector push. In particular, `OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_INSECURE`, and `OTEL_EXPORTER_OTLP_HEADERS` (including
their `OTEL_EXPORTER_OTLP_TRACES_*` forms) are ignored for the endpoint,
transport mode, and metadata map. Goobers explicitly pins the validated
`instance.yaml` plus `GOOBERS_OTLP_*` result so process-wide SDK settings
cannot weaken TLS or bypass credential references.
