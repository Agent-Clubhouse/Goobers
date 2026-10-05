# HAW-EVT-007: terminal producer outbox retention

This slice adds a cursor and two calls to the existing bounded daemon event
sweep. The host already provides joined-run custody and journal location through
the event execution service; no new daemon service or background loop is added.

Exclusive terminal verification, durable settlement, compaction, tombstones,
ancestry retention and exact observation stay in internal packages. Composed
acceptance tests prove completed recovery does not execute again after compaction
and failed consumers can still ResumeFromTerminal after the replay window.
