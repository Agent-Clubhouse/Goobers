package startintent

import (
	"context"
	"sync"

	"github.com/goobers/goobers/internal/localscheduler"
)

type leasedStarter struct {
	next    localscheduler.Starter
	release func()
	once    sync.Once
}

func (s *leasedStarter) close() {
	s.once.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
}
func (s *leasedStarter) RegisterDispatch() func() {
	if next, ok := s.next.(localscheduler.DispatchRegistration); ok {
		return next.RegisterDispatch()
	}
	return func() {}
}
func (s *leasedStarter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	defer s.close()
	return s.next.Start(ctx, req)
}
