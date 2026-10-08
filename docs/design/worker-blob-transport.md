# Resident worker blob transport

Status: implemented — blob and surrender endpoint transport.
Delivered-by: #5293
Scope-delta: No remaining transport scope delta; endpoint dispatch workers use both blob and surrender APIs without a shared artifact mount.
Verified: 2ea5900aa (2026-10-02)

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
retained. While the daemon is not ready (HTTP 503 or no answer, 4-9 minutes on a
rollout) the probe waits with backoff for `--blob-probe-wait` (env
`GOOBERS_WORKER_BLOB_PROBE_WAIT`, default 20m), logging INFO progress. A
starting daemon's 503 (both "daemon is starting" and the API recovery gate's
"recovering") advertises its derived startup budget
(`Goobers-Startup-Budget-Remaining`, seconds), a progress token
(`Goobers-Startup-Progress`) and a per-process id (`Goobers-Startup-Daemon`):
the probe waits until that budget ends plus five minutes, and each change of
the token restarts the `--blob-probe-wait` window, so a slow but advancing
daemon is waited for while a stuck one is not. A new daemon id re-baselines the
token, and only the first daemon seen extends the wait by its budget; after a
restart the wait ends at most one `--blob-probe-wait` window later, so a
crash-looping daemon is not mistaken for progress. Neither extends the wait
past 6h. The bound expiring exits `WORKER_DAEMON_NOT_READY`.
A 4xx, a non-503 5xx or a
read-back mismatch fails immediately with the daemon's error body. Local directory workers without `--dispatch-namespace` do not contact
the plane and need no shared signing key.

In endpoint mode, dispatch workers also read the identity-keyed surrender plane
through `--daemon-api`, without a shared blob or surrender directory. Stage pods
retain their existing write-once surrender POST. The worker reads presence from
`GET /api/v1/runs/{run}/stages/{stage}/attempts/{attempt}/surrender/seen` and results
from GET on the surrender path itself. An absent result maps to the existing
no-surrender condition. Presence does not imply successful execution; the engine
still validates and classifies the surrendered result. Directory-backed dispatch
keeps its existing `surrender/` directory alongside the blob tree. Upgrade the
daemon to a version serving the worker read routes before switching dispatch
workers to endpoint mode; an older daemon will refuse those reads.

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

## Surrender read authentication and bounds

Surrender reads use another dedicated bearer, `goobers-worker-surrender-read.`,
with its own HMAC domain. It grants only the two surrender GET routes. It cannot
write a surrender, get or put blobs, read config, emit journal records, invoke
credentials, or access any other route. Pod credentials, human administrator
roles, blob-worker bearers, and the existing config-digest credential cannot
read surrendered results or presence. The handlers enforce that worker identity
again even if the server is configured with a permissive authorizer.

The worker mints a fresh two-minute token per read, using the same shared signing
key configuration and five-minute maximum lifetime as the other worker bearer.
The same restart/reload rotation behavior applies. No bearer values are passed
to stage pods, startup output, or journals by this transport.

Each result read has the existing one MiB surrender write limit. The directory
backend opens a regular file beneath its root and reads at most the limit plus
one byte; the HTTP handler also checks the returned size. The client independently
bounds result responses to one MiB and presence responses to 128 bytes, rejects
missing/malformed presence, and applies a 30-second HTTP timeout. Responses use
`Cache-Control: no-store`. Server error bodies are never copied into client
errors. A worker read client rejects Put locally; the only write credential is
still the stage pod's run-scoped bearer.
