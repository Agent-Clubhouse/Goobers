# Azure DevOps onboarding example

## What `init` produces

Scaffold a complete instance for your repository:

```sh
goobers init --template=standard --repo=example-org/example-project/example-repo --ci-command='["dotnet","test"]' --required-capabilities=dotnet@8 ./ado-instance
goobers validate --strict ./ado-instance
```

Replace `example-org/example-project/example-repo` with your real
organization/project/repository. The .NET command is an example, not an ADO
requirement; `--pr-ci` uses the pull request's own CI instead. Without `--repo`,
`--provider=ado` writes `your-org`/`your-project`/`your-repo` placeholders,
which `validate` reports until you replace them.

With `--provider=ado` (or an ADO `--repo`), `init` writes:

- **Workflows.** `implementation`, `backlog-curation` and `merge-review`, with
  the `implementer`, `reviewer` and `curator` goobers. `merge-review` reviews
  and lands the pull requests `implementation` opens, under your branch
  policies. `--workflows` selects a subset. `work-nomination` is refused on ADO,
  because its `file-issues` stage files GitHub issues only.
- **Authentication.** The repository uses `azure-cli` by default, so no PAT or
  token variable is needed on a developer machine signed in with `az login`.
  `--repo-auth-kind` also accepts `workload-identity`, `managed-identity` and
  `pat`; only `pat` records a token variable (`GOOBERS_ADO_TOKEN`). The
  scaffold adds no `envPassthrough`.
- **Instructions.** The curator gets Azure Boards instructions: labels are
  work-item tags, dependencies are Predecessor links, and iterations are not
  managed (`set-milestone` is not used on ADO).

The generated repository entry in `instance.yaml` is:

```yaml
repos:
  - provider: ado
    owner: example-org
    project: example-project
    name: example-repo
    auth:
      kind: azure-cli
```

See [ADO limitations](../../docs/guides/ado-limitations.md) for what is not
supported on ADO.

## PAT onboarding with `connect`

`goobers connect` fills in a placeholder scaffold and records PAT
authentication:

```sh
goobers init --template=standard --provider=ado --ci-command='["dotnet","test"]' --required-capabilities=dotnet@8 ./ado-instance
goobers connect contoso/platform/widgets ./ado-instance
```

Set `GOOBERS_ADO_TOKEN` securely in your environment; the instance records only
the variable name. `connect` switches the placeholder repository to:

```yaml
repos:
  - provider: ado
    owner: contoso
    project: platform
    name: widgets
    auth:
      kind: pat
    token:
      env: GOOBERS_ADO_TOKEN
```

[`gaggle.yaml.example`](gaggle.yaml.example) is a complete gaggle for these exact
example coordinates. Copy it to `ado-instance/config/gaggles/example/gaggle.yaml`
and adapt its repository coordinates, Boards project, branch and toolchain.
Keep the generated manifest, workflows, goobers and instance credential grants;
the gaggle alone is not a complete instance.

## Boards backlog

The Boards `backlog.project` can differ from the repository project. The
backlog can also live in a GitHub repository instead of Boards
(`backlog.provider: github`, `backlog.project: owner/name`), with its own
`repos[]` entry for the backlog credential; see
[ADO limitations](../../docs/guides/ado-limitations.md) for that mixed
topology.

A work item with a predecessor link is not claimed until the predecessor is
done. By default a predecessor in the Resolved, Completed or Removed state
category is done, so a Resolved Bug no longer blocks. A team whose process
uses Resolved as "awaiting QA" can override this in the gaggle's backlog
block; `byType` state names take precedence for that type:

```yaml
backlog:
  provider: ado
  project: example-project
  doneStates:
    categories: [Resolved, Completed, Removed]   # default when omitted
    byType:
      Bug: [Closed]
```

`validate` rejects an unknown category name. GitHub and Gitea backlogs accept
`doneStates` and ignore it.

Check both Git and Boards access, then optionally seed one tagged Task:

```sh
goobers validate --strict --check-repos ./ado-instance
goobers connect contoso/platform/widgets --seed ./ado-instance
```

See [production onboarding](../../docs/guides/arbitrary-repo-onboarding.md#3-initialize-the-instance)
for seeding bounds and tag semantics, and [ADO authentication](../../docs/guides/ado-authentication.md)
for PAT scopes, other authentication sources and PR-status evidence.
