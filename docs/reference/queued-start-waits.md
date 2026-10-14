# Waiting for start admission

An accepted ordinary start keeps its original payload, configuration pins,
acceptance time and reserved run identity while these operating constraints hold:

- workflow or instance parallel capacity and memory pressure;
- hourly and daily run budgets;
- the workflow's open-PR cap;
- provider quota with its recorded reset time;
- an explicitly disabled workflow or gaggle.

The shared start-intent service and durable daemon trigger adapter return such
requests to their accepted queue with the scheduler's reason visible in status.
A later drain rechecks current admission; waiting does not imply permission,
reset a budget, replace the pinned definition or add a force override. Successful
admission clears the old waiting reason. A store reopen retains the accepted
request and reason. Existing admission deadlines can still expire an unlaunched
request according to their existing custody rules.

Direct scheduler callers retain their capacity-only transient-error contract.
Unknown refusal reasons, missing capabilities, unavailable pinned placement,
harness failures and provider authentication refusals remain definitive. Missing
or revoked authorization is not interpreted as a temporary operating constraint.

This slice reuses the existing accepted state and reason field. It does not yet
add the full typed waiting metadata, next-check scheduling, operator queue
controls, or event/child-specific admission policy described in the event design.

[Bounded pending-start scans](pending-start-sweeps.md) advance through held starts
and retain scan progress across daemon reopen, so a held prefix does not pin the
next eligible window. Custody and normal admission remain separate from scanning.

## Default manual admission deadline

New ordinary manual starts capture a seven-day admission deadline with their
first durable acceptance. This covers the normal pinned HTTP and standalone
start paths. An exact retry keeps the original deadline, acceptance time, and
reserved run identity; it cannot extend the waiting period.

The deadline is checked by the existing admission owner. Expiry records a
terminal rejection with the reason `accepted trigger expired before execution
admission` only after proving the request has not launched. A matching journal
wins over the clock. Unreadable evidence or an uncertain launch keeps custody,
and the deadline does not cancel an already running workflow.

Previously accepted receipts retain their original deadline, including an absent
deadline. Explicit host deadlines, such as file-delegated admission limits, keep
their existing meaning. This default does not add expiry to scheduler signals,
schedule demand, direct-engine starts, or children. Those source policies and
the design's distinct public `expired` state remain separate work.
