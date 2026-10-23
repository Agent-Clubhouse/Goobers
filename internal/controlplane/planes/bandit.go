package planes

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/bandit"
)

// BanditService owns the observation backend. Cloud composition binds it to the
// active lease. The local runner continues to sample its run journals directly.
// Workers only receive the HTTP client, never database credentials.
type BanditService struct{ store bandit.ObservationStore }

// NewBanditService requires the host's explicitly selected backend.
func NewBanditService(store bandit.ObservationStore) (*BanditService, error) {
	if store == nil {
		return nil, errors.New("bandit observation store is required")
	}
	return &BanditService{store: store}, nil
}

// Observe performs one validated read or idempotent outcome publication.
func (s *BanditService) Observe(ctx context.Context, request bandit.ObservationRequest) (bandit.ObservationResponse, error) {
	if err := request.Validate(); err != nil {
		return bandit.ObservationResponse{}, err
	}
	if request.Operation == "record" {
		return bandit.ObservationResponse{}, s.store.Record(ctx, *request.Record)
	}
	observations, err := s.store.Observations(ctx, request.Stage)
	return bandit.ObservationResponse{Observations: observations}, err
}

var _ bandit.ObservationPlane = (*BanditService)(nil)
