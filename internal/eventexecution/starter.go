package eventexecution

import (
	"context"
	"errors"
	"sync"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Starter retains the immutable generation through the actual driver lifetime.
// Close is also safe on admission refusal, when Start will never be called.
type Starter struct {
	Next    localscheduler.Starter
	Inputs  eventing.ExecutionInputs
	Release func()
	once    sync.Once
}

// Close releases one construction/archive lease exactly once.
func (s *Starter) Close() {
	s.once.Do(func() {
		if s.Release != nil {
			s.Release()
		}
	})
}

// RegisterDispatch preserves the ordinary starter's shutdown registration.
func (s *Starter) RegisterDispatch() func() {
	if next, ok := s.Next.(localscheduler.DispatchRegistration); ok {
		return next.RegisterDispatch()
	}
	return func() {}
}

// Start carries only the prepared event bundle and refuses mismatched dispatch.
func (s *Starter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	defer s.Close()
	if s.Next == nil || req.Item != nil || req.EventInputs != nil {
		return localscheduler.StartResult{}, errors.New("eventexecution: invalid prepared starter")
	}
	if _, err := s.Inputs.Validate(req.RunID, req.Gaggle); err != nil {
		return localscheduler.StartResult{}, err
	}
	req.EventInputs = &s.Inputs
	return s.Next.Start(ctx, req)
}
