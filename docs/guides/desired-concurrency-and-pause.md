# Desired concurrency, refill, and pausing work

This guide covers two related jobs:

- keeping a backlog workflow busy with `spec.readiness.desiredConcurrentRuns`
  (refill);
- pausing work safely and then resuming it.

It also lists which controls actually stop new work. Several obvious-looking
controls do not stop refill.

## Admission model

Each workflow's `spec.readiness` block decides when the daemon may start
another run. These limits apply together, and the tightest one wins:

| Field | Default | Meaning |
| --- | --- | --- |
| `maxConcurrentRuns` | `1` | Hard cap on this workflow's active runs. |
| `desiredConcurrentRuns` | unset (no refill) | The number of active runs the daemon tries to keep. It must be between `1` and `maxConcurrentRuns`. |
| `maxRunsPerHour` | `10` | Runs admitted in a rolling hour. `0` also means `10`. |
| `maxRunsPerDay` | unset (no cap) | Runs admitted in a rolling day. |

Two more limits sit above the workflow: the instance's
`runConditions.maxParallelRuns` caps runs across the whole instance, and
`goobers run --force` bypasses only the hourly and daily budgets, never the
concurrency caps.

`maxConcurrentRuns` is a ceiling. `desiredConcurrentRuns` is a target: when a
run finishes or capacity frees up, the daemon looks for more eligible backlog
items and starts runs until active runs reach the target again. These are
*refill* runs. A refill run is a normal run of the workflow with the trigger
kind `item`.

### When refill is active

Refill is wired for a workflow only when all of these are true:

1. `desiredConcurrentRuns` is set.
2. The instance has at least one repository configured.
3. The workflow declares no `backlog-item` trigger, not even a disabled one.
   A `backlog-item` trigger already polls the backlog and starts one run per
   item, so the daemon does not add a second poll on top of it. Setting that
   trigger to `enabled: false` stops its polling but does not turn refill on.
4. The workflow's `start` task runs `goobers backlog-query`. The daemon reuses
   that task's label filters (`trustLabel`, `requireLabels`, `excludeLabels`)
   and the gaggle's backlog filters to count eligible items.

If any of these is false, `desiredConcurrentRuns` is still validated and still
shown by `goobers status`, but it starts nothing.

Refill does not need any enabled trigger. A workflow with only a `manual`
trigger, or whose `schedule`, `signal`, and `webhook` triggers are all set to
`enabled: false`, still refills. A declared `backlog-item` trigger is the
exception: it keeps refill off whether or not it is enabled.

### What refill does on each tick

- It polls only while active runs are below the target, and at most once every
  30 seconds per workflow.
- It starts at most `desired − active − runs already planned this tick` runs,
  and never more than the number of eligible items.
- With no eligible items it starts nothing and the workflow stays idle.
- If admission refuses a refill run (for example, a budget is spent), the
  daemon journals `tick.skipped` with the reason `refill blocked: <reason>`
  and retries after about 30 seconds.
- When a run finishes, the daemon re-checks refill straight away instead of
  waiting for the next poll.

### Reading refill in `goobers status`

The workflow summary shows an `A/D/MAX` column (active, desired, maximum). A
workflow without `desiredConcurrentRuns` shows `A/MAX`. When refill is below
target and admission keeps refusing it, a `blocked: <condition>` line follows
the workflow row. `goobers status --json` reports the same values as
`desiredRuns` and `admissionBlocked`.

`goobers status` folds workflows whose only trigger is `manual` into a
one-line summary, which hides their `A/D/MAX` row and `blocked:` line. For a
refill workflow like the example below, use `goobers status --all` or
`goobers status --workflow=<name>`. `goobers validate` and `goobers status`
also warn that such a workflow "will not fire autonomously"; that warning
does not take refill into account, so ignore it for refill workflows.

### Minimal refill example

<!-- example: refill -->
```yaml
apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: refill-example
spec:
  instance:
    name: refill-example
    environment: dev
  gaggles:
    - acme
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: acme
spec:
  project:
    provider: github
    owner: example
    name: acme
  backlog:
    provider: github
    project: example/acme
  isolation:
    namespace: gaggle-acme
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: implement
spec:
  gaggle: acme
  triggers:
    - type: manual
  readiness:
    maxConcurrentRuns: 3
    desiredConcurrentRuns: 2
    maxRunsPerHour: 6
  start: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: Claim one approved backlog item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        maxItems: "1"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
```

The daemon keeps two `implement` runs going while approved items exist. It
never runs more than three at once, and starts at most six per hour.

### Turning refill off

Remove `desiredConcurrentRuns` from the workflow. Do not set it to `0`: the
schema requires at least `1`, so `goobers validate` rejects `0` and the daemon
keeps its last-known-good config. Removing the field stops refill but leaves
triggers and explicit runs alone. To stop everything, pause the workflow as
described below.

## Which controls stop new work

| Control | Schedule, signal, webhook triggers | `backlog-item` trigger | Refill | Explicit `goobers run` | Runs already in flight |
| --- | --- | --- | --- | --- | --- |
| Trigger `enabled: false` | that trigger stops | that trigger stops | **keeps running** if no `backlog-item` trigger is declared; a declared `backlog-item` trigger keeps refill off even when disabled | allowed | continue |
| Workflow enable API set to `false` ([workflow-enable.md](../workflow-enable.md)) | stop | stop | **keeps running** if no `backlog-item` trigger is declared; otherwise there is no refill | allowed | continue |
| Remove `desiredConcurrentRuns` | unchanged | unchanged | stops | allowed | continue |
| Workflow `spec.enabled: false` | stop | stop | stops | refused | continue |
| Gaggle `spec.enabled: false` | stop for every workflow in the gaggle | stop | stops | refused | continue |
| `goobers down`, `goobers service stop` | stop while the daemon is down | stop | stops | dispatched locally (see below) | drain, then the daemon exits |

`manual` triggers ignore `enabled`. The workflow enable API only changes
trigger `enabled` fields, so it also leaves refill on. Disabling a `schedule`,
`signal`, or `webhook` trigger never stops refill. A workflow that declares a
`backlog-item` trigger has no refill at all, enabled or not, so disabling its
triggers stops all of its autonomous starts.

`spec.enabled: false` is the only control that stops every kind of new work
for a workflow while the daemon keeps running. The daemon skips the workflow
on every tick without polling providers. Explicit `goobers run` and
`goobers signal` requests are refused with a reason that starts
`conditions: disabled:`, which is permanent until the config changes. Runs
already in flight finish normally.

<!-- example: paused-workflow -->
```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: implement
spec:
  gaggle: acme
  enabled: false
  triggers:
    - type: manual
  readiness:
    maxConcurrentRuns: 3
    desiredConcurrentRuns: 2
    maxRunsPerHour: 6
  start: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: Claim one approved backlog item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        maxItems: "1"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
```

To pause every workflow in a gaggle at once, set `enabled: false` on the
gaggle:

<!-- example: paused-gaggle -->
```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: acme
spec:
  enabled: false
  project:
    provider: github
    owner: example
    name: acme
  backlog:
    provider: github
    project: example/acme
  isolation:
    namespace: gaggle-acme
```

The paused example keeps `desiredConcurrentRuns` so that resuming only needs
`enabled` removed.

## Candidate, installed, and active config

A pause only takes effect once the change reaches the running daemon. Keep
three copies of the config apart:

- **Candidate**: an edit not yet installed, such as a branch of a config
  repository. Check it with
  `goobers validate --source-tree --instance <instance.yaml> <checkout>`.
- **Installed**: the config the instance loads from. `goobers validate [path]`
  checks `instance.yaml` and the config directory. Passing validation does not
  activate anything.
- **Active**: the definitions the live daemon is using. With a local config
  directory the daemon watches for edits by default (`goobers up
  --watch-config`). `goobers apply [path]` reconciles immediately: exit 0
  means applied or already current; exit 1 means no live daemon, or the config
  was rejected and the daemon kept its last-known-good definitions. With a Git
  `workflowSource`, the daemon follows the tracked ref, so commit the pause to
  that repository; a local edit is not what the daemon reads. `instance.yaml`
  is read only at daemon startup.

Admitted runs keep the config generation they started with; see
[run-config-generations.md](run-config-generations.md).

## Pause runbook

1. **Record the current state** so you can confirm the pause and resume later:

   ```text
   goobers status --daemon
   goobers status --workflow=implement
   goobers runs list --phase=running --json
   goobers claims active --json
   ```

2. **Pause in config.** Set `spec.enabled: false` on the workflow, or on the
   gaggle to pause all its workflows. Make the change where the daemon reads
   its config (the local config directory or the tracked Git ref).

3. **Validate**: `goobers validate` (or the `--source-tree` form for a
   candidate checkout). Fix errors before going on.

4. **Activate**: `goobers apply`. Expect `applied ...` or `already current`
   and exit 0. Exit 1 with `config rejected` means the daemon is still running
   the old, unpaused definitions.

5. **Verify that no new work starts.** `goobers status` does not label a
   disabled workflow, so verify through runs and claims:

   ```text
   goobers runs list --phase=running --workflow=implement --json
   goobers claims active --gaggle=acme --json
   ```

   Over the next few minutes the set of running runs must only shrink, and no
   new claims may appear for the paused workflow. The `A/D/MAX` column keeps
   showing the configured target while paused; only the active count matters.

6. **Drain or cancel.** By default, let in-flight runs finish. To stop a
   specific run now:

   ```text
   goobers run cancel --request-id=<id> <run-id>
   ```

   `run cancel` releases the run's backlog claim and records phase `aborted`.
   If the response is lost or uncertain, retry with the **same**
   `--request-id`; receipts are kept for at least seven days, and an
   unfinished request is never silently run twice. `--request-id` needs the
   daemon API and cannot be combined with `--no-api`. Use `goobers run abort`
   only for a stuck run when no daemon is running.

7. **Optionally stop the daemon.** This is not needed for the pause. Stop it
   only for maintenance:

   - Supervised install: `goobers service stop`. It keeps the registration and
     waits up to 50 seconds for the service to report stopped. If it times out
     it exits 1, but the drain continues; check again with
     `goobers service status`, which exits 0 only while running.
   - Windows per-user task: `goobers service task-stop`, then
     `goobers service task-status`.
   - Unsupervised `goobers up`: `goobers down`. It exits 0 as soon as the
     request is delivered, not when the daemon has exited. The daemon drains
     in-flight runs before exiting (indefinitely, unless it was started with
     `--drain-timeout`).

   `goobers status --daemon` exits 1 both for a stopped daemon and for an
   unhealthy one, so match its output instead of the exit code. After the
   instance-root lines, a stopped daemon prints a line that starts with
   `recorded daemon is not running:` or `daemon not running;`:

   ```powershell
   goobers down
   do {
     Start-Sleep -Seconds 5
   } until (goobers status --daemon | Select-String -Quiet '^(recorded daemon is not running:|daemon not running;)')
   ```

   ```sh
   goobers down
   until goobers status --daemon | grep -Eq '^(recorded daemon is not running:|daemon not running;)'; do
     sleep 5
   done
   ```

   A supervisor restarts the daemon at the next boot or logon even after a
   clean stop. The config pause from step 2 is what keeps work stopped after a
   restart.

## Resume runbook

1. If you stopped the daemon, start it again (`goobers service start`, or
   `goobers up`) **with the pause still in place**, and wait for
   `goobers status --daemon` to report `daemon running` with exit 0.
2. Remove `enabled: false` from the workflow or gaggle (or set it to `true`).
3. Validate it (`goobers validate`) and activate it (`goobers apply`).
4. Confirm with `goobers status --workflow=implement` that the active count in
   `A/D/MAX` climbs back toward the target as eligible items exist, and that no
   `blocked:` line persists.
5. If you removed `desiredConcurrentRuns` instead of disabling the workflow,
   add it back and apply.

## Do not

- Do not set `desiredConcurrentRuns: 0`. It is rejected; remove the field
  instead.
- Do not rely on trigger `enabled: false` or the workflow enable API to stop a
  refill workflow.
- Do not kill daemon or agent processes by name. Use `goobers down`,
  `goobers service stop`, or `goobers run cancel`.
- Do not delete or edit run journals to stop work. Use `goobers run cancel`,
  or `goobers run abort` when no daemon is running.
- Do not start a daemon just to check whether a pause works. Verify against
  the running daemon with the commands above.
- Do not use `goobers run` or `goobers signal` as a probe. Without a live
  daemon they dispatch work locally.
