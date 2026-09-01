# Onboard an arbitrary repository (tiers 1-2)

This guide takes a GitHub repository that has never used Goobers through one
curation run and one implementation pull request. It also shows how to scale
the same local instance with more gaggles and repositories. It is the
repository-neutral version of the
[self-hosting runbook](../../reference-workflows/README.md).

The [quickstart](quickstart.md) is an optional disposable tutorial; it is not a
prerequisite for this production-oriented path. Use this guide when the target
is an existing repository whose real branch, CI, review, and credential
conventions must remain authoritative.

The guide uses the complete
[`config-examples/`](../../config-examples/) definitions as a starting point,
then removes workflows that are not needed for the first acceptance cycle.
Finish the single-repository path before adding another gaggle.

`goobers init --guided` follows the same convention: it loads the canonical
work-nomination, backlog-curation, and implementation modules from
`config-examples/gaggles/acme-web`, then adapts repository identity, branch,
issue scope, harness, CI command, and required capabilities from the choices
and evidence collected by the wizard. It does not reuse the deliberately
simplified `quickstart@v1` tutorial workflow.

This guide uses the recommended outside layout: a separate config source and
instance root, neither inside the target repository. Before choosing paths, see
[Choose where an instance and its config live](instance-placement.md) for the
supported placements, decision table, and trust implications.

## 1. Prepare the host and target repository

Install or build:

- `goobers`, `git`, and the GitHub Copilot CLI on the daemon's `PATH`.
- `gh` for the label and test-issue commands below.
- The target repository's build, test, and lint tools on the daemon's `PATH`.

See [Stack support](stack-support.md) for which languages have a shipped reference gaggle
today, and how a gaggle declares its own toolchain requirement (`ciCommand`,
`requiredCapabilities`) for any other stack.

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
export GOOBERS_INSTANCE="$HOME/goobers-widget"
export GOOBERS_CONFIG_SOURCE="$HOME/goobers-widget-config"
export GOOBERS_TARGET="acme/widget-service"
```

`GOOBERS_CONFIG_SOURCE` is the desired-state tree to version and review.
`GOOBERS_INSTANCE` is runtime state and must be separate from both the config
source and target repository.

## 2. Create least-privilege tokens

Use [GitHub's fine-grained personal access token settings](https://github.com/settings/personal-access-tokens/new).
When creating the token, **select the Resource owner that owns the target
repository** — for example, `odsp-microsoft` for a repository under that
organization — **rather than leaving your default personal account selected**.
Then choose **Only select repositories** and select exactly the target
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
export COPILOT_GITHUB_TOKEN="$GOOBERS_COPILOT_TOKEN"
```

`GOOBERS_COPILOT_TOKEN` is the source named by `instance.yaml`. Goobers injects
it as `COPILOT_GITHUB_TOKEN` only into agentic subprocesses that declare
`agent:model`. The separate `COPILOT_GITHUB_TOKEN` export lets the harness
preflight authenticate before that capability credential is resolved. The
preflight copies the ambient value only into its tool-disabled sign-in probe;
it does not expose the token to unrelated stages. Alternatively, run `copilot
login` as the daemon's OS account and persist that account's credential store.

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
goobers init --guided
```

Provide the existing local clone for `$GOOBERS_TARGET`. The tutorial discovers
GitHub or Azure DevOps identity, default branch, CI command, toolchain, and CLI
authentication. Choose `$GOOBERS_CONFIG_SOURCE` as a custom configuration
folder, or use the recommended peer folder suggested beside the target clone.
The tutorial never asks for token values.

For an agent-driven path, use:

```text
Use the Goobers Getting Started skill to inspect <target-path>, create the smallest
validated configuration at <config-path>, explain each write, and ask only when
required evidence or behavior cannot be safely derived.
```

The configuration folder does not have to be a Git repository, but Git is
recommended for review and history. Initialize and push it to GitHub or Azure
DevOps after generation when that governance model fits your team.

The checked-in source has this shape:

```text
instance.yaml.example
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

`instance.yaml.example` contains only credential locators such as environment
variable names. Journals, scheduler data, workcopies, credential values, and
`telemetry.db` remain under `$GOOBERS_INSTANCE`, never in the source tree.
Guided setup validates the source, materializes its definitions into the fresh
instance, validates the instance, and prints the exact
`config-repo -> instance/gaggle -> target-repo/backlog` mapping.
For later changes, author the checked-in source, stop the daemon, and run
`goobers config materialize "$GOOBERS_INSTANCE"` before restarting. The command
validates the recorded source, applies `instance.yaml.example`, `manifest.yaml`,
and `gaggles/` to the runtime instance, and leaves journals, scheduler data,
workcopies, credential values, and telemetry untouched.

## 4. Review the generated desired state

Make all workforce and instance-setting edits under
`$GOOBERS_CONFIG_SOURCE`; do not edit the materialized runtime copy. If the
source is a GitHub checkout, pull the desired revision before editing or
materializing it.

Search for placeholders before proceeding:

```sh
grep -R -n -E 'acme-web|Acme Web|owner: acme|name: web|acme/web' \
  "$GOOBERS_CONFIG_SOURCE" || true
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
create_label "goobers:needs-human" "D93F0B" "Decision only a human can make; not a status/parked state"
create_label "goobers:blocked-on-sibling" "C5DEF5" "Parked pending a named sibling issue/PR resolving; self-heals"
create_label "goobers:needs-remediation" "E99695" "Parked after a mechanical failure (repass exhausted, CI/infra failure); needs a fix, not a decision"
create_label "goobers:auto-close" "0E8A16" "Close a tracking issue after all children close"
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
forbid self-approval. The implementation workflow separately applies
`goobers:blocked-on-sibling` and `goobers:needs-remediation` when a run parks
for a status reason rather than a decision — see
[Needs-human label taxonomy](../design/needs-human-taxonomy.md) for the full
decision-vs-status model and which stage applies which label. Apply
`goobers:auto-close` to a `tracking` issue only when it should close
automatically after reconciliation verifies that all native and checklist
children are closed. Without that opt-in, reconciliation only removes the
completed parent's `tracking` label.

## 6. Validate before any live cycle

```sh
goobers validate --source-tree "$GOOBERS_CONFIG_SOURCE"
goobers config materialize "$GOOBERS_INSTANCE"
goobers validate "$GOOBERS_INSTANCE"
goobers validate --check-harness "$GOOBERS_INSTANCE"
```

The materialize command validates the source again and replaces only
`instance.yaml` and `config/`; running the explicit source validation first
keeps failures easy to diagnose. Fix every error before starting the daemon.
Typical foreign-layout failures are a manifest gaggle with no matching
directory, a workflow or goober whose `spec.gaggle` still names the template,
a missing instructions file, an unknown capability, or a stale workflow name
in `spec.workflows`.

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

Onboarding metrics, including any time-to-first-PR value derived from these
journals, stay in this local instance. Goobers sends no aggregate adoption or
usage feed to the project maintainers. A user may choose to self-report a
result, but there is no built-in reporting path (`SEC-048`).

For the acceptance run, the implementation trace should show the claim,
implement stage, reviewer verdict, local CI, branch push, PR open, CI poll, and
issue close-out in order. The run journal under `gaggles/<gaggle>/runs/<run-id>/` and the
instance journal under `scheduler/` are the durable sources; `status` and
`trace` render them.

The daemon prints a heartbeat unless started with `--quiet`. When GitHub
exhausts a primary or secondary rate limit, status reports when dispatch can
resume; the scheduler waits rather than spinning requests. Most scheduled
ticks should eventually be `no-work` once the backlog drains.

Author repositories, credential locators, timezone, and instance run
conditions in `$GOOBERS_CONFIG_SOURCE/instance.yaml.example`; author workforce
definitions under `$GOOBERS_CONFIG_SOURCE/gaggles/` and its `manifest.yaml`.
Stop the daemon, validate the source, run
`goobers config materialize "$GOOBERS_INSTANCE"`, and restart after changing
either. `up --watch-config` watches only the materialized runtime copy and is
not a substitute for applying the checked-in source.

## 9. Stop safely

Press `Ctrl-C` in the foreground daemon, or send its exact process ID
`SIGTERM`. `goobers up` stops admitting work, asks in-flight runs to drain, and
prints the remaining workflow/run IDs every 10 seconds while stages reach their
next checkpoints. Graceful drain waits indefinitely by default. Send a second
`SIGINT`/`SIGTERM`, or start with `--drain-timeout <duration>`, to terminate
in-flight stage process groups without a prompt; the next `goobers up` resumes
those non-terminal runs from their last durable checkpoints.

Do not use `kill -9`, delete `gaggles/*/runs/`, or delete `scheduler/` as a normal stop
procedure. Confirm shutdown with:

```sh
goobers status --daemon "$GOOBERS_INSTANCE"
```

## 10. Scale out with more gaggles and repositories

Repository selection is gaggle-aware. Each gaggle's `project` and `backlog`
connections resolve the instance `repos` entry whose provider, owner, and name
match, and provider stages inside a run receive that gaggle's repository
through the run environment. One instance root therefore supports both
scaling shapes:

- **More gaggles on the same repository.** Add a gaggle with its own workflow
  names, budget, isolation identity, and non-overlapping backlog labels. The
  worked example below adds a documentation gaggle.
- **One gaggle per additional repository.** Add a second `repos` entry and a
  gaggle whose `project` and `backlog` connections point at it, then confirm
  with `goobers validate --check-repos`. This is the recommended shape for
  repositories you operate together: one daemon, one journal, shared run
  conditions, and per-workflow budgets.

Separate instance roots remain the right choice when a repository needs an
isolation boundary — different credentials or trust postures, different
machines, or independent journals and budgets — not because of a repository
count. See [Choose where an instance and its config
live](instance-placement.md) for that decision.

Two constraints apply to either shape. Goober and workflow names are
instance-global, not gaggle-scoped, so a copied gaggle that keeps a name such
as `coder` fails validation with a duplicate-name error; prefix names per
gaggle as in step 2 below. And a new gaggle only claims work its backlog
labels actually match: the `goobers init` scaffold defaults the backlog labels
and `trustLabel` to `goobers`, which a real repository often does not carry,
and a workflow whose labels match nothing claims nothing without an error —
check `gh label list --repo <owner>/<name>` and set the trust label from
section 5 before the first cycle.

### Current single-repo residue

Three built-in behaviors still resolve through the first `repos` entry
regardless of gaggle. Account for them when a second repository shares the
instance:

- The open-PR poll behind `readiness.maxOpenPRs` counts the first repository's
  open PRs only, so the cap throttles every gaggle by that one count.
- Terminal branch-delete cleanup targets the first repository only; branches
  left by terminal runs against another repository are not deleted.
- The backlog counter that sizes scheduled work queries the gaggle's own
  repository but resolves its credential from the first `repos` entry; a
  second repository readable only by a different token can fail to count.

### Worked example: a documentation gaggle for the same repository

Multiple gaggles can safely share the configured repository. For example, add a
documentation gaggle with its own workflow names, budget, isolation identity,
and non-overlapping backlog labels. Update
`$GOOBERS_CONFIG_SOURCE/instance.yaml.example` without adding a second `repos`
entry:

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

Duplicate the first gaggle directory as
`$GOOBERS_CONFIG_SOURCE/gaggles/widget-docs/`, then:

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
   `widget-docs` to `$GOOBERS_CONFIG_SOURCE/manifest.yaml`:

   ```yaml
   spec:
     gaggles:
       - widget
       - widget-docs
   ```

4. Leave `project.connectionRef` and `backlog.connectionRef` out of both
   gaggles. The local runner resolves every access's token from `instance.yaml`
   `repos[]` by repository identity and never consults the field, so a
   declaration only earns a `REF012` finding (#3296).
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
6. Stop the daemon, run the source validation, materialization, and both
   instance validation commands from section 6, then restart it.

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

## Next: authoring workflows and custom stages

After the acceptance cycle works, tailor the workforce definitions in
`$GOOBERS_CONFIG_SOURCE`:

- **[Use the Goobers agent toolkit](dsl-authoring-skill.md)** — turn a
  plain-English process description into Gaggle, Goober, and Workflow
  definitions using the bundled `goobers-dsl-author` skill.
- **[Custom deterministic stage cookbook](custom-stage-cookbook.md)** — add
  inline scripts or in-repo project scripts as deterministic stages, with
  concrete examples from the `config-examples/` reference.
- **[Needs-human label taxonomy](../design/needs-human-taxonomy.md)** — the
  full decision-vs-status model and which stage applies which park label.
