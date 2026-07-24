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
//   - taskOutcome does not honor the #415 non-retryable escalation bypass
//     (ISSUE_OVER_SCOPE / NEEDS_DECOMPOSITION route straight to escalation on
//     the local runner, bypassing the Next gate).
//   - WorkspaceBranchOutput (#392) sticky workspace-branch rebinding is not
//     applied between stages.
//   - Cumulative agentic usage budgets (limits.maxTokens / maxCostUSD) are not
//     enforced here — the local runner fails closed via enforceStageBudget.
//     Moot until the agentic executor seam is wired (stages needing it fail
//     closed today), but it must land with that wiring.
//   - The context-manifest artifact is journaled even when workspace
//     provisioning failed; the gate-evaluator has no per-attempt deadline; a
//     provider-mutation ref.touched analogue is absent; InputsFrom failures
//     produce no stage-attributed events; and StartWorkflow leaves
//     WorkflowIDReusePolicy at the default (a completed run-id can re-execute).
//
// The verdict-ArtifactPointer projection hazard (a pre-scrub pointer can dangle
// when scrubbing changes the bytes) is the same class and tracked with them.
package engine
