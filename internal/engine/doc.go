// Package engine is the Goobers Temporal runner: the tier-3 adapter around the
// substrate-neutral workflow core in internal/workflow (see docs/ARCHITECTURE.md
// §3, §11). The core owns definition compilation and the compiled state machine;
// this package hosts that machine as a deterministic Temporal workflow (Run),
// walking the states and driving the canonical invocation → result → verdict
// envelopes between nodes. The same compiled machine backs the local runner (V0)
// without Temporal, which is what makes "one system, three tiers" enforceable.
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
package engine
