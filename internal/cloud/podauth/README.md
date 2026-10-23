# Cloud pod-token keys and credential-grant revocation

The cloud role constructs `podauth.New` before serving requests, injects its
`PodKeys` and `GrantKeys` into dispatch/credential services, and runs
`RunKeyReload` for the role lifetime. `VerifyCredentialGrant` is the credential
refresh admission seam; it checks signature, expiry and durable revocation before
credentials are resolved. The role composition is owned by HA-P3-18. This library
refuses Registry mode, missing signing keys, and missing durable revocations.
Local single-key installations retain their existing token format and behavior.

Cloud auth explicitly uses `LoadKeyRingFile` and requires a JSON key ring; raw
files and malformed JSON fail boot. The existing `LoadSignedKeyFile` and
`LoadDerivedSignedKeyFile` APIs continue to treat files as arbitrary trimmed raw
bytes, without format detection or reload. A versioned key file contains base64
key material (at least 32 decoded bytes):

```json
{
  "current": {"id": "v1", "key": "<base64-key>"},
  "previous": {"id": "v2", "key": "<different-base64-key>"}
}
```

`previous` is optional. IDs contain 1–64 URL-safe letters, digits, underscores or
hyphens. IDs are part of the MAC-covered payload; an unknown ID fails closed.
Only `current` mints. An untagged token verifies against `current` only. The legacy raw-file
loaders continue to produce a single untagged key. Credential-refresh key material
is derived independently for each version using the existing HKDF domain.

The watcher reads the file path every five seconds, including after Secret
symlink replacement. It atomically swaps a validated snapshot; a missing,
malformed, oversized or weak replacement retains the last good keys and reports
an error to the role's required callback. The role must expose these errors in
health/telemetry and wait for successful reload on every replica before promotion.

For an initial upgrade from untagged tokens, first distribute the versioned file
with the same current key and wait for outstanding untagged tokens to expire.
Then rotate as follows:

1. Add the new key as `previous`, preserving the old `current`. Wait for every
   verifier to report a successful reload (or finish the rolling restart).
2. Promote the new key to `current` and retain the old key as `previous`. Either
   version verifies on every replica throughout the staggered promotion.
3. After every minting replica has promoted, retain the old verifier for the
   longest outstanding token lifetime: **24 hours** for credential-refresh and
   child-workflow grants (pod tokens alone are capped at 8 hours). Then remove it.

`Revocations` reads `control.grant_revocation` on the primary for every grant
admission with a two-second timeout. That same query reads `clock_timestamp()`
and rejects grants whose signed expiry has passed on the database clock. A slow
API host clock therefore cannot re-admit a revoked grant after controller cleanup. It has no replica-local negative cache.
After a revoke commits, all newly started checks see it; already-started reads
are bounded by the timeout. Database errors fail closed. Instances are isolated
by `(instance_id, grant_id)`. Rows contain only random grant IDs, owner epochs,
revocation times and expiry metadata, never bearer tokens or signing material.

Writers use the API pool for immutable, idempotent INSERTs. Revocation does not
wait for controller ownership: it is a monotonic security fact. Both reads and
writes use `goobers_api` credentials. The controller separately deletes only
expired rows in a lease-fenced transaction using its own pool. The historical
`owner_epoch` column remains nullable for rolling compatibility; new API facts
have no controller epoch.

Cloud integration tests use PostgreSQL 16 through `GOOBERS_TEST_POSTGRES_URL`
and the `cloudintegration` build tag. They create isolated test databases; no
cloud resources are required.
