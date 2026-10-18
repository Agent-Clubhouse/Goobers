package main

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Recover is a single trusted read; it never issues a grant or waits for a new
// acceptance. Terminal results still need delivery to the original parent.
func (h *daemonChildHandoff) Recover(ctx context.Context, env apiv1.InvocationEnvelope) (runner.ChildHandoffRequest, error) {
	if env.ChildWorkflowOrigin == nil || env.Gaggle != h.layout.Gaggle() {
		return runner.ChildHandoffRequest{}, errors.New("parent recovery origin differs from gaggle")
	}
	s, err := h.service()
	if err != nil {
		return runner.ChildHandoffRequest{}, err
	}
	if err = s.childQueue.CheckChildParentOpen(ctx, triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}); err != nil {
		return runner.ChildHandoffRequest{}, err
	}
	child, err := s.childQueue.CurrentChild(ctx, triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}, env.ChildWorkflowOrigin.StageOccurrence)
	if err != nil || child.RunID == "" {
		return runner.ChildHandoffRequest{}, err
	}
	if child.CancellationRequested {
		return runner.ChildHandoffRequest{}, triggerqueue.ErrParentCancelled
	}
	if !child.TombstonedAt.IsZero() || !child.AcknowledgedAt.IsZero() {
		return runner.ChildHandoffRequest{}, nil
	}
	request, err := h.observe(ctx, s, env)
	if err != nil {
		return request, err
	}
	if request.RequestID != "" {
		return request, nil
	}
	// A completed child without a disposition must first return its verified
	// result. Its slot stays owned until the normal explicit disposition flow.
	request = runner.ChildHandoffRequest{Gaggle: env.Gaggle, ParentRunID: env.RunID, Action: "wait", ChildRunID: child.RunID, AcceptanceID: child.AcceptanceID, InvocationKey: child.Identity.InvocationKey, SourceDigest: child.ProposalDigest, Origin: *env.ChildWorkflowOrigin}
	request.RequestID = childHandoffRequestDigest(request)
	return request, nil
}

func (p *parentStagePod) recoverAcceptedWait(ctx context.Context, writer *journal.Run, env apiv1.InvocationEnvelope) error {
	host := daemonChildHandoff{layout: p.service.layout.ForGaggle(env.Gaggle)}
	request, err := host.Recover(ctx, env)
	if errors.Is(err, triggerqueue.ErrParentCancelled) || errors.Is(err, triggerqueue.ErrParentSettled) {
		return nil
	} // stopped custody still closes during cancellation
	if err != nil || request.RequestID == "" {
		return err
	}
	return runner.RecoverContainedParentWait(writer, request)
}
