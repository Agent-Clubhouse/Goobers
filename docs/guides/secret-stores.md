# External secret stores

Any token ref in `instance.yaml` can read its value from a declared external
secret store instead of an environment variable or file (SEC-010, #683). A
store declares WHERE secrets live, never a value; refs opt in per token with
`store: <storeName>/<secretName>` — still exactly one source per ref.
Instances that declare no stores and use only `env`/`file` refs behave exactly
as before.

## Declaring a store

```yaml
secretStores:
  - name: prod-kv
    kind: azure-key-vault
    vaultURI: https://acme.vault.azure.net
    auth:
      kind: workload-identity
    # cacheTTLSeconds: 300
```

`azure-key-vault` fetches secrets. The identity behind `auth` needs
Key Vault data-plane read access (the `Key Vault Secrets User` RBAC role).
`secretStores` is read once at process start; changing it requires a daemon
restart, like the rest of `instance.yaml`.

## Store-backed refs

Everywhere a token ref is accepted — repo tokens, per-capability
`credentials` grants, the webhook secret, telemetry OTLP headers, the
workflowSource token — the same shape works:

```yaml
repos:
  - provider: github
    owner: acme
    name: web
    token:
      store: prod-kv/github-token
```

The part after the first `/` is the vault-relative secret name; the latest
version is read (no version pins — rotate in the vault).

**Azure DevOps repos work the same way.** The daemon resolves a
store-backed ADO PAT for the repository's grants and for the ci-poll executor,
and the validate, getting-started, and worktree paths pass a store resolver
too. Stage commands never read the PAT themselves: they receive it as the
credential of a capability they declared, so a `store:` PAT works inside
stages, local or in a pod. The operator commands `goobers status` and
`goobers run`, which build their Azure DevOps connection from the repository's
`auth` block on the host, still pass no store resolver and fail closed on a
`store:` PAT.

## Authenticating to the store

Auth to the store itself always uses an ambient identity — never a token
ref, which would be circular. Exactly the declared kind is tried; there is no
`DefaultAzureCredential`-style fallback chain, so a misconfigured identity is
a diagnosable error, never a silent switch to whichever ambient credential
happens to work.

| `auth.kind` | Use | Configuration |
| --- | --- | --- |
| `workload-identity` | Kubernetes / CI federation | `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_FEDERATED_TOKEN_FILE` (or `clientId`) |
| `managed-identity` | Azure-hosted VMs/containers | `clientId` optional for a user-assigned identity |
| `azure-cli` | Local development | `az login` |

## Caching and rotation

Each secret-fetching store carries a per-secret in-memory TTL cache (default 300 seconds,
`cacheTTLSeconds` to tune). A burst of stage starts costs one vault
round-trip per secret; a value rotated in the vault is picked up within the
TTL without restarting the daemon. Errors are never cached.

## Security behavior

- Resolution fails closed end to end: an undeclared store, an unknown or
  empty secret, a malformed ref, and a store-backed ref reaching a build path
  without store support are all errors — never a fallback to an
  unauthenticated or unconfigured path.
- Resolved values are registered with the journal and telemetry scrubbers
  exactly like env/file-resolved tokens, and are never passed via
  command-line arguments or persisted configuration.
- A secret never belongs in a stage's `inputs:` (#2931). Stage inputs are
  history-resident: they are merged into the invocation envelope, and on the
  engine tier that envelope is a Temporal activity argument persisted verbatim
  in durable workflow history. Declare a credential capability and let the
  value resolve worker-side at stage start instead. `goobers validate` reports
  `SEC001` for a secret-shaped literal in `inputs:` at author time, and the
  engine refuses to execute a stage whose serialized envelope carries a value
  the credential plane minted.
- One-shot commands (`goobers validate --check-repos`, `goobers status`,
  `goobers push-branch`) build their own short-lived store registry; the
  daemon builds one registry per process so every consumer shares one cache.


## Key wrapping library

`keyvault-key` and `file-key` declarations provide a separate `KeyStore` library
surface using RSA-OAEP-256. Declaring them does not enable run encryption or
change any runtime default. The secret resolver skips these entries, and token
refs cannot address them. A typed `instance.KeyRef{Store, Name, Version}` cannot
address an `azure-key-vault` secret store.

```yaml
secretStores:
  - name: wrapping-vault
    kind: keyvault-key
    vaultURI: https://acme.vault.azure.net
    auth:
      kind: workload-identity
  - name: local-wrapping
    kind: file-key
    directory: /private/var/lib/goobers/wrapping-keys
```

Callers explicitly construct `secretstore.NewKeyRegistry` and invoke `Wrap` or
`Unwrap`. Azure uses exactly the declared authentication kind, as above, and
requires data-plane wrap/unwrap access to the chosen key. Returned Azure key
identifiers must match the configured vault, requested name, and any pinned
version. Calls have a 30-second ceiling and honor shorter caller deadlines.
Unwrapped material is never cached; the caller owns its lifetime. Key stores
reject nonzero `cacheTTLSeconds`.

`Wrap` accepts an empty version to select the backend's current version, and
returns its concrete backend version alongside ciphertext. Persist both that
version and the original store/name with the ciphertext. `Unwrap` requires an
explicit version; it never falls back to the latest key. These versions are
backend identifiers, independent of application signing-key IDs.

For `file-key`, operators provision 2048–8192-bit RSA private keys in unencrypted
PKCS#1 or PKCS#8 PEM files at `<directory>/<name>/<version>.pem`. The directory
and name subdirectory must be private (0700); PEM files and the `active` marker
must be private regular files (0600). Final file symlinks are rejected and reads
are confined to the configured directory. Names and versions contain only
letters, digits, and hyphens, with a maximum of 127 characters. The `active`
file contains a version, optionally followed by a newline. Reads are bounded
(16 KiB PEM, 128 bytes marker). Files are loaded on demand; no key files are
created, overwritten, or cached by the library.

To rotate a local key, provision a new immutable version file and atomically
replace `active` with the new version. Retain old version files until all
ciphertext using them is retired. Never replace key material under an existing
version. Apply the same retention rule to Azure key versions. This API wraps
small data keys, not bulk content: RSA-OAEP-256's plaintext limit is the RSA
modulus size in bytes minus 66 bytes.
