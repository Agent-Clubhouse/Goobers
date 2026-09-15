# Daemon API discovery

Remote clients do not need a local `goobers` executable to discover or operate
an instance. Start with the version-independent bootstrap route:

```http
GET /.well-known/goobers
```

The response identifies the daemon build and authentication mode, then links
to its preferred API, OpenAPI 3.1 document, runtime capabilities, instance
inventory, health, and recovery-safe readiness resource. It also reports a
provider-neutral integer `daemonProtocolVersion`, durable `daemonInstanceId`,
per-process `daemonBootId`, the exact OpenAPI SHA-256, and the current
capability-document ETag. API-major-keyed links let future daemons advertise
more than one HTTP API family without making a client fetch the wrong
contract. Paths are same-origin and relative to the daemon origin.

`GET /api/v1/openapi.json` describes every route supported by the daemon
binary. `GET /api/v1/capabilities` overlays deployment state: optional services
that are not configured and routes blocked during crash recovery are reported
with `available: false`, a machine-readable code, and a reason. Clients should
use the OpenAPI document for request construction and the capability document
for whether an operation is currently usable. The native OpenAPI document is
self-contained: every `$ref` targets its own Components Object.

The initial bounded remote-read profile contains `health`, `instance`, `runs`,
and `events`. OpenAPI operations and capability entries mark this explicitly
with `x-goobers-remote-invocable` and `remoteInvocable`; additive daemon routes
do not enter that profile automatically. Separately mounted configuration
authoring routes are intentionally excluded from the daemon OpenAPI,
capability document, and compatibility manifest.

The discovery, OpenAPI, and capability routes are classified as
`api-metadata`, not ordinary `read-only-navigation`. A remote broker can
therefore reserve the native metadata for its own validation and publish a
separately filtered caller-facing contract without accidentally admitting the
native endpoints through a generic read allowlist.

Discovery, OpenAPI, and capability responses provide ETags and honor
`If-None-Match`. They negotiate gzip through `Accept-Encoding`; gzip responses
use a weak representation validator while identity responses use a strong
validator. Both validators identify the same canonical document, and the
advertised OpenAPI SHA-256 always covers the uncompressed deterministic bytes.
The OpenAPI SHA-256 covers the deterministic UTF-8 JSON bytes before any HTTP
content coding. Health and instance readiness include the same compact
protocol summary so an already connected client can detect daemon restart or
contract/capability changes without downloading OpenAPI on every heartbeat.

All three discovery routes use the same authentication and authorization
pipeline as the rest of the API. In authenticated deployments a principal with
the `view` role may read them; local-trust loopback deployments require no
bearer token. OpenAPI responses require private-cache revalidation, including
conditional responses; shared caches must not reuse protected metadata.
Mutation schemas require `actor` in local-trust mode; authenticated deployments
derive the actor from the principal instead. The events operation declares
`Last-Event-ID` for resuming the event stream.
Discovery is generated from immutable build/boot identity and
the canonical route contract; it does not read journals, projections, or
scheduler health.
