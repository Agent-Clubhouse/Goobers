// Package localscheduler is the tiers-1–2 embedded scheduler (issue #21,
// ARCHITECTURE.md §7, docs/requirements/scheduler.md). It is embedded in the
// local runner daemon (`goobers up`) — no separate service, no database.
//
// It owns three things:
//
//   - Cron evaluation: evaluates each workflow's declared schedule trigger and
//     fires a run when due (SCH-041). V0 ships standard 5-field cron,
//     descriptors, and "@every <dur>" — the same grammar internal/workflow's
//     compile-time CheckSchedules structurally validates; this package owns
//     firing.
//   - Run conditions: enforces max-parallel and per-workflow run budgets before
//     starting a run (SCH-003), skipping (never failing) and journaling the
//     reason when saturated.
//   - The claim ledger: the authoritative, atomic, lease-based source of truth
//     for exactly-once backlog-item processing (SCH-020/BL-005). The provider
//     marker (#12's ClaimWorkItem) mirrors this ledger for human visibility; it
//     is never the source of truth.
//
// This is the one scheduler. The earlier Temporal-era (M7) scheduler fork,
// internal/scheduler, was deleted per goobernetes-architecture.md D5/§4
// (#2055 resolved: supersede) — distributed execution reuses this package's
// policy on the daemon, unforked.
//
// Every scheduling decision and claim-ledger transition is journaled to the
// instance journal (journal.InstanceLog, scheduler/events.jsonl) under the same
// envelope and append-only rules as a run journal, so the portal, telemetry,
// and Tutor read scheduling history the same way they read runs.
package localscheduler
