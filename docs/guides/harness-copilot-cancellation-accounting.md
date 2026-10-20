# Copilot usage when a stage is cancelled

A controlled Copilot session keeps its owned headless server alive for a bounded
accounting grace period after caller cancellation or deadline expiry. Prompt and
readiness RPCs retain the original cancellation/deadline. Once the prompt wait
returns, the adapter attempts `session.abort`, reads available session usage, and
shuts the session down using a cancellation-independent context. Those final RPCs
share a five-second budget. An independent process watchdog enforces the same
grace window even if finalization cannot progress; owned-process cleanup still
joins the process before the adapter returns.

Finalization runs once per session. The later adapter telemetry step uses the
retained snapshot instead of querying a server that has already exited or adding
the same usage twice. Completion-repair sessions reset that finalization state.

If the endpoint has exited, fails, returns invalid usage, or does not answer in
time, the run error explicitly says usage is unknown. An unconfirmed abort is
also reported as possibly incomplete accounting. These diagnostics contain no
native response details. Neither path invents a measured zero, and cancellation
or timeout remains identifiable in the returned error.

This guarantee covers SDK-controlled sessions. Custom launcher telemetry and
provider-side billing settlement retain their existing behavior.
