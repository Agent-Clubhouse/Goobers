// Package scheduling owns backend-neutral control-plane scheduling loops.
package scheduling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
)

const defaultStallBatch = 100

// ErrNoWorker means cancellation cannot be delivered because no worker serves
// the run. The sweep escalates that run to a server-side termination.
var ErrNoWorker = errors.New("stall sweep: no worker serves run")

// StallActivity is the journal backend's durable activity-clock observation.
type StallActivity = journal.StallActivity

// StallSource pages running journals with their durable append clock.
type StallSource = journal.StallBackend

// EngineControl stops engine workflows without editing their journals.
type EngineControl interface {
	Cancel(context.Context, string) error
	Terminate(context.Context, string, string) error
}

// StallConfig binds one cloud instance to its control lease.
type StallConfig struct {
	Pool           *pgxpool.Pool
	InstanceID     string
	Manager        *cloudlease.Manager
	Handle         *cloudlease.Handle
	Source         StallSource
	Engine         EngineControl
	StalledTimeout time.Duration
	MaxRunDuration time.Duration
	BatchSize      int
}

// StallSweeper applies cloud stall and maximum-duration policy.
type StallSweeper struct{ config StallConfig }

// NewStallSweeper validates and constructs a cloud sweep.
func NewStallSweeper(config StallConfig) (*StallSweeper, error) {
	if config.Pool == nil || config.InstanceID == "" ||
		config.Manager == nil || config.Handle == nil ||
		config.Handle.InstanceID != config.InstanceID ||
		config.Source == nil || config.Engine == nil ||
		config.StalledTimeout <= 0 || config.MaxRunDuration < 0 {
		return nil, errors.New("stall sweep: invalid cloud configuration")
	}
	if config.BatchSize <= 0 {
		config.BatchSize = defaultStallBatch
	}
	if config.BatchSize > 100 {
		return nil, errors.New("stall sweep: batch size exceeds 100")
	}
	return &StallSweeper{config: config}, nil
}

// StallSweepResult reports external actions admitted by this pass.
type StallSweepResult struct {
	Cancelled  int
	Terminated int
}

// Sweep processes a bounded stable page. Controller downtime is deliberately
// absent: cloud stages continue on workers while the controller is unavailable.
func (s *StallSweeper) Sweep(ctx context.Context) (StallSweepResult, error) {
	var result StallSweepResult
	var after string
	for {
		candidates, err := s.config.Source.StallCandidates(
			ctx, after, s.config.BatchSize)
		if err != nil {
			return result, err
		}
		for _, candidate := range candidates {
			after = candidate.Identity.RunID
			if !candidate.Identity.EngineDriven() {
				continue
			}
			reason, timeout, due, err := s.due(candidate)
			if err != nil {
				return result, err
			}
			if !due {
				continue
			}
			action, err := s.claim(ctx, candidate.Identity.RunID, reason)
			if err != nil {
				return result, err
			}
			switch action.state {
			case "pending_cancel":
				err := s.config.Engine.Cancel(ctx, candidate.Identity.RunID)
				if errors.Is(err, ErrNoWorker) {
					if err := s.terminate(ctx, candidate.Identity.RunID, reason); err != nil {
						return result, err
					}
					result.Terminated++
					continue
				}
				if err != nil {
					return result, fmt.Errorf(
						"stall sweep: cancel %s: %w", candidate.Identity.RunID, err)
				}
				if err := s.markCancelled(ctx, candidate.Identity.RunID); err != nil {
					return result, err
				}
				result.Cancelled++
			case "cancel_requested":
				if action.cancelRequestedAt != nil &&
					!candidate.DatabaseNow.Before(action.cancelRequestedAt.Add(timeout)) {
					if err := s.terminate(ctx, candidate.Identity.RunID, reason); err != nil {
						return result, err
					}
					result.Terminated++
				}
			case "terminated":
			default:
				return result, fmt.Errorf("stall sweep: invalid action state %q", action.state)
			}
		}
		if len(candidates) < s.config.BatchSize {
			return result, nil
		}
	}
}

func (s *StallSweeper) due(
	candidate StallActivity,
) (reason string, timeout time.Duration, due bool, err error) {
	stallTimeout := s.config.StalledTimeout
	maxDuration := s.config.MaxRunDuration
	if candidate.Identity.RunControls != nil {
		if err := runcontrol.ValidatePinned(candidate.Identity.RunControls); err != nil {
			return "", 0, false, err
		}
		stallTimeout, _ = time.ParseDuration(
			candidate.Identity.RunControls.StalledRunTimeout)
		maxDuration = 0
		if candidate.Identity.RunControls.MaxRunDuration != "" {
			maxDuration, _ = time.ParseDuration(
				candidate.Identity.RunControls.MaxRunDuration)
		}
	}
	if maxDuration > 0 &&
		!candidate.DatabaseNow.Before(candidate.Identity.StartedAt.Add(maxDuration)) {
		return "max_duration", stallTimeout, true, nil
	}
	if !candidate.DatabaseNow.Before(candidate.LastAppendAt.Add(stallTimeout)) {
		return "stalled", stallTimeout, true, nil
	}
	return "", stallTimeout, false, nil
}

type stallAction struct {
	state             string
	cancelRequestedAt *time.Time
}

func (s *StallSweeper) claim(
	ctx context.Context,
	runID string,
	reason string,
) (stallAction, error) {
	var action stallAction
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		return tx.QueryRow(txCtx, `
			INSERT INTO control.stall_action (
				instance_id,run_id,reason,state,detected_at,owner_epoch
			) VALUES ($1,$2,$3,'pending_cancel',clock_timestamp(),$4)
			ON CONFLICT (instance_id,run_id) DO UPDATE
			SET owner_epoch=excluded.owner_epoch,
			    reason=CASE
			      WHEN control.stall_action.state='pending_cancel'
			      THEN excluded.reason ELSE control.stall_action.reason END
			WHERE control.stall_action.instance_id=$1
			  AND control.stall_action.owner_epoch<=$4
			RETURNING state,cancel_requested_at`,
			s.config.InstanceID, runID, reason, epoch,
		).Scan(&action.state, &action.cancelRequestedAt)
	})
	return action, err
}

func (s *StallSweeper) markCancelled(ctx context.Context, runID string) error {
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.stall_action
			SET state='cancel_requested',cancel_requested_at=clock_timestamp(),
			    owner_epoch=$3
			WHERE instance_id=$1 AND run_id=$2
			  AND state='pending_cancel' AND owner_epoch=$3`,
			s.config.InstanceID, runID, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

func (s *StallSweeper) terminate(
	ctx context.Context,
	runID string,
	reason string,
) error {
	if err := s.config.Engine.Terminate(ctx, runID, reason); err != nil {
		return fmt.Errorf("stall sweep: terminate %s: %w", runID, err)
	}
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.stall_action
			SET state='terminated',terminated_at=clock_timestamp(),owner_epoch=$3
			WHERE instance_id=$1 AND run_id=$2
			  AND state IN ('pending_cancel','cancel_requested') AND owner_epoch=$3`,
			s.config.InstanceID, runID, epoch,
		)
		return changed(tag.RowsAffected(), err)
	})
}

func (s *StallSweeper) withFence(
	ctx context.Context,
	action func(context.Context, pgx.Tx, int64) error,
) error {
	if s.config.Handle.InstanceID != s.config.InstanceID {
		return cloudlease.ErrStale
	}
	return s.config.Manager.WithFence(ctx, s.config.Handle,
		func(txCtx context.Context, tx pgx.Tx) error {
			return action(txCtx, tx, s.config.Handle.Epoch)
		})
}

func changed(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return cloudlease.ErrStale
	}
	return nil
}
