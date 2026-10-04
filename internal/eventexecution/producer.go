package eventexecution

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/journal"
)

// ConsumerProducer derives causal authority from a host-opened retained journal,
// matching its immutable inputs to the queue. Binding, actor and stage must come
// from the authenticated publication service, never event body fields. That
// service remains responsible for active-attempt and allowed-event-type checks.
// Child and human-continuation adapters require separately verified ancestry.
func (s *Service) ConsumerProducer(ctx context.Context, reader *journal.Reader, binding, actor, stage string) (eventing.Producer, error) {
	if reader == nil || s.Queue == nil {
		return eventing.Producer{}, errors.New("eventexecution: publication ancestry unavailable")
	}
	id, err := reader.Identity()
	if err != nil {
		return eventing.Producer{}, err
	}
	if id.Event == nil || id.Child != nil || id.ContinuedFromRunID != "" {
		return eventing.Producer{}, errors.New("eventexecution: publication ancestry requires an original event consumer")
	}
	record, start, err := s.Queue.VerifiedEventStart(ctx, id.Gaggle, id.Event.GroupID)
	if err != nil {
		return eventing.Producer{}, err
	}
	if err = VerifyIdentity(reader, id, record, start); err != nil {
		return eventing.Producer{}, err
	}
	if err = s.VerifyMembership(ctx, reader, id, start); err != nil {
		return eventing.Producer{}, err
	}
	return s.Queue.EventConsumerProducer(ctx, eventing.Producer{Gaggle: id.Gaggle, RunID: id.RunID, Binding: binding, Actor: actor, Stage: stage}, id.Event.GroupID)
}
