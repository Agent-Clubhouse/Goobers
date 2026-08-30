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
//   - Transient worktree-provision failures (worktree.IsTransientProvisionError)
//     are not reclassified to invoke.InfrastructureFailure on this path
//     (workerhost.WorktreeWorkspaces.Provision → classifySeamError), so a clone/
//     fetch flake burns the policy budget instead of the infra budget the local
//     runner (#572) gives it.
//   - Learning-episode injection (#3874, newly observed by the parity harness
//     and not yet an entry in the finding-002 inventory): on the SAME gate-retry
//     arm that writes the retry decision, the local runner also records a
//     learning/episode-<gate>-<seq>.json artifact and threads a
//     learning.episode[<seq>] context pointer into the re-entered stage
//     (recordLearningInjection). The engine does neither, so a repassed stage is
//     re-invoked here without the correction feedback its local counterpart
//     receives. It is bounded and named by the parity rows that walk a fail
//     branch (parity_row_retry_decision_test.go), never silently tolerated.
//   - Cumulative agentic usage budgets (limits.maxTokens / maxCostUSD) are not
//     enforced here — the local runner fails closed via enforceStageBudget.
//     Moot until the agentic executor seam is wired (stages needing it fail
//     closed today), but it must land with that wiring.
//   - The context-manifest artifact is journaled even when workspace
//     provisioning failed; the gate-evaluator has no per-attempt deadline; and
//     InputsFrom failures produce no stage-attributed events.
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
