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
