package readservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ChildAdmissionSource reads a child's optional human-admission hold. It
// returns sql.ErrNoRows when the child was admitted without review.
type ChildAdmissionSource func(context.Context, triggerqueue.ChildIdentity) (triggerqueue.ChildAdmissionReview, error)

// ChildAdmissionHistory is the reviewable admission hold of a queued child and
// its pinned advisory evidence. Decisions are made through the operator CLI.
type ChildAdmissionHistory struct {
	State       triggerqueue.ChildAdmissionState `json:"state"`
	Reason      string                           `json:"reason"`
	RequestedAt time.Time                        `json:"requestedAt"`
	DecidedAt   *time.Time                       `json:"decidedAt,omitempty"`
	Actor       string                           `json:"actor,omitempty"`
	Novelty     *journal.ChildNovelty            `json:"novelty,omitempty"`
}

func (s *Local) childAdmission(ctx context.Context, child triggerqueue.ChildRecord) (*ChildAdmissionHistory, error) {
	if s.sources.ChildAdmission == nil {
		return nil, nil
	}
	review, err := s.sources.ChildAdmission(ctx, child.Identity)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrChildHistoryUnavailable) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if review.ChildID != child.ChildID {
		return nil, errors.New("invalid child admission source")
	}
	out := &ChildAdmissionHistory{State: review.State, Reason: review.Reason, RequestedAt: review.RequestedAt, DecidedAt: childHistoryTime(review.DecidedAt), Actor: review.Actor}
	var evidence journal.ChildNovelty
	if json.Unmarshal(review.Evidence, &evidence) == nil && evidence.Valid() {
		out.Novelty = &evidence
	}
	return out, nil
}
