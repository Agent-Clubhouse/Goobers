# Decision-gate A/B harness and reliability report

Use this pattern when shadow mode is no longer enough and you need a matched,
opt-in comparison of **gate off** versus **gate on**. The goal is to measure
counts with sample sizes, not to infer a win from anecdotes.

The paired setup uses two independent Goobers instances against the same
repository:

- the **gate-off** arm keeps runtime behavior off while still emitting
  `decisiongate.shadow` records;
- the **gate-on** arm enables the gate and emits `decisiongate.enforce`
  records.

The report tool at
[`examples/decision-gate-ab/ab-report.ps1`](../../examples/decision-gate-ab/ab-report.ps1)
reads both arms' journals and daemon logs, prints them side by side, and
refuses to print any percentage whose denominator is below the configured
minimum sample size.

> The gate-on arm assumes you have a build that actually wires
> `decisionGate.mode: enforce` into the harness. Keep the entire experiment
> opt-in and isolated until that build is proven for your workflow.

## 1. Keep the two arms structurally separate

Start from the same workflow and gaggle definition, then split the two arms by
construction:

1. Give each arm its own instance root, API port, daemon log files, and branch
   namespace.
2. Give each arm its own required-label route, for example
   `ab:gate-off` and `ab:gate-on`.
3. Exclude repo-wide workflows that would otherwise observe or mutate shared
   state outside the experiment.

The multi-instance safety rules are the same as any other shared-repo setup:
use disjoint `requireLabels` and distinct `branchNamespace` values together.
See [Run multiple Goobers instances against one repo](multiple-instances-one-repo.md)
for the general coordination model.

## 2. Feed matched issues, not the same issue twice

Do **not** send both arms to the same backlog item. Instead, build matched
pairs:

- same workflow path and repository surface;
- similar size and dependency shape;
- same trust/ready labels;
- one pair label that identifies the match, such as `ab:pair-017`;
- exactly one arm label on each issue: `ab:gate-off` or `ab:gate-on`.

The more consistent the pairing, the less noise you introduce. The report does
not prove that the issue sets were well matched; that discipline is operational.

## 3. Configure the gate-off arm

The gate-off arm should leave outcomes unchanged but still log candidate
spurious bad-input claims:

```yaml
decisionGate:
  mode: shadow
  baseURLEnv: DECISION_GATE_BASE_URL
  keyEnv: DECISION_GATE_API_KEY
  modelEnv: DECISION_GATE_MODEL
  fallback: agent
  shadowSample: 1
```

Keep the implementation workflow routed only to its arm:

```yaml
inputs:
  trustLabel: "goobers:approved"
  requireLabels: "goobers:ready,ab:gate-off"
```

## 4. Configure the gate-on arm

Match everything else, then switch only the arm-specific routing and gate mode:

```yaml
decisionGate:
  mode: enforce
  baseURLEnv: DECISION_GATE_BASE_URL
  keyEnv: DECISION_GATE_API_KEY
  modelEnv: DECISION_GATE_MODEL
  fallback: agent
```

```yaml
inputs:
  trustLabel: "goobers:approved"
  requireLabels: "goobers:ready,ab:gate-on"
```

Use the same thresholds, same model selection, same fallback policy, and same
workflow revision in both arms. Otherwise the result stops being an A/B test and
turns into a bundle of unrelated changes.

## 5. Run the experiment

1. Validate and start both instances.
2. Let each arm work only its own matched issues.
3. Preserve both daemon logs for the reporting window.
4. Choose a fixed reporting window with `-Since` when you want to exclude
   older runs.

The report expects each arm's instance root and daemon logs. A typical call is:

```powershell
pwsh -File .\examples\decision-gate-ab\ab-report.ps1 `
  -GateOffInstance C:\goobers\ab-off\instance `
  -GateOffDaemonLogs C:\goobers\ab-off\daemon.err.log,C:\goobers\ab-off\daemon.out.log `
  -GateOnInstance C:\goobers\ab-on\instance `
  -GateOnDaemonLogs C:\goobers\ab-on\daemon.err.log,C:\goobers\ab-on\daemon.out.log `
  -Since 2026-10-01T00:00:00Z `
  -MinSample 30
```

## 6. Read the report

The script prints both arms side by side and always includes counts:

- runs scanned and stage-finish counts from the journals;
- stage outcomes and terminal run phases per arm;
- failure codes seen on non-success stage attempts;
- decision-gate records, split into known-valid, known-invalid, and
  unknown-validity samples;
- counts of **valid handoffs called bad** and how many of those still ended in
  a non-success outcome versus recovered to success.

Percentages appear only when the denominator reaches `-MinSample`. Below that,
the script prints counts and `n=<sample>` only. This avoids the false precision
that the issue explicitly rejects.

## 7. Handle missing `inputValid` as unknown

Some builds may emit `decisiongate.shadow` or `decisiongate.enforce` without an
`inputValid` field. The report treats those records as **unknown validity**.
That is intentional:

- `inputValid=true` means the deterministic input check says the handoff was
  valid, so `agentClaimedBad=true` is a grounded false bad-input claim.
- `inputValid=false` means the model saw an actually invalid handoff.
- missing `inputValid` means the report cannot know which case it was, so the
  sample stays out of valid-versus-invalid percentages.

Do not fold unknown-validity records into the valid denominator by hand.

