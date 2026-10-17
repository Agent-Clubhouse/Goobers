# Enforce the merge-review verdict as a required check

A merge-review verdict that says a pull request needs changes has to stop every
way of merging it: a person pressing the merge button, the provider's own
auto-merge, and Goobers' `merge-pr`. A comment or a label cannot do that. The
provider's merge policy only sees status checks. So `merge-review` publishes
its verdict as a commit status, and the repository makes that status a
required check (#4199).

## Which verdict is authoritative

Only the `merge-review` workflow's verdict counts. It is the `review` gate
(the reviewer goober), as published by that workflow's `apply-verdict` stage
after it re-checks the verdict against the pull request's live head and base.
Nothing else publishes the status:

- An advisory-mode run (`advisoryMode: true`) posts its non-blocking comment
  only. It never sets the status.
- Review gates in other workflows, such as `pr-remediation`'s own checks, are
  inputs to remediation, not merge verdicts.
- The identity that publishes is the `github:pr:review` credential, the same
  identity that submits the native review. Pin the required check to that
  identity where the provider allows it (see GitHub below).

## What a success means

A success status means only that the authoritative review passed for that
head and base. It does not cover the rest of `merge-pr`'s landing conjuncts:
the pre-merge check, sibling election, the unanswered-feedback gate and Tutor
sign-off. A human merge or native auto-merge that relies on the status still
bypasses those. Keep them in the repository's own policy (required CI,
conversation resolution, required reviewers) where they matter for human
merges.

## The status contract

| | |
| --- | --- |
| Context | `goobers/merge-review` (GitHub, Gitea); `goobers/validation` (Azure DevOps) |
| `pass` | success |
| `needs-changes`, `fail`, `escalate` | failure |
| `defer` (waiting on a sibling PR) | pending on GitHub and Gitea; failure on Azure DevOps |
| Description | `<decision> for base <base SHA>: <summary>`, at most 140 characters |
| Target URL | the verdict comment, or the pull request |

The status is pinned to the head and the base:

- **Head.** On GitHub and Gitea the status is posted on the exact commit that
  was reviewed. A commit status belongs to one commit, so a verdict for an
  older head never reaches a newer one. A new push has no verdict status
  until merge-review reviews it, and a required check blocks it until then.
  On Azure DevOps a pass is posted on the reviewed iteration only (the stage
  fails if a push landed after the review). A non-pass is posted on the latest
  iteration, because a failure can only block and the next review replaces it.
- **Base.** The description records the base the review was computed against.
  `merge-pr` refuses a pass recorded for any base other than the one its own
  review pinned. When `apply-verdict` voids a review because the base moved
  under an unchanged head, it replaces the verdict status standing on that
  head with a pending `stale for base <sha>` status, which blocks until the
  next review. A head with no verdict status is left alone.
  A provider's merge button does not read the description. If the base moves
  without Goobers noticing, for example when a person merges another PR, a
  standing pass stays green for a native merge until merge-review next
  reviews that PR (#7461). Repositories that need strict base pinning for
  human merges should also require branches to be up to date before merging.
  Updating the branch creates a new head, and the new head has no verdict
  status until it is reviewed.

Publishing is idempotent. When the same state and description already stand
on the head, nothing is posted, so a PR re-reviewed every cycle does not
accumulate statuses toward GitHub's cap of 1000 per commit and context. If a
head ever reaches that cap, `apply-verdict` warns and the latest accepted
status still stands.

A later SHA-pinned `pass` replaces a failure on the same head. A stale verdict
never blocks a changed PR forever: the new head carries no old status, and the
next review of an unchanged head publishes a new status.

The verdict status is not CI. Goobers leaves it out of a pull request's CI
state, so a `needs-changes` verdict does not look like a failing build to
`ci-poll`, `gather-ci-failures`, or the CI-remediation lane.

## Overriding a failure

To land a pull request over a failing or pending verdict (for example, one
escalated to a person who decided it is fine), either:

- Resolve it the normal way: address the findings, or clear the escalation
  and let merge-review review the head again. A new pass replaces the failure.
- Use the provider's own override, such as an administrator bypassing the
  required check, or temporarily removing the requirement. Goobers' `merge-pr`
  still refuses that head while the status stands, so merge it by hand.

## Goobers' own landing path

`merge-pr` reads the verdict status from the same locked poll it lands from.
It refuses while the head's `goobers/merge-review` status is failure or
pending, or is a pass recorded for a different base. This applies whatever
the stage's `verdict` input or `commitMessage` says. A head with no verdict
status is not refused by `merge-pr`. In that case the repository's required
check does the refusing, and a credential that cannot write statuses (below)
does not stop landings that every other conjunct allows. Refusing that case
too is tracked in #7462. The combined status API does not say who posted a
status, so this conjunct trusts any writer of the context. That only adds
refusals. For native merges, the required check's expected source (below) is
what keeps other writers out.

## Missing-verdict shadow evidence

`merge-pr` records one `merge-verdict-shadow` runner annotation after each
locked live poll. This is observation-only: there is no require-mode flag,
new status publication, or changed landing/refusal decision. A missing status
still permits the existing landing path when all other gates permit it.

The event records `observation` (present, absent, unsupported, unreadable),
closed `reasonClass`, `pollPhase`, `verdictState`, `baseMatch` and `outcome`
fields, plus `landingAttempted` and `wouldBlock`. `wouldBlock` is true only
when a complete native-status read found no verdict and the existing path
attempted landing. It does not mean landing succeeded. Opt-out labels,
advisory mode, other gate refusals and unknown reads are excluded. Present
malformed or stale evidence is classified separately; existing refusal logic
continues to decide the outcome.

GitHub proves absence from explicit non-null status arrays across the pages
its existing poll reads. Gitea's current poll reads one [paginated combined-status page](https://docs.gitea.com/enterprise/api/22/operations/repo-get-combined-status-by-ref/):
absence requires an explicit array and `total_count` equal to its length.
Missing/null arrays, incomplete Gitea pages and unknown status shapes are
unreadable, even if the existing provider policy permits landing. ADO has no
native verdict reader on this path and is unsupported, not absent. These
observations reuse the existing poll; they perform no additional provider call.

Repository canonical identity, PR and actual/expected head/base SHAs are
subject evidence fields, not counter dimensions. Aggregate only the closed
provider/state/reason/outcome fields. Status descriptions, comment bodies,
tokens and raw provider/annotation errors are not copied to this event.
Annotation failures produce a fixed safe warning and leave the merge result
unchanged. The existing file journal or run-scoped pod journal plane carries
these events; the latter keeps its existing bounded emission timeout.

The daemon's ordinary instance-journal age compaction removes these annotations
using `telemetry.retention.window` (90 days by default). This is an age window,
not a byte/rate cap or deduplication policy: repeated invocations emit repeated
evidence, and a stopped or failing retention sweep cannot promise bounded disk
usage. Per-run artifact pruning has separate enable/grace controls; this change
does not change any retention policy. Operators should measure absent eligible
attempts separately from unsupported/unreadable and already-blocked attempts
before proposing enforcement. Native context-writer trust remains as described
above; this telemetry does not establish a new status authority.

## Configure the required check

### GitHub

1. Give the `github:pr:review` credential **Commit statuses: Read and write**.
   On a fine-grained PAT this is a repository permission. On a GitHub App
   installation, grant the App the same permission. Without it,
   `apply-verdict` logs `the review credential may not write commit statuses`
   on every verdict and publishes the comment and label only.
2. In the branch protection rule or ruleset for the target branch, enable
   **Require status checks to pass** and add `goobers/merge-review`. When the
   review identity is a GitHub App, choose that App as the check's expected
   source, so no other writer can satisfy the check.
3. Optional: enable **Require branches to be up to date before merging** for
   strict base pinning of human merges (see above).

If the branch uses a **merge queue**, GitHub also evaluates required checks on
the queue's merge-group commit. Goobers publishes the verdict only on the pull
request head, so requiring `goobers/merge-review` on a merge-queue branch
stalls the queue. On those branches, rely on `merge-pr` until Goobers
publishes the verdict for merge groups (#7460).

### Gitea

Give the review credential write access to repository statuses. Then, in the
branch protection for the target branch, enable status checks and add the
pattern `goobers/merge-review`.

### Azure DevOps

Azure DevOps keeps its existing context: `apply-verdict` publishes the verdict
as the PR iteration status genre `goobers`, name `validation`. Add a branch
policy **Status check** for that genre and name, and enable **Reset status
whenever there are new changes**, so each new iteration needs a new verdict.
The credential needs permission to contribute PR statuses. See
[Azure DevOps authentication](ado-authentication.md).

## Related

- [GitHub token scopes](github-token-scopes.md) for the `github:pr:review` grant.
- `goobers apply-verdict --help` and `goobers merge-pr --help` for the stage
  contracts.
