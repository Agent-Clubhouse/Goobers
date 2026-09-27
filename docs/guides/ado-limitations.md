# Azure DevOps limitations

DSL 2.0 supports Azure DevOps (ADO) without new DSL vocabulary and without
changing GitHub or Gitea behavior. Some scopes are intentionally out of
reach for v0.5.0; this guide lists them so a gaggle fails validation with a
clear reason instead of misbehaving at run time.

## Mixed-provider backlog and project (topology b)

A gaggle can keep its code in Azure DevOps and its backlog in a GitHub (or
Gitea) repository. Declare the backlog repository as `owner/name` and give
it its own `repos[]` entry, with a token or GitHub App, next to the ADO
repository:

```yaml
# gaggle.yaml
spec:
  project:
    provider: ado
    owner: example-org
    project: example-project
    name: example-repo
  backlog:
    provider: github
    project: example-org/example-backlog

# instance.yaml
repos:
  - provider: ado
    owner: example-org
    project: example-project
    name: example-repo
    auth:
      kind: azure-cli
  - provider: github
    owner: example-org
    name: example-backlog
    token:
      env: GOOBERS_BACKLOG_TOKEN
```

No new DSL is involved. Each stage is routed by role
(`docs/design/ado-parity-dsl-2-0.md` §7.2):

- **Backlog work goes to the backlog provider.** `backlog-query`,
  `issue-close-out`, the issue half of `post-merge`, `backlog-assignment`,
  `backlog-health`, `backlog-dedupe`, `check-issue-staleness`, the
  decomposition stages (`select-source`, `validate-plan`, `publish-batch`),
  `set-milestone`, and the daemon's park, failure and claim-release handlers
  read and write the GitHub issues. Claims are keyed by the backlog provider.
- **Credentials follow the capability family.** `github:issues:*` and
  `github:milestones:write` are backed by the backlog repository's `repos[]`
  entry. Pull-request and repository capabilities are backed by the ADO
  repository's. Without a backlog entry, a backlog capability has no
  credential and the stage fails. It is never given the ADO credential.
  The reverse holds too:
  - The ADO repository must match its `repos[]` entry exactly. The usual
    fallback to the first `repos[]` entry is off in topology (b).
  - A `daemonIdentity` (a GitHub PAT or App) backs only `github:issues:write`,
    for the backlog repository.
  - A stage that would open the ADO repository with a backlog-family
    credential is refused with an error instead of sending it to Azure DevOps.
  - **Caveat: an explicit `credentials:` entry still wins.** An instance
    `credentials:` entry for a pull-request or repository capability (for
    example `github:pr:write` or `repo:push`) replaces the ADO repository's
    credential for that capability, as it does everywhere else, so stages
    that declare it send that token to Azure DevOps. Goobers cannot tell
    from the entry which service issued the token. Use an Azure DevOps
    credential in such an entry, or remove it so the repository's own
    credential backs the capability. `validate` warns about each such entry
    while a gaggle is in topology (b) (CFG012). CFG012 is strict-neutral:
    `goobers validate --strict` prints it but does not fail on it.
- **Pull requests name the issue by URL.** On Azure DevOps, `#42` in a pull
  request description or squash commit message means ADO work item 42. So
  `open-pr` writes `Fixes https://github.com/example-org/example-backlog/issues/42`,
  and never `Fixes #42`. The ADO merge commit carries the same URL. After the
  merge, `post-merge` closes only issues that the pull request references by a
  URL into the backlog repository. A bare `#N` is ignored. In the other
  direction, the comment `post-merge` leaves on the GitHub issue names the
  ADO pull request by its URL. Any `#N` that `open-pr` copies from the
  backlog into the pull request is rewritten to that backlog issue's URL:
  in the title (which becomes the squash commit title), the issue title and
  acceptance criteria, and the reviewer's summary, rationale and findings.
- **GitHub pull-request extras are skipped.** In `backlog-query`, the open-PR
  eligibility backstop and contested-file ordering read GitHub pull requests,
  and there are none here, so they do not run. Native ADO work-item linking
  also does not run, because the item is not an ADO work item.

Some behaviour depends on what a stage declares:

- `issue-close-out` needs a pull-request credential (`github:pr:write`) to
  link the pull request in its close-out comment. Without one it still closes
  out the issue, but it writes a generic comment.
- `open-pr` re-checks that the claimed issue is still open only if it
  declares `github:issues:read`. Without it the re-check is skipped with a
  warning. The re-check fails open.

`validate` still refuses an Azure DevOps backlog for GitHub or Gitea code
(CFG010), because nothing in the gaggle names the backlog's ADO
organization. A GitHub or Gitea backlog whose `backlog.project` is not
`owner/name` is also refused (CFG010).

Mismatches between two non-ADO providers (for example a GitHub project with
a Gitea backlog) behave as before: `validate` warns (CFG011) and backlog
stages query the project provider. CFG011 is strict-neutral:
`goobers validate --strict` prints it but does not fail on it.

An Azure DevOps project split is unaffected and keeps working. That is the
case where the project repository is in one ADO project and
`spec.backlog.project` names a different ADO project.

Not yet covered in topology (b):

- `goobers run --continue` of a run that claimed a GitHub issue. It stops
  with an error naming the provider mismatch.
- Stage pods on the cluster substrate. A stage pod has no instance config, so
  it cannot route backlog work and credentials by role. The engine refuses
  every stage of a topology (b) run before a pod is created (failure code
  `cross_provider_backlog_pod_unsupported`). Place the gaggle's stages on a
  self runner.
- The daemon's terminal claim-marker release for a Gitea backlog. As on a
  plain Gitea gaggle, backlog curation reconciles the marker instead.
- A stage that is not routed by role and opens the Azure DevOps repository
  with a `github:issues:*` or `github:milestones:write` credential. That
  credential belongs to GitHub in topology (b), so the stage stops with the
  refusal above. Every shipped backlog stage listed above is routed by role.

## One gaggle, two code providers (topology c)

A single gaggle coordinating code repositories on both ADO and GitHub is
DSL 3.0 scope (`docs/design/provider-access-layer.md`), not this plan.

## Azure DevOps Server (on-premises)

Only Azure DevOps Services at `dev.azure.com` is supported. Legacy
`<organization>.visualstudio.com` repository and pull-request URLs are also
recognised (for example by `goobers connect`), since they address the same
service. Azure DevOps Server (on-premises) is out of scope.

## Service-hook triggers

Goobers polls Azure DevOps; it does not consume ADO service-hook events.

## Iterations (milestones)

Goobers does not model Azure Boards iterations in v0.5.0. `set-milestone`
refuses on ADO and suggests an Azure Boards iteration instead.
`goobers init --provider=ado` gives the curator Azure Boards instructions that
do no milestone or iteration housekeeping. Iteration paths are per-team trees,
so a numeric milestone does not map onto them cleanly.

## Work nomination and `file-issues`

The `file-issues` stage files GitHub issues only, so the `work-nomination`
workflow does not run on ADO. `goobers init --provider=ado` refuses
`--workflows=work-nomination` with that reason, and its default modules are
`implementation`, `backlog-curation` and `merge-review`. The browser wizard
uses the same defaults for an Azure DevOps repository and does not offer
work nomination.

## Other non-goals for DSL 2.0

These belong to DSL 3.0 (`docs/design/provider-access-layer.md`), not this plan:

- New capability names, provider-neutral renames, or aliases. ADO workflows
  keep the `github:*` capability names.
- Several credentials for the same provider in one gaggle or scope.
- Unifying the provider interfaces.
