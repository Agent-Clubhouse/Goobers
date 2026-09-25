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

## Repository remote URL forms

`goobers connect`, `push-branch`'s credential routing, and `validate`'s
target-repository match all recognize the same set of Azure DevOps remote/URL
shapes, normalized to the `organization`/`project`/`repository` coordinate
above:

- `https://dev.azure.com/<organization>/<project>/_git/<repository>`, and the
  short form Azure DevOps itself emits when project and repository share a
  name, `https://dev.azure.com/<organization>/_git/<repository>`
- the legacy pre-rename host, `https://<organization>.visualstudio.com/[DefaultCollection/]<project>/_git/<repository>`
- `git@ssh.dev.azure.com:v3/<organization>/<project>/<repository>` (or
  `ssh://git@ssh.dev.azure.com/v3/...`), and its legacy
  `<organization>@vs-ssh.visualstudio.com:v3/<organization>/<project>/<repository>`
  equivalent
- for `goobers connect` only, the bare three-part slug,
  `<organization>/<project>/<repository>`; `push-branch` and `validate` never
  treat a bare slug or a local mirror path as Azure DevOps

Matching is case-insensitive on the host and on the configured organization,
project and repository names, and tolerates a username-only origin
(`https://<organization>@dev.azure.com/...`). An origin that embeds a password
(`https://user:secret@dev.azure.com/...`) is refused by `push-branch`; remove
the password from the remote and configure the repository's `auth` instead. A
legacy `*.visualstudio.com` remote is accepted for matching and credential
routing only — Goobers never rewrites an operator's configured remote, and
every URL Goobers itself generates stays the canonical `dev.azure.com` form.

SSH remotes are matched for routing only: `push-branch` still resolves the
repository's configured Azure DevOps credential for an SSH origin, but the SSH
transport ignores that HTTP credential, so the push authenticates with the
operator's SSH key.

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
- REST and Git credential representations are registered with the journal and
  telemetry scrubber.
- Git receives credentials through its child environment, never command-line
  arguments, repository remotes, or persisted Git configuration.
- Credential-source failures fail closed; Goobers never falls back to another
  configured identity.
- A push into an ADO branch protected by an enabled policy is not a
  credential failure. ADO's Git server rejects the raw `git push` itself —
  `! [remote rejected] ... (TF402455: Pushes to this branch are not
  permitted...)`, with `GitRefUpdateRejectedByPolicyException` in the
  underlying exception text — and `push-branch` and the remediation/rebase
  force-pushes classify that rejection as `branch_policy_protected`, never
  `auth_failed`, and never retry it as a ref race or with a fresh credential:
  the fix is to land the change through a pull request, not to re-run with a
  different token.

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
