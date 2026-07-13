// Package executor implements deterministic stage execution (ARCHITECTURE.md
// §5, TSK-020): running a declared shell command inside a stage's worktree and
// mapping the result to the stage contract's ResultEnvelope, plus the ci-poll
// automated-gate primitive that drives the implementation workflow's
// CI-poll/repass loop (GT-020).
//
// ShellExecutor implements invoke.Deterministic — the existing engine↔runtime
// seam (internal/invoke) — so it plugs into the runner (#17) the same way any
// other implementation of that interface would, without this package
// depending on the runner. It depends only on already-merged V0 packages:
// api/v1alpha1 (stage contract, #10), internal/credentials (capability-scoped
// tokens, #14), and internal/journal (artifact recording + secret scrubbing,
// #8). It does not create or manage worktrees itself — InvocationEnvelope.Workspace
// is guaranteed to already exist by whoever dispatches the stage (#16, #17).
//
// Isolation: the child process's environment is built explicitly (PATH/HOME/
// TMPDIR carried forward so subprocesses like `make` can find their own
// tools, plus one var per declared, granted capability) — never
// os.Environ() wholesale (SEC-045). A timeout kills the whole process group,
// not just the direct child, so runaway subprocesses cannot outlive the
// stage. Captured stdout/stderr pass through a local secret scrubber (in
// addition to whatever the journal's own scrubber does on persist) before
// this package uses them for anything, so a credential this executor itself
// materialized can never appear in a returned artifact or output preview.
//
// api/v1alpha1.DeterministicRun carries only Command/Image at V0; timeout and
// the optional result-file declaration travel through Task.Inputs (merged
// into InvocationEnvelope.Inputs by whoever builds the envelope) under the
// well-known keys in this package, rather than as new DeterministicRun
// fields — see the InputTimeout/InputResultFile/InputMaxOutputBytes
// constants. This avoids a concurrent edit to the shared DSL types while
// #17/#19/#20 are also converging on the executor seam.
package executor
