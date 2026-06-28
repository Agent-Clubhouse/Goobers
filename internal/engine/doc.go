// Package engine is the Goobers workflow execution engine. It translates a
// Workflow definition (api/v1alpha1.WorkflowSpec — an ordered set of Task and
// Gate states) into a deterministic Temporal workflow that walks the state
// machine, driving the canonical invocation → result → verdict envelopes
// between nodes.
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
