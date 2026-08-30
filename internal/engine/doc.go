// Package engine is the Goobers Temporal runner: the tier-3 adapter around the
// substrate-neutral workflow core in internal/workflow (see docs/ARCHITECTURE.md
// §3, §11). The core owns definition compilation and the compiled state machine;
// this package hosts that machine as a deterministic Temporal workflow (Run),
// walking the states and driving the canonical invocation → result → verdict
// envelopes between nodes. The same compiled machine backs the local runner (V0)
// without Temporal, which is what makes "one system, three tiers" enforceable.
//
// Tier-3 (V2) — quarantined, not on the V0 path. See docs/ARCHITECTURE.md §11.
// Revived in V2; internal/runner is the V0-live runner. (#125: this package
// previously read as the live adapter with no banner, even though
// buildInvocation's own comment already calls it "superseded".)
//
// Design rules (Temporal determinism):
//   - The workflow function (Run) contains no wall-clock reads, randomness, or
//     I/O. Every side effect — invoking a goober, running a deterministic task,
//     evaluating a gate — happens in an Activity.
//   - A run executes against a pinned definition snapshot carried in RunInput,
//     so registering a new version never mutates an in-flight run (WF-016).
//
// The actual goober invocation is stubbed behind the GooberInvoker interface;
// the runtime (M8) provides the implementation. The engine ships fakes for its
// own tests.
//
// # Drift ledger (#156) — known local-runner divergences, tracked not fixed
//
// The A1 revival closed the envelope/retry/gate/registry gaps (#621/#622/#624/
// #626) and the A2 dual-runner conformance harness (#637) now diffs journals
// over the conformance surface. A 2026-07-24 holistic review surfaced further
// divergences from internal/runner that this revival slice does NOT yet close;
// they are recorded here (and belong on #156) because the engine is quarantined
// and off the V0 path — none affects the live local runner — and because
// closing them is follow-on work, not a reason to weaken the conformance
// surface to make a fixture pass:
//
//   - Cumulative agentic usage budgets (limits.maxTokens / maxCostUSD) are not
//     enforced here — the local runner fails closed via enforceStageBudget.
//     Moot until the agentic executor seam is wired (stages needing it fail
//     closed today), but it must land with that wiring.
//   - The context-manifest artifact is journaled even when workspace
//     provisioning failed; the gate-evaluator has no per-attempt deadline; and
//     InputsFrom failures produce no stage-attributed events.
//
// Plan item E4-E9 (#3882) closed the implementation-lane entries this ledger
// used to carry — the transient worktree-provision reclassification and the
// learning-episode injection gap the parity harness itself surfaced — along
// with the seven finding-002 inventory rows beside them: the cached verdict,
// the reviewer diff artifact with its dedup and empty-diff fast-fails, the
// repass cause and remediation-evidence obligation, the onTimeout salvage
// marker, the CONTEXT_NOT_INSPECTED re-dispatch, the base-sync conflict
// detail, and the #3366 unpushed-diff capture. Each is now pinned by a row of
// the parity table for the same reason E2's are.
//
// Plan item E2 (#3874) closed four of the entries this ledger used to carry:
// the #415 non-retryable escalation bypass, the retry-decision annotation and
// its knownOutcome shortcut, RunResult.NoWork, and stage-qualified inputsFrom
// resolution. Each is now pinned by a row of the runner-vs-engine parity table
// (internal/engine/parity_row_*_test.go) rather than by prose here, which is
// the point of that table: a closed divergence that is only asserted in a
// comment reopens silently.
//
// The #629 remnant closed the provider-mutation ref.touched gap and moved result
// and verdict scrubbing to the activity boundary, before Temporal records the
// payload. Verdict ArtifactPointers therefore address the exact scrubbed bytes
// the projection commits instead of dangling when redaction changes a digest.
package engine
