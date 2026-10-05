# Accepted child wait recovery

The command addition adapts the daemon-owned accepted-child queue to the runner's
existing handoff interface during recovery. It performs one bounded child lookup,
preserves cancellation fences, and restores a host wait after physical pod custody
has been reconciled. Retry counters, usage, original context, durable wait records,
and continuation scheduling remain in `internal/runner`; no queue SQL or Git engine
is duplicated in the command layer. This adapter needs the daemon service registry
and queue ownership and therefore belongs beside `daemonChildHandoff`.
