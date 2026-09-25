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
  # clientId: optional-user-assigned-identity-client-id
```

The standard `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and
`AZURE_FEDERATED_TOKEN_FILE` settings configure the identity. Set `clientId`
when one projected workload token is trusted by multiple user-assigned
identities and this repository must use a different identity than the ambient
`AZURE_CLIENT_ID`.

Use managed identity on a supported Azure host:

```yaml
auth:
  kind: managed-identity
  # clientId: optional-user-assigned-identity-client-id
```

The daemon resolves these identities (see
[Where the credential resolves](#where-the-credential-resolves)), so the
workload identity or managed identity must be available to the daemon's own
process or pod, not only to an interactive shell. The Kubernetes reference
describes the
[daemon pod's workload identity](../../deploy/reference/README.md#azure-devops-workload-identity-for-the-daemon).
The service principal or managed identity must be added to the Azure DevOps
organization at Basic access; Stakeholder access cannot use Repos.

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

Global PATs stop working on 2026-12-01. Prefer `azure-cli`, workload identity
or managed identity. Where a PAT is still needed, use an organization-scoped
PAT held in a declared [secret store](secret-stores.md):

```yaml
repos:
  - provider: ado
    owner: my-organization
    project: my-project
    name: my-repository
    auth:
      kind: pat
    token:
      store: my-vault/ado-pat
```

The daemon resolves a store-backed PAT for the repository's grants and for
the ci-poll executor.

Select scopes for the operations you enable, not full access:

| Operation | PAT scope |
| --- | --- |
| Read repository content | Code (read), `vso.code` |
| Push branches and create pull requests | Code (read and write), `vso.code_write` |
| Query the Boards backlog and validate its project | Work Items (read), `vso.work` |
| Seed tasks, update tags, and mutate work items | Work Items (read and write), `vso.work_write` |
| Publish PR status evidence | Code (status), `vso.code_status` |

These are Azure DevOps scopes, not Goobers stage capabilities. The identity also
needs access to the target organization, repository and Boards project; scopes
do not override branch policies or project permissions. See Microsoft's
[scope reference](https://learn.microsoft.com/en-us/azure/devops/integrate/get-started/authentication/oauth?view=azure-devops)
and [PR status API](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create?view=azure-devops-rest-7.1).

For the PAT-based onboarding path, `connect --seed` uses the same named token
for Git reachability and Boards creation. `validate --check-repos` separately
checks Boards read access. See the [production onboarding guide](arbitrary-repo-onboarding.md#3-initialize-the-instance).

## Where the credential resolves

Every auth kind resolves in the daemon. The daemon registers the repository's
credential source as the source of the repository's grants, the same way it
does for a GitHub App, and mints a value when a stage starts:

| Kind | Resolved by the daemon from | Value | Scheme | Stated expiry |
| --- | --- | --- | --- | --- |
| `azure-cli` | the daemon's own Azure CLI login | Microsoft Entra token, refreshed before expiry | `bearer` | yes |
| `workload-identity` | the daemon's federated workload identity (`AZURE_*`) | Microsoft Entra token, refreshed before expiry | `bearer` | yes |
| `managed-identity` | the daemon host's managed identity | Microsoft Entra token, refreshed before expiry | `bearer` | yes |
| `pat` | `token.env`, `token.file`, `token.keychain` or `token.store` | the PAT | `basic` | no (unbounded) |

Each kind backs every repository capability: `repo:push`, `provider:pr:write`,
`provider:ci:cancel`, `ado:pr:complete`, and the `github:*` capabilities that
DSL 2.0 routes to the Azure DevOps repository, unless a `credentials:` entry
or `daemonIdentity` sources a capability from its own token. A stage receives a credential only
for the capabilities it declares:

- A local stage receives `GOOBERS_CRED_<CAPABILITY>`.
- A stage pod resolves the same values from the daemon's credential plane at
  stage start.
- A deterministic stage that received at least one credential, local or in a
  pod, also receives `GOOBERS_REPO_AUTH_SCHEME` (`basic` or `bearer`), so it
  never infers the scheme from the token's shape. Agentic stages do not
  receive it.

The daemon tracks each Entra token's expiry and refreshes it shortly before it
lapses, but the stage does not receive the expiry. A delivered token can
therefore have only a few minutes left.

The workload and managed identity sources that back grants are built on first
use, so a host without the identity can still run read-only commands such as
`goobers status`. The daemon's gaggle runtime still builds the identity at
startup to authenticate its worktree git operations, so a daemon whose
workload-identity projection is missing fails to start.

A reference repository (`additionalRepos`) that authenticates as a Microsoft
Entra identity keeps using the gaggle's own repository source for its checkout.

Built-in stage commands still build their Azure DevOps connection from the
repository's configured `auth` block. They move to the delivered credential in a
later release.

## Publishing review and CI evidence

`goobers report-pr-status` is a workflow stage that publishes a provider-native
Azure DevOps PR status. Place it after successful review and local-CI gates;
its default `succeeded` state relies on that ordering and is not an independent
test runner. Feed `prNumber` from the `open-pr` result through task inputs.

The default status context is genre `goobers`, name `validation`. Inputs also
allow `state` (`succeeded`, `failed`, or `pending`), `description`, `targetUrl`,
and `resultFile`. Configure the repository's status-check branch policy to
require the intended context; publishing status does not install a policy or
bypass other required checks. Run `goobers report-pr-status --help` for the
current input contract.

## Runtime environment

The `goober-runtime` worker that read these sources from `GOOBERS_ADO_*`
environment variables was retired per goobernetes-architecture.md D5 (#2055
resolved: supersede); the `goobers` binary configures ADO credentials through
the instance config surface documented above.

## Security behavior

- Entra tokens are cached with an expiry-aware refresh window.
- A 401 invalidates an expiring credential and retries exactly once.
- PAT sources are not retried as though they were refreshable.
- The daemon registers every value it resolves with the journal and telemetry
  scrubber when it mints it, in each form the value can travel in: the raw
  token, the `Bearer` header value, and the base64 `Basic` header value.
  Providers that the daemon constructs with a registrar register the same
  forms for each request.
- A stage command that builds its own Azure DevOps connection from the `auth`
  block does not register what it resolves with the exact-value scrubber;
  the pattern scrubber still applies to its output.
- Git receives credentials through its child environment, never command-line
  arguments, repository remotes, or persisted Git configuration.
- Credential-source failures fail closed; Goobers never falls back to another
  configured identity.

### MSA passthrough header

Every `azure-cli`, workload-identity, and managed-identity credential is a
Bearer token. On an organization that is not Microsoft Entra-backed, or for a
Microsoft account (MSA) on an Entra-backed one, a request carrying only a
valid Bearer token gets a sign-in redirect or a `401` (`TF400813`) instead of
a response. Goobers sends `X-VSS-ForceMsaPassThrough: true` alongside every
Bearer `Authorization` header — on REST calls and as a second Git
`http.<url>.extraheader` config value — so Bearer requests work the same way
against both kinds of organization. Microsoft's own `az devops` tooling sends
this header on every request for the same reason.

A PAT (`Basic`) credential never carries this header: PAT authentication
already succeeds on every organization, so the header would be untested and
unnecessary there.

## Azure Boards work items

The configured organization and project scope all work-item operations. Goobers
generates bounded WIQL for assignee, updated-time, provider-native state, and tag
filters. Common `open`/`closed` filtering uses each returned item's process state
category so custom state names remain correct. Workflow definitions continue to
use the provider-neutral work-item model. Azure Boards tags are exposed as
labels, `System.AssignedTo` is mapped by display name, and comments use the
work-item comments API. An `assignedTo`/roster identity match (e.g.
`respectAssignee`, backlog-assignment) accepts either the account's display
name or its stable `uniqueName` account identifier, case-insensitively.

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
