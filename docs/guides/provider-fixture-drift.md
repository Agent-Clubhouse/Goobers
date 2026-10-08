# Provider fixture drift

The provider fixture drift workflow refreshes the read-only GitHub issues and
pull-request contract request sets, replays the normalized responses through
the real provider, and compares them with the hermetic fixtures committed at
`test/providers/testdata/github_contract.json` and
`test/providers/testdata/github_pr_contract.json`.

The workflow is intentionally inert until #1478 is complete. It is available
only through `workflow_dispatch`; the schedule remains commented out, and a
manual run fails with a clear provisioning error before any live request when
one of these dedicated settings is absent:

- repository variable `PROVIDER_FIXTURE_REPOSITORY` (`owner/name`);
- repository variable `PROVIDER_FIXTURE_ISSUE` (the stable seeded issue);
- repository variable `PROVIDER_FIXTURE_PR` (the stable seeded pull request);
- Actions secret `GH_READONLY_VALIDATION_PAT` (Issues and Pull requests
  read-only access to that repository).

Do not substitute the ambient Actions token. Provision the designated fixture
repository and least-privilege credential, reconcile the first candidate, then
uncomment the workflow schedule as the final #1478 enablement step. This
reporting workflow is not a required merge check and must not be added to the
required CI aggregate.

## Refresh locally

Use a temporary output path so a live response never overwrites the baseline
before review:

```sh
export GOOBERS_PROVIDER_FIXTURE_TOKEN='<dedicated read-only token>'
go run ./test/providerfixtures refresh \
  -repository owner/name \
  -issue 7 \
  -output /tmp/github-issue-provider-candidate.json
go run ./test/providerfixtures contract \
  -fixture /tmp/github-issue-provider-candidate.json
go run ./test/providerfixtures drift \
  -baseline test/providers/testdata/github_contract.json \
  -candidate /tmp/github-issue-provider-candidate.json
git diff --no-index \
  test/providers/testdata/github_contract.json \
  /tmp/github-issue-provider-candidate.json

go run ./test/providerfixtures refresh \
  -repository owner/name \
  -pull-request 8 \
  -output /tmp/github-pr-provider-candidate.json
go run ./test/providerfixtures contract \
  -fixture /tmp/github-pr-provider-candidate.json
go run ./test/providerfixtures drift \
  -baseline test/providers/testdata/github_pr_contract.json \
  -candidate /tmp/github-pr-provider-candidate.json
git diff --no-index \
  test/providers/testdata/github_pr_contract.json \
  /tmp/github-pr-provider-candidate.json
```

Issue refresh records the list-open-issues and get-issue requests. Pull-request
refresh records list-open-prs and get-pr; replay verifies creator, assignee, and
requested-reviewer mappings from those responses. Both rewrite the repository
identity, timestamps, database/node IDs, and rate-limit counters to stable
values. Tokens, authorization headers, dates, request IDs, and other
transport-only headers are never serialized.

## Respond to a failure

The workflow separates the two outcomes:

1. **Provider contract assertions** means the refreshed API response no longer
   decodes or maps through `providers.GitHubProvider` as required. Fix the
   provider or restore the designated fixture data; do not accept a new
   baseline merely to make this step green.
2. **Normalized fixture drift** means contract behavior still works, but
   material normalized response content differs. Download the candidate
   artifact, inspect the diff, and decide whether GitHub changed its contract
   or the fixture repository was edited unexpectedly.

For an intentional upstream change, update the provider and its assertions
first, rerun both checks, then replace the checked-in fixture with the reviewed
normalized candidate. Pull-request CI continues to replay only that checked-in
fixture and never receives live credentials or network access.

## Azure DevOps fixture

The separate `provider-fixture-drift-ado.yml` workflow applies the same
reporting-only contract and drift checks to Azure Boards. It records the
list-open-work-items and get-work-item provider paths, including ADO's WIQL,
`workitemsbatch` hydration and work-item-state requests, and compares them with
`test/providers/testdata/ado_contract.json`.

The workflow remains `workflow_dispatch`-only and is not part of required CI.
It reuses `ADO_ORG_URL`, `ADO_PROJECT`, and the `ADO_PAT` secret.

The workflow owns the seeded work item's lifecycle rather than pinning its
number. `refresh` without `-work-item` reads the oldest open work item titled
`goobers provider fixture (do not close)` and tagged `goobers-fixture`. The
workflow passes `-provision-fixture`, so when that item has been closed,
removed, or never seeded, the refresh creates a new one (of `-fixture-type`,
default `Issue`) and records it. Closed fixtures are never reopened or edited.
Re-running the workflow is therefore the recovery for a missing fixture, and
needs no repository change or repository variable. This needs a PAT with
work-item write scope. Without `-provision-fixture`, a missing fixture fails
with an error that names the flag.

The recorded listing is filtered to the `goobers-fixture` tag: the listing
returns the oldest open items first up to a limit, so in a busy project an
unfiltered one never reaches a recently seeded item, and it would churn with
every unrelated item anyway. The PAT is sent using ADO Basic authentication and
is never serialized.

The checked-in baseline is a synthetic placeholder until the first live
candidate is reviewed, so the drift step reports drift on the first live run.
Review the uploaded candidate artifact, then replace the baseline with it.

Refresh the ADO candidate locally with:

```sh
export ADO_PAT='<ADO PAT>'
go run ./test/providerfixtures refresh \
  -provider ado \
  -organization-url 'https://dev.azure.com/organization' \
  -project project \
  -output /tmp/ado-provider-candidate.json
go run ./test/providerfixtures contract \
  -fixture /tmp/ado-provider-candidate.json
go run ./test/providerfixtures drift \
  -baseline test/providers/testdata/ado_contract.json \
  -candidate /tmp/ado-provider-candidate.json
```

Add `-provision-fixture` to create the fixture when no open one exists, or pass
`-work-item N` instead to pin a specific item (a read-only PAT is enough for
either read path).

ADO normalization replaces the organization and project, identity GUIDs and
descriptors, revisions, timestamps, and rate-limit counters. It also records
the fixture's work-item number as `7`, so a recreated fixture with a new
number does not read as drift.

### Provision the seeded work item

`go run ./test/adolive provision` can also seed the work item this workflow
reads (#4602), with the same title, body, and tag the workflow uses. It only
creates the item when none with that title and tag exists in any state, so it
does not replace a closed fixture; the workflow's `-provision-fixture` does. It uses the same tool that provisions the live ADO write leg's
scratch repository (`ado-live-write.yml`, #5727). The tool is a dry run by
default: it reads the project and prints what it would create. Pass `-apply` to
create only what is missing. A second run finds everything and changes nothing.
The tool never updates or deletes an object, and it reads the token only from
`ADO_PAT`:

```sh
export ADO_PAT='<ADO PAT with code and work-item write scopes>'
go run ./test/adolive provision \
  -organization-url 'https://dev.azure.com/example-org' \
  -project example-project \
  -repository example-scratch \
  -apply
```

The tool:

- creates a blocking minimum-reviewers policy and a blocking
  `goobers-live/live-write` status policy, each scoped **exactly** to the
  scratch repository's `main` (`-base` overrides it);
- exits non-zero if any blocking policy covers `refs/heads/goobers-live/`,
  because such a policy would refuse every push the live leg makes. It never
  creates a prefix-scoped policy itself;
- creates a blocking "Require a merge strategy" policy scoped the same way, so
  the write leg can check how the provider classifies its evaluation (#6106).
  The leg never completes a pull request, so the policy never decides anything;
- finds or creates an open work item titled `goobers provider fixture (do not
  close)` and tagged `goobers-fixture` (`-fixture-type` picks the type; the
  default is `Issue`);
- finds or creates the spec fixture pair the read-only conformance leg
  (`ado-live-conformance.yml`) reads, both tagged `goobers-live-fixture`: an
  ancestry parent (`-parent-type`, default `Feature`) with a description, and
  a child of the project's requirement type (`-spec-type` overrides it) with
  an empty description, acceptance criteria, and a Hierarchy link to the
  parent. If the child exists without that link, the tool adds it, which is
  its only write to an existing object. It refuses to replace a link to a
  different parent;
- with `-ci-pipeline` only, finds or creates the `goobers-live-ci-failure`
  YAML build definition on the scratch repository. The write leg's CI failure
  evidence scenario (#5652) commits the definition's YAML, one step that fails
  on purpose, to its own `goobers-live/` branch and queues the build there, so
  nothing lands on `main`. It needs Azure Pipelines hosted parallelism in the
  organization and a PAT with Build (Read & execute). Without both, leave the
  flag off and that scenario stays skipped.

It ends by printing the repository variables to set:
`ADO_WRITE_REPOSITORY` for the live write leg,
`ADO_LIVE_SPEC_WORK_ITEM`
for the conformance leg's spec-fixture test, and, with `-ci-pipeline`,
`ADO_LIVE_CI_FAILURE_PIPELINE` for the write leg's CI failure scenario. A
repository admin sets them; the tool cannot. Until the last two are set, the
tests that need them skip with a notice rather than fail. The scratch repository must already exist, and it must
be a different repository from the testbed repository.
