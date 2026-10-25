package engine

import (
	"context"
	"encoding/json"
	"errors"

	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/dispatcher"
)

// childAuthorityRelease bounds the worker's release of a stage attempt's
// child-workflow grant after its activity ended; an unreleased grant goes
// inert when the attempt ends, so release is best effort.
const childAuthorityRelease = 30 * time.Second

// childStageAuthority retains the exact grant lent to this dispatch.
// stageChildAuthority acquires, before a fresh pod is created, the
// child-workflow grant of an engine parent's child-capable agentic stage
// attempt and stamps it onto the attempt (docs/design/
// engine-parent-child-authority.md §2). It returns the release to run when the
// activity ends, or nil when the attempt needs no grant: a deterministic
// stage or a stage without a committed child origin. Adoption recovers the
// same grant so a replacement worker also observes accepted requests.
type childStageAuthority struct {
	release     func(error)
	observation ChildHandoffObservation
}

func (a *Activities) stageChildAuthority(ctx context.Context, input DispatchStageInput, attempt *dispatcher.Attempt) (*childStageAuthority, error) {
	origin := input.Envelope.ChildWorkflowOrigin
	if input.Run != nil || origin == nil {
		return nil, nil
	}
	if !input.ChildWorkflowsEnabled {
		return nil, temporal.NewNonRetryableApplicationError("engine: child handoff is disabled by the frozen run pin", childCustodyRefusedType, nil)
	}
	if a.ChildAuthority == nil {
		return nil, temporal.NewNonRetryableApplicationError("engine: a child-capable stage needs the control child authority, which this worker does not have", childCustodyRefusedType, ErrNotConfigured)
	}
	runID := input.Envelope.RunID
	grant, err := a.ChildAuthority.AcquireChildAuthority(ctx, ChildAuthorityRequest{RunID: runID, Origin: *origin})
	if errors.Is(err, ErrChildCustodyUnavailable) {
		// The control plane, not the stage, failed: retry as infrastructure
		// so the attempt never charges the run's stage budget for it.
		return nil, temporal.NewApplicationErrorWithCause(err.Error(), FailureTypeInfrastructure, err)
	}
	if err != nil {
		return nil, classifyChildCustody(ctx, err)
	}
	access, err := grant.Access(runID)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError("engine: control returned an invalid child-workflow grant", childCustodyRefusedType, err)
	}
	encoded, err := json.Marshal(access)
	if err != nil {
		return nil, err
	}
	attempt.ChildWorkflowAccess = string(encoded)
	release := ChildAuthorityRelease{RunID: runID, Origin: *origin, GrantID: grant.GrantID}
	return &childStageAuthority{observation: ChildHandoffObservation{ChildAuthorityRelease: release}, release: func(outcome error) {
		if !releasesChildAuthority(outcome) {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), childAuthorityRelease)
		defer cancel()
		for delay := time.Second; ; delay *= 2 {
			err := a.ChildAuthority.ReleaseChildAuthority(cleanup, release)
			if !errors.Is(err, ErrChildCustodyUnavailable) {
				return
			}
			select {
			case <-cleanup.Done():
				return
			case <-time.After(delay):
			}
		}
	}}, nil
}

// releasesChildAuthority reports whether the activity's outcome ends the
// attempt's use of its grant. A Temporal retry of the activity is the same
// attempt and re-acquires the same live grant, so the grant is kept on a
// retryable failure; it is also kept when the pod outlives the activity (a
// worker handoff), and on a timeout Temporal may retry. A kept grant that is
// never used again goes inert when the attempt ends or the grant expires.
func releasesChildAuthority(outcome error) bool {
	var application *temporal.ApplicationError
	switch {
	case outcome == nil, errors.Is(outcome, context.Canceled):
		return true
	case errors.Is(outcome, dispatcher.ErrWorkerHandoff):
		return false
	case errors.As(outcome, &application):
		return application.NonRetryable()
	default:
		return false
	}
}
