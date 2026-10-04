package enginestartintent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrUncertain means execution may have happened; absence never grants resend.
var ErrUncertain = errors.New("direct engine: attempted start remains uncertain; exact Temporal execution is not observable")

// Backend owns one exact transport/codec binding. Observe proves full input and
// task-queue identity, not merely the existence of a matching workflow ID.
type Backend interface {
	Start(context.Context, engine.RunInput, string) error
	Observe(context.Context, engine.RunInput, string) (bool, error)
	Close()
}

// Service captures only from trusted host compilation. Open resolves the current
// credentials for the accepted binding before the durable attempted marker.
type Service struct {
	Queue   *triggerqueue.Store
	Capture func(context.Context, Request) (engine.RunInput, func(), error)
	Open    func(context.Context, Request) (Backend, error)
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Accept keeps exact replay independent of mutable authored configuration.
func (s *Service) Accept(ctx context.Context, request Request) (triggerqueue.Record, bool, error) {
	if err := request.Validate(); err != nil {
		return triggerqueue.Record{}, false, err
	}
	prior, err := s.Queue.ByKey(ctx, request.Key())
	if err == nil {
		e, parseErr := Parse(prior.Payload)
		if parseErr != nil || prior.Actor != Actor || e.Request != request {
			return triggerqueue.Record{}, false, triggerqueue.ErrConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return triggerqueue.Record{}, false, err
	}
	if s.Capture == nil {
		return triggerqueue.Record{}, false, errors.New("direct engine: host capture unavailable")
	}
	input, release, err := s.Capture(ctx, request)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	defer release()
	raw, err := json.Marshal(input)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	e := Envelope{Kind: Kind, Request: request, RunID: input.RunID, ConfigGeneration: input.ConfigGeneration, InputDigest: Digest(raw)}
	payload, err := e.Marshal()
	if err == nil {
		_, err = e.Input(raw)
	}
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	return s.Queue.AcceptDirectEngine(ctx, request.Key(), Actor, payload, raw, s.now())
}

// Dispatch makes at most one potentially effectful call for a receipt. A process
// death after BeginDispatch retains uncertain custody even if history is absent.
func (s *Service) Dispatch(ctx context.Context, record triggerqueue.Record) error {
	if record.State == triggerqueue.Dispatched {
		return nil
	}
	e, input, err := s.load(ctx, record)
	if err != nil {
		return err
	}
	if record.State != triggerqueue.Accepted && record.State != triggerqueue.Dispatching {
		return triggerqueue.ErrTransition
	}
	backend, err := s.Open(ctx, e.Request)
	if err != nil {
		return err
	}
	defer backend.Close()
	if record.State == triggerqueue.Accepted {
		if err = s.Queue.BeginDispatch(ctx, record.ID); err != nil {
			return err
		}
		if err = backend.Start(ctx, input, e.InputDigest); err != nil {
			return errors.Join(ErrUncertain, err)
		}
	}
	observed, err := backend.Observe(ctx, input, e.InputDigest)
	if err != nil {
		return err
	}
	if !observed {
		return ErrUncertain
	}
	return s.Queue.Finish(ctx, record.ID, triggerqueue.Dispatched, e.RunID, "", s.now())
}

func (s *Service) load(ctx context.Context, record triggerqueue.Record) (Envelope, engine.RunInput, error) {
	e, err := Parse(record.Payload)
	if err != nil || record.Actor != Actor || record.Key != e.Request.Key() {
		return e, engine.RunInput{}, errors.Join(err, errors.New("direct engine: receipt authority differs"))
	}
	raw, err := s.Queue.DirectEngineInput(ctx, record.ID)
	if err != nil {
		return e, engine.RunInput{}, err
	}
	in, err := e.Input(raw)
	return e, in, err
}

// RetainedGenerations protects accepted and uncertain sources. Confirmed direct
// histories retain their existing external-generation ownership marker.
func RetainedGenerations(ctx context.Context, queue *triggerqueue.Store) (map[string]bool, error) {
	pins := map[string]bool{}
	after := ""
	for {
		page, err := queue.RetainedPage(ctx, after, 100)
		if err != nil {
			return nil, err
		}
		for _, record := range page {
			after = record.ID
			var header struct {
				Kind string `json:"kind"`
			}
			if err = json.Unmarshal(record.Payload, &header); err != nil {
				return nil, err
			}
			if header.Kind != Kind {
				continue
			}
			e, err := Parse(record.Payload)
			if err != nil {
				return nil, err
			}
			pins[e.ConfigGeneration] = true
		}
		if len(page) < 100 {
			return pins, nil
		}
	}
}
