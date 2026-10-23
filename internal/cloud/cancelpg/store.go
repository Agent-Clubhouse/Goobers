// Package cancelpg stores cancellation receipts in PostgreSQL. An unfinished
// receipt is uncertain, never permission to repeat a side effect, so it is
// never pruned; only the live owner may record its outcome.
package cancelpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/cloud/lease"
)

// Store is the PostgreSQL cancel-receipt store for one lease. Zero limits use
// the local store's limits.
type Store struct {
	Pool       *pgxpool.Pool
	Fence      *lease.Manager
	MaxRecords int
	Retention  time.Duration
}

func (s Store) maxRecords() int {
	if s.MaxRecords > 0 {
		return s.MaxRecords
	}
	return cancelreceipt.MaxRecords
}

func (s Store) retention() time.Duration {
	if s.Retention > 0 {
		return s.Retention
	}
	return cancelreceipt.Retention
}

func validKey(key string) bool {
	if key == "" || len(key) > 200 || strings.TrimSpace(key) != key {
		return false
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Begin binds key to the actor and exact payload. An existing key resolves
// before any capacity check: the same request returns its receipt, another
// request is ErrConflict. Only fresh=true authorizes execution.
func (s Store) Begin(ctx context.Context, handle *lease.Handle, key, actor string, payload []byte) (receipt cancelreceipt.Receipt, fresh bool, err error) {
	if !validKey(key) || len(actor) > 1024 || len(payload) == 0 || len(payload) > cancelreceipt.MaxBytes {
		return cancelreceipt.Receipt{}, false, errors.New("cancelpg: invalid request")
	}
	err = s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		var found bool
		receipt, found, err = resolve(ctx, tx, handle, key, actor, payload)
		if found || err != nil {
			return err
		}
		// New keys serialize on the instance's capacity lock, then re-resolve so
		// a concurrent Begin of the same key is an existing key, not a race.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('control.cancel_receipt:' || $1, 0))", handle.InstanceID); err != nil {
			return fmt.Errorf("lock cancel receipt capacity: %w", err)
		}
		if receipt, found, err = resolve(ctx, tx, handle, key, actor, payload); found || err != nil {
			return err
		}
		if err := s.reserve(ctx, tx, handle, key, actor, payload); err != nil {
			return err
		}
		fresh = true
		return nil
	})
	if err != nil {
		return cancelreceipt.Receipt{}, false, err
	}
	return receipt, fresh, nil
}

func resolve(ctx context.Context, tx pgx.Tx, handle *lease.Handle, key, actor string, payload []byte) (cancelreceipt.Receipt, bool, error) {
	var savedActor string
	var savedPayload, result []byte
	err := tx.QueryRow(ctx, `
		SELECT actor, payload, result FROM control.cancel_receipt
		WHERE instance_id=$1 AND key=$2`,
		handle.InstanceID, key,
	).Scan(&savedActor, &savedPayload, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return cancelreceipt.Receipt{}, false, nil
	}
	if err != nil {
		return cancelreceipt.Receipt{}, false, fmt.Errorf("read cancel receipt: %w", err)
	}
	if actor != savedActor || !bytes.Equal(payload, savedPayload) {
		return cancelreceipt.Receipt{}, true, cancelreceipt.ErrConflict
	}
	return cancelreceipt.Receipt{Result: result, Complete: result != nil}, true, nil
}

func (s Store) reserve(ctx context.Context, tx pgx.Tx, handle *lease.Handle, key, actor string, payload []byte) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM control.cancel_receipt
		WHERE instance_id=$1 AND finished_at < now() - $2::interval`,
		handle.InstanceID, s.retention(),
	); err != nil {
		return fmt.Errorf("prune cancel receipts: %w", err)
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM control.cancel_receipt WHERE instance_id=$1", handle.InstanceID).Scan(&count); err != nil {
		return fmt.Errorf("count cancel receipts: %w", err)
	}
	if count >= s.maxRecords() {
		return cancelreceipt.ErrFull
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.cancel_receipt (instance_id, key, actor, payload, owner_epoch)
		VALUES ($1, $2, $3, $4, $5)`,
		handle.InstanceID, key, actor, payload, handle.Epoch,
	); err != nil {
		return fmt.Errorf("reserve cancel receipt: %w", err)
	}
	return nil
}

// Finish records the outcome once, by the live owner only. Any later
// completion, including an identical one, is ErrTransition, as in the local
// store: a caller that lost the reply resolves it through Begin.
func (s Store) Finish(ctx context.Context, handle *lease.Handle, key string, result []byte) error {
	if len(result) == 0 || len(result) > cancelreceipt.MaxBytes {
		return cancelreceipt.ErrTransition
	}
	return s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.cancel_receipt SET result=$3, finished_at=now()
			WHERE instance_id=$1 AND key=$2 AND result IS NULL AND owner_epoch=$4`,
			handle.InstanceID, key, result, handle.Epoch,
		)
		if err != nil {
			return fmt.Errorf("finish cancel receipt: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return cancelreceipt.ErrTransition
		}
		return nil
	})
}

// Bind returns the store as cancelreceipt.Receipts for one acquired lease.
func (s Store) Bind(handle *lease.Handle) cancelreceipt.Receipts {
	return bound{store: s, handle: handle}
}

type bound struct {
	store  Store
	handle *lease.Handle
}

func (b bound) Begin(ctx context.Context, key, actor string, payload []byte) (cancelreceipt.Receipt, bool, error) {
	return b.store.Begin(ctx, b.handle, key, actor, payload)
}

func (b bound) Finish(ctx context.Context, key string, result []byte) error {
	return b.store.Finish(ctx, b.handle, key, result)
}

// Adopt moves a predecessor's unfinished receipts to the live epoch so the
// successor can resolve them. The predecessor epoch must match exactly and be
// older than the live one.
func (s Store) Adopt(ctx context.Context, handle *lease.Handle, predecessorEpoch int64) (int64, error) {
	var adopted int64
	err := s.Fence.WithFence(ctx, handle, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE control.cancel_receipt SET owner_epoch=$2
			WHERE instance_id=$1 AND owner_epoch=$3 AND $3 < $2 AND result IS NULL`,
			handle.InstanceID, handle.Epoch, predecessorEpoch,
		)
		if err != nil {
			return fmt.Errorf("adopt cancel receipts: %w", err)
		}
		adopted = tag.RowsAffected()
		return nil
	})
	return adopted, err
}
