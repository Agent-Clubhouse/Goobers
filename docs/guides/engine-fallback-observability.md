# Engine fallback observability

Engine selection remains per workflow, all-or-nothing. A declined engine
dispatch still runs through the local starter with the same request and result.
The decline now carries structured evidence; it does not enforce isolation.

`goobers status` and `goobers status --json` expose the latest observed fallback
per workflow, including its run ID, reason, and self-pinned stages/unpinned gates.
The workflow list/detail API exposes `engineFallback`; run list/detail responses
carry the same field from the run projection. No raw journal inspection is needed.

These are **observed decisions**, not a fresh solve or a claim that a workflow
with no observations is eligible. Workflow observations reset on daemon start or
accepted config reload and remain absent until that workflow next falls back.
Run-level evidence remains attached to its original run and follows normal run
retention. The instance fold keeps one observation per workflow, capped at 1,024
workflows (oldest inserted workflow evicted first).

Reason codes:

| `reasonClass` | Meaning |
| --- | --- |
| `engine_not_configured` | Instance has no enabled engine configuration |
| `no_pinned_placements` | No pinned rows, including local-mode inventories |
| `placement_ineligible` | Self-pinned tasks or unpinned agentic gates |
| `placement_failed` | Placement solve failed |
| `definition_refused` | Engine does not support the definition |
| `unknown` | Historical annotation predates classification |

`placementDeclared` reports explicit task/gate `runsOn` declarations or a gaggle
placement floor. Do not infer it from nonempty pins: legacy workflows can carry
implicit self-pins. Text status warns when declarations exist and otherwise
reports informational fallback. A declaration is not proof of a prior successful
migration; the signal identifies an ineligible declared placement, not its history.

The fallback starter emits a scheduler span with action
`engine_starter_selection`, the normal workflow/gaggle/run attributes, and:

- `goobers.engine.fallback.reason_class`
- `goobers.engine.fallback.placement_declared`
- `goobers.engine.fallback.self_pinned_count`
- `goobers.engine.fallback.unpinned_gate_count`

Alert on `placement_declared=true`, then use the reason class and run evidence to
find the offending stages. Expected local-only operation can be excluded. Stage
names and raw refusal prose are not telemetry dimensions. Fully engine-selected
workflows emit no fallback span or annotation. Telemetry export is best-effort
and does not change admission, routing, or the wrapped starter's return value.
