# Daemon API discovery

Remote clients do not need a local `goobers` executable to discover or operate
an instance. Start with the version-independent bootstrap route:

```http
GET /.well-known/goobers
```

The response identifies the daemon build and authentication mode, then links
to its preferred API, OpenAPI 3.1 document, runtime capabilities, instance
inventory, and health resource. Paths are relative to the daemon origin so
reverse proxies can preserve the advertised contract.

`GET /api/v1/openapi.json` describes every route supported by the daemon
binary. `GET /api/v1/capabilities` overlays deployment state: optional services
that are not configured and routes blocked during crash recovery are reported
with `available: false` and a reason. Clients should use the OpenAPI document
for request construction and the capability document for whether an operation
is currently usable. Workflow triggering, trigger status, cancellation,
interventions, escalation resolution, and workflow enablement have explicit
request and response schemas. Specialized worker and pod planes remain
discoverable with their media type and generic JSON shape.

All three discovery routes use the same authentication and authorization
pipeline as the rest of the API. In authenticated deployments a principal with
the `view` role may read them; local-trust loopback deployments require no
bearer token.
