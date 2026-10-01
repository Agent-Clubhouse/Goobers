# Azure DevOps authentication

Goobers supports four Azure DevOps credential sources. Authentication only
proves an identity; Azure DevOps permissions and Goobers stage capabilities
still authorize each operation.

`goobers init --template=standard --provider=ado` writes `azure-cli`
authentication by default, with no token variable. Pass `--repo-auth-kind`
with `workload-identity`, `managed-identity` or `pat` to choose another source;
only `pat` records a token variable, `GOOBERS_ADO_TOKEN`. Passing
`--repo-token-env=NAME` without `--repo-auth-kind` selects `pat` reading `NAME`;
with any other kind it is refused, because only `pat` reads a token variable.
`goobers connect` records PAT authentication.

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

### Diagnosing Azure CLI failures

When Azure CLI credential acquisition fails, Goobers reports whether executable
lookup, process startup, cancellation, a deadline, or a nonzero exit caused the
failure when the subprocess error identifies that condition. A nonzero exit
includes its exit code, but does **not** by itself mean the user is signed out.
Unclassified failures remain unclassified. Failed CLI output and arbitrary
runner error text are withheld because they can contain partial credentials.
Repository validation still fails; Goobers does not bypass authentication or
change the configured credential source.

For a nonzero exit, Goobers checks the CLI output for a fixed set of markers
and reports only the resulting classification (never the output itself), which
also appears in the run journal and `goobers status` error message:

| Reported failure | Error code | What to do |
| --- | --- | --- |
| `Azure CLI sign-in expired or requires interaction` (an expired or revoked refresh token, MFA or other interaction required, or the CLI asking for `az login`) | `azure_cli_sign_in_required` | Run `az login` (with `--tenant` if `auth.tenant` is set) as the user Goobers runs as. |
| `no Azure CLI account is signed in` | `azure_cli_no_account` | Same as above. |
| `Azure CLI could not reach the network` (name resolution or connection failures, for example after the host slept or a VPN dropped) | `azure_cli_network_unreachable` | Restore network, DNS, VPN or proxy access, then retry. Signing in again does not help. |
| `process exited with code N` (no recognized marker) | `azure_cli_exit` | Run the check below. |

A sign-in error returned by Microsoft Entra ID takes precedence over network
markers, because receiving it proves the network was reachable.

For a command failure, run this token-free output check in the same user and
process environment as Goobers:

```powershell
az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798 --query expiresOn --output tsv
```

If `auth.tenant` is set, add `--tenant` with that same value. Use `az login`
only if the check requests sign-in; otherwise follow the CLI's local diagnosis.
For lookup or startup failures, check the installation, `PATH`, launcher and
executable permissions. For a timeout, check CLI responsiveness and network
access. Do not paste unfiltered token-command output into logs or support
reports.

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
(`https://<organization>@dev.azure.com/...`, or `git@` on the SSH hosts). An
origin that embeds a password (`https://user:secret@dev.azure.com/...`), or
any other username, which may be a token (`https://<token>@dev.azure.com/...`),
is refused by `push-branch`, and the refusal masks it; remove it from the
remote and configure the repository's `auth` instead. A
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
| Publish PR status evidence | Code (status), `vso.code_status`, plus Code (read), `vso.code`: publishing first reads the pull request's iterations |

These are Azure DevOps scopes, not Goobers stage capabilities. The identity also
needs access to the target organization, repository and Boards project; scopes
do not override branch policies or project permissions. See Microsoft's
[scope reference](https://learn.microsoft.com/en-us/azure/devops/integrate/get-started/authentication/oauth?view=azure-devops)
and [PR status API](https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create?view=azure-devops-rest-7.1).

For the PAT-based onboarding path, `connect --seed` uses the same named token
for Git reachability and Boards creation. `validate --check-repos` separately
checks Boards read access. See the [production onboarding guide](arbitrary-repo-onboarding.md#3-initialize-the-instance).

## Repository permissions

Scopes limit what a credential may call. Repository permissions decide what the
identity behind it may do. Grant the Goobers identity these Git repository
permissions on each target repository:

| Permission | Why Goobers needs it |
| --- | --- |
| Contribute | Push run branches |
| Create branch | Create run branches |
| Contribute to pull requests | Open, comment on and complete pull requests |
| Force push (rewrite history and delete branches) | Rewrite or delete branches the identity did not create, such as a human-opened pull request's source branch during remediation. Azure DevOps already lets a branch's creator force-push and delete its own run branches. Recommended. |

Do not grant it these permissions. Goobers never bypasses branch policy, and its
identity should not be able to:

| Permission | Why it must not be held |
| --- | --- |
| Bypass policies when completing pull requests | Lets a pull request land without its required reviewers and checks |
| Bypass policies when pushing | Lets a push skip the policies on a protected branch |

Scope blocking branch policies to the branches they protect, such as the
default branch. A blocking policy with a **Prefix** scope over `refs/heads/`
makes every branch accept changes only through a pull request, so Goobers
cannot push its run branches.

### What `validate --check-repos` reports

After each repository is reachable, `goobers validate --check-repos` makes
these reads against the configured organization only. It changes nothing.

| Check | Result |
| --- | --- |
| Identity | Prints the id and UPN of the identity the credential authenticates as |
| Contribute, Contribute to pull requests, Create branch | `ADOACCESS001` error when the identity lacks one. `validate` exits 1. |
| Force push | `ADOACCESS002` warning when the identity lacks it at repository level. Only branches the identity did not create are affected. |
| Either bypass permission | `ADOACCESS003` warning when the identity holds it |
| Branch policies | `ADOACCESS004` warning for each enabled, blocking policy with a Prefix scope over `refs/heads/` |
| A read that fails | `ADOACCESS005` warning. The result is unknown, not missing. |
| Boards states | For each Azure Boards backlog, prints the states of the work item type Goobers creates. A `BACKLOG002` warning names a type with no Completed state, and each `backlog.doneStates.byType` type or state name the project does not have. |

Permissions are evaluated on the repository (security token
`repoV2/<projectId>/<repositoryId>`). A deny set only on a single branch is not
seen.

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

The daemon tracks each Entra token's expiry. Before it delivers a token with
less than 20 minutes left, locally or through the credential plane, it rebuilds
the credential source and fetches again, so the stage starts with a fresh
token. For `workload-identity` and `managed-identity` the rebuild bypasses the
Azure SDK's token cache. For `azure-cli` the Azure CLI keeps its own cache and
may return the same token until a few minutes before it expires; the daemon
then delivers that token, which is still valid. The daemon never fails a stage
because a refresh did not produce a longer-lived token.

The stage receives each token's expiry as the non-secret
`GOOBERS_CREDENTIAL_EXPIRES_<CAPABILITY>` (an RFC 3339 UTC timestamp) beside
`GOOBERS_CRED_<CAPABILITY>`. A PAT states no expiry and gets no such variable.

A stage cannot refresh what it was delivered: when Azure DevOps rejects the
value with HTTP 401, the request fails without a retry, with an error that
names the capability and keeps the 401 response. It is reported as an
authentication failure (`github_auth_failed`). The delivered expiry decides
the wording:

- at or after the expiry, the credential "expired at" that time;
- before it, the credential was "revoked or without access to this resource"
  (Azure DevOps answers 401 for a missing scope or project access too);
- with no delivered expiry (a PAT, or a variable set by hand), "expired,
  revoked, or without access to this resource".

The next attempt receives a new value, which helps with expiry but not with
missing access.

The workload and managed identity sources that back grants are built on first
use, so a host without the identity can still run read-only commands such as
`goobers status`. The daemon's gaggle runtime still builds the identity at
startup to authenticate its worktree git operations, so a daemon whose
workload-identity projection is missing fails to start.

A reference repository (`additionalRepos`) that authenticates as a Microsoft
Entra identity keeps using the gaggle's own repository source for its checkout.

Built-in stage commands authenticate only with the credential delivered for
their declared capabilities. They never read the repository's `auth` block, so
an undeclared capability means no credential on Azure DevOps, as on GitHub. Under
DSL 2.0 the declared capability selects the credential for the operation on the
repository the stage routes to:

| Declared capability | Authenticates |
| --- | --- |
| `github:pr:write`, `provider:pr:write` | pull-request reads, threads, labels and statuses |
| `github:issues:read`, `github:issues:write`, `github:milestones:write` | Azure Boards work items |
| `repo:push` | `push-branch` and the remediation fetches and force-pushes |
| `github:pr:merge` | pull-request completion in `merge-pr` and `merge-queue-poll` |
| `ado:pr:complete` | the same completion, instead of `github:pr:merge`, when the stage declares it (optional) |
| `ado:work-items:write` | linking the pull request `open-pr` opened to its work item natively (optional; the repository credential backs it when the stage declares it) |

This needs no `runner.envPassthrough` entry for a PAT or an Azure identity
variable, and a stage pod needs no Azure identity of its own: only the daemon
does. A `GOOBERS_CRED_<CAPABILITY>` set by hand for a standalone invocation,
with no `GOOBERS_REPO_AUTH_SCHEME`, is sent as a PAT.

Native work-item linking is best-effort. When `open-pr` has a claimed Azure
Boards item but no `ado:work-items:write` credential was delivered, it warns,
opens the pull request with the text reference to the item, and adds a note to
the pull request description that the item is not linked natively. When the
credential is delivered and Azure DevOps rejects the link, the stage fails. The
shipped workflows do not declare `ado:work-items:write`; add it to the `open-pr`
stage to link natively. No `credentials:` entry is needed: the repository
credential backs it, as it backs `provider:pr:write`. A GitHub or Gitea
repository credential never backs it.

The daemon states one authorization scheme per stage, taken from the
repository's `auth` kind, and it applies to every credential the stage
receives, including a `credentials:` entry. On a gaggle whose repository
authenticates as a Microsoft Entra identity (`azure-cli`, `workload-identity`,
`managed-identity`), a `credentials:` value, such as one that overrides the
repository credential for `ado:work-items:write`, is therefore sent as `Bearer`
and must be an Entra access token; a PAT there is rejected.

Operator commands that are not stages, such as `goobers status` and
`goobers run`, still read the repository's `auth` block on the host where they
run.

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

- Entra tokens are cached with an expiry-aware refresh window, and a token
  delivered to a stage has at least 20 minutes left whenever the source can
  mint one.
- A 401 invalidates an expiring credential and retries exactly once. A
  credential delivered to a stage is never resent after a 401; that request
  fails with an error that says whether the credential expired or was revoked
  or lacks access (when its expiry was delivered) and still classifies as an
  authentication failure.
- PAT sources are not retried as though they were refreshable.
- The daemon registers every value it resolves with the journal and telemetry
  scrubber when it mints it, in each form the value can travel in: the raw
  token, the `Bearer` header value, and the base64 `Basic` header value.
  Providers that the daemon constructs with a registrar register the same
  forms for each request. A stage pod registers the same forms for the
  values it resolved before it scrubs the stage's output, its result file
  and the message of a failed stage.
- A stage pod's workspace checkout sends the delivered value in the stated
  scheme: `Basic` for a PAT, and `Bearer` with the MSA passthrough header for
  an Entra token.
- Stage commands build no Azure DevOps connection of their own; every value
  a stage uses was registered by the daemon when it was minted.
- Git receives credentials through its child environment, never command-line
  arguments, repository remotes, or persisted Git configuration.
- Credential-source failures fail closed; Goobers never falls back to another
  configured identity.
- A push into an ADO branch protected by an enabled policy is not a
  credential failure. ADO's Git server rejects the raw `git push` itself —
  `! [remote rejected] ... (TF402455: Pushes to this branch are not
  permitted...)`, with `GitRefUpdateRejectedByPolicyException` in the
  underlying exception text. `push-branch` reports the protected branch on
  stderr and does not retry it as a ref race; it writes no result file, so
  it records no error code. The remediation/rebase force-pushes classify the
  same rejection as `provider_branch_policy_protected` (the provider error
  class in telemetry). Neither treats it as `auth_failed` or retries with a
  fresh credential: the fix is to land the change through a pull request,
  not to re-run with a different token.

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
iteration paths are left unchanged. A claim posts a claim breadcrumb comment on
the work item, re-reads the comment thread so concurrent schedulers settle on
the earliest breadcrumb, and then adds the visible `goobers:claimed` tag in a
revision-tested patch that leaves unrelated tags alone. Releasing a claim posts
a release breadcrumb and removes the tag. When a run ends without a close-out
stage (for example a `no-work` outcome or an abort), the daemon's terminal
cleanup performs the same release against the gaggle's backlog project before
it frees the local claim, and it never ends a claim that a newer run now
holds. Claims taken for a work item on a `goobers:ready` selector record
the ready time Goobers reads from the work item's update history, the time
the `goobers:ready` tag was added (ADO-N21). An item whose history cannot be
read in full (past Azure DevOps' 10,000-revision cap) is released and
skipped.

Only breadcrumbs written by the identity the credential authenticates as
count: the comment's `createdBy.id` must equal the `authenticatedUser.id` that
`connectionData` returns for the credential. A breadcrumb posted by any other
identity is ignored, and a claim fails if that identity cannot be read.

> **Rotating the identity can claim items twice.** Claims are matched by
> identity GUID, not by display name. If you switch the credential to a
> different identity (for example from a PAT to a service principal), the new
> identity ignores the old one's breadcrumbs, so it treats those items as
> unclaimed and may claim them again while an old-identity run still holds
> them. A release by the new identity removes the `goobers:claimed` tag but
> not the old breadcrumb. Let in-flight runs finish, or release their claims,
> before you rotate. Remove any leftover `goobers:claimed` tags by hand
> afterwards.

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
