# Workflow config drift

`goobers config diff` compares an instance's active workflow definitions with
the shipped canonical config:

```sh
goobers config diff ~/goobers-instance
goobers config diff --against /path/to/Goobers/selfhost ~/goobers-instance
```

The active path must be an instance root; its workflows are loaded from
`config/`. The canonical path is a config source tree containing `manifest.yaml`
and `gaggles/`. It defaults to `./selfhost`, so run the first form from a
Goobers source checkout or pass `--against` explicitly.

The comparison is name-based and deterministic. YAML file layout, map order,
and workflow/task/gate order do not affect the result. A structural difference
prints the workflow and, when applicable, task or gate, field path, active
value, and canonical value. Structural drift exits 1; no structural drift exits
0 even when operational tuning is listed.

## Operational tuning allowlist

Only these workflow paths may differ without failing:

| Path | Meaning |
|---|---|
| `spec.triggers[]` presence | Trigger enablement or disablement |
| `spec.triggers[].schedule` | Cron or interval cadence |
| `spec.readiness.maxConcurrentRuns` | Concurrent workflow-run cap |
| `spec.readiness.maxRunsPerHour` | Hourly run budget |
| `spec.readiness.maxRunsPerDay` | Daily run budget |
| `spec.readiness.maxOpenPRs` | Open-PR backpressure cap |

These differences print as `INFO`. Every other workflow field is structural,
including task and gate sets, commands, inputs, `inputsFrom`, expected outputs,
capabilities, branch targets, and `next` routing, and prints as `ERROR`.

The code allowlist lives in `internal/configdiff/configdiff.go`. Extend that
single list and its classification tests deliberately when a new per-instance
operational knob is introduced.
