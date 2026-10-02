# Closed execution projection repair

The engine repair sweep queries only closed Temporal executions. A transient
query, storage, or observer error stays retryable. When an exact closed execution
has permanently unprojectable history (for example, no terminal `run.finished`),
the sweep reports its first failure through `engine_projection_failed` and writes
a durable JSON record beneath the gaggle's runs directory:

```
.engine-projection-deadletters/<execution-key>.json
```

The record contains the Temporal namespace, workflow ID, execution ID, closed
status, gaggle, failure reason, and recording time. Later sweeps skip that exact
execution, including after daemon restarts. A different execution with the same
workflow ID is inspected independently. The record does not fabricate a run
terminal event or replace an existing live journal.

Inspect these records when investigating missing projections. After correcting
the projector or source data, remove the affected record to retry on the next
sweep. The manual `goobers engine-project` path remains available and does not
consult the suppression records. A corrupt or unreadable record raises an error
rather than silently hiding the execution.
