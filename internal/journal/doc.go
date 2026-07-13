// Package journal owns the run journal — the product's provenance contract
// (docs/ARCHITECTURE.md §4). Every run, on any runner and at any tier, produces
// the same inspectable, append-only record; the portal, telemetry rollup,
// Tutor, and humans debug from the journal, never from runner internals.
//
// Layout, per run:
//
//	runs/<run-id>/
//	  run.yaml       # pinned identity: workflow name+version, gaggle, trigger, input refs
//	  state.json     # current machine state; atomically replaced checkpoint (derived)
//	  events.jsonl   # append-only event journal; every event carries a monotonic seq
//	  inputs/        # immutable, content-digested snapshots of run inputs
//	  artifacts/     # stage outputs, stored by content digest, referenced by pointer
//	  spans/         # per-stage trace spans (telemetry, not conformance)
//
// Durability at tiers 1–2 is append + fsync; crash recovery replays state.json +
// the tail of events.jsonl and detects/repairs a torn final write. The same
// on-disk shape is the projection target for the tier-3 Temporal runner, so this
// package is the single definition of the format both runners emit (§3.3).
//
// Two rules are load-bearing and enforced here:
//
//   - Append-only events, immutable snapshots. Nothing is edited after the fact;
//     repairs append corrective events. The one sanctioned exception is secret
//     remediation (Redact), which replaces a leaked blob and appends a redaction
//     event recording the old→new digests.
//   - Redaction at the boundary. Every event, snapshot, and artifact is scrubbed
//     before write and before digesting, so digests commit to the scrubbed bytes
//     and no raw secret lands at rest (SEC-041, TEL-013).
package journal
