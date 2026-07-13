// Package runner is the substrate-neutral local runner core (ARCHITECTURE.md
// §3.1): it advances a compiled workflow.Machine (#9) stage-by-stage, durably
// records every transition to the run journal (#8), and dispatches each task
// through the pre-existing internal/invoke seam (Deterministic/Goober) — the
// same interface #18's shell executor and #19's agentic executor implement
// against, already wired at internal/engine/activities.go. Gate dispatch (#20)
// goes through internal/gate.Evaluator, which wraps that same invoke.Automated/
// invoke.Goober boundary.
//
// This file defines the one small seam of its own the runner needs on top of
// invoke.*: ArtifactRecorder, and the per-run construction factories, because
// a task executor (e.g. internal/executor.ShellExecutor) binds its journal
// reference at CONSTRUCTION time, not per call — so it must be built fresh for
// each run's own *journal.Run, never shared across runs.
package runner

import (
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// ArtifactRecorder persists stage output bytes into the run journal by content
// digest. Mirrors internal/executor.ArtifactRecorder and internal/gate's
// Journal interface — *journal.Run satisfies all three structurally (same
// RecordArtifact method), so the runner hands its run's own journal.Run
// straight through with no adapter.
type ArtifactRecorder interface {
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// NewDeterministicFunc constructs the deterministic-task executor for one run,
// bound to rec so its captured output/result-file artifacts land in that
// run's journal (e.g. internal/executor.NewShellExecutor(injector, rec)).
// Required if the workflow has any deterministic task.
type NewDeterministicFunc func(rec ArtifactRecorder) (invoke.Deterministic, error)

// NewAgenticFunc constructs the agentic executor for one named goober, bound
// to rec. Keyed by goober name since a single Runner/run can target more than
// one distinct goober (e.g. "coder" for a task, "reviewer" for its gate); the
// caller's closure resolves that name to its instructions/model/harness
// config. Required if the workflow has any agentic task or agentic gate. The
// same constructed invoke.Goober serves both a Task.Goober's Invoke and a
// paired AgenticGate's Review — one instance, two methods.
type NewAgenticFunc func(gooberName string, rec ArtifactRecorder) (invoke.Goober, error)
