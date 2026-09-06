# Goobers reference workflows (`reference-workflows/`)

"Begins to build itself" (`docs/ARCHITECTURE.md` §12, the V0 definition of
done): this is the **blessed reference config** — a canonical, proven-to-work
example of the config-as-code that lets a Goobers instance target the
Goobers repo itself (`Agent-Clubhouse/Goobers`). Copy it into your own
instance root and feed issues into the backlog to watch them get curated,
scoped, and implemented into PRs by the instance running on your own
machine. This directory describes a real, working pattern — it is **not**
itself a live instance, and it is not guaranteed to be byte-identical to any
specific deployment's actual running config (including whichever instance is
presently dogfooding this repo), which is maintained separately and can
drift from what's checked in here.

This directory is the **config repo content** (`manifest.yaml` +
`gaggles/goobers/`) plus a template `instance.yaml.example` and this guide.
It is not itself an instance root — `goobers init` creates one, and you point
its `config/` at this directory's contents (see below).

Complete the regular-instance steps in the
[canonical quickstart](../docs/guides/quickstart.md) first. The setup below
contains only the deltas for targeting the Goobers repository with its
self-hosting workflows.

## What's in here

The shipped tree loads **11 goobers and 11 workflows**.
<!-- reference-inventory: goobers=11 workflows=11 -->

| Goober role | Purpose |
|---|---|
| `analyst` | Diagnoses Tutor signals from telemetry and journals. |
| `config-author` | Drafts Tutor changes inside the confined config roots. |
| `curator` | Deduplicates, tags, scopes, and roadmaps approved issues. |
| `decomposer` | Designs read-only, validated delivery slices for oversized work. |
| `docs` | Updates documentation from a scoped signal. |
| `implementer` | Implements claimed issues and PR remediation in a worktree. |
| `nominator` | Files evidence-backed backlog items. |
| `quality-lead` | Collates the quality sprint's parallel findings. |
| `quality-researcher` | Audits one quality lens without write authority. |
| `reviewer` | Produces independent implementation and PR-lifecycle verdicts. |
| `test-quality-analyst` | Classifies recurring test failures and drafts bounded fix or quarantine findings. |

| Workflow | Purpose |
|---|---|
| `backlog-curation` | Curates maintainer-approved backlog items. |
| `decomposition` | Converts oversized approved work into validated child batches. |
| `docs-updater` | Turns a documentation signal into a reviewed PR. |
| `implementation` | Implements a ready issue and opens a PR. |
| `merge-review` | Reviews eligible PRs and, when explicitly enabled, lands them. |
| `pr-remediation` | Rebases or fixes managed PRs from CI and review evidence. |
| `quality-sprint` | Runs parallel quality audits and nominates findings. |
| `self-update` | Stages an operator-requested Goobers binary update. |
| `test-suite-quality` | Detects recurring flaky tests and nominates fix or bounded quarantine proposals. |
| `tutor` | Diagnoses run evidence and proposes confined config changes. |
| `work-nomination` | Nominates repository work from telemetry and repo signals. |

## Guardrails (confirmed, not just described)

These are load-bearing on a public repo and are enforced by the checked-in
config itself, not left to operator discretion:

- **`goobers:approved` trust-label gate is ON** in both backlog-consuming
  workflows (`backlog-curation`, `implementation`) — backlog item text is
  untrusted input (`SEC-047`); only a maintainer-applied label makes an item
  claimable. Nothing a nominator files is pre-approved — it lands unclaimed.
- **The reviewer gate is ON** in `implementation` — every implementation run
  gets an independent, no-write-capability reviewer verdict (`pass` /
  `needs-changes` / `fail`) before a PR can reach the CI-poll/repass loop.
- **Merging is an explicit, fail-closed opt-in.** `implementation` never
  merges; it ends after opening and checking its PR. The separate
  `merge-review` workflow grants `github:pr:merge` to its deterministic
  `merge-pr` stage, which is the checked-in instance's merge authority.
  Removing that task/grant and routing a published pass verdict to a terminal
  branch restores review-only operation. The command refuses to acquire
  undeclared merge authority.
- **Budgets are low by default.** `implementation` fires twice daily
  (`readiness.maxConcurrentRuns: 1`, `maxRunsPerHour: 1`, cadence 2x/day) — a
  hard ceiling of exactly 2 runs/day, via cadence × hourly cap (the same
  pattern `config-examples/gaggles/acme-web` documents; `WorkflowSpec` also
  has a native `maxRunsPerDay` field, #340, as a more direct alternative).
  Curation and nomination run 3x/day and 1x/day respectively, each capped at
  `maxRunsPerHour: 1`.
- **Capability grants are minimal per goober** and checked at validate time
  (fail-closed on mismatch — a workflow task using a capability its goober
  doesn't grant is a validation **error**, not a warning). Every agentic role
  has `agent:model`; write grants are limited to the role's job. `curator`
  has issue and milestone writes, `nominator` has issue writes plus a
  stage-disabled-by-default issue-approval grant, `implementer`, `docs`, and
  `config-author` have `repo:push`, and the remaining roles are read-only.
- **The Tutor is confined to this config root.** When the self-tuning Tutor
  loop (#104 / epic #36) opens a config-as-code PR, its `open-pr` stage checks
  the run's git diff and refuses (fail-closed, no PR opened) to touch anything
  outside it — platform code, CI, and credentials are unreachable through it.
  Since Tutor PRs land in this same repo as platform code, `.github/CODEOWNERS`
  owns `/reference-workflows/`. Persona/gate-tune changes may follow normal
  merge-review; workflow structure, skill body, and validation changes require
  a maintainer and are never auto-merged. Structural credential scoping rides #35. See
  [`docs/guides/tutor-write-boundary.md`](../docs/guides/tutor-write-boundary.md).

## Tokens and scopes

The shipped configuration has three credential paths:

| Environment variable | Required | Identity and purpose |
|---|---|---|
| `GOOBERS_GITHUB_TOKEN` | Yes | Repository identity used for provider reads/writes, branch pushes, and the opt-in merge path. |
| `GOOBERS_GITHUB_REVIEW_TOKEN` | Yes | Separate reviewer identity used only for native PR reviews. |
| `GOOBERS_COPILOT_TOKEN` | Headless only | Model identity for `agent:model`; an interactive installation can use the stored Copilot CLI sign-in instead. |

`GOOBERS_GITHUB_TOKEN` must be a fine-grained PAT scoped to
`Agent-Clubhouse/Goobers` only, with:

- **Issues:** read and write (curation, nomination, and the backlog-query
  stage all read/write issues, labels, and milestones).
- **Pull requests:** read and write (`open-pr`, `ci-poll`, review requests).
- **Contents:** read and write (managed branches, opt-in merge, and merged
  branch cleanup).
- **Checks / statuses:** read (CI-poll needs to see check results).
- **Actions:** read and write (`merge-review` admits pending CI for review and,
  after publishing a managed non-pass verdict, cancels only still-running
  Actions runs pinned to that exact reviewed head).

`GOOBERS_GITHUB_REVIEW_TOKEN` is a fine-grained PAT with **Pull requests:
read and write**, owned by a different GitHub identity. GitHub does not allow
the identity that authored a PR to approve it, so `github:pr:review` cannot
reuse the repository token for goober-authored PRs.

Neither token needs administration permission or permission to alter branch
protection. Keep required checks and branch protection enabled: `merge-pr`
uses the repository token only after every gate below passes and then
re-checks live provider state.

## Opt-in merge policy and gates

The checked-in `merge-review` workflow opts into autonomous landing. It does
not turn a model verdict directly into a merge:

| Gate/check | Merge posture |
|---|---|
| `issue-staleness-gate` | Stops when the linked issue changed after implementation. |
| `review` | Produces an independent verdict pinned to the selected head and base. |
| `elect-gate` | Resolves overlapping sibling ordering; election never bypasses verdict publication. |
| `advisory-verdict` | Makes PRs outside managed branch prefixes review-only. |
| `published-verdict` | Requires the native, separately authenticated review decision to be `pass`. |
| `cancel-pending-ci` | On a managed non-pass verdict, cancels a bounded set of pending Actions runs only when the PR still points at the exact reviewed SHA; failures never undo the verdict. |
| `scope-gate` | Prevents an oversized or parked PR from reaching the landing command. |
| `merge-opt-out-gate` | Honors a late `goobers:no-merge-review` opt-out. |
| `merge-gate` | Routes only an actual merge or queue enrollment onward; refusals remain unmerged. |
| `queue-opt-out-gate` | Stops a dequeued PR after it opts out. |
| `queue-gate` | Requires the merge queue's eventual `merged` outcome before post-merge work. |

Immediately before landing, `merge-pr` serializes the poll/decide/merge
window and independently re-checks the `github:pr:merge` grant, pass verdict,
required CI, draft state, head SHA, relevant base movement, Tutor human-signoff
rules, run-aborted and no-merge labels, and single-lander evidence for
overlapping PRs. GitHub branch policy determines direct merge versus merge
queue. Only a confirmed merge reaches `post-merge` to close linked issues,
fan out remediation, and clean up the managed branch.

## Apply the self-hosting configuration

After the canonical quickstart has created and validated a regular instance:

1. **Replace the seeded config with this one:**

   ```sh
   rm -rf ~/goobers-instance/config
   mkdir -p ~/goobers-instance/config
   cp -r reference-workflows/gaggles ~/goobers-instance/config/
   cp reference-workflows/manifest.yaml ~/goobers-instance/config/
   cp reference-workflows/instance.yaml.example ~/goobers-instance/instance.yaml
   ```

2. **Sign in and set repository tokens** (never inline them into
   `instance.yaml` — the loader rejects that, `CFG-009`/`SEC-010`):

   ```sh
   export GOOBERS_GITHUB_TOKEN=ghp_...
   export GOOBERS_GITHUB_REVIEW_TOKEN=github_pat_...
   copilot # sign in once; the local daemon reuses this stored session
   ```

   PowerShell:

   ```powershell
   $env:GOOBERS_GITHUB_TOKEN = "ghp_..."
   $env:GOOBERS_GITHUB_REVIEW_TOKEN = "github_pat_..."
   copilot # sign in once; the local daemon reuses this stored session
   ```

   For a headless service or CI account, configure the commented
   `agent:model` entry in `instance.yaml` and set
   `GOOBERS_COPILOT_TOKEN` to a fine-grained PAT with Copilot Requests:
   Read-only.

3. **Validate the self-hosting definitions:**

   ```sh
   goobers validate ~/goobers-instance
   # OK: instance.yaml valid; config/ valid (1 gaggle(s), 11 goober(s), 11 workflow(s))
   ```

4. **Bootstrap the label taxonomy** on the target repo (idempotent — safe to
   re-run; `gh label create --force` creates or updates in place):

   ```sh
   for l in \
     "goobers:approved:0E8A16:Maintainer-approved — eligible for curation/implementation (SEC-047)" \
     "goobers:ready:1D76DB:Curated and scoped — eligible for implementation" \
     "goobers:claimed:FBCA04:Currently claimed by an in-flight run" \
     "goobers:nominated:5319E7:Filed by the nominator — awaiting maintainer approval" \
     "goobers:needs-human:D93F0B:Needs a decision only a human can make" \
    "goobers:auto-close:0E8A16:Close a tracking issue after all children close" \
   ; do
     IFS=: read -r ns name color desc <<<"$l"
     gh label create "$ns:$name" --color "$color" --description "$desc" \
       --repo Agent-Clubhouse/Goobers --force
   done
   gh label create goobers --color 006B75 \
     --description "Scopes which issues count as a gaggle's backlog (gaggle.yaml backlog.labels)" \
     --repo Agent-Clubhouse/Goobers --force
   ```

   The bare `goobers` label is the gaggle's backlog scope — `gaggle.yaml` sets
   `backlog.labels: [goobers]`, a hard AND-filter — so apply it to every issue
   the gaggle should treat as backlog; an issue without it is invisible to
   every backlog-consuming workflow.

5. **Start the daemon** (scheduler + runner + telemetry rollup):

   ```sh
   goobers up ~/goobers-instance
   ```

   Collector push is optional. To inspect local traces in Jaeger, follow the
   [Jaeger quickstart](../docs/guides/jaeger-quickstart.md).

## What to expect per cycle

- **Curation** (03:22 / 11:22 / 19:22 local): claims up to 20
  `goobers:approved` items with neither output marker, dedupes/tags/splits
  them, and marks each `goobers:ready` or `goobers:needs-human` with an
  explanatory comment.
- **Implementation** (08:17 / 20:17 local, ≤2/day): claims exactly one
  `goobers:approved` + `goobers:ready` item, implements it in an isolated
  worktree, passes it through the reviewer gate and a local `make ci` gate,
  opens a PR, polls CI with a bounded repass loop (repassing to the
  implementer on `needs-changes` or a CI failure), and comments on the
  originating issue once CI passes. It stops there; landing belongs to the
  independently triggered `merge-review` workflow.
- **Nomination** (06:41 local, once daily): reviews telemetry/repo signals,
  checks the existing backlog for duplicates, and files well-evidenced
  issues carrying `goobers:nominated` + an evidence footer. Filed issues are
  **not** pre-approved — a maintainer reviews and applies `goobers:approved`
  before curation will touch them.
- **Decomposition** (04:53 local, once daily, or on direct invocation): selects
  the same oldest eligible escalation, designs and validates bounded single-PR
  slices, and publishes them only after deterministic validation.
- **PR lifecycle:** `merge-review` reviews and may land eligible managed PRs;
  `pr-remediation` rebases or fixes PRs that need work.
- **Maintenance:** `docs-updater` handles documentation signals,
  `quality-sprint` nominates findings from parallel audits,
  `test-suite-quality` nominates recurring flaky-test fixes or bounded
  quarantines, `tutor` proposes confined config improvements, and
  `self-update` stages requested binary updates. Their trigger and budget
  details live in their workflow YAML.

## Observing a run

```sh
goobers status                 # instance overview: workflows, next cron fires, active/recent runs
goobers trace <run-id>          # a run's full journal: timeline, stages, attempts, gate verdicts, artifacts
goobers trace --json <run-id>    # same, for scripting
```

Every scheduling decision and claim-ledger transition is also inspectable
directly with standard tools — the journal is human-readable first
(`docs/requirements/*`, `internal/journal/README.md`,
`internal/localscheduler/README.md`):

```sh
cat ~/goobers-instance/runs/<run-id>/events.jsonl | jq -c '{seq, type, stage, status}'
jq -c 'select(.type=="trigger.fired" or .type=="tick.skipped")' \
  ~/goobers-instance/scheduler/events.jsonl
```

Everything the instance did — every claim, trigger, run, stage attempt, gate
verdict, and artifact — is reconstructible from this journal alone; `goobers
trace` is a rendering of it, not a separate source of truth.

## Stopping safely

`goobers up` drains on `SIGTERM`: it finishes the current stage attempt,
checkpoints, and exits — `Ctrl-C` or `kill <pid>` is safe at any time. A
restart resumes any non-terminal run from its last completed stage (journal
replay) and picks the cron schedule back up from its last known fire (the
embedded scheduler's missed-tick policy collapses any downtime into at most
one catch-up run per workflow, never a backlog replay).

## Sensitive-info check

Nothing in this directory contains a token, a personal path, or a
machine-specific value — `instance.yaml.example`'s `token.env` is an
environment variable **name**, not a value, and the instance root itself is
never checked in (you choose it at `goobers init` time). Safe to keep this
directory in version control as-is.
