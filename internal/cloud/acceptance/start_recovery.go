package acceptance

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const startRecoveryBatch = 32
const startRecoveryTimeout = 10 * time.Second

// ErrPermanentStart marks a resolver refusal that can never succeed on retry
// (drifted pins, a definition the engine cannot walk, a missing workflow, an
// expired admission deadline). The controller finishes such a row as rejected
// with the reason instead of re-acquiring the generation on every sweep.
var ErrPermanentStart = errors.New("cloud start is permanently refused")

// StartInputResolver reconstructs pinned workflow input from durable acceptance.
// The controller supplies configuration-generation resolution; admission still
// replaces all start authority from the bound store before contacting Temporal.
type StartInputResolver func(context.Context, triggerqueue.Record) (engine.RunInput, error)

// StartStatus exposes uncertain custody without publishing private input.
type StartStatus struct {
	AcceptanceID string     `json:"acceptanceId"`
	RunID        string     `json:"runId"`
	State        string     `json:"state"`
	RetryAt      *time.Time `json:"retryAt,omitempty"`
}

// StartStatusPage returns bounded, read-only status for quarantined dispatches
// and dispatches older than one minute. A controller/API can page by AcceptanceID without holding a lease.
func (s *Store) StartStatusPage(ctx context.Context, after string, limit int) ([]StartStatus, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("cloud start status limit must be 1..100")
	}
	rows, err := s.pool.Query(ctx, `SELECT id, run_id, start_retry_at FROM control.acceptance AS a
 WHERE instance_id=$1 AND state='dispatching' AND id>$2
 AND NOT EXISTS (SELECT 1 FROM control.child_lineage AS c
  WHERE c.instance_id=a.instance_id AND c.acceptance_id=a.id)
 AND (start_retry_at IS NOT NULL OR dispatch_started_at < clock_timestamp()-interval '1 minute')
 ORDER BY id LIMIT $3`, s.instanceID, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (StartStatus, error) {
		status := StartStatus{State: "start ownership uncertain"}
		err := row.Scan(&status.AcceptanceID, &status.RunID, &status.RetryAt)
		return status, err
	})
}

// RecoverStarts runs one bounded recovery sweep. Each attempt is independently
// time-bounded, with at most four concurrent starts. An uncertain run never
// fails another attempt or changes its acceptance custody. The controller calls
// this in its background reconciliation loop, independently of startup.
func (s *Store) RecoverStarts(ctx context.Context, starter engine.Starter, resolve StartInputResolver) error {
	if starter == nil || resolve == nil {
		return errors.New("cloud start recovery requires starter and pinned input resolver")
	}
	records, err := s.reserveStartRetries(ctx)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, record := range records {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			workers.Wait()
			return ctx.Err()
		}
		workers.Go(func() {
			defer func() { <-slots }()
			attempt, cancel := context.WithTimeout(ctx, startRecoveryTimeout)
			defer cancel()
			in, err := resolve(attempt, record)
			if err != nil {
				return
			}
			in.AcceptanceID = record.ID
			// Start performs mine/adopt confirmation itself. Errors and NotMine leave
			// this row dispatching for status and its already scheduled backoff retry.
			_, _ = starter.Start(attempt, in)
		})
	}
	workers.Wait()
	return ctx.Err()
}

func (s *Store) reserveStartRetries(ctx context.Context) ([]triggerqueue.Record, error) {
	var records []triggerqueue.Record
	err := s.withFence(ctx, func(ctx context.Context, tx pgx.Tx, epoch int64) error {
		rows, err := tx.Query(ctx, acceptanceColumns+` FROM control.acceptance AS a
   WHERE instance_id=$1 AND state='dispatching' AND owner_epoch>0 AND owner_epoch<=$2
   AND NOT EXISTS (SELECT 1 FROM control.child_lineage AS c
    WHERE c.instance_id=a.instance_id AND c.acceptance_id=a.id)
   AND ((owner_epoch<$2 AND start_retry_epoch<=owner_epoch) OR start_retry_at<=clock_timestamp()
     OR (start_retry_at IS NULL AND dispatch_started_at < clock_timestamp()-interval '1 minute'))
   ORDER BY start_retry_at NULLS FIRST, dispatch_started_at, id
   FOR UPDATE SKIP LOCKED LIMIT $3`, s.instanceID, epoch, startRecoveryBatch)
		if err != nil {
			return err
		}
		records, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (triggerqueue.Record, error) { return scanRecord(row) })
		if err != nil {
			return err
		}
		for _, record := range records {
			_, err = tx.Exec(ctx, `UPDATE control.acceptance
    SET start_retry_at=clock_timestamp()+interval '10 seconds'+ LEAST(60,1<<start_retry_count)*interval '1 second',
    start_retry_count=LEAST(6,start_retry_count+1), start_retry_epoch=$3
    WHERE instance_id=$1 AND id=$2`, s.instanceID, record.ID, epoch)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return records, err
}
