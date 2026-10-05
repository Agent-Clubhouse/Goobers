package executor

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// EventPublisher is a host-bound publication callback. No model-selectable
// endpoint, run, branch, actor, root or gaggle is accepted by this executor.
type EventPublisher func(context.Context, apiv1.InvocationEnvelope) (triggerqueue.EventReceipt, error)

// PublishEventExecutor adapts durable publication to the deterministic registry.
type PublishEventExecutor struct{ Publish EventPublisher }

// Run advances on receipt acceptance, including a successful no-match receipt.
func (e *PublishEventExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	if e == nil || e.Publish == nil {
		return apiv1.ResultEnvelope{}, errors.New("publish-event is available only in the local daemon runner")
	}
	receipt, err := e.Publish(ctx, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{Status: "success", Outputs: map[string]any{"receiptId": receipt.ID, "eventId": receipt.EventID, "state": string(receipt.State)}}, nil
}
