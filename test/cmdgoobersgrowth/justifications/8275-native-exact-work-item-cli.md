# Native exact-work-item CLI targeting

The command package grows to expose the existing exact-work-item start intent
through native `run` flags and carry it through API, standalone, detached, and
same-root file adapters. These adapters already live in `cmd/goobers`; their
flag parsing, capability checks, receipt handling, and legacy-file refusals
belong beside the corresponding command and daemon-sweep code.

Reusable target validation, durable acceptance, pinned inputs, replay/conflict
handling, and exact scheduling remain in the existing internal packages. The
additive daemon-lock capability round-trips through `internal/instancelock`.
This change adds no command-package file, queue, storage ledger, cloud-specific
transport, or alternative scheduler. Caller-keyed file delegation remains
rejected rather than introducing command-owned retention machinery.

The additional predicates keep the affected command functions within the
existing complexity caps. This declaration covers only the exact-target CLI
adapter growth in this PR; it does not change the informational snapshot or
raise a complexity or growth threshold.
