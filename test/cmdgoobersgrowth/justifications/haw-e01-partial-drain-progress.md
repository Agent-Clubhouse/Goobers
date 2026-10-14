# Interrupted daemon sweep progress

The daemon drain owner must reserve one candidate immediately before inspection,
so interruption after the first candidate cannot advance past99 uninspected
records. The shared store cursor and per-record execution claims remain unchanged.
A100-entry visited set prevents cycling over a small held queue within one pass.

The additional command code belongs with the existing drain owner because it
combines cancellation, candidate enumeration and the distinct execution adapters.
The store continues to expose its bounded transactional reservation primitive.
No generic queue or scheduler abstraction is duplicated. Tests exercise200 held
records with an interruption on every pass and a daemon-store reopen midway.
This step does not add or claim a wall-clock deadline for active admission.
