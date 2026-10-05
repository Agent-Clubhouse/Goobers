package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (s *interactiveStageRestart) SupportsChildStageRestart() bool {
	return s != nil && s.setup != nil && s.service != nil && s.setup.ChildRestarts != nil && s.setup.ChildRestarts.restart != nil
}

func (s *interactiveStageRestart) restartChildStage(admission, execution context.Context, p httpapi.Principal, plan runner.StageRestartPlan) (intervention.StageRestartAcceptance, error) {
	if !s.SupportsChildStageRestart() {
		return intervention.StageRestartAcceptance{}, restartRefusal("child_restart_unavailable", "Generated child restart admission is unavailable.")
	}
	ctx, cancel := context.WithTimeout(admission, 30*time.Second)
	defer cancel()
	var accepted intervention.StageRestartAcceptance
	err := s.setup.InteractiveAccess.WithRestartAdmission(ctx, p, plan.Source.Gaggle, func(ctx context.Context, load interactiveaccess.RestartSourceLoader) error {
		l := s.setup.ChildRestarts
		release, owned := l.runners.acquireChildCustody(plan.Source.RunID)
		if !owned {
			return restartRefusal("restart_source_active", "The child source still has an execution or custody owner.")
		}
		defer release()
		epoch, duplicate, err := s.acceptChildRestart(ctx, p, &plan, load)
		if err != nil {
			return err
		}
		accepted = intervention.StageRestartAcceptance{RunID: epoch.RunID, Duplicate: duplicate, Queued: true}
		child, err := l.queue.ChildForExecutionRun(ctx, epoch.RunID)
		if err != nil {
			return nil
		} // custody is already durable; the sweep retries it
		if child.ActiveRunID() != epoch.RunID || child.State.Terminal() || child.CancellationRequested {
			accepted.Queued = false
			return nil
		}
		result, err := s.launchChildRestart(ctx, execution, plan, load)
		if err == nil {
			accepted = result
			accepted.Duplicate = duplicate || result.Duplicate
		}
		return nil
	})
	return accepted, err
}

func (s *interactiveStageRestart) acceptChildRestart(ctx context.Context, p httpapi.Principal, plan *runner.StageRestartPlan, load interactiveaccess.RestartSourceLoader) (triggerqueue.ChildExecution, bool, error) {
	l := s.setup.ChildRestarts
	ref, err := retainedChildExecutionRef(ctx, l.queue, plan.Source, false)
	if err != nil {
		return triggerqueue.ChildExecution{}, false, err
	}
	prior, err := l.queue.ChildExecution(ctx, ref.Child.Identity, plan.Continuation.RunID)
	if err == nil {
		return replayChildRestart(p, plan, prior)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return triggerqueue.ChildExecution{}, false, err
	}
	if err = stampRestartAuthority(s.layout, p, plan); err != nil {
		return triggerqueue.ChildExecution{}, false, err
	}
	if err = s.preflightNewChildRestart(ctx, ref, load); err != nil {
		return triggerqueue.ChildExecution{}, false, err
	}
	ref, result, err := l.sealRestartSource(ctx, *plan)
	if err != nil {
		return triggerqueue.ChildExecution{}, false, err
	}
	raw, err := runner.MarshalStageRestartPlan(*plan)
	if err != nil {
		return triggerqueue.ChildExecution{}, false, err
	}
	return l.queue.BeginChildRestart(ctx, triggerqueue.ChildRestartRequest{Identity: ref.Child.Identity, RunID: plan.Continuation.RunID, SourceRunID: plan.Source.RunID, SourceTerminalSeq: plan.Continuation.ExpectedTerminalSeq, SourceResultRef: result, Actor: plan.Continuation.Operator, Stage: plan.Continuation.Target, Plan: raw, PlanDigest: journal.Digest(raw)}, l.dispatch.now())
}

// Authenticated claim order or membership may change between retries. Current
// admission checks the new principal; exact request comparison retains the first
// accepted authority snapshot, which is independently checked again at execution.
func replayChildRestart(p httpapi.Principal, plan *runner.StageRestartPlan, prior triggerqueue.ChildExecution) (triggerqueue.ChildExecution, bool, error) {
	saved, err := runner.ParseStageRestartPlan(prior.Plan)
	if err != nil {
		return prior, false, err
	}
	raw := saved.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName]
	authority, err := interactiveaccess.ParseRestartAuthority(raw)
	if err != nil || authority.Issuer != p.Issuer || authority.Subject != p.Subject {
		return prior, false, interactiveaccess.ErrDenied
	}
	plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName] = raw
	plan.Continuation.InputIntegrity[interactiveaccess.RestartAuthorityInputName] = saved.Continuation.InputIntegrity[interactiveaccess.RestartAuthorityInputName]
	plan.Continuation.InputSource[interactiveaccess.RestartAuthorityInputName] = saved.Continuation.InputSource[interactiveaccess.RestartAuthorityInputName]
	candidate, err := runner.MarshalStageRestartPlan(*plan)
	if err != nil || !bytes.Equal(candidate, prior.Plan) {
		return prior, false, triggerqueue.ErrConflict
	}
	*plan = saved
	return prior, true, nil
}

func (s *interactiveStageRestart) launchChildRestart(ctx, execution context.Context, plan runner.StageRestartPlan, load interactiveaccess.RestartSourceLoader) (intervention.StageRestartAcceptance, error) {
	// Common replay comparison needs the same derived lineage/fork inputs as
	// first publication. Prepare them before it checks an existing journal.
	if _, err := s.preflightChildRestart(ctx, &plan, load); err != nil {
		return intervention.StageRestartAcceptance{}, err
	}
	return s.service.LaunchStageRestart(ctx, execution, plan, func(ctx context.Context, _ *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error) {
		return nil, ctx.Err()
	})
}

func (s *interactiveStageRestart) reconcileChildRestart(ctx context.Context, ref childExecutionRef) error {
	if ref.Execution == nil || !s.SupportsChildStageRestart() {
		return errors.New("child restart admission unavailable")
	}
	plan, err := runner.ParseStageRestartPlan(ref.Execution.Plan)
	if err != nil {
		return err
	}
	authority, err := interactiveaccess.ParseRestartAuthority(plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName])
	if err != nil {
		return err
	}
	// The existing bounded trigger sweep owns the retry. It never resamples
	// guidance, selects a new run ID, or revives the original automation identity.
	return s.setup.InteractiveAccess.WithRestartAdmission(ctx, authority.Principal(), ref.Envelope.Gaggle, func(ctx context.Context, load interactiveaccess.RestartSourceLoader) error {
		l := s.setup.ChildRestarts
		release, owned := l.runners.acquireChildCustody(plan.Source.RunID)
		if !owned {
			return &childStartDeferred{Reason: "restart source custody is active"}
		}
		defer release()
		_, err := s.launchChildRestart(ctx, l.dispatch.lifecycleContext(context.WithoutCancel(ctx)), plan, load)
		return err
	})
}
