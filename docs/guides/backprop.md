# Backprop: attribute run outcomes and verify fixes (preview)

Backprop explains *why* a workflow run ended the way it did. When an enrolled
run finishes, Goobers projects the run journal into a credit graph (run,
stages, nested agents, model invocations, tool calls, evaluators, runtime and
environment), assigns each element a signed share of the outcome with an
explicit uncertainty, and classifies the likely cause of every failing stage.
A read-only fault auditor then groups repeated failure signatures across runs
and workflows and names the most likely owner: Goobers itself, an external
component (harness, model, provider, environment), or the workflow definition.

Backprop is **report-only**. It never changes a run's phase, gate verdict, CI
admission, or publication; it never files issues, edits workflows, or rolls
back runs. The contract and its limits are specified in the
[credit graph design](../design/credit-graph.md).

> Not to be confused with `goobers config templates backprop`, which copies
> runtime edits of a templated gaggle back into your config checkout. See
> [Tracked gaggle templates](gaggle-templates.md).

## Preview status

Backprop is a **preview** feature (`workflow.spec.backprop.enabled` and
`workflow.spec.backprop.version` in the [feature matrix](../feature-matrix.md)).
Today:

- Enrollment is **per workflow** only. There is no gaggle-wide switch and no
  shadow mode over unenrolled workflows yet.
- Attribution is computed **once, at run end**. There are no per-stage
  checkpoints.
- Learning uses only what the run journal recorded. You cannot supply your own
  ground truth (for example, "this merged PR was later reverted") yet.
- Cohorts and fault-audit findings are JSON only: the daemon's telemetry stats
  read API and `goobers telemetry-query`. The `goobers telemetry stats` CLI
  (text or `--json`) reads only the local rollup and does not include them.

These gaps are tracked under the Backprop scopes epic
([#7121](https://github.com/Agent-Clubhouse/Goobers/issues/7121)).

## Enable Backprop for a workflow

Backprop requires `dslVersion` `"3.0"` or `"3.1"`. Both are **preview** DSL
versions, so the workflow must also carry its own preview acknowledgement;
without it validation refuses the pin (`DVL011`). Add both to the workflow
definition (other fields elided):

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: implementation
  annotations:
    goobers.dev/allow-preview-features: "true"
spec:
  backprop:
    enabled: true
    version: v1
```

- `enabled` and `version` are both required. `v1` is the only supported
  attribution contract.
- Omitting `backprop`, or setting `enabled: false`, performs no attribution
  work.
- The preview acknowledgement belongs on each Workflow. A Manifest-level
  annotation is deprecated and does not authorize anything.
- A workflow pinned to an older `dslVersion` that declares `backprop` fails
  validation (`WF010`). Preview the mechanical migration with
  `goobers fix --to 3.0`, then apply it with `goobers fix --to 3.0 --write`.
  DSL 3.0 changes more than Backprop (for example, a stage that reuses an
  earlier stage's repo workspace must declare `repoFrom`), so review the diff
  and fix any remaining `goobers validate` errors.

Validate the configuration, then confirm enrollment:

```sh
goobers validate ./instance
goobers status ./instance
```

The workflow summary in `goobers status` prints
`backprop: enabled (v1)` beneath each enrolled workflow.

Only runs that **start** after enrollment are attributed. Runs that finished
earlier are not backfilled.

## Read one run's attribution

After an enrolled run terminalizes, Goobers writes `attribution.json` beside
the run journal. Its `status` is one of:

| Status | Meaning |
|---|---|
| `complete` | Attribution ran over enough recorded evidence. |
| `insufficient-evidence` | Attribution ran, but too much of the run was unrecorded to trust it; treat the result as a hint. |
| `failed` | Analysis itself failed (`failure` says why). The run's own outcome is unaffected. |

`goobers trace` shows the status and contract version in its summary, and the
full record under `attribution` in JSON output:

```sh
goobers trace --summary <run-id> ./instance
goobers trace --json <run-id> ./instance
```

```text
backprop: complete (v1)
```

In `attribution.json`, `attribution.contributions` lists every graph node with its
`share` of the outcome, a signed `score` (positive when the element is
estimated to have helped, negative when it hurt; a tool that succeeded inside
a failing run scores positive), `uncertainty`, `confidence`, and `provenance`
(`recorded` or `unknown`). `attribution.causes` lists failure-cause findings
for failing stages using a fixed taxonomy: `bad-tool-choice`,
`bad-tool-result`, `bad-interpretation`, `weak-instructions`, `routing`,
`model`, `topology`, `environment`, and `unknown`. Each cause names its
assumptions and evidence. Missing provenance is never turned into blame: it
yields an `unknown` cause with reduced confidence. In `goobers trace --json`
output the whole record sits under the top-level `attribution` key, so these
fields are at `attribution.attribution.contributions` and
`attribution.attribution.causes`.

`goobers trace --api=<url>` (a remote trace) does not include attribution.

## Read cohort attribution and fault-audit findings

Cohorts and findings are computed from stored `attribution.json` records by
two read surfaces:

- **The daemon's telemetry stats API** (`GET /api/v1/telemetry/stats`). This
  is the read-only view: it never applies a report cooldown, so looking at
  findings here never hides them from the filing pass. Narrow it with the
  `gaggle`, `workflow`, `since`, and `until` (RFC3339) query parameters. On a
  default local instance the API listens on `127.0.0.1:8080` without
  credentials (see [Tier-2 OIDC authentication](oidc-authentication.md) for
  exposed listeners):

  ```sh
  curl "http://127.0.0.1:8080/api/v1/telemetry/stats?gaggle=default&workflow=implementation&since=2026-10-01T00:00:00Z"
  ```

- **`goobers telemetry-query`**, the candidate-findings connector. It includes
  the same fields when the `credit-assignment` aggregate is evaluated (the
  default when no `--aggregate` is given). This is the pass that feeds
  filing: it records a 24-hour report cooldown per gaggle and workflow, so an
  open finding it reports is omitted (and counted in `suppressed`) by later
  `telemetry-query` runs inside that window. Prefer the stats API for ad-hoc
  inspection.

  ```sh
  goobers telemetry-query --aggregate credit-assignment --format candidate-findings --gaggle default --workflow implementation --window 168h ./instance
  ```

`goobers telemetry stats` on the command line reads only the local telemetry
rollup and does not include these fields today.

Both surfaces return two Backprop fields:

- `attributionCohorts`: per EffectiveVersion and workload (the run's trigger
  kind), the `runCount`,
  `topContributingPaths` (node path, `share`, `confidence`, evidence links),
  and `counterEvidence`. EffectiveVersion pins the workflow and goober digests
  plus the recorded model and harness version, so runs from different
  versions are never pooled. A run that used more than one model/harness pair
  has no EffectiveVersion and is left out of cohorts.
- `faultAudit`: a `report-only` audit over the window (the stats API defaults
  to the last seven days when `since` is omitted; `telemetry-query` uses
  `--window`, which defaults to `24h`). Findings are split by owner:

  | Field | Owner |
  |---|---|
  | `productReliabilityFindings` | Goobers runtime (daemon, worktree, scheduler, journal, claims, admission, publication, recovery, shared CI). Must recur across unrelated workflows. |
  | `externalFindings` | Harness, model, provider, credentials, network, filesystem, operating system. Must recur across unrelated workflows. |
  | `workflowFindings` | The workflow definition. Must be localized to one workflow, EffectiveVersion, and node path. |
  | `mixedOrUnknownFindings` | Anything sparse (fewer than three runs), missing provenance, contradictory, or not crossing the required boundary. |

Each finding carries a stable `id` (`backprop-` followed by 20 hex characters),
`confidence`, `runIds`, `workflows`, `effectiveVersions`, `nodePaths`, journal
and artifact `evidence` links, `counterEvidence`, `alternativeDomains`,
`recommendedOwner`, `recommendedAction`, and `verification`. The same failure
signature always produces the same finding ID across runs and passes.

## Record a fix and verify it

When you deploy a fix for a finding, record it:

```sh
goobers telemetry mark-fix --finding=backprop-0123456789abcdef0123 ./instance
goobers telemetry mark-fix --finding=backprop-0123456789abcdef0123 --applied-at=2026-10-09T15:00:00Z ./instance
```

`--applied-at` (RFC3339) defaults to now. The finding must already have been
returned by the telemetry stats API or `telemetry-query`; an unknown ID is
rejected.
`mark-fix` only writes auditor state under
`scheduler/backprop-audit/state.json`; it does not touch workflows, issues, or
run journals. Exit codes: `0` recorded, `1` state error, `2` usage or config
error.

Subsequent audit passes compare runs observed after the fix time with the
finding's baseline and report its `verification`:

| State | Meaning |
|---|---|
| `open` | No fix recorded. |
| `verification-pending` | Fix recorded, but there are not yet enough matching post-fix runs: at least three clean, completed runs with no causes, covering every affected workflow. Absence of post-fix runs never counts as recovery. |
| `recovered` | Enough matching post-fix runs completed cleanly. |
| `repeated` | The same signature recurred after the fix. |

Fix markers and the baselines they verify against are retained; unfixed
cooldowns and baselines older than 30 days are pruned.

## Troubleshooting

- **No `attribution.json` for a run.** Confirm the workflow is enrolled
  (`goobers status`) and that the run started after enrollment. Attribution
  errors are journaled as `backprop_attribution_failed` or
  `backprop_enrollment_failed` and are visible in `goobers trace <run-id>`.
- **Everything lands in `mixedOrUnknownFindings`.** The window has too few
  enrolled runs, or the runs lack exact provenance. Widen `since` (or
  `--window`), or wait for more runs; read each finding's `counterEvidence`
  for the specific reason.
- **`mark-fix` says the finding has not been reported.** Query the telemetry
  stats API over a window that includes the finding first, then retry.
- **`goobers telemetry stats --json` has no `faultAudit`.** Expected: use the
  daemon's telemetry stats API or `telemetry-query` as described above.

## Related

- [Credit graph contract](../design/credit-graph.md)
- [CLI reference](../cli/README.md)
