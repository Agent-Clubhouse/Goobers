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
// Plan item E10's residual (#3929) removed a divergence of a different kind:
// one this package created for itself. Both drivers must decide whether a
// gate's retry route earns a learning episode, and the engine answered it with
// its own re-derived gateSendsBack predicate over its upstream map while the
// runner answered it not at all (it injected on every retry route). #3917
// caught the disagreement on a FORWARD branch — a gate routing onward to a
// stage that has never run — and registered it as an expected parity failure.
//
// The ruling is that neither derivation was the question: the gate already
// computes, charges and journals a repass attempt, and an episode belongs to a
// TRUE REPASS, which is exactly repassAttempt >= 1. That predicate now lives
// once, in runner.LearningEpisodeAppliesToRepass, and both drivers call it;
// gateSendsBack is deleted. Deriving a fact twice is how two drivers drift, so
// the shared helper is the fix rather than an incidental tidy-up.
//
// #3931 removed a third kind: not a divergence between the drivers, but a
// defect they SHARED, and therefore one the parity table graded green.
//
// An episode's nextAttempt — the attempt the correction is addressed to — was
// derived as the failing stage's attempt plus one, on both sides, through the
// shared builder. That is right only when the gate sends work back to the
// stage that failed. Every shipped nontrivial send-back separates them:
// implementation.yaml's `local-gate: fail -> implement` grades a `local-ci`
// subject, `ci-gate: fail -> remediate-ci` grades `ci-poll`. There the subject
// runs once per cycle while the target accumulates re-entries, so the
// correction was addressed to an attempt of the target that had already
// happened with different content. The episode now carries the TARGET's own
// next attempt, derived from the target's entry history
// (runner.ResolveLearningEpisodeAddressing), while SourceAttempt keeps naming
// the subject — the two answer different questions and had been conflated.
//
// The lesson for this ledger is about fixtures rather than about attempts. The
// defect survived three E10 rows and a ruling because every one of them was a
// TRIVIAL send-back, where the two derivations produce the same number and no
// assertion can tell them apart. A parity table cannot see a shared defect at
// all, and a degenerate fixture cannot see a defect either way; the fix is a
// non-degenerate row (E10-learning-episode-send-back), not a stronger
// assertion on a degenerate one.
//
// nextAttempt is inside the episode's bytes, so the change moves the artifact's
// content digest. It does NOT move learning.EpisodeID, which is addressed over
// SourceRunID, SourceSeq and finding identities alone — the join key every
// cross-run consumer correlates on is stable, which is what bounded the
// migration to one Temporal workflow version rather than a data migration. The
// engine's switch is gated on workflow.GetVersion("learning-episode-target-
// attempt"); the runner's is not, because the runner re-reads recorded
// artifacts where the engine re-derives them on replay.
//
// #3932 was a divergence internal to the local runner, invisible here for a
// structural reason worth recording: ruling R9 refuses spec.parallels at run
// start on the engine, so no parity row can reach it. The runner has two walks
// that take a gate retry arm — stepGate and, at maxConcurrentBranches > 1,
// runBranch — and the second carried a hand-copied HALF of the first, the
// verdict pointer without the learning episode. A scheduling bound decided
// whether a repass received its correction. Both now share one producer
// (runner.recordGateRetryInjection) and one predicate, and the equivalence of
// the two widths is asserted directly. Divergence-by-duplication is the same
// shape as #3929's, one layer down: the fix is a shared producer, not a second
// correct copy.
//
// One boundary note, because it was gotten wrong twice: the learning-episode
// PRODUCER on the generic retry arm (learningEpisode here, recordLearningInjection
// in the runner) is lane-agnostic, not implementation-lane, and was filed as
// E10 (#3913). It landed here with E4-E9 ahead of that split; #3913 therefore
// kept the halves the behaviour did not carry — the E10 parity rows, the
// dedicated replay proof that the digest a REPLAY re-derives is the digest the
// original walk produced, and the shared-helper suite — rather than a second
// copy of the behaviour. Removing the producer to "restore" the boundary breaks
// those rows; the boundary lives in the inventory and the rows, not in a
// second implementation.
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
