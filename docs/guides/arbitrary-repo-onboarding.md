# Onboard an arbitrary repository (tiers 1-2)

This guide takes a GitHub repository that has never used Goobers through one
curation run and one implementation pull request. It also shows how to add a
second gaggle to the same local instance. It is the repository-neutral version
of the [self-hosting runbook](../../selfhost/README.md).

The guide uses the complete
[`config-examples/`](../../config-examples/) definitions as a starting point,
then removes workflows that are not needed for the first acceptance cycle.
Finish the single-repository path before adding another gaggle.

## 1. Prepare the host and target repository

Install or build:

- `goobers`, `git`, and the GitHub Copilot CLI on the daemon's `PATH`.
- `gh` for the label and test-issue commands below.
- The target repository's build, test, and lint tools on the daemon's `PATH`.

The target repository needs:

- An enabled Issues backlog.
- A default branch that the token can clone and branch from.
- A non-interactive command that represents local CI.
- GitHub CI on pull requests, so the implementation workflow's `ci-poll`
  stage can reach a passing or failing state.

Keep branch protection and human review authoritative. The two workflows used
here open a pull request but never merge it.

Set these values for the examples:

```sh
export GOOBERS_SRC=/path/to/Goobers
export GOOBERS_INSTANCE="$HOME/goobers-widget"
export GOOBERS_TARGET="acme/widget-service"
```

`GOOBERS_SRC` is the Goobers source checkout containing `config-examples/`.
`GOOBERS_INSTANCE` is runtime state and must not be inside the target
repository.

## 2. Create least-privilege tokens

Use fine-grained personal access tokens and select only the target
repository. The repository token needs the permissions used by the two
workflows:

| Permission | Access | Used for |
|---|---|---|
| Contents | Read and write | Clone, push the run branch |
| Issues | Read and write | Query, claim, label, and comment |
| Pull requests | Read and write | Open the implementation PR and poll it |
| Checks | Read-only | Observe PR CI check runs |
| Commit statuses | Read-only | Observe PR CI commit statuses |

Agentic stages need a separate personal fine-grained token with **Copilot
Requests: Read-only**. It needs no repository access. For the full rationale,
cross-organization constraints, and rotation guidance, see
[GitHub token scopes](github-token-scopes.md).

Export the values in the shell that will start `goobers up`; never put token
values in YAML:

```sh
export GOOBERS_GITHUB_TOKEN=github_pat_...
export GOOBERS_COPILOT_TOKEN=github_pat_...
```

`GOOBERS_COPILOT_TOKEN` is the source named by `instance.yaml`. Goobers injects
it as `COPILOT_GITHUB_TOKEN` only into agentic subprocesses that declare
`agent:model`.

The harness preflight intentionally runs with a default-deny base environment,
so it does not inherit an ambient `COPILOT_GITHUB_TOKEN`. Before validation,
sign in once with the same OS account that will run the daemon:

```sh
copilot login
```

Complete the device flow and keep that account's credential store (or
`~/.copilot/` fallback) persistent. `validate --check-harness` and daemon
startup use this stored sign-in; live agentic stages use the capability-scoped
token from `instance.yaml`.

The reviewer in this guide is an agentic gate that returns a journaled verdict;
it does not submit a native GitHub review. A separate
`github:pr:review` credential is only needed if you later enable a workflow
that submits native reviews.

Preflight the repository token before changing the instance:

```sh
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh repo view "$GOOBERS_TARGET"
```

## 3. Initialize the instance

```sh
goobers init "$GOOBERS_INSTANCE"
rm -rf "$GOOBERS_INSTANCE/config"
cp -R "$GOOBERS_SRC/config-examples" "$GOOBERS_INSTANCE/config"
```

`init` creates the instance root and runtime directories. Replacing only its
seeded `config/` preserves `instance.yaml`, journals, scheduler state, and
telemetry.

Configure `instance.yaml` for the target. Split `GOOBERS_TARGET` into its owner
and repository name; environment variables are not expanded inside YAML.

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: widget-service
    token:
      env: GOOBERS_GITHUB_TOKEN
credentials:
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN
telemetry:
  enabled: true
runner:
  livenessTimeout: 2m
timezone: America/Los_Angeles
runConditions:
  maxParallelRuns: 1
  stalledRunTimeout: 45m
  claimsLockTimeout: 30s
```

`runner.livenessTimeout` defaults to two minutes and marks a daemon unhealthy
when its scheduler tick heartbeat in `scheduler/up.lock` is older than that.
`timezone` is an IANA location used for every workflow schedule; omit it for
UTC. Token references may use `token.file` instead of `token.env`, but never an
inline token value. `stalledRunTimeout` defaults to 45 minutes and escalates a
running journal that has received no event or stage heartbeat for that period.
`claimsLockTimeout` defaults to 30 seconds and bounds cross-process claim-ledger
lock acquisition.

## 4. Make the workforce repository-specific

The reference config contains curation, implementation, nomination, merge, and
sample workflows. Start with only the two needed for this walkthrough:

```sh
rm -f \
  "$GOOBERS_INSTANCE/config/gaggles/acme-web/workflows/default-implement.yaml" \
  "$GOOBERS_INSTANCE/config/gaggles/acme-web/workflows/merge-review.yaml" \
  "$GOOBERS_INSTANCE/config/gaggles/acme-web/workflows/todo-check.yaml" \
  "$GOOBERS_INSTANCE/config/gaggles/acme-web/workflows/work-nomination.yaml"
rm -rf \
  "$GOOBERS_INSTANCE/config/gaggles/acme-web/goobers/coder" \
  "$GOOBERS_INSTANCE/config/gaggles/acme-web/goobers/nominator"
```

Use an editor to make the following changes. The config remains valid only
when directory names and references agree.

1. Rename `config/gaggles/acme-web/` to a stable gaggle identifier such as
   `widget`.
2. In `config/manifest.yaml`, replace `acme-web` in `spec.gaggles` with
   `widget`. Give the manifest and instance meaningful names. For local
   tiers, remove the example `secretRef.keyVault` values; `secretRef.name` is
   a connection label, while the actual token source remains in
   `instance.yaml`.
3. In `gaggles/widget/gaggle.yaml`, set `metadata.name: widget`, the project
   owner/name/default branch, and `spec.backlog.project:
   acme/widget-service`. Keep its connection refs aligned with the two
   connections in the manifest.
4. Replace every remaining `spec.gaggle: acme-web` with `spec.gaggle:
   widget`.
5. Add `agent:model` to `spec.capabilities` in the retained curator,
   implementer, and reviewer goober definitions. Preserve their existing
   grants.
6. In `goobers/reviewer/goober.yaml`, remove `merge-review` from
   `spec.workflows` because that workflow was removed.
7. Replace references to "Acme Web" in each `instructions.md`. Tell the
   implementer and reviewer where the repository's conventions live, which
   fast targeted checks to use, and what changes are out of scope.
8. In `workflows/implementation.yaml`, replace
   `command: ["make", "ci"]` in the `local-ci` stage with the target
   repository's real non-interactive CI command.

The goober capability lists should retain their workload grants while adding
the model credential:

```yaml
# curator
capabilities:
  - agent:model
  - github:issues:write

# implementer
capabilities:
  - agent:model
  - repo:push

# reviewer
capabilities:
  - agent:model
```

Also add `agent:model` to each retained agentic task's capabilities:

```yaml
# workflows/backlog-curation.yaml: curate
capabilities:
  - agent:model
  - github:issues:write

# workflows/implementation.yaml: implement
capabilities:
  - agent:model
  - repo:push
```

The agentic review gate has no task-level capability list; it receives
`agent:model` from the reviewer goober definition.

The resulting config should have this shape:

```text
config/
  manifest.yaml
  gaggles/
    widget/
      gaggle.yaml
      goobers/
        curator/
        implementer/
        reviewer/
      workflows/
        backlog-curation.yaml
        implementation.yaml
```

Search for placeholders before proceeding:

```sh
grep -R -n -E 'acme-web|Acme Web|owner: acme|name: web|acme/web' \
  "$GOOBERS_INSTANCE/config" || true
```

Review the two workflow definitions rather than treating them as opaque
templates:

- Keep `inputs.trustLabel: "goobers:approved"` in both query stages. It is the
  fail-closed boundary between untrusted issue text and an agentic stage.
- Keep implementation's `requireLabels: "goobers:ready"` and
  `excludeLabels: "goobers/status:in-review"`.
- Keep the `review` agentic gate and its `needs-changes` branch back to
  `implement`.
- Add `maxRunsPerDay: 2` under implementation's `readiness` while proving the
  integration. Adjust cron schedules only after the manual acceptance run.
- Do not add a merge stage. A human decides whether to merge the first PR.

## 5. Bootstrap the label taxonomy

GitHub rejects attempts to apply labels that do not exist. Create the workflow
labels idempotently:

```sh
create_label() {
  GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh label create "$1" \
    --repo "$GOOBERS_TARGET" --color "$2" --description "$3" --force
}

create_label "goobers:approved" "0E8A16" "Maintainer-approved; eligible for agentic work"
create_label "goobers:ready" "1D76DB" "Curated and scoped; eligible for implementation"
create_label "goobers:claimed" "FBCA04" "Currently claimed by an in-flight run"
create_label "goobers:nominated" "5319E7" "Filed by a nominator; awaiting approval"
create_label "goobers:needs-human" "D93F0B" "Needs a human decision"
create_label "goobers/status:in-review" "BFDADC" "Implementation PR is awaiting merge"
create_label "type:bug" "D73A4A" "Defect in existing behavior"
create_label "type:feature" "A2EEEF" "New capability"
create_label "type:chore" "EDEDED" "Maintenance, tooling, or documentation"
create_label "tracking" "C5DEF5" "Tracks smaller implementation issues"
create_label "stale" "EEEEEE" "Awaiting confirmation after inactivity"
```

Also create the target repository's `area:*` labels and list them in the
curator instructions. The curator should under-tag rather than inventing a
label taxonomy during a run.

Only a maintainer should apply `goobers:approved`. The curator may add
`goobers:ready` or `goobers:needs-human`, but its instructions must continue to
forbid self-approval.

## 6. Validate before any live cycle

```sh
goobers validate "$GOOBERS_INSTANCE"
goobers validate --check-harness "$GOOBERS_INSTANCE"
```

Fix every error before starting the daemon. Typical foreign-layout failures
are a manifest gaggle with no matching directory, a workflow or goober whose
`spec.gaggle` still names the template, a missing instructions file, an
unknown capability, or a stale workflow name in `spec.workflows`.

Validation checks definitions and harness availability. The earlier `gh repo
view` confirms network access to the target; the first implementation's
`local-ci` stage confirms that the configured CI command actually works in an
isolated worktree.

## 7. Run one curation-to-PR acceptance cycle

Create a small, reversible, single-change issue suitable for the target
repository, then apply only the trust label:

```sh
ISSUE_URL=$(
  GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue create \
    --repo "$GOOBERS_TARGET" \
    --title "Document the widget health-check response" \
    --body "Add one short example to the existing health-check documentation."
)
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue edit "$ISSUE_URL" \
  --add-label "goobers:approved"
```

Run curation manually:

```sh
goobers run backlog-curation "$GOOBERS_INSTANCE"
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue view "$ISSUE_URL" \
  --json labels,comments
```

Expected curation behavior:

- It considers only issues carrying `goobers:approved`.
- It claims a batch, deduplicates/tags/scopes each item, and comments on every
  mutation.
- It marks the test issue `goobers:ready` when it is implementable, or
  `goobers:needs-human` with a specific requested decision.
- It releases the claim when the curation run finishes.

Resolve any requested human decision before continuing. Once the issue carries
both `goobers:approved` and `goobers:ready`, run implementation:

```sh
goobers run implementation "$GOOBERS_INSTANCE"
```

Expected implementation behavior:

1. Claims one approved, ready issue.
2. Invokes the implementer in an isolated worktree.
3. Records an independent reviewer-gate verdict; `needs-changes` returns to
   implementation with the rationale.
4. Runs the configured local CI command.
5. Pushes the run branch and opens one PR.
6. Polls GitHub CI and repasses on a real failure.
7. Comments on the issue and marks it `goobers/status:in-review` after CI
   passes. It does not merge the PR.

An empty curation or implementation query is a normal completed `no-work` run,
not a provider failure. If the manual run says there is no work, inspect the
issue labels first.

## 8. Observe and operate the daemon

Start scheduled cycles after the manual acceptance path works:

```sh
goobers up "$GOOBERS_INSTANCE"
```

In another terminal:

```sh
goobers status --daemon "$GOOBERS_INSTANCE"
goobers status --watch "$GOOBERS_INSTANCE"
goobers status --workflow implementation --limit 10 "$GOOBERS_INSTANCE"
goobers trace <run-id> "$GOOBERS_INSTANCE"
goobers trace --json <run-id> "$GOOBERS_INSTANCE"
goobers trace --transcripts <run-id> "$GOOBERS_INSTANCE"
```

For the acceptance run, the implementation trace should show the claim,
implement stage, reviewer verdict, local CI, branch push, PR open, CI poll, and
issue close-out in order. The run journal under `gaggles/<gaggle>/runs/<run-id>/` and the
instance journal under `scheduler/` are the durable sources; `status` and
`trace` render them.

The daemon prints a heartbeat unless started with `--quiet`. When GitHub
exhausts a primary or secondary rate limit, status reports when dispatch can
resume; the scheduler waits rather than spinning requests. Most scheduled
ticks should eventually be `no-work` once the backlog drains.

`instance.yaml` is read at startup. Restart after changing repositories,
credentials, timezone, or instance run conditions. Definition edits under
`config/` also require a restart unless `up --watch-config` was explicitly
enabled.

## 9. Stop safely

Press `Ctrl-C` in the foreground daemon, or send its exact process ID
`SIGTERM`. `goobers up` stops admitting work, asks in-flight runs to drain, and
checkpoints before exiting. If a stage exceeds the bounded drain window, the
next `goobers up` resumes the non-terminal run from its journal.

Do not use `kill -9`, delete `gaggles/*/runs/`, or delete `scheduler/` as a normal stop
procedure. Confirm shutdown with:

```sh
goobers status --daemon "$GOOBERS_INSTANCE"
```

## 10. Add a second gaggle for the same repository

The current local runtime resolves several built-in provider and cleanup stages
through the first `repos` entry. Until repository selection is gaggle-aware,
keep exactly one operational repository in each instance root. To operate
against another repository, repeat this guide with a separate instance root.

Multiple gaggles can safely share the configured repository. For example, add a
documentation gaggle with its own workflow names, budget, isolation identity,
and non-overlapping backlog labels. Update `instance.yaml` without adding a
second `repos` entry:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: widget-service
    token:
      env: GOOBERS_GITHUB_TOKEN
credentials:
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN
telemetry:
  enabled: true
timezone: America/Los_Angeles
runConditions:
  maxParallelRuns: 2
  stalledRunTimeout: 45m
  claimsLockTimeout: 30s
  workflowDailyBudgets:
    widget-implementation: 2
    widget-docs-implementation: 2
```

Duplicate the first gaggle directory as `config/gaggles/widget-docs/`, then:

1. Change the copied gaggle's directory, `metadata.name`,
   `spec.isolation.namespace`, `spec.isolation.identityRef`, every
   `spec.gaggle`, display names, and instruction text. Keep both its project
   and backlog on `acme/widget-service`. Give the isolation fields values
   unique to the second gaggle, such as `namespace: gaggle-widget-docs` and
   `identityRef: widget-docs-identity`; do not retain the first gaggle's
   values.
2. Give both gaggles' goobers and workflows globally unique names. For
   example, rename the goober directories and `metadata.name` values to
   `widget-curator` / `widget-docs-curator`, `widget-implementer` /
   `widget-docs-implementer`, and `widget-reviewer` /
   `widget-docs-reviewer`; rename the workflows to `widget-implementation` /
   `widget-docs-implementation` and their curation equivalents. Update task
   `goober` values, the review gate's `agentic.goober`, and each goober's
   `spec.workflows` references.
3. Leave the existing repository/backlog connections unchanged and add
   `widget-docs` to `config/manifest.yaml`:

   ```yaml
   spec:
     gaggles:
       - widget
       - widget-docs
   ```

4. Keep both gaggles' `project.connectionRef` and `backlog.connectionRef`
   pointed at those shared connections.
5. Route issues disjointly. Create `area:core` and `area:docs` in the target
   repository:

   ```sh
   create_label "area:core" "0052CC" "Routed to the core widget gaggle"
   create_label "area:docs" "0075CA" "Routed to the widget docs gaggle"
   ```

   In each workflow's `query-backlog` inputs, preserve `trustLabel` and the
   existing exclusions, then set these required labels:

   | Gaggle | Curation `requireLabels` | Implementation `requireLabels` |
   |---|---|---|
   | `widget` | `"area:core"` | `"goobers:ready,area:core"` |
   | `widget-docs` | `"area:docs"` | `"goobers:ready,area:docs"` |

   Apply exactly one routing area to each approved issue.
6. Re-run both validation commands and restart the daemon.

`goobers status` includes a `GAGGLE` column, and each run identity and telemetry
record carries its gaggle. Instance-level `maxParallelRuns` applies across all
gaggles; each workflow's `readiness` and the named daily budgets apply to that
workflow. Use unique workflow names in filters and manual runs:

```sh
goobers run widget-docs-backlog-curation "$GOOBERS_INSTANCE"
goobers status --workflow widget-docs-implementation "$GOOBERS_INSTANCE"
```

This completes the tier-1/2 onboarding path: the repository has an explicit
trust gate, independently routed workforce definitions and budgets, observable
curation/implementation cycles, and a safe daemon lifecycle.
