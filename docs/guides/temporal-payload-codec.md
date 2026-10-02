# Temporal payload encryption

Temporal payload encryption is opt-in by `temporal.payloadCodec.keyRef` presence.
The daemon, workers, orphan sweeps, engine commands and instance-aware Kubernetes
preflight share the same converter. Liveness and completed-run memo readers use
that instance converter too. Local instances without a key retain the exact SDK
default converter and plaintext payload bytes.

## Configuration contract

```yaml
secretStores:
  - name: wrapping-vault
    kind: keyvault-key
    vaultURI: https://acme.vault.azure.net
    auth:
      kind: workload-identity
temporal:
  payloadCodec:
    keyRef:
      store: wrapping-vault
      name: history
      # version: optional-backend-version-for-new-wraps
    strict: false
```

`keyRef` is a typed key reference and must address a `keyvault-key` or `file-key`
store. A secret-fetching store is refused. See [key store configuration](secret-stores.md#key-wrapping-library)
for provisioning, file permissions, and authentication details. Unused key stores
are not authenticated when the selected codec is constructed.

`temporalcodec.DataConverter` returns the exact SDK default converter when
`keyRef` is absent, preserving default payload bytes. With a key, it wraps that
converter with the codec. Construction performs no key operation. Runtime clients explicitly receive the
returned converter; there is no mutable process-wide converter. Changing the
configuration requires restarting all clients/workers for the instance.

## Sealed payload contract

Each payload uses a fresh random 256-bit AES key and a fresh 96-bit GCM nonce.
The selected key store wraps that data key using its RSA-OAEP-256 operation.
AES-GCM encrypts the serialized original protobuf payload, including all original
metadata. The outer metadata includes:

- `encoding`: `binary/goobers-aesgcm-v1`
- `goobers-key-store` and `goobers-key-name`: the configured key identity
- `goobers-key-version`: the concrete backend version returned by wrap
- `goobers-wrapped-key`: the wrapped data key
- `goobers-nonce`: the AES-GCM nonce

All six outer metadata fields are authenticated as deterministic protobuf AAD.
The decoder pins store/name to configuration, rejects malformed or extra codec
metadata, and unwraps only the persisted explicit backend version. Metadata is
visible; it must contain identifiers, never secret values. Payload binding does
not prevent replay or reordering of an entire valid payload: workflow-context
integrity remains Temporal's responsibility.

Rotation selects a new version for new wraps. Keep old backend versions available
for decoding old payloads, even after changing a configured wrap-version pin.
These backend versions are independent of application signing-key IDs. Data keys
are cleared after each operation and never cached; callers own decoded content.
Go's runtime and cryptographic implementations may retain internal copies until
collection, so this is not a guarantee of complete process-memory erasure.

With `strict: false`, legacy plaintext payloads pass unchanged, including within
mixed batches. `strict: true` rejects them and requires a key reference. Strict
mode is an operator migration decision; the library does not infer that all old
histories have been rewritten.

Limits are 2 MiB per serialized original payload, 64 payloads and 8 MiB of
original payloads per batch, with bounded overhead allowed for sealed input.
One 30-second deadline covers the entire batch. Invalid input, tampering, missing
versions, and backend errors fail the entire operation without returning a
partial decoded batch or including payload/backend details in error messages.

## Remote HTTP handler contract

`temporalcodec.NewHTTPHandler` implements Temporal's `POST /encode` and
`POST /decode` JSON contract. It requires an explicit `oidcauth.Config`; there is
no anonymous or local-trust fallback. Every POST authenticates an OIDC bearer
token and explicitly requires `view` (or the stronger `operate`/`admin` roles),
including `/encode`. The handler does not use a generic POST-to-operate policy.

Browser access requires an exact configured origin. Wildcard origins are refused.
Allowed CORS preflight requests carry no decoded data; the subsequent POST still
requires authentication. The handler bounds request and response bodies to
16 MiB, limits concurrent authorized operations to four, respects request
cancellation, and sends `Cache-Control: no-store`. Authentication and codec errors
are generic and never echo request bodies or backend responses.

## Codec server and doctor

Run a separate codec server with the same instance configuration and wrapping-key
access as the daemon/workers:

```sh
goobers temporal codec-server --listen 127.0.0.1:8444 \
  --tls-cert /private/tls/server.pem --tls-key /private/tls/server-key.pem \
  --allow-origin https://temporal.example.com /path/to/instance

goobers doctor --temporal-codec --report json /path/to/instance
goobers doctor --k8s --instance /path/to/instance --checks temporal-namespace
```

The server **requires `api.auth.oidc`, a key reference, and TLS**, even on loopback.
It uses the daemon's issuer, audience, roles claim and role mappings. Configure
Temporal Web UI's remote codec endpoint to this HTTPS base URL and supply a bearer
token for that audience whose mapped role includes `view`. The browser must trust
the certificate and reach the endpoint. Use the exact UI origin with no path or
trailing slash. Never place a token in the endpoint URL or share it in configuration.
A reverse proxy must preserve `Authorization` and HTTPS to the codec listener.

The listener has 5-second header, 20-second read, 45-second write and 60-second idle
timeouts, a 16-KiB header cap, and at most four authorized codec operations in flight.
Signal shutdown cancels request/key operations, allows five seconds to drain, then
closes connections and joins the serving goroutine. No request bodies are logged.
`doctor` reports configured opt-in, key identity and strict mode; it explicitly does
not certify key reachability or that stored histories have been migrated.

## Reference rollout and retained histories

The [authenticated reference](../../deploy/reference/authenticated/README.md)
opts into a separate RSA wrapping-key Secret by default. Provision it once using
[the reference key script](../../config-examples/temporal-codec/provision-key.sh)
and retain it alongside history backups. See the
[reference instructions](../../deploy/reference/temporal-codec/README.md) for
mounts, rotation and explicit plaintext opt-out.

Roll out compatible codec-enabled readers/workers before enabling strict mode.
Keep `strict: false` while old histories, retries, schedules, pending tasks or old
clients can still produce plaintext. Upgrading does not rewrite persisted history;
old plaintext remains visible to anyone with direct history access. The real SDK
acceptance test starts legacy, sealed and mixed histories, inspects raw stored
payloads, and replays all three with the configured converter. Strict mode rejects
the legacy and mixed cases. Keep historical wrapping versions until all affected
history and backups have expired; losing them makes replay and result reads fail.

This seals payload bodies and original payload metadata, including invocation
instructions, activity inputs/results, signals and memos. Temporal routing and
visibility identifiers, search attributes, and failure message/stack fields are
not encrypted by a PayloadCodec. Do not put credentials in envelopes or those
fields. Transport TLS remains a separate `engine.tls` setting.
