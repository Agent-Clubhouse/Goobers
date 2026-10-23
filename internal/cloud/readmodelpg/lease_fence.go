package readmodelpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
)

// LeaseFence adapts the production lease manager and live handle to Fence.
type LeaseFence struct {
	manager *cloudlease.Manager
	handle  *cloudlease.Handle
}

// NewLeaseFence binds read-model writes to a real live lease handle.
func NewLeaseFence(manager *cloudlease.Manager, handle *cloudlease.Handle) (*LeaseFence, error) {
	if manager == nil || handle == nil {
		return nil, errors.New("readmodelpg: lease manager and handle are required")
	}
	return &LeaseFence{manager: manager, handle: handle}, nil
}

// WithFence runs one read-model mutation under the lease library's bounded
// transaction and supplies the handle's live epoch to row-level CAS checks.
func (f *LeaseFence) WithFence(
	ctx context.Context,
	write func(context.Context, pgx.Tx, int64) error,
) error {
	if write == nil {
		return errors.New("readmodelpg: fenced write is required")
	}
	return f.manager.WithFence(ctx, f.handle, func(ctx context.Context, tx pgx.Tx) error {
		return write(ctx, tx, f.handle.Epoch)
	})
}

var _ Fence = (*LeaseFence)(nil)
