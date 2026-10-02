# Temporal payload codec library

The first part of #5288 provides an **inactive library surface** for sealing
Temporal payloads. Existing Temporal clients, workers, and history readers still
use their current converters. Declaring the configuration below alone does not
encrypt Temporal history. Client wiring, the codec-server CLI, reference
Kubernetes opt-in, doctor reporting, and SDK history/replay acceptance tests are
the second part of #5288; that issue remains open until those land.

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
converter with the codec. Construction performs no key operation. Callers must
explicitly attach the returned converter to their Temporal clients; no existing
runtime caller does so in this first part.

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

The embedding server must supply TLS, read/header/write timeouts, and listener
lifecycle. No listener, CLI subcommand, ingress, or runtime endpoint is activated
by this library. The hermetic HTTP tests use signed OIDC tokens and the Temporal
SDK remote codec client; they do not establish encrypted-history integration.
