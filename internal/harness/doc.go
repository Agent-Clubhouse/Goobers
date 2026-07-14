// Package harness implements the agentic stage executor: the harness-adapter
// seam an agentic stage uses to drive an external coding agent
// (docs/ARCHITECTURE.md §5, GBO-051), plus the concrete GitHub Copilot CLI
// adapter and a scripted fake adapter used for deterministic tests and as the
// conformance-fixture harness.
//
// Seam shape (GBO-051, issue #19):
//
//   - Adapter is the only extension point. Adding a second harness (e.g.
//     Claude Code) means writing a new Adapter and registering it — never
//     touching Executor or the runner (proven, not just asserted, by
//     TestRegistrySwapRequiresNoExecutorChange).
//   - A stage finishes by writing its result (or verdict) as JSON to a
//     declared path inside its workspace — the local analog of the
//     "completion tool" (GBO-013). The Adapter reads that file back; a
//     missing or invalid file fails the call closed, never a partial
//     success.
//   - Credentials arrive pre-scoped: Executor materializes a
//     credentials.Set for exactly the invocation envelope's declared
//     capabilities before calling the adapter, so an adapter can never
//     resolve a credential for a capability the stage didn't declare
//     (internal/credentials.Set.Token fails closed on that itself).
//   - The harness transcript is captured and handed to the caller-supplied
//     SpanRecorder (typically internal/journal.Run.RecordSpan) after
//     passing through the caller-supplied Scrubber. This package does
//     import internal/journal (since #73/#94), but only for its small,
//     stable Ref/Scrubber value types — never its full durability/
//     event-log machinery — mirroring how internal/credentials defines
//     its own SecretRegistrar seam rather than depending on journal for
//     that narrower purpose.
package harness
