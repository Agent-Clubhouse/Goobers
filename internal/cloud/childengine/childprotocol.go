package childengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
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
	errResultDiffers            = errors.New("childengine: completion does not match the sealed child result")
	// errChildAlreadyFinished refuses parking on a child whose result is
	// sealed: the parent has resumed or is resuming, and a late Park (from a
	// timed-out attempt) must not lend the running parent's slot again.
	errChildAlreadyFinished = errors.New("childengine: the child has finished; its parent cannot park on it")
)

// maxContinuationBytes is the engine's bound on a continuation artifact.
const maxContinuationBytes = 64 << 10

var _ engine.ChildService = (*Service)(nil)

// ParentCapacity releases a parked parent's scheduler concurrency permit and
// fairly reacquires it before the parent continues. Both are idempotent: the
// engine retries them across lost acknowledgements.
type ParentCapacity interface {
	Release(context.Context, engine.ChildParkInput) error
	Reacquire(context.Context, engine.ChildCompletionInput) error
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
	child, err := s.boundChild(ctx, in.Binding)
	if err != nil {
		return err
	}
	// Finalize marks the child terminal with its sealed result before
	// Complete can run, so a Park arriving after Complete always sees it.
	if child.State.Terminal() {
		return errChildAlreadyFinished
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
// reacquires the parent's capacity, and returns the continuation the resumed
// stage reads: a pointer to the sealed result receipt itself.
//
// Control never writes the parent's journal. The journal admits only its run
// owner's token (journalpg writeRun), so the parent journals this pointer on
// its own path (ChildContinuedKind). The receipt needs no new blob: Finalize
// stored it in the shared blob store, and control.child_lineage references it
// (result_receipt_digest, result_ref), which control.blob_is_referenced
// retains against GC. That reference lives until the child is tombstoned,
// which waits for the parent to settle, so it covers the whole window from
// here until the parent has journaled the pointer and resumed.
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
	stored, err := s.Custody.ChildResult(ctx, child.Identity)
	if err != nil {
		return apiv1.ContextPointer{}, err
	}
	if stored.ReceiptDigest != sealed.ResultRef || len(stored.Receipt) == 0 || len(stored.Receipt) > maxContinuationBytes {
		return apiv1.ContextPointer{}, triggerqueue.ErrChildResultUnavailable
	}
	if s.Capacity == nil {
		return apiv1.ContextPointer{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, ErrCapacityPlaneNotComposed)
	}
	if err := s.Capacity.Reacquire(ctx, in); err != nil {
		return apiv1.ContextPointer{}, err
	}
	return apiv1.ContextPointer{Name: "child-result", Artifact: &apiv1.ArtifactPointer{
		Path: "child-results/" + strings.TrimPrefix(stored.ReceiptDigest, "sha256:") + ".json", Digest: stored.ReceiptDigest, Size: int64(len(stored.Receipt)),
	}}, nil
}

func sameTerminalInput(a, b childworkflow.TerminalResultInput) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}
