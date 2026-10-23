package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// SourceReceipt reads one durable source delivery.
func (s *Store) SourceReceipt(ctx context.Context, key string) (triggerqueue.SourceReceipt, error) {
	receipt, err := s.sourceReceipt(ctx, s.pool, key)
	return receipt, noRows(err)
}

func (s *Store) sourceReceipt(ctx context.Context, query rowQuerier, key string) (triggerqueue.SourceReceipt, error) {
	var receipt triggerqueue.SourceReceipt
	err := query.QueryRow(ctx, `
		SELECT source_key, actor, fingerprint, acceptance_ids, accepted_at
		FROM control.source_receipt
		WHERE instance_id=$1 AND source_key=$2`,
		s.instanceID, key,
	).Scan(&receipt.Key, &receipt.Actor, &receipt.Fingerprint, &receipt.AcceptanceIDs, &receipt.AcceptedAt)
	receipt.AcceptedAt = receipt.AcceptedAt.UTC()
	return receipt, err
}

// AcceptSource atomically records recipients, cursor movement, and demand transfer.
func (s *Store) AcceptSource(
	ctx context.Context,
	batch triggerqueue.SourceBatch,
	now time.Time,
) (triggerqueue.SourceReceipt, bool, error) {
	if !validText(batch.Key, 256, true) || !validText(batch.Actor, 1024, true) ||
		!validText(batch.Fingerprint, 128, true) || now.IsZero() ||
		len(batch.Starts) > triggerqueue.MaxSourceStarts {
		return triggerqueue.SourceReceipt{}, false, errors.New("cloud acceptance: invalid source batch")
	}
	var receipt triggerqueue.SourceReceipt
	var repeated bool
	err := s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, _ int64) error {
		var err error
		receipt, repeated, err = s.acceptSource(ctx, tx, batch, now)
		return err
	})
	return receipt, repeated, err
}

func (s *Store) acceptSource(ctx context.Context, tx pgx.Tx, batch triggerqueue.SourceBatch, now time.Time) (triggerqueue.SourceReceipt, bool, error) {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('source-accept:' || $1, 0))`,
		batch.Key,
	); err != nil {
		return triggerqueue.SourceReceipt{}, false, err
	}
	prior, err := s.sourceReceipt(ctx, tx, batch.Key)
	if err == nil {
		if prior.Actor != batch.Actor || prior.Fingerprint != batch.Fingerprint {
			return triggerqueue.SourceReceipt{}, false, triggerqueue.ErrConflict
		}
		return prior, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return triggerqueue.SourceReceipt{}, false, err
	}
	if batch.PendingLimit != nil {
		count, err := s.pendingWorkflowStarts(ctx, tx, *batch.PendingLimit)
		if err != nil {
			return triggerqueue.SourceReceipt{}, false, err
		}
		if count+len(batch.Starts) > batch.PendingLimit.MaxPending {
			return triggerqueue.SourceReceipt{}, false, triggerqueue.ErrWorkerOccupancy
		}
	}
	receipt := triggerqueue.SourceReceipt{
		Key: batch.Key, Actor: batch.Actor, Fingerprint: batch.Fingerprint,
		AcceptedAt: now.UTC(), AcceptanceIDs: []string{},
	}
	for ordinal, start := range batch.Starts {
		if len(start.Payload) == 0 || len(start.Payload) > triggerqueue.MaxPayloadBytes {
			return triggerqueue.SourceReceipt{}, false, errors.New("cloud acceptance: invalid source start")
		}
		runID, err := randomRunID()
		if err != nil {
			return triggerqueue.SourceReceipt{}, false, err
		}
		id := "trigger-" + runID
		keyDigest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", batch.Key, ordinal)))
		key := "source-start:" + hex.EncodeToString(keyDigest[:])
		fingerprint := sha256.Sum256(start.Payload)
		if _, err := tx.Exec(ctx, `
			INSERT INTO control.acceptance
				(instance_id, id, idem_key, actor, fingerprint, payload, state, run_id, accepted_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'accepted', $7, $8)`,
			s.instanceID, id, key, batch.Actor, fingerprint[:], start.Payload, runID, now.UTC(),
		); err != nil {
			return triggerqueue.SourceReceipt{}, false, err
		}
		receipt.AcceptanceIDs = append(receipt.AcceptanceIDs, id)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.source_receipt
			(instance_id, source_key, actor, fingerprint, acceptance_ids, accepted_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		s.instanceID, batch.Key, batch.Actor, batch.Fingerprint, receipt.AcceptanceIDs, now.UTC(),
	); err != nil {
		return triggerqueue.SourceReceipt{}, false, err
	}
	if err := advanceCursor(ctx, tx, s.instanceID, batch.Advance); err != nil {
		return triggerqueue.SourceReceipt{}, false, err
	}
	if err := transferDemand(ctx, tx, s.instanceID, batch.Demand, len(batch.Starts)); err != nil {
		return triggerqueue.SourceReceipt{}, false, err
	}
	return receipt, false, nil
}

// PendingWorkflowStarts counts queued starts for one workflow.
func (s *Store) PendingWorkflowStarts(ctx context.Context, limit triggerqueue.WorkflowPendingLimit) (int, error) {
	return s.pendingWorkflowStarts(ctx, s.pool, limit)
}

func (s *Store) pendingWorkflowStarts(
	ctx context.Context,
	query interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	limit triggerqueue.WorkflowPendingLimit,
) (int, error) {
	if !validText(limit.Gaggle, 256, true) || !validText(limit.Workflow, 256, true) || limit.MaxPending < 1 {
		return 0, errors.New("cloud acceptance: invalid pending workflow limit")
	}
	rows, err := query.Query(ctx, `
		SELECT payload, run_id FROM control.acceptance
		WHERE instance_id=$1 AND state IN ('accepted','dispatching')`,
		s.instanceID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	active := make(map[string]bool, len(limit.ActiveRunIDs))
	for _, runID := range limit.ActiveRunIDs {
		active[runID] = true
	}
	count := 0
	for rows.Next() {
		var payload []byte
		var runID string
		if err := rows.Scan(&payload, &runID); err != nil {
			return 0, err
		}
		var shape struct {
			Target struct {
				Gaggle   string `json:"gaggle"`
				Workflow string `json:"workflow"`
			} `json:"target"`
		}
		if json.Unmarshal(payload, &shape) == nil &&
			shape.Target.Gaggle == limit.Gaggle && shape.Target.Workflow == limit.Workflow &&
			(runID == "" || !active[runID]) {
			count++
		}
	}
	return count, rows.Err()
}

// CaptureScheduleDemand records a pinned schedule obligation and cursor advance.
func (s *Store) CaptureScheduleDemand(ctx context.Context, capture triggerqueue.ScheduleDemandCapture) error {
	if !validText(capture.ID, 256, true) || len(capture.Payload) == 0 ||
		len(capture.Payload) > triggerqueue.MaxPayloadBytes || capture.AcceptedAt.IsZero() {
		return errors.New("cloud acceptance: invalid schedule demand")
	}
	fingerprint, err := demandFingerprint(capture)
	if err != nil {
		return err
	}
	return s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, _ int64) error {
		return s.captureScheduleDemand(ctx, tx, capture, fingerprint)
	})
}

func (s *Store) captureScheduleDemand(ctx context.Context, tx pgx.Tx, capture triggerqueue.ScheduleDemandCapture, fingerprint string) error {
	var prior string
	err := tx.QueryRow(ctx, `
		SELECT fingerprint FROM control.schedule_demand
		WHERE instance_id=$1 AND id=$2`,
		s.instanceID, capture.ID,
	).Scan(&prior)
	if err == nil {
		if prior != fingerprint {
			return triggerqueue.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !capture.Independent {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM control.schedule_demand
				WHERE instance_id=$1 AND scope=$2 AND independent=false
			)`,
			s.instanceID, capture.Advance.Scope,
		).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if err := advanceCursor(ctx, tx, s.instanceID, &capture.Advance); err != nil {
				return err
			}
			return nil
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.schedule_demand
			(instance_id, id, scope, payload, accepted_at, independent, fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.instanceID, capture.ID, capture.Advance.Scope, capture.Payload,
		capture.AcceptedAt.UTC(), capture.Independent, fingerprint,
	); err != nil {
		return err
	}
	if err := advanceCursor(ctx, tx, s.instanceID, &capture.Advance); err != nil {
		return err
	}
	return nil
}

// ScheduleDemand reads the oldest obligation for a scope.
func (s *Store) ScheduleDemand(ctx context.Context, scope string) (triggerqueue.ScheduleDemand, error) {
	return s.readDemand(ctx, s.pool, `
		SELECT id, scope, payload, observed_count, queued, accepted_at, independent
		FROM control.schedule_demand
		WHERE instance_id=$1 AND scope=$2
		ORDER BY accepted_at, id LIMIT 1`,
		scope,
	)
}

// ScheduleDemandByID reads one exact obligation.
func (s *Store) ScheduleDemandByID(ctx context.Context, id string) (triggerqueue.ScheduleDemand, error) {
	return s.readDemand(ctx, s.pool, `
		SELECT id, scope, payload, observed_count, queued, accepted_at, independent
		FROM control.schedule_demand
		WHERE instance_id=$1 AND id=$2`,
		id,
	)
}

func (s *Store) readDemand(ctx context.Context, query rowQuerier, sqlText, value string) (triggerqueue.ScheduleDemand, error) {
	var demand triggerqueue.ScheduleDemand
	err := query.QueryRow(ctx, sqlText, s.instanceID, value).Scan(
		&demand.ID, &demand.Scope, &demand.Payload, &demand.Count,
		&demand.Queued, &demand.AcceptedAt, &demand.Independent,
	)
	demand.AcceptedAt = demand.AcceptedAt.UTC()
	return demand, noRows(err)
}

// ScheduleDemandPage returns retained obligations by ID.
func (s *Store) ScheduleDemandPage(ctx context.Context, after string, limit int) ([]triggerqueue.ScheduleDemand, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud acceptance: invalid demand page")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, scope, payload, observed_count, queued, accepted_at, independent
		FROM control.schedule_demand
		WHERE instance_id=$1 AND id>$2 ORDER BY id LIMIT $3`,
		s.instanceID, after, limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.ScheduleDemand, error) {
		var demand triggerqueue.ScheduleDemand
		err := row.Scan(
			&demand.ID, &demand.Scope, &demand.Payload, &demand.Count,
			&demand.Queued, &demand.AcceptedAt, &demand.Independent,
		)
		return demand, err
	})
}

// ObserveScheduleDemand seals the worker count once.
func (s *Store) ObserveScheduleDemand(ctx context.Context, id string, count int) error {
	if count < 0 || count > triggerqueue.MaxRecords {
		return errors.New("cloud acceptance: invalid demand count")
	}
	return s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, _ int64) error {
		return s.observeScheduleDemand(ctx, tx, id, count)
	})
}

func (s *Store) observeScheduleDemand(ctx context.Context, tx pgx.Tx, id string, count int) error {
	var prior int
	err := tx.QueryRow(ctx, `
		SELECT observed_count FROM control.schedule_demand
		WHERE instance_id=$1 AND id=$2 FOR UPDATE`,
		s.instanceID, id,
	).Scan(&prior)
	if err != nil {
		return noRows(err)
	}
	if prior >= 0 {
		if prior != count {
			return triggerqueue.ErrConflict
		}
		return nil
	}
	if count == 0 {
		_, err = tx.Exec(ctx, "DELETE FROM control.schedule_demand WHERE instance_id=$1 AND id=$2", s.instanceID, id)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE control.schedule_demand SET observed_count=$3
			WHERE instance_id=$1 AND id=$2`,
			s.instanceID, id, count,
		)
	}
	if err != nil {
		return err
	}
	return nil
}

func transferDemand(ctx context.Context, tx pgx.Tx, instanceID string, transfer *triggerqueue.DemandTransfer, starts int) error {
	if transfer == nil {
		return nil
	}
	if transfer.Before < 0 || transfer.After <= transfer.Before || transfer.After-transfer.Before != starts {
		return errors.New("cloud acceptance: invalid demand transfer")
	}
	tag, err := tx.Exec(ctx, `
		UPDATE control.schedule_demand SET queued=$3
		WHERE instance_id=$1 AND id=$2 AND queued=$4 AND observed_count >= $3`,
		instanceID, transfer.ID, transfer.After, transfer.Before,
	)
	if err := changed(tag.RowsAffected(), err); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		DELETE FROM control.schedule_demand
		WHERE instance_id=$1 AND id=$2 AND queued=observed_count`,
		instanceID, transfer.ID,
	)
	return err
}

func demandFingerprint(capture triggerqueue.ScheduleDemandCapture) (string, error) {
	capture.AcceptedAt = time.Time{}
	capture.Advance.Before = capture.Advance.Before.UTC()
	capture.Advance.After = capture.Advance.After.UTC()
	raw, err := json.Marshal(capture)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

var _ triggerqueue.SchedulerPolicyStore = (*Store)(nil)
