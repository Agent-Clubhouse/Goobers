# GitHub provider fixture drift

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
