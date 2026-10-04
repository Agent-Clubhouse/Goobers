# Workbench browser and session source installation

The daemon composes the authorized workbench service with its shared read cache,
redaction registry, HTTP routes, and native session reader factory. Composition
waits until the credential endpoint is bound. Source validation, provider
projection, policy leases, credentials and cache isolation remain in internal
packages. The command adapter only chooses the installed service and preserves
model-only sessions when no authorized backlog source is configured. No shared
growth baseline is changed.
