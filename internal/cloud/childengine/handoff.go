package childengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type handoffQueue interface {
	childworkflow.ChildQueue
	CurrentChild(context.Context, triggerqueue.ChildParent, string) (triggerqueue.ChildRecord, error)
	ConfirmChildHandoff(context.Context, triggerqueue.ChildAuthority, *triggerqueue.ChildRecord, []byte, bool, bool) error
	RecoverChildHandoff(context.Context, triggerqueue.ChildAuthority) ([]byte, error)
}

// ObserveChildHandoff resolves the pinned active parent and exact bound grant.
// Final confirmation and sealing recheck mutable custody under the controller
// fence, after immutable source validation.
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
	if request.Stopped && !request.Finish {
		return nil, childworkflow.ErrAuthorityChanged
	}
	retained, err := queue.RecoverChildHandoff(ctx, bound)
	if err != nil {
		return nil, err
	}
	if len(retained) != 0 {
		return decodeStoppedHandoff(retained, authority, request.Origin)
	}
	if err := queue.CheckChildAuthority(ctx, bound, now); err != nil {
		return nil, err
	}
	child, err := queue.CurrentChild(ctx, parent, o.StageOccurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, queue.ConfirmChildHandoff(ctx, bound, nil, nil, request.Finish, request.Stopped)
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
		return nil, queue.ConfirmChildHandoff(ctx, bound, &child, nil, request.Finish, request.Stopped)
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
	data, err := json.Marshal(handoff)
	if err != nil {
		return nil, err
	}
	if err := queue.ConfirmChildHandoff(ctx, bound, &child, data, request.Finish, request.Stopped); err != nil {
		return nil, err
	}
	return &handoff, nil
}

func decodeStoppedHandoff(data []byte, authority childworkflow.Authority, origin apiv1.ChildWorkflowOrigin) (*journal.ChildHandoffRequest, error) {
	var handoff journal.ChildHandoffRequest
	if json.Unmarshal(data, &handoff) != nil || handoff.Validate(&origin) != nil || handoff.Gaggle != authority.Origin.Gaggle || handoff.ParentRunID != authority.Origin.RunID || handoff.Action != "wait" || handoff.RequestID != engine.ChildHandoffDigest(handoff) {
		return nil, childworkflow.ErrAuthorityChanged
	}
	return &handoff, nil
}

func (a Authorities) recoverStoppedHandoff(ctx context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	authority, err := a.Parents.PrepareOrigin(ctx, request.RunID, request.Origin)
	if err != nil {
		return engine.ChildAuthorityGrant{}, err
	}
	queue, ok := a.Queue.(handoffQueue)
	if !ok {
		return engine.ChildAuthorityGrant{}, engine.ErrChildCustodyUnavailable
	}
	o := authority.Origin
	bound, err := queue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: o.Gaggle, ParentRunID: o.RunID}, o.StageOccurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.ChildAuthorityGrant{}, nil
	}
	if err != nil {
		return engine.ChildAuthorityGrant{}, err
	}
	if bound.AttemptID != o.AttemptID {
		return engine.ChildAuthorityGrant{}, nil
	}
	if bound.ConfigDigest != o.ConfigDigest || bound.PolicyDigest != o.PolicyDigest {
		return engine.ChildAuthorityGrant{}, childworkflow.ErrAuthorityChanged
	}
	data, err := queue.RecoverChildHandoff(ctx, bound)
	if err != nil || len(data) == 0 {
		return engine.ChildAuthorityGrant{}, err
	}
	handoff, err := decodeStoppedHandoff(data, authority, request.Origin)
	return engine.ChildAuthorityGrant{GrantID: bound.GrantID, StoppedHandoff: handoff}, err
}
