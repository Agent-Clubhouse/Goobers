// Package runner is the substrate-neutral local runner core (ARCHITECTURE.md
// §3.1): it advances a compiled workflow.Machine (#9) stage-by-stage, durably
// records every transition to the run journal (#8), and dispatches each task
// through the StageExecutor seam defined in this file — the interface #18
// (deterministic) and #19 (agentic) implement against. Gate dispatch (#20) is
// NOT a seam defined here: it goes through internal/gate.Evaluator directly
// (built independently against the pre-existing internal/invoke interfaces),
// which already owns bounded-repass, escalation, and gate.evaluated
// journaling — see run.go's use of it.
package runner

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

// StageRequest is everything the runner hands a stage executor or gate
// evaluator for one attempt. Two things never travel any other way:
//
//   - Workspace: req.Envelope.Workspace is already the absolute path to a
//     fresh, isolated, disposable worktree (ARCHITECTURE §5) — the runner
//     creates it (internal/worktree, #16) and populates the field before
//     dispatch. An executor never talks to the worktree manager.
//   - Credentials: req.Credentials is materialized fresh per attempt, scoped
//     to exactly the stage's declared capabilities (internal/credentials,
//     #14) — never serialized onto the envelope, because the envelope is
//     journaled and echoed to downstream stages as context; a credential must
//     never land at rest or reach a stage that didn't declare it.
type StageRequest struct {
	// Envelope is the stage contract's invocation envelope (#10):
	// goal/workspace/contextPointers/capabilities/inputs/limits.
	Envelope apiv1.InvocationEnvelope
	// Credentials is scoped to Envelope's own capability declarations —
	// Credentials.Token(cap) fails closed for anything not declared by this
	// stage, even if some other stage in the run has that grant.
	Credentials *credentials.Set
}

// StageOutput is what a StageExecutor returns instead of a bare
// ResultEnvelope. Artifacts are not yet ArtifactPointers: an executor doesn't
// know the journal's on-disk layout (that's runner/journal-owned, #8), so it
// hands back raw content and the runner commits it via
// journal.Run.RecordArtifact, turning the resulting Ref into the
// ArtifactPointer that actually lands in Result.Artifacts and gets journaled.
// This keeps §2.4/§5 ("stages exchange pointers, never implicit shared
// state") true without leaking journal internals into every executor.
type StageOutput struct {
	// Result is the stage's outcome: status, scalar outputs, summary, error.
	// Leave Artifacts empty here — see Produced.
	Result apiv1.ResultEnvelope
	// Produced are the stage's non-scalar outputs, as raw bytes. The runner
	// commits each to the journal and appends the resulting ArtifactPointer to
	// Result.Artifacts before the stage.finished event is journaled.
	Produced []ProducedArtifact
}

// ProducedArtifact is one artifact an executor produced, pre-journal-commit.
type ProducedArtifact struct {
	// Name is the artifact's logical handle — becomes the ContextPointer.Name
	// a downstream stage sees this artifact under.
	Name string
	// Data is the raw artifact bytes. The journal scrubs (before digesting —
	// ARCHITECTURE §4) and commits them by content digest.
	Data []byte
	// MediaType optionally categorizes Data (advisory; carried onto the
	// resulting ArtifactPointer).
	MediaType string
}

// StageExecutor runs one deterministic or agentic stage attempt to
// completion. The runner selects an executor by apiv1.Task.Type — #18
// registers for TaskDeterministic, #19 for TaskAgentic — and calls Execute
// exactly once per attempt; retries (bounded, policy-driven) are the runner's
// concern (§5), not the executor's — an executor never retries internally.
type StageExecutor interface {
	Execute(ctx context.Context, req StageRequest) (StageOutput, error)
}

// Executors selects a StageExecutor by task type. The runner's dispatch step
// looks up m[task.Type]; an unregistered type is a configuration error, not a
// silent no-op.
type Executors map[apiv1.TaskType]StageExecutor
