package childengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// The rest of the engine child protocol on the control plane (#8085 d):
// parking a parent on its child, sealing the child's result, and resuming the
// parent with it (docs/design/engine-child-workflows.md §5-§6).

var (
	// ErrCapacityPlaneNotComposed refuses parking while no capacity plane can
	// release the parked parent's scheduler concurrency permit. Holding the
	// permit while parked would deadlock a single-slot tenant on its own child.
	ErrCapacityPlaneNotComposed = errors.New("childengine: capacity plane not composed: a parked parent's concurrency permit cannot be released")
	// ErrContinuationNotComposed refuses resuming a parent while no fenced
	// path can record its continuation artifact in the parent's journal.
	ErrContinuationNotComposed = errors.New("childengine: continuation path not composed: the parent's continuation artifact cannot be recorded")
	errResultDiffers           = errors.New("childengine: completion does not match the sealed child result")
)

var _ engine.ChildService = (*Service)(nil)

// ParentCapacity releases a parked parent's scheduler concurrency permit and
// fairly reacquires it before the parent continues. Both are idempotent: the
// engine retries them across lost acknowledgements.
type ParentCapacity interface {
	Release(context.Context, engine.ChildParkInput) error
	Reacquire(context.Context, engine.ChildCompletionInput) error
}

// ContinuationWriter records the bounded continuation artifact a resumed
// parent stage reads, in the parent's journal, as a fenced controller write.
type ContinuationWriter interface {
	WriteChildContinuation(ctx context.Context, binding engine.ChildRunBinding, wait livejournal.AppendReceipt, data []byte) (apiv1.ContextPointer, error)
}

// boundChild is the exact accepted child a binding names, bound to the same
// parent execution and manifest. Unlike admission it allows a cancelled child
// and a closed parent: a child's end always goes through the finalizer.
func (s *Service) boundChild(ctx context.Context, b engine.ChildRunBinding) (triggerqueue.ChildRecord, error) {
	identity := identityOf(b.Request)
	child, err := s.Custody.GetChild(ctx, identity)
	if err != nil {
		return child, err
	}
	r := b.Request
	if child.Identity != identity || child.AcceptanceID != r.AcceptanceID || child.RunID != r.ChildRunID || child.ProposalDigest != r.SourceDigest || !child.TombstonedAt.IsZero() {
		return child, errNotLiveChild
	}
	start, err := s.Custody.ChildEngineStart(ctx, identity)
	if err != nil {
		return child, err
	}
	if start.ManifestDigest != b.ManifestDigest || start.ParentWorkflowID != b.ParentWorkflowID || start.ParentTemporalRunID != b.ParentTemporalRunID || !start.Deadline.Equal(b.Deadline) {
		return child, errBindingDiffers
	}
	return child, nil
}

// Park releases the parent's capacity while it waits on its bound child.
func (s *Service) Park(ctx context.Context, in engine.ChildParkInput) (err error) {
	defer func() { err = classify(err) }()
	if err := s.configured(); err != nil {
		return err
	}
	if _, err := s.boundChild(ctx, in.Binding); err != nil {
		return err
	}
	if s.Capacity == nil {
		return fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, ErrCapacityPlaneNotComposed)
	}
	return s.Capacity.Release(ctx, in)
}

// Finalize seals the child's terminal result from its own journal: the state
// and finish time the child's workflow decided, the result evidence its stages
// recorded. A retry returns the same sealed result; a changed one conflicts.
func (s *Service) Finalize(ctx context.Context, in engine.ChildFinalizeInput) (_ childresult.TerminalReturn, err error) {
	defer func() { err = classify(err) }()
	if err := s.configured(); err != nil {
		return childresult.TerminalReturn{}, err
	}
	if s.Journal == nil {
		return childresult.TerminalReturn{}, errors.New("childengine: finalizing a child requires the control journal")
	}
	child, err := s.boundChild(ctx, in.Binding)
	if err != nil {
		return childresult.TerminalReturn{}, err
	}
	if child.State == triggerqueue.ChildQueued {
		// Bound by a control plane that predates bind-as-dispatch.
		if err := s.Custody.DispatchEngineChild(ctx, child.Identity); err != nil {
			return childresult.TerminalReturn{}, err
		}
		if child, err = s.boundChild(ctx, in.Binding); err != nil {
			return childresult.TerminalReturn{}, err
		}
	}
	rd, err := s.Journal(ctx, child.RunID)
	if err != nil {
		return childresult.TerminalReturn{}, fmt.Errorf("%w: child journal: %w", engine.ErrChildCustodyUnavailable, err)
	}
	events, err := rd.Events()
	if err != nil {
		return childresult.TerminalReturn{}, fmt.Errorf("%w: child journal: %w", engine.ErrChildCustodyUnavailable, err)
	}
	references, summary, err := childworkflow.ResultEvidence(rd, events)
	if err != nil {
		return childresult.TerminalReturn{}, err
	}
	if summary == "" {
		summary = "Child workflow " + string(in.State)
	}
	input := childworkflow.TerminalResultInput{State: in.State, FinishedAt: in.FinishedAt, Summary: summary, References: references}
	result, err := childworkflow.CaptureTerminalResult(ctx, s.Custody, child, input)
	if err != nil {
		return childresult.TerminalReturn{}, err
	}
	if !child.State.Terminal() {
		if err := s.Custody.SetChildState(ctx, child.Identity, triggerqueue.ChildStateUpdate{Expected: child.State, State: in.State, ResultRef: result.ResultRef}, s.Now()); err != nil {
			return childresult.TerminalReturn{}, err
		}
	}
	return childresult.TerminalReturn{Version: childresult.TerminalReturnVersion, Identity: child.Identity, AcceptanceID: child.AcceptanceID, RunID: child.RunID,
		SourceDigest: child.ProposalDigest, Input: result.Input, ResultRef: result.ResultRef}, nil
}

// Complete verifies the parent resumes with exactly the sealed result,
// records the continuation artifact the resumed stage reads, and reacquires
// the parent's capacity.
func (s *Service) Complete(ctx context.Context, in engine.ChildCompletionInput) (_ apiv1.ContextPointer, err error) {
	defer func() { err = classify(err) }()
	if err := s.configured(); err != nil {
		return apiv1.ContextPointer{}, err
	}
	child, err := s.boundChild(ctx, in.Binding)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	sealed, err := childworkflow.ReadTerminalResult(ctx, s.Custody, child)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	if sealed.ResultRef != in.Result.ResultRef || !sameTerminalInput(sealed.Input, in.Result.Input) {
		return apiv1.ContextPointer{}, errResultDiffers
	}
	if s.Continuations == nil {
		return apiv1.ContextPointer{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, ErrContinuationNotComposed)
	}
	if s.Capacity == nil {
		return apiv1.ContextPointer{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, ErrCapacityPlaneNotComposed)
	}
	data, err := json.Marshal(in.Result)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	pointer, err := s.Continuations.WriteChildContinuation(ctx, in.Binding, in.Wait, data)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	if err := s.Capacity.Reacquire(ctx, in); err != nil {
		return apiv1.ContextPointer{}, err
	}
	return pointer, nil
}

func sameTerminalInput(a, b childworkflow.TerminalResultInput) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}
