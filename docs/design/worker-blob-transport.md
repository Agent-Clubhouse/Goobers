# Resident worker blob transport

An instance-backed worker requires exactly one artifact-store mode:

- `--blob-store <directory>` (default `GOOBERS_BLOB_STORE`) uses the existing
  directory-backed store.
- `--blob-endpoint <http(s) URL>` uses the daemon's digest-addressed blob plane
  for both context materialization and staging-artifact writes. With no
  directory selected, `GOOBERS_BLOB_ENDPOINT` supplies its default.

An explicit endpoint conflicts with a selected directory (`WORKER_BLOB_MODE`).
When a directory is selected, `GOOBERS_BLOB_ENDPOINT` retains its existing
meaning as the endpoint stamped into dispatched stage pods. Instance-less
workers retain the no-artifact startup posture; these store requirements apply
to workers with `--instance`. URLs cannot contain credentials, queries, or
fragments. Neither mode falls back to a different store after a failure.

A directory-backed dispatch worker performs a bounded startup check before it
polls: PUT a fresh, random 32-byte probe through the configured blob plane, then
read that digest from its directory. A missing or different value refuses
startup with `WORKER_BLOB_STORE_MISMATCH`. The small content-addressed probe is
retained. Local directory workers without `--dispatch-namespace` do not contact
the plane and need no shared signing key.

The initial endpoint implementation covers worker artifact transport. Dispatch
workers still require the shared `--blob-store` mount for the identity-keyed
`surrender/` directory: the surrender API currently exposes writes only. The
separate worker surrender read transport tracked by #5293 removes that remaining
mount requirement; endpoint artifact transport alone does not do so.

## Worker authentication

Endpoint workers and dispatch directory probes require `api.podTokenKeyFile`
with the same key as the daemon. The worker mints a fresh two-minute bearer for
each HTTP request. Its `goobers-worker-blob.` prefix and HMAC domain are distinct
from pod tokens and from the config-digest worker credential. Its principal may
only GET or PUT blobs. Human roles or pod scopes cannot broaden that grant.
The config-digest credential cannot read or write blobs or surrender results;
`GOOBERS_POD_TOKEN` never substitutes for blob-worker authentication.

As with existing signed daemon bearers, a verifier rejects a different key,
expired token, invalid identity, and lifetime beyond five minutes. A key-file
rotation takes effect when worker and daemon reload/restart their signing-key
configuration; outstanding tokens signed by the old key then fail. Bearer
values are not emitted to startup output or journals. GET responses are bounded
to 64 MiB at the client; artifact evidence readers may request a smaller bound.
