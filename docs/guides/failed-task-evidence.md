# Failed-task evidence contract

When a task fails, the evidence that survives depends on how it failed. That
evidence is retained in the run journal and may or may not be delivered to a
later stage. This guide records the current DSL 2.0 behavior, shows how to pick
out the evidence for one exact execution, and covers what to do when an
in-workflow collector cannot run.

`internal/runner/failure_evidence_test.go` pins this contract. If you change any
row below, update that test and this guide together.

## Evidence kinds

| Kind | Journal record | Delivered to a later stage? |
| --- | --- | --- |
| Captured stdout and stderr | `artifact.recorded` named `<task-id>:<stage>/stdout.log` and `.../stderr.log`, also listed in the producer's `stage.finished.artifacts` | Yes, as `<stage>.artifact[N]` context pointers |
| Shell result file (`inputs.resultFile`) | `artifact.recorded` named `<task-id>:<stage>/result`; its scalar fields are lifted into `stage.finished.outputs` | Pointer through `contextFrom`; scalars through `inputsFrom` |
| `outbox` files | `artifact.recorded` named `outbox/<declared path>` at `artifacts/outbox/<stage>/attempt-<N>/occurrence-<S>/...` | No. Outbox is export-only |
| Failure status | `stage.finished` with `status: failure` and `error.code` | Only through the gate or routing decision |

Stream and result records do not carry `stage` or `attempt` fields. Outbox
records do. Context manifests are also `artifact.recorded` events, named
`context/<stage>-attempt-<N>.json`. They describe what a consumer received and
are not producer evidence.

## Failure matrix

| Condition | Producer `stage.finished` | Retained evidence | Received by a `contextFrom` consumer | Routing |
| --- | --- | --- | --- | --- |
| Nonzero exit | `failure`, `nonzero_exit`; result-file scalars stay in `outputs` | stdout, stderr, result, outbox | stdout, stderr and result pointers | `failure-class` returns `fail`; a gate fail branch runs |
| Timeout (`timeoutSeconds`, `limits.maxDurationSeconds`) | `failure`, `timeout`, retryable | stdout, stderr (may be empty), outbox. The result file is **not** collected | stdout and stderr pointers | `failure-class` returns `infra`; the journal records a `stage.retry.decision` annotation |
| Shell context canceled without a timeout | `failure`, `canceled`, not retryable | stdout and stderr. The result file is **not** collected | stdout and stderr pointers | As for any failed stage |
| Launch failure (missing executable, bad working directory) | `failure`, `exec_start`, no artifacts | Nothing from the producer | Empty context manifest | `failure-class` returns `fail` |
| Outbox export failure | `failure`, `outbox_export_failed`; the original command status and error are kept in an `outbox.export.failed` annotation | stdout, stderr and result. The outbox batch may be missing or partial; the annotation's `outboxEvidenceState` says so, and any recorded `outbox/` items must still be verified | Not applicable; the run fails | The run fails even with `continueOnError` |
| `continueOnError: true` on the producer | `failure` as above, with `outputs` discarded | As above | Stream and result pointers are still delivered. Scalar outputs are discarded, so `inputsFrom` cannot read them | The task's `next` runs |
| Consumer dispatch refusal (a required `inputsFrom` output is missing) | Unchanged | Producer evidence stays in the journal | **Nothing.** The consumer gets `stage.started`, then an `error` event (`executor_error`) and no context manifest | `run.finished` is `failed` with terminal cause `infrastructure-failure` (`retry-exhaustion` when the consumer declares `retry.maxAttempts > 1`); later gates are not evaluated |
| Retry (`retry.maxAttempts`) | Only dispatch errors and retryable agentic artifact-contract misses are retried. Each retried attempt leaves `stage.started` then `error` (`executor_error`); only the last attempt has `stage.finished`. A failed result such as `nonzero_exit` or `timeout` is not retried | The last attempt's; outbox is under `attempt-<N>` | The last attempt's pointers | As for the final attempt |
| Gate repass re-entering the producer | A new `stage.started` and `stage.finished`, and `attempt` restarts at `1` | Per occurrence; outbox is under a new `occurrence-<S>` | **All** occurrences' pointers accumulate under the same names (`<stage>.artifact[0..N]`), plus a `learning.episode[N]` pointer | As for the gate |

A failed producer still routes through its gate when `next` names a gate. A
run whose last stage failed cannot complete unless a gate passed.

## Select the exact occurrence

Do not identify an occurrence by its attempt number. Repasses reuse
`attempt: 1`, and context-pointer names repeat across repasses. Select by
journal sequence number instead:

1. Find the consumer's last `stage.started` and note its `seq`.
2. On the same `branch`, find the producer's last `stage.finished` with a
   lower `seq`. Its `artifacts` list (`path`, `digest`, `size`) is the
   authoritative list of stream and result evidence. Note its `seq`.
3. Find the producer `stage.started` that opened that occurrence.
4. The occurrence's evidence is every `artifact.recorded` event on the
   producer's `branch` with a `seq` strictly between those two producer
   events, excluding `context/` names. The branch filter matters in parallel
   runs, because sibling branches share one sequence and stream records carry
   no `stage`.
   Outbox paths in this set also have an `occurrence-<S>` segment inside that
   window.
5. Verify each item. Read `<run dir>/<ref.path>`, then compare its length with
   `ref.size` and its SHA-256 with `ref.digest` (`sha256:<hex>`).

| State | Meaning |
| --- | --- |
| verified | The file exists and its size and digest match |
| absent | The file is missing |
| malformed | The file exists but its size or digest differs, or the path escapes the run directory |
| partial | Some items of the occurrence are verified and others are not |

Treat anything other than `verified` as untrusted. Report the state instead of
guessing from neighboring occurrences.

## Read the journal

The journal is at `gaggles/<gaggle>/runs/<run-id>/events.jsonl` under the
instance root. `goobers trace --json <run-id>` prints the same events as JSON
Lines. For example, the following command lists a producer's evidence by
occurrence:

```sh
goobers trace --json "$RUN_ID" | jq -c '
  select((.type == "stage.started" or .type == "stage.finished") and .stage == "produce")
    // select(.type == "artifact.recorded" and ((.name // "") | startswith("context/") | not))
  | {seq, type, stage, attempt, status, code: .error.code, name, ref, artifacts}'
```

Plain deterministic stages do not receive context pointers. To read the journal
from an in-workflow collector stage, set `run.injectRunContext: true` on it (or
make its command `goobers`, which injects the run context automatically); the
stage then gets `GOOBERS_RUN_ID` and can run
`goobers trace --json "$GOOBERS_RUN_ID"` and apply the selection and
verification steps above. Agentic consumers receive `contextFrom` pointers
under `.goobers/context/` and through the goobers-io tools.

## Outer recovery

When the consumer is refused at dispatch, or the run fails before a collector
runs, no in-workflow stage sees the evidence. Collect it from outside the run:

1. Wait for `run.finished`. Its `terminalCause` explains why the run ended.
2. Select the producer's last finished occurrence using the steps above.
3. Verify each item, then attach the verified files, or their states, to the
   follow-up issue or rerun.

## What is not covered yet

- No CLI command or built-in stage implements the selection and verification
  steps.
- Outbox files are not delivered through `contextFrom`.
- Pointer names are not qualified by occurrence after a repass. Changing this
  needs a new DSL version.
- A timed-out task's result file is not collected.
