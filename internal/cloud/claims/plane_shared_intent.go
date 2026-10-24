package claims

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

// sharedIntentStore rechecks the durable operation after the provider read,
// immediately before CAS. No transaction spans network I/O. There remains a
// recheck-to-CAS window: a delayed request can race a newer intent. Remote
// revision CAS (including release's explicit tombstone) rejects earlier reads,
// and token-checked finalization prevents stale execution authority. Provider
// client calls are bounded to 30s. Unacknowledged remote custody expires with
// the provider lease (at most 24h); pending local intent remains for recovery.
type sharedIntentStore struct {
	sharedclaim.Store
	plane    *Plane
	handle   *cloudlease.Handle
	key      localscheduler.ClaimKey
	reserved lockedRow
	release  bool
}

func (s sharedIntentStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	err := s.plane.fencedWith(ctx, s.handle, func(ctx context.Context, tx pgx.Tx, _ int64) error {
		row, err := s.plane.lockRow(ctx, tx, s.key)
		if errors.Is(err, pgx.ErrNoRows) {
			return sharedclaim.ErrConflict
		}
		if err != nil {
			return err
		}
		if row.token != s.reserved.token || row.entry.SharedOwner != s.reserved.entry.SharedOwner ||
			row.entry.ReleasePending != s.release || (!s.release && (!row.entry.SharedAdmissionPending || row.entry.SharedRevoked)) {
			return sharedclaim.ErrConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.Store.CompareAndSwap(ctx, key, revision, record)
}
