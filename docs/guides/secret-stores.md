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

`azure-key-vault` is the only kind today. The identity behind `auth` needs
Key Vault data-plane read access (the `Key Vault Secrets User` RBAC role).
`secretStores` is read once at process start; changing it requires a daemon
restart, like the rest of `instance.yaml`.

## Store-backed refs

Everywhere a token ref is accepted — repo tokens, per-capability
`credentials` grants, the webhook secret, telemetry OTLP headers, the
workflowSource token, an ADO PAT — the same shape works:

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

Each store carries a per-secret in-memory TTL cache (default 300 seconds,
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
- One-shot commands (`goobers validate --check-repos`, `goobers status`,
  `goobers push-branch`) build their own short-lived store registry; the
  daemon builds one registry per process so every consumer shares one cache.
