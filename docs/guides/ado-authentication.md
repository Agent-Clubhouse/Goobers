# Azure DevOps authentication

Goobers supports four Azure DevOps credential sources. Authentication only
proves an identity; Azure DevOps permissions and Goobers stage capabilities
still authorize each operation.

## Local interactive authentication

Sign in with Azure CLI:

```powershell
az login
```

`az login` does not create a PAT. Goobers requests an expiring Microsoft Entra
bearer token for the Azure DevOps resource and refreshes it before expiry:

```yaml
repos:
  - provider: ado
    owner: my-organization
    project: my-project
    name: my-repository
    auth:
      kind: azure-cli
      # tenant: optional-tenant-id
```

The matching gaggle project uses the same three-part repository identity:

```yaml
project:
  provider: ado
  owner: my-organization
  project: my-project
  name: my-repository
  branch: main
```

## Unattended authentication

Use workload identity federation in Kubernetes or CI:

```yaml
auth:
  kind: workload-identity
```

The standard `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and
`AZURE_FEDERATED_TOKEN_FILE` settings configure the identity.

Use managed identity on a supported Azure host:

```yaml
auth:
  kind: managed-identity
  # clientId: optional-user-assigned-identity-client-id
```

## PAT compatibility

PAT authentication remains available for controlled headless environments.
Token values are indirect and must never be written inline:

```yaml
repos:
  - provider: ado
    owner: my-organization
    project: my-project
    name: my-repository
    auth:
      kind: pat
    token:
      env: GOOBERS_ADO_TOKEN
```

Omitting `auth` while configuring `token` preserves the legacy PAT behavior.
Token files must pass Goobers' private-file permission check.

## Runtime environment

The superseded `goober-runtime` worker supports the same sources through:

| Variable | Purpose |
| --- | --- |
| `GOOBERS_ADO_AUTH_KIND` | `pat`, `azure-cli`, `workload-identity`, or `managed-identity` |
| `GOOBERS_ADO_ORG` | Azure DevOps organization |
| `GOOBERS_ADO_PROJECT` | Azure DevOps project |
| `GOOBERS_ADO_TENANT` | Optional Azure CLI tenant |
| `GOOBERS_ADO_CLIENT_ID` | Optional user-assigned managed identity |
| `GOOBERS_ADO_TOKEN` | PAT value when `kind=pat` |

## Security behavior

- Entra tokens are cached with an expiry-aware refresh window.
- A 401 invalidates an expiring credential and retries exactly once.
- PAT sources are not retried as though they were refreshable.
- REST and Git credential representations are registered with the journal and
  telemetry scrubber.
- Git receives credentials through its child environment, never command-line
  arguments, repository remotes, or persisted Git configuration.
- Credential-source failures fail closed; Goobers never falls back to another
  configured identity.

Azure DevOps provider parity is still incremental. Authentication does not make
GitHub-specific workflow commands provider-neutral; use only ADO operations
implemented by the provider and keep human branch policies authoritative.
