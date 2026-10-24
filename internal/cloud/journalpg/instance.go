package journalpg

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

const instanceStream = journal.StreamScheduler

// OpenInstance acquires the durable scheduler stream without resetting its
// counter. Later health streams can use the same (instance,stream,seq) schema.
func (b *Backend) OpenInstance(ctx context.Context) (journal.InstanceHandle, journal.OwnerToken, journal.RecoverReport, error) {
	return b.openInstanceStream(ctx, instanceStream)
}

func (b *Backend) openInstanceStream(ctx context.Context, stream string) (journal.InstanceHandle, journal.OwnerToken, journal.RecoverReport, error) {
	h, err := b.liveOwner()
	if err != nil {
		return "", "", journal.RecoverReport{}, err
	}
	owner := &instanceOwner{
		token: journal.OwnerToken(rand.Text()), epoch: h.Epoch, stream: stream,
	}
	var seq int64
	err = b.config.Lease.WithFence(ctx, h, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO journal.instance_seq(instance_id,stream,owner_epoch,owner_token)
  SELECT instance_id,$5,epoch,$6 FROM control.lease WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4
  ON CONFLICT(instance_id,stream) DO UPDATE SET owner_epoch=EXCLUDED.owner_epoch,owner_token=EXCLUDED.owner_token
  WHERE (journal.instance_seq.owner_token IS NULL OR journal.instance_seq.owner_epoch<$4) AND journal.instance_seq.owner_epoch<=$4
  AND EXISTS(SELECT 1 FROM control.lease WHERE instance_id=$1 AND name=$2 AND holder=$3 AND epoch=$4)
  RETURNING last_seq`, h.InstanceID, h.Name, h.Holder, h.Epoch, stream, owner.token).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return journal.ErrRunOwned
		}
		return err
	})
	if err != nil {
		return "", "", journal.RecoverReport{}, err
	}
	handle := journal.InstanceHandle(rand.Text())
	b.mu.Lock()
	b.instances[handle] = owner
	b.mu.Unlock()
	return handle, owner.token, journal.RecoverReport{LastSeq: uint64(seq)}, nil
}

// BindInstance is a local-file compatibility operation. Cloud callers must use
// OpenInstance so external appenders cannot bypass lease-fenced durability.
func (b *Backend) BindInstance(journal.InstanceAppender) (journal.InstanceHandle, journal.OwnerToken, error) {
	return "", "", errors.New("journalpg: external instance appenders are unsupported; use OpenInstance")
}
func (b *Backend) instanceOwner(handle journal.InstanceHandle, token journal.OwnerToken) (*instanceOwner, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := b.instances[handle]
	if owner == nil || token == "" || owner.token != token {
		return nil, errors.New("journalpg: stale instance owner token")
	}
	return owner, nil
}
func (b *Backend) withInstance(ctx context.Context, owner *instanceOwner, write func(context.Context, pgx.Tx, int64) error) error {
	h, err := b.liveOwner()
	if err != nil {
		return err
	}
	if h.Epoch != owner.epoch {
		return lease.ErrStale
	}
	// The only moment a write can lose a partition-routing race is the ATTACH
	// of the current month over a live DEFAULT partition (migrate.PartitionMaintainer);
	// the whole transaction is safe to run again.
	for range 3 {
		if err = b.withInstanceOnce(ctx, h, owner, write); !isPartitionRace(err) {
			return err
		}
	}
	return err
}

func isPartitionRace(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		strings.Contains(pgErr.Message, "violates partition constraint")
}

func (b *Backend) withInstanceOnce(ctx context.Context, h *lease.Handle, owner *instanceOwner, write func(context.Context, pgx.Tx, int64) error) error {
	return b.config.Lease.WithFence(ctx, h, func(ctx context.Context, tx pgx.Tx) error {
		var seq int64
		err := tx.QueryRow(ctx, `SELECT last_seq FROM journal.instance_seq WHERE instance_id=$1 AND stream=$2 AND owner_token=$3 AND owner_epoch=$4 FOR UPDATE`, b.config.InstanceID, owner.stream, owner.token, h.Epoch).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return lease.ErrStale
		}
		if err != nil {
			return err
		}
		return write(ctx, tx, seq)
	})
}

// InstanceAppend commits one event and its gap-free stream counter together.
func (b *Backend) InstanceAppend(ctx context.Context, handle journal.InstanceHandle, token journal.OwnerToken, event journal.Event) error {
	owner, err := b.instanceOwner(handle, token)
	if err != nil {
		return err
	}
	clean, err := prepareEvent(&runOwner{options: normalizedOptions(
		journal.RunWriterOptions{Clock: b.config.InstanceClock})}, event)
	if err != nil {
		return err
	}
	return b.withInstance(ctx, owner, func(ctx context.Context, tx pgx.Tx, seq int64) error {
		if key := emitKey(clean); key != nil {
			// Cross-month idempotency: the unpartitioned key table does not
			// depend on which month the append-time clock (or a successor
			// replica's skewed clock) falls in. seq is the next sequence we are
			// about to allocate under the instance_seq row lock, so a duplicate
			// leaves the stream gap-free.
			tag, err := tx.Exec(ctx, `INSERT INTO journal.instance_emit_key(instance_id,stream,emit_key,seq)
				VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, b.config.InstanceID, owner.stream, key, seq+1)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `UPDATE journal.instance_seq SET last_seq=last_seq+1 WHERE instance_id=$1 AND stream=$2 AND owner_token=$3 AND owner_epoch=$4 RETURNING last_seq`, b.config.InstanceID, owner.stream, owner.token, owner.epoch).Scan(&seq); err != nil {
			return err
		}
		clean.Seq = uint64(seq)
		if clean.Progress != nil {
			clean.Progress.Sequence = clean.Seq
		}
		raw, err := journal.MarshalEvent(clean)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO journal.instance_event(instance_id,stream,seq,at,type,emit_key,epoch,body,run_id)
  SELECT instance_id,stream,$5,$6,$7,$8,owner_epoch,$9,$10 FROM journal.instance_seq WHERE instance_id=$1 AND stream=$2 AND owner_token=$3 AND owner_epoch=$4`, b.config.InstanceID, owner.stream, owner.token, owner.epoch, seq, clean.Time, string(clean.Type), emitKey(clean), raw, clean.RunID)
		if err = requireWrite(tag.RowsAffected(), err); err != nil {
			return err
		}
		return nil
	})
}

// RetireInstance durably releases this stream incarnation for reopening.
func (b *Backend) RetireInstance(ctx context.Context, handle journal.InstanceHandle, token journal.OwnerToken) error {
	owner, err := b.instanceOwner(handle, token)
	if err != nil {
		return err
	}
	err = b.withInstance(ctx, owner, func(ctx context.Context, tx pgx.Tx, _ int64) error {
		tag, err := tx.Exec(ctx, `UPDATE journal.instance_seq SET owner_token=NULL WHERE instance_id=$1 AND stream=$2 AND owner_token=$3 AND owner_epoch=$4`, b.config.InstanceID, owner.stream, owner.token, owner.epoch)
		return requireWrite(tag.RowsAffected(), err)
	})
	if err == nil {
		b.mu.Lock()
		delete(b.instances, handle)
		b.mu.Unlock()
	}
	return err
}

// InstanceRead returns committed scheduler events after a durable stream cursor.
func (b *Backend) InstanceRead(ctx context.Context, after uint64, limit int) ([]journal.Event, error) {
	if after > 1<<63-1 {
		return nil, nil
	}
	bound := int64(limit)
	if bound <= 0 {
		bound = 1<<63 - 1
	}
	rows, err := b.config.Pool.Query(ctx, `SELECT body FROM journal.instance_event WHERE instance_id=$1 AND stream=$2 AND seq>$3 ORDER BY seq LIMIT $4`, b.config.InstanceID, instanceStream, int64(after), bound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []journal.Event
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var event journal.Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		if !event.KnownSchema() {
			return nil, journal.ErrJournalMigrationRequired
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// InstanceLastSeq returns the stream's committed sequence watermark (0 when the
// stream has never been written).
func (b *Backend) InstanceLastSeq(ctx context.Context) (uint64, error) {
	var seq int64
	err := b.config.Pool.QueryRow(ctx, `SELECT last_seq FROM journal.instance_seq WHERE instance_id=$1 AND stream=$2`, b.config.InstanceID, instanceStream).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return uint64(seq), err
}
