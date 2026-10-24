// Package acceptance implements shared PostgreSQL trigger acceptance custody.
package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Store implements the shared triggerqueue policy stores for one instance.
type Store struct {
	pool       *pgxpool.Pool
	instanceID string
	manager    *cloudlease.Manager
	handle     *cloudlease.Handle
	childBlobs blobstore.Store
}

// New constructs an instance-scoped acceptance store.
func New(pool *pgxpool.Pool, instanceID string) (*Store, error) {
	if pool == nil || strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("cloud acceptance requires a pool and instance")
	}
	return &Store{pool: pool, instanceID: instanceID}, nil
}

// BindFence supplies the current controller lease authority.
func (s *Store) BindFence(manager *cloudlease.Manager, handle *cloudlease.Handle) {
	s.manager, s.handle = manager, handle
}

// BindChildBlobs supplies content-addressed storage for child custody payloads.
func (s *Store) BindChildBlobs(blobs blobstore.Store) { s.childBlobs = blobs }

// Close leaves ownership of the shared pool with the composition root.
func (s *Store) Close() error { return nil }

// Accept records an ordinary trigger or returns its exact idempotent replay.
func (s *Store) Accept(ctx context.Context, key, actor string, payload []byte, now time.Time) (triggerqueue.Record, bool, error) {
	if !validText(key, 256, true) || !validText(actor, 1024, false) ||
		len(payload) == 0 || len(payload) > triggerqueue.MaxPayloadBytes || now.IsZero() {
		return triggerqueue.Record{}, false, errors.New("cloud acceptance: invalid acceptance")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize capacity accounting and replay resolution for this instance.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('accept:' || $1, 0))", s.instanceID); err != nil {
		return triggerqueue.Record{}, false, err
	}
	prior, err := s.byKey(ctx, tx, key)
	if err == nil {
		if prior.Actor != actor || !bytes.Equal(prior.Payload, payload) {
			return triggerqueue.Record{}, false, triggerqueue.ErrConflict
		}
		return prior, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return triggerqueue.Record{}, false, err
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM control.acceptance WHERE instance_id=$1", s.instanceID).Scan(&count); err != nil {
		return triggerqueue.Record{}, false, err
	}
	if count >= triggerqueue.MaxRecords {
		return triggerqueue.Record{}, false, triggerqueue.ErrFull
	}
	runID, err := randomRunID()
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	record := triggerqueue.Record{
		ID: "trigger-" + runID, Key: key, Actor: actor, Payload: append([]byte(nil), payload...),
		State: triggerqueue.Accepted, RunID: runID, AcceptedAt: now.UTC(),
	}
	fingerprint := sha256.Sum256(payload)
	tag, err := tx.Exec(ctx, `
		INSERT INTO control.acceptance
			(instance_id, id, idem_key, actor, fingerprint, payload, state, run_id, accepted_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'accepted', $7, $8)
		ON CONFLICT (instance_id, idem_key) DO NOTHING`,
		s.instanceID, record.ID, key, actor, fingerprint[:], payload, runID, record.AcceptedAt,
	)
	if err != nil {
		return triggerqueue.Record{}, false, err
	}
	if tag.RowsAffected() == 0 {
		prior, err := s.byKey(ctx, tx, key)
		if err != nil {
			return triggerqueue.Record{}, false, err
		}
		if prior.Actor != actor || !bytes.Equal(prior.Payload, payload) {
			return triggerqueue.Record{}, false, triggerqueue.ErrConflict
		}
		return prior, true, tx.Commit(ctx)
	}
	return record, false, tx.Commit(ctx)
}

// ByKey reads internal idempotency custody.
func (s *Store) ByKey(ctx context.Context, key string) (triggerqueue.Record, error) {
	record, err := s.byKey(ctx, s.pool, key)
	return record, noRows(err)
}

func (s *Store) byKey(ctx context.Context, query rowQuerier, key string) (triggerqueue.Record, error) {
	return scanRecord(query.QueryRow(ctx, acceptanceSelect+" AND idem_key=$2", s.instanceID, key))
}

// ByRun reads the acceptance that owns an engine run (the acceptance's run ID is
// the workflow ID). The terminal awaiter identifies a closed run from it.
func (s *Store) ByRun(ctx context.Context, runID string) (triggerqueue.Record, error) {
	record, err := scanRecord(s.pool.QueryRow(ctx, acceptanceSelect+" AND run_id=$2", s.instanceID, runID))
	return record, noRows(err)
}

// Get binds an acceptance read to its actor.
func (s *Store) Get(ctx context.Context, id, actor string) (triggerqueue.Record, error) {
	record, err := scanRecord(s.pool.QueryRow(ctx, acceptanceSelect+" AND id=$2 AND actor=$3", s.instanceID, id, actor))
	return record, noRows(err)
}

// Pending returns accepted custody in FIFO order while skipping rows locked by another drain.
func (s *Store) Pending(ctx context.Context, limit int) ([]triggerqueue.Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: batch limit must be 1..100")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, acceptanceColumns+`
		FROM control.acceptance AS a
		WHERE a.instance_id=$1 AND a.state='accepted'
		  AND NOT EXISTS (`+childLineageOwned+`)
		ORDER BY a.accepted_at, a.id
		FOR UPDATE SKIP LOCKED LIMIT $2`,
		s.instanceID, limit,
	)
	if err != nil {
		return nil, err
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.Record, error) {
		return scanRecord(row)
	})
	if err != nil {
		return nil, err
	}
	return records, tx.Commit(ctx)
}

// RetainedPage returns accepted and uncertain rows that pin config generations.
func (s *Store) RetainedPage(ctx context.Context, after string, limit int) ([]triggerqueue.Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: batch limit must be 1..100")
	}
	rows, err := s.pool.Query(ctx, acceptanceColumns+`
		FROM control.acceptance
		WHERE instance_id=$1 AND state IN ('accepted','dispatching') AND id>$2
		ORDER BY id LIMIT $3`,
		s.instanceID, after, limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.Record, error) {
		return scanRecord(row)
	})
}

// BeginDispatch claims an accepted row and stamps the current owner epoch.
func (s *Store) BeginDispatch(ctx context.Context, id string) error {
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance AS a
			SET state='dispatching', owner_epoch=$3, dispatch_started_at=clock_timestamp(),
 start_retry_at=NULL, start_retry_count=0
			WHERE a.instance_id=$1 AND a.id=$2 AND a.state='accepted'
			  AND NOT EXISTS (
				SELECT 1 FROM control.child_lineage AS c
				JOIN control.child_parent AS p
				  ON p.instance_id=c.instance_id AND p.gaggle=c.gaggle
				 AND p.parent_run_id=c.parent_run_id
				WHERE c.instance_id=a.instance_id AND c.acceptance_id=a.id
				  AND (c.owner_epoch<>$3 OR p.owner_epoch<>$3
				       OR p.cancelled_at IS NOT NULL
				       OR c.state IN ('completed','failed','cancelled'))
			  )`,
			s.instanceID, id, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// Requeue returns a dispatching row to accepted custody.
func (s *Store) Requeue(ctx context.Context, id, reason string) error {
	if len(reason) > 1024 {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance
			SET state='accepted', owner_epoch=NULL, reason=$4
			WHERE instance_id=$1 AND id=$2 AND state='dispatching'
			  AND owner_epoch=$3 AND finished_at IS NULL`,
			s.instanceID, id, epoch, reason,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// RecordDispatch records the accepted run identity.
func (s *Store) RecordDispatch(ctx context.Context, id, runID string) error {
	if len(runID) > 256 || id != "trigger-"+runID {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance SET reason=''
			WHERE instance_id=$1 AND id=$2 AND state='dispatching'
			  AND owner_epoch=$3 AND run_id=$4`,
			s.instanceID, id, epoch, runID,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// Finish records a definite terminal scheduler outcome.
func (s *Store) Finish(ctx context.Context, id string, state triggerqueue.State, runID, reason string, now time.Time) error {
	if (state != triggerqueue.Dispatched && state != triggerqueue.Rejected) ||
		len(runID) > 256 || len(reason) > 1024 || now.IsZero() ||
		(state == triggerqueue.Rejected && runID != "") {
		return triggerqueue.ErrTransition
	}
	assignedRunID, assigned := strings.CutPrefix(id, "trigger-")
	if !assigned || assignedRunID == "" ||
		(state == triggerqueue.Dispatched && runID != assignedRunID) {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance
			SET state=$4, reason=$5, finished_at=GREATEST(accepted_at, $6)
			WHERE instance_id=$1 AND id=$2 AND state='dispatching'
			  AND owner_epoch=$3 AND run_id=$7 AND finished_at IS NULL`,
			s.instanceID, id, epoch, state, reason, now.UTC(), assignedRunID,
		)
		return changed(tag.RowsAffected(), err)
	})
}

// RejectAccepted finishes a row that was never dispatched as rejected with a
// reason, under the controller fence. It is the terminal outcome for an
// accepted start the resolver refuses permanently; it cannot touch a row that
// has begun dispatching or one a generated child owns (child custody ends only
// through its own lifecycle).
func (s *Store) RejectAccepted(ctx context.Context, id, reason string, now time.Time) error {
	if _, assigned := strings.CutPrefix(id, "trigger-"); !assigned || len(reason) > 1024 || now.IsZero() {
		return triggerqueue.ErrTransition
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.acceptance AS a
			SET state='rejected', reason=$4, owner_epoch=$3, finished_at=GREATEST(a.accepted_at, $5)
			WHERE a.instance_id=$1 AND a.id=$2 AND a.state='accepted' AND a.finished_at IS NULL
			  AND NOT EXISTS (`+childLineageOwned+`)`,
			s.instanceID, id, epoch, reason, now.UTC(),
		)
		return changed(tag.RowsAffected(), err)
	})
}

// Uncertain returns dispatching rows not owned by a currently live epoch.
func (s *Store) Uncertain(ctx context.Context, liveEpochs []int64, limit int) ([]triggerqueue.Record, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: batch limit must be 1..100")
	}
	rows, err := s.pool.Query(ctx, acceptanceColumns+`
		FROM control.acceptance AS a
		WHERE a.instance_id=$1 AND a.state='dispatching'
		  AND (a.owner_epoch IS NULL OR NOT (a.owner_epoch=ANY($2)))
		  AND NOT EXISTS (`+childLineageOwned+`)
		ORDER BY a.accepted_at, a.id LIMIT $3`,
		s.instanceID, normalizedEpochs(liveEpochs), limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.Record, error) {
		return scanRecord(row)
	})
}

func (s *Store) withFence(ctx context.Context, action func(context.Context, pgx.Tx, int64) error) error {
	if s.manager == nil || s.handle == nil || s.handle.InstanceID != s.instanceID {
		return triggerqueue.ErrTransition
	}
	return s.manager.WithFence(ctx, s.handle, func(txCtx context.Context, tx pgx.Tx) error {
		return action(txCtx, tx, s.handle.Epoch)
	})
}

func normalizedEpochs(epochs []int64) []int64 {
	if epochs == nil {
		return []int64{}
	}
	return epochs
}

const acceptanceColumns = `
	SELECT id, idem_key, actor, payload, state, run_id, reason, accepted_at`

const acceptanceSelect = acceptanceColumns + `
	FROM control.acceptance WHERE instance_id=$1`

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanRecord(row pgx.Row) (triggerqueue.Record, error) {
	var record triggerqueue.Record
	err := row.Scan(
		&record.ID, &record.Key, &record.Actor, &record.Payload,
		&record.State, &record.RunID, &record.Reason, &record.AcceptedAt,
	)
	record.AcceptedAt = record.AcceptedAt.UTC()
	return record, err
}

func changed(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return triggerqueue.ErrTransition
	}
	return nil
}

func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sql.ErrNoRows
	}
	return err
}

func randomRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("cloud acceptance: random run ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validText(value string, limit int, required bool) bool {
	if strings.TrimSpace(value) != value || len(value) > limit || (required && value == "") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

var _ triggerqueue.AcceptanceStore = (*Store)(nil)
