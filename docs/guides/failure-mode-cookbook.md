# Failure-mode cookbook

This guide covers the most common errors you will encounter when authoring,
validating, and operating Goobers workflows. Each entry names the symptom,
explains the cause, and shows the fix.

## Reading `goobers trace`

The trace command renders the run journal as a human-readable event stream:

```sh
goobers trace <run-id> "$GOOBERS_INSTANCE"
```

Each line is a timestamped event:

```text
[1] run.started workflow=implementation gaggle=acme-web
[2] stage.started stage=query-backlog attempt=1
[3] artifact.recorded name=.../claimed-item.json digest=sha256:... size=412
[4] stage.finished stage=query-backlog attempt=1 status=success outputs={"claimed-item":"..."}
[5] stage.started stage=implement attempt=1
...
[N] stage.finished stage=implement attempt=1 status=failure reason="empty diff"
[N+1] gate.evaluated gate=review verdict=escalate target=park-escalated
[N+2] stage.started stage=park-escalated
[N+3] stage.finished stage=park-escalated status=success
[N+4] run.finished phase=escalated
```

**Useful flags:**

| Flag | Shows |
|---|---|
| (none) | Event stream; stage status; gate verdicts; artifact records. |
| `--transcripts` | The full agent transcript for every agentic stage. |
| `--json` | Machine-readable event stream; useful for `jq` filtering. |

To read a specific artifact (e.g. the coder's stdout):

```sh
RUN_DIR="$GOOBERS_INSTANCE/gaggles/acme-web/runs/$RUN_ID"
ARTIFACT=$(
  jq -r 'select(.type == "artifact.recorded" and (.name | endswith("implement/stdout.log"))) | .ref.path' \
    "$RUN_DIR/events.jsonl"
)
cat "$RUN_DIR/$ARTIFACT"
```

**Run phases:**

| Phase | Meaning |
|---|---|
| `completed` | All terminal tasks finished successfully. |
| `failed` | A stage returned a non-retryable failure. |
| `escalated` | A bounded repass budget exhausted, or an empty diff or infra failure reached `@escalate`. The issue is parked with `goobers:needs-remediation`. |
| `aborted` | A gate routed to `@abort`. The issue is parked with `goobers:needs-human`. |

---

## Validation errors

Run validation before every daemon start:

```sh
goobers validate --source-tree "$GOOBERS_CONFIG_SOURCE"
goobers validate "$GOOBERS_INSTANCE"
goobers validate --check-harness "$GOOBERS_INSTANCE"
```

### `connectionRef "repo-token" not found in manifest`

**Cause:** A `gaggle.yaml` references a connection name that does not appear in
`manifest.yaml`'s `spec.connections`.

**Fix:** Add the missing connection entry to `manifest.yaml`:

```yaml
spec:
  connections:
    - name: repo-token
      type: repo
      provider: github
      secretRef:
        name: github-token
```

Then verify the `type` matches usage: `repo` for `spec.project.connectionRef`,
`backlog` for `spec.backlog.connectionRef`.

### `gaggle "example" not found` (in a workflow or goober)

**Cause:** A `workflow.yaml` or `goober.yaml` has `spec.gaggle: example` but no
gaggle named `example` exists in the instance.

**Fix:** Update `spec.gaggle` to match the actual `metadata.name` from
`gaggle.yaml`. This commonly happens after renaming the starter gaggle.

### `gaggle not listed in manifest`

**Cause:** A gaggle directory exists under `gaggles/` but is missing from
`manifest.yaml`'s `spec.gaggles` list.

**Fix:** Add the gaggle name:

```yaml
spec:
  gaggles:
    - my-gaggle
```

### `broken state reference: next "implement" does not exist`

**Cause:** A task's `next` field (or a gate branch target) names a task or
gate that does not exist in the workflow's `tasks` or `gates` lists.

**Fix:** Check spelling against the exact `name` values in the workflow. The
common case is a renamed task whose callers were not updated.

### `unreachable state: "park-escalated" is never reached`

**Cause:** A task or gate is declared but no `next` or branch target points
to it.

**Fix:** Either remove the orphan state or add a branch that routes to it. All
declared states must be reachable from `start`.

### `incomplete gate outcomes: "escalate" branch missing`

**Cause:** An automated gate is missing one or more required branches. Agentic
gates require `pass`, `fail`, and `needs-changes`; some automated checks add
`escalate` and `timeout`.

**Fix:** Add the missing branches. Use `@abort` or `@escalate` when there is no
meaningful action to take on that outcome:

```yaml
branches:
  pass: close-out
  fail: remediate-ci
  escalate: park-escalated
  timeout: park-escalated
```

### `capability "github:issues:write" in task exceeds goober grants`

**Cause:** An agentic task lists a capability that the referenced goober's
`spec.capabilities` does not include.

**Fix:** Add the capability to the goober's definition, or remove it from the
task. Both must agree — validation enforces the invariant that a task cannot
grant more than its goober holds.

### `unknown capability "repo:write"`

**Cause:** A capability string that does not exist in the release's registry.

**Fix:** Use only canonical capability names. Check the full registry in
[workflow-authoring.md](workflow-authoring.md#canonical-capabilities) or run
`goobers features --json | jq .capabilities`.

### `missing instructions file "instructions.md"`

**Cause:** A goober's `spec.instructions` path does not exist relative to the
goober definition directory.

**Fix:** Create the file at the expected path, or update `spec.instructions` to
the correct relative path.

### `workflow "default-implement" in goober.spec.workflows not found`

**Cause:** A goober's `spec.workflows` list references a workflow name that does
not exist.

**Fix:** Update the list to use the exact `metadata.name` from the workflow
file. This is a display-only field — it does not affect dispatch — but
validation fails on stale references.

### `dslVersion "1.4" not supported`

**Cause:** The workflow declares a DSL version the installed binary does not
support.

**Fix:** Run `goobers versions --json` to see the supported DSL versions and
update `dslVersion` accordingly. When upgrading Goobers, run
`goobers upgrade-workflow` to migrate workflows automatically.

### `harness "copilot": sign-in probe failed`

**Cause:** `validate --check-harness` cannot find a valid Copilot sign-in.

**Fix:**

```sh
copilot login
```

Complete the device flow with the same OS account that runs the daemon. Keep
the credential store (`~/.copilot/`) persistent. Re-run validation after
signing in.

---

## Common run failures

### `status=no-work`: the run found nothing to claim

**Cause:** `query-backlog` found no eligible issues. This is a normal outcome,
not an error.

**Diagnosis:** Check the issue labels on the target repository:

```sh
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue list \
  --repo "$GOOBERS_TARGET" \
  --label "goobers:approved" \
  --json number,title,labels
```

**Common causes:**
- No issues carry the `trustLabel` (`goobers:approved` or `goobers`).
- All eligible issues are already claimed or carry an `excludeLabels` label
  such as `goobers:ready` or `goobers/status:in-review`.
- The `requireLabels` filter (e.g. `goobers:ready`) is not satisfied.

### `status=failure reason="empty diff"`

**Cause:** The implementer finished without committing any changes.

**Diagnosis:**

```sh
goobers trace --transcripts <run-id> "$GOOBERS_INSTANCE"
```

Read the implementer's transcript. Common causes: the issue was already
implemented on a sibling branch; the goal was ambiguous; the session hit a
timeout.

**Fix:** Edit the issue to add clearer acceptance criteria, then re-run. If the
session consistently times out, raise `timeoutSeconds` on the task or
`spec.timeoutSeconds` on the goober.

### `status=failure reason="local-ci failed"`

**Cause:** The deterministic `local-ci` stage ran the configured CI command and
it exited non-zero.

**Diagnosis:**

```sh
goobers trace <run-id> "$GOOBERS_INSTANCE"
```

Find the `local-ci` stage's stdout artifact and read the build/test output:

```sh
jq -r 'select(.type == "artifact.recorded" and (.name | endswith("local-ci/stdout.log"))) | .ref.path' \
  "$RUN_DIR/events.jsonl" | xargs -I{} cat "$RUN_DIR/{}"
```

**Fix:** The implementer committed broken code. On the next repass, the
CI failure evidence is attached as context. If the issue itself requires
knowledge the implementer does not have, add acceptance criteria or a pointer
to the relevant documentation.

### `status=failure reason="push failed: permission denied"`

**Cause:** The `push-branch` stage could not push to the repository. The token
lacks Contents write permission.

**Fix:** Verify the token's permissions:

```sh
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh repo view "$GOOBERS_TARGET" --json pushedAt
```

If that succeeds but push still fails, the token may lack branch-creation
permission. Re-issue the fine-grained PAT with Contents: read and write.

### Issue stuck with `goobers:claimed` after a failed run

**Cause:** A run that parked with `@abort` or `@escalate` releases the claim
via the terminal `park-*` stages. If the run was killed before those stages
ran, the claim label remains.

**Fix:** Remove the `goobers:claimed` label manually:

```sh
GH_TOKEN="$GOOBERS_GITHUB_TOKEN" gh issue edit <number> \
  --repo "$GOOBERS_TARGET" \
  --remove-label "goobers:claimed"
```

Then run `goobers backlog-query --reconcile` to resync the claim ledger:

```sh
goobers run backlog-curation "$GOOBERS_INSTANCE"
```

Or call reconcile directly:

```sh
goobers backlog-query --reconcile \
  --instance "$GOOBERS_INSTANCE" \
  --gaggle my-gaggle
```

### `phase=escalated` with `reason="repass-budget-exhausted"`

**Cause:** The workflow's retry budget for the `implement` task was exhausted.
The issue is parked with `goobers:needs-remediation`.

**Diagnosis:** Read the full transcript to understand why each repass failed:

```sh
goobers trace --transcripts <run-id> "$GOOBERS_INSTANCE"
```

**Fix:** Add clearer acceptance criteria to the issue, remove `goobers:needs-remediation`, add `goobers:approved` (and `goobers:ready` if the curation workflow is enabled), then re-run.

### `ci-poll timeout`

**Cause:** GitHub CI did not reach a terminal state within the poll window.

**Diagnosis:** Check the PR directly. If CI is still running, GitHub may have
queued the job — this is not a Goobers failure.

**Fix:** The issue is parked with `goobers:needs-remediation`. Once CI
completes (passing or failing), remove the label and re-run. If CI consistently
takes too long, increase the `ci-poll` timeout in the workflow's task
`timeoutSeconds`.

---

## Daemon and scheduler

### `goobers up` exits immediately

**Cause:** The instance failed its startup validation check.

**Fix:** Run `goobers validate "$GOOBERS_INSTANCE"` and fix all errors before
running `goobers up`.

### Runs not starting on their cron schedule

**Cause:** Several possibilities — the daemon is not running, the workflow's
`readiness.maxConcurrentRuns` is already saturated, the `workflowDailyBudgets`
ceiling was hit, or the token is rate-limited.

**Diagnosis:**

```sh
goobers status --daemon "$GOOBERS_INSTANCE"
goobers status --watch "$GOOBERS_INSTANCE"
```

Look for `rate-limited-until` or `budget-exhausted` in the status output.

### Rate limit wait

When GitHub exhausts a primary or secondary rate limit, `goobers status`
reports when dispatch can resume. The scheduler waits rather than spinning
requests. No action is required; the next eligible tick will fire automatically
after the reset window.
