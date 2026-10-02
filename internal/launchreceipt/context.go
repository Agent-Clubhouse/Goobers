package launchreceipt

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/journal"
)

type bindingKey struct{}

// WithBinding transports a controller-owned start through runtime context. It
// must never be populated from model output or invocation-envelope fields.
func WithBinding(ctx context.Context, binding Binding) context.Context {
	return context.WithValue(ctx, bindingKey{}, binding)
}

// ContextBinding returns the exact durable start selected by the runtime.
func ContextBinding(ctx context.Context) (Binding, bool) {
	binding, ok := ctx.Value(bindingKey{}).(Binding)
	return binding, ok
}

// WithJournalStart binds an append-returned sequence to immutable run pins.
// Narrow test journals may omit pins; production launch admission rejects a
// missing binding rather than reconstructing one from a journal high-water mark.
func WithJournalStart(ctx context.Context, source any, event journal.Event, seq uint64) context.Context {
	pins, ok := source.(interface {
		PinnedRun() (string, string, string)
		Branch() int
	})
	if !ok || seq == 0 {
		return ctx
	}
	runID, workflowDigest, gooberDigest := pins.PinnedRun()
	branch := event.Branch
	if branch == 0 {
		branch = pins.Branch()
	}
	return WithBinding(ctx, Binding{
		RunID: runID, Stage: event.Stage, Branch: branch, StartedSeq: seq,
		AttemptID: journal.StageAttemptID(runID, branch, event.Stage, seq),
		Number:    event.Attempt, Class: event.AttemptClass,
		Review:         event.Type == journal.EventReviewerStarted,
		WorkflowDigest: workflowDigest, GooberDigest: gooberDigest,
	})
}

// ForInvocation rejects missing or cross-attempt authority without accepting
// any identity, role, or configuration claim from model-authored output.
func ForInvocation(ctx context.Context, env apiv1.InvocationEnvelope, review bool) (Binding, error) {
	binding, ok := ContextBinding(ctx)
	if !ok || !binding.valid() || binding.RunID != env.RunID || binding.Number != int(env.Attempt) || binding.Review != review ||
		(env.TaskID != binding.Stage && env.TaskID != binding.RunID+":"+binding.Stage) {
		return Binding{}, ErrInvalid
	}
	return binding, nil
}
