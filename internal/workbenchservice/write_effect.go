package workbenchservice

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

func (s *WriterService) execute(ctx context.Context, bound ReadBinding, load interactiveaccess.SourceCredentialLoader, record triggerqueue.WorkbenchCommand) (triggerqueue.WorkbenchCommand, error) {
	receipt := workbench.BacklogPatchReceipt{OperationDigest: record.Input.OperationDigest, Outcome: "not-applied", RevisionSemantics: workbenchprovider.BacklogCapabilities(bound.Source).RevisionSemantics}
	credential, err := load(ctx, interactiveaccess.Target{Kind: "backlog"})
	if err == nil {
		receipt = s.invokePatch(ctx, bound, credential, record, receipt)
	}
	// Persist the actual outcome even when the HTTP caller canceled. The enclosing
	// policy callback remains held until this bounded cleanup joins. Failure leaves
	// attempting custody, preventing replay after any potentially committed effect.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	completed, err := s.Queue.CompleteWorkbenchCommand(cleanup, record.Input.Scope, record.ID, record.RequestDigest, receipt, s.now())
	if err != nil {
		return record, err
	}
	return completed, nil
}
func (s *WriterService) invokePatch(ctx context.Context, bound ReadBinding, credential interactiveaccess.Credential, record triggerqueue.WorkbenchCommand, fallback workbench.BacklogPatchReceipt) workbench.BacklogPatchReceipt {
	client, err := s.ReadService.Backlog(ctx, bound, credential)
	if err != nil {
		return fallback
	}
	native, ok := client.(workbenchprovider.NativeWriter)
	if !ok {
		return fallback
	}
	writer, err := workbenchprovider.NewBacklogWriter(bound.Scope, bound.Source, native)
	if err != nil {
		return fallback
	}
	receipt, _ := writer.Patch(ctx, record.Input.Request)
	// Never turn malformed post-call evidence into not-applied. Preserve a bounded
	// unknown result when an adapter cannot return a valid receipt identity.
	if receipt.OperationDigest != record.Input.OperationDigest {
		fallback.Outcome = "unknown"
		return fallback
	}
	return receipt
}
