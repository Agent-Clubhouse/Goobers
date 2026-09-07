# Live stage timing in dashboard lists

Runs, Overview, Gaggle (active and recent), and Workflow lists distinguish the
whole workflow's elapsed time from each active stage/evaluation. Agentic work
names the goober recorded when that attempt started; deterministic work names
only its stage. Parallel branches remain separate, and a task retry starts a
new elapsed interval. Gate timing covers the gate evaluation, including its
internal evaluator retries; it is not an estimate of model token-generation time.

The lists advance once per second without a navigation or refresh. They anchor
to the daemon's reported workflow duration rather than subtracting browser time
from a server timestamp, so ordinary browser/daemon clock skew does not inflate
the durations. One shared timer serves mounted active rows and is removed when
they unmount. Terminal durations remain fixed; the run detail's per-stage and
per-attempt history is unchanged.

The API's optional `activeStages` list carries name, kind, branch, attempt,
goober (when known), and `startedAt`. Both the SQLite read model and journal
fallback fold the same start/finish events; no per-row journal reopening or
lookup into the latest workflow configuration is needed. Old events without an
owner show stage timing without inventing a goober. Missing start timestamps
show “elapsed unavailable”.

Active state is capped at 1,024 entries per run as a guard against unbalanced
journals. Overflow sets `activityTruncated` and is displayed explicitly. Finish
events remove matching attempts; run completion and resume boundaries clear the
state. No new historical artifact or unbounded timing ledger is introduced.
