package childengine

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type handoffQueue interface {
	childworkflow.ChildQueue
	CurrentChild(context.Context, triggerqueue.ChildParent, string) (triggerqueue.ChildRecord, error)
}

// ObserveChildHandoff resolves the pinned active parent and exact bound grant.
// Finish revokes before reading acceptance: no later start can race the read.
func (a Authorities) ObserveChildHandoff(ctx context.Context, request engine.ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
	if err := request.Validate(); err != nil {
		return nil, errors.Join(engine.ErrChildCustodyRefused, err)
	}
	authority, err := a.Parents.PrepareOrigin(ctx, request.RunID, request.Origin)
	if err != nil {
		return nil, classify(err)
	}
	queue, ok := a.Queue.(handoffQueue)
	if !ok {
		return nil, engine.ErrChildCustodyUnavailable
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	handoff, err := observeHandoff(ctx, queue, authority, request, now)
	return handoff, classify(err)
}

func observeHandoff(ctx context.Context, queue handoffQueue, authority childworkflow.Authority, request engine.ChildHandoffObservation, now time.Time) (*journal.ChildHandoffRequest, error) {
	o := authority.Origin
	parent := triggerqueue.ChildParent{Gaggle: o.Gaggle, ParentRunID: o.RunID}
	bound, err := queue.ChildAuthority(ctx, parent, o.StageOccurrence)
	if err != nil {
		return nil, err
	}
	if bound.GrantID != request.GrantID || bound.AttemptID != request.Origin.AttemptID || bound.ConfigDigest != o.ConfigDigest || bound.PolicyDigest != o.PolicyDigest || o.RunID != request.RunID || o.StageOccurrence != request.Origin.StageOccurrence {
		return nil, childworkflow.ErrAuthorityChanged
	}
	if request.Finish {
		if !bound.Revoked {
			if err := queue.RevokeChildAuthority(ctx, bound); err != nil {
				return nil, err
			}
		}
	} else if err := queue.CheckChildAuthority(ctx, bound, now); err != nil {
		return nil, err
	}
	child, err := queue.CurrentChild(ctx, parent, o.StageOccurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || child.RunID == "" {
		return nil, err
	}
	if child.CancellationRequested || !child.TombstonedAt.IsZero() {
		return nil, childworkflow.ErrAuthorityUnavailable
	}
	// A completed child was already consumed by the engine continuation. The
	// next stage invocation must not yield it again.
	if child.State.Terminal() {
		return nil, nil
	}
	receipt, err := queue.VerifiedChildStart(ctx, child.Identity, authority.Actor)
	if err != nil {
		return nil, err
	}
	envelope, err := childworkflow.DecodeStartEnvelope(receipt.Payload)
	if err != nil {
		return nil, err
	}
	source, err := queue.ChildProposal(ctx, child.Identity)
	if err != nil {
		return nil, err
	}
	if err := childworkflow.ValidateRetainedCustody(authority, envelope, child, source); err != nil {
		return nil, err
	}
	if _, err := childworkflow.ValidateRetainedStart(authority, envelope, source.Source); err != nil {
		return nil, err
	}
	handoff := journal.ChildHandoffRequest{Gaggle: o.Gaggle, ParentRunID: o.RunID, Action: "wait", ChildRunID: child.RunID, AcceptanceID: child.AcceptanceID, InvocationKey: child.Identity.InvocationKey, SourceDigest: child.ProposalDigest, Origin: request.Origin}
	handoff.RequestID = engine.ChildHandoffDigest(handoff)
	if err := handoff.Validate(&request.Origin); err != nil {
		return nil, err
	}
	return &handoff, nil
}
