// Package executor implements deterministic stage execution (ARCHITECTURE.md
// §5, TSK-020): running a declared shell command inside a stage's worktree,
// and the ci-poll built-in deterministic-stage kind that drives the
// implementation workflow's CI-poll/repass loop.
//
// ci-poll is a TASK (TaskDeterministic), not a gate: it polls CI to a
// terminal state and reports the verdict as ResultEnvelope.Outputs["ciStatus"]
// ("success"/"failure"). A separate automated gate then branches on that
// output via internal/gate's "ci-status" check (#20) — gates read a task's
// flattened Outputs, never poll anything themselves (ARCHITECTURE.md §2.4).
// TaskExecutor is the single dispatcher a caller registers for
// apiv1.TaskDeterministic: it routes to ShellExecutor or CIPollExecutor by
// InvocationEnvelope.Inputs[InputKind].
//
// Every type here is deliberately decoupled from the in-flight local runner
// (#17): ShellExecutor.Run and CIPollExecutor.Run take plain values (a
// workspace path, a TokenSource, config structs) and return raw
// ProducedArtifact bytes rather than committing anything to a journal or
// depending on internal/runner's types. This lets #18 be built, tested, and
// merged independently of #17's timeline; whoever wires
// runner.Executors[apiv1.TaskDeterministic] (very likely a small follow-up in
// this package once internal/runner lands on main) adapts TaskExecutor.Run's
// return values into runner.StageOutput — converting []ProducedArtifact into
// []runner.ProducedArtifact is a one-line loop, since the two types are
// structurally identical by design.
//
// TokenSource is this package's own minimal credential-lookup interface
// (Token(ctx, capability) (string, error)) rather than a dependency on
// *credentials.Set directly — *credentials.Set satisfies it structurally, so
// a caller (or a test) can supply any token source without pulling in
// internal/credentials' construction machinery. Every resolved token is
// still registered with a local journal.RegistryScrubber and captured
// stdout/stderr are scrubbed through it before this package uses them for
// anything, so a credential can never reach a produced artifact.
//
// Known integration gaps, intentionally left to whoever wires the runner
// (flagged in #mission-runner-core rather than guessed at here):
//   - How apiv1.DeterministicRun (Command/Image) and a gate's
//     AutomatedGate{Check,Params} reach a stage/gate's InvocationEnvelope is
//     not yet settled by #17's dispatch code (as of this package's initial
//     version, runner.prepareStage does not thread either through). This
//     package's Run methods take DeterministicRun/config explicitly so they
//     work as soon as that's fixed, whichever shape it lands in.
//   - How a PR number produced by an earlier "open PR" task reaches ci-poll's
//     env.Inputs["prNumber"] is a workflow-wiring concern (#27) — ci-poll
//     itself only requires the value to already be there.
package executor
