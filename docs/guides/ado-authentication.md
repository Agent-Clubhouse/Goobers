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

The `goober-runtime` worker that read these sources from `GOOBERS_ADO_*`
environment variables was retired per goobernetes-architecture.md D5 (#2055
resolved: supersede); the `goobers` binary configures ADO credentials through
the instance config surface documented above.

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

## Azure Boards work items

The configured organization and project scope all work-item operations. Goobers
generates bounded WIQL for assignee, updated-time, provider-native state, and tag
filters. Common `open`/`closed` filtering uses each returned item's process state
category so custom state names remain correct. Workflow definitions continue to
use the provider-neutral work-item model. Azure Boards tags are exposed as
labels, `System.AssignedTo` is mapped by display name, and comments use the
work-item comments API.

Close and reopen mutations select the target work-item state by the process
state category instead of assuming one process template's state names. Numeric
GitHub milestones have no Azure Boards equivalent and are rejected; existing
iteration paths are left unchanged. Claims write `goobers:claimed` plus an
internal run-owner tag in one revision-tested patch, so concurrent schedulers
settle on one visible owner without overwriting unrelated tags.

Repository and pull-request parity remains incremental. Keep human branch
policies authoritative for ADO repo operations that the provider does not yet
implement.

## Git transport quota

Azure DevOps applies git transport limits separately from REST API limits.
Managed mirror clones, incremental fetches, and partial-clone blob backfills
therefore reserve against any active ADO window in Goobers' shared provider
quota ledger before starting git. An exhausted window stops the operation
before credentials are resolved or traffic is sent.

Large ADO repositories should use large-repo mode. Its single pinned workspace,
incremental mirror fetch, and heads-and-tags-only partial-clone refspec avoid
per-stage rematerialization, pull-request ref discovery, and unnecessary blob
backfill, giving ADO the minimum-traffic checkout shape Goobers supports.
