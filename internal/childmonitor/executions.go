package childmonitor

import (
	"context"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (s *Service) executionHistory(ctx context.Context, child triggerqueue.ChildRecord, page *apicontract.ChildWorkflowPage) error {
	if child.ExecutionEpoch == 0 {
		return nil
	}
	history, err := s.Queue.ChildExecutionHistory(ctx, child.Identity)
	if err != nil {
		return err
	}
	for _, execution := range history {
		view := apicontract.ChildWorkflowExecution{Epoch: execution.Epoch, RunID: execution.RunID, Current: execution.RunID == child.ActiveRunID(), State: string(execution.State), SourceRunID: execution.SourceRunID, Actor: s.scrub(execution.Actor), Stage: s.scrub(execution.Stage), AcceptedAt: execution.AcceptedAt, UpdatedAt: execution.UpdatedAt}
		if id, err := s.identity(execution.RunID); err == nil {
			verified, err := s.publicationChild(ctx, id)
			view.RunAvailable = err == nil && verified.ChildID == child.ChildID
		}
		page.ExecutionHistory = append(page.ExecutionHistory, view)
	}
	return nil
}
