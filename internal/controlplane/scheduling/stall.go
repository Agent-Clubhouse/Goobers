// Package scheduling owns backend-neutral control-plane scheduling loops.
package scheduling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/boundedagg"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
)

const defaultStallBatch = 100

// ErrNoWorker means cancellation cannot be delivered because no worker serves
// the run. The sweep escalates that run to a server-side termination.
var (
	ErrNoWorker     = errors.New("stall sweep: no worker serves run")
	ErrEngineClosed = errors.New("stall sweep: engine workflow already closed")
)

// StallActivity is the journal backend's durable activity-clock observation.
type StallActivity = journal.StallActivity

// StallSource pages running journals with their durable append clock.
type StallSource = journal.StallBackend

// EngineControl stops engine workflows without editing their journals.
type EngineControl interface {
	Cancel(context.Context, string) error
	Terminate(context.Context, string, string) error
}

// RunTerminalizer durably closes the journal/read-model cleanup path after a
// server-side engine termination.
type RunTerminalizer interface {
	Terminalize(context.Context, journal.RunIdentity, string) error
}

// StallConfig binds one cloud instance to its control lease.
type StallConfig struct {
	Pool           *pgxpool.Pool
	InstanceID     string
	Manager        *cloudlease.Manager
	Handle         *cloudlease.Handle
	Source         StallSource
	Engine         EngineControl
	Terminalizer   RunTerminalizer
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
		config.Source == nil || config.Engine == nil || config.Terminalizer == nil ||
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
	var sweepErrs []error
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
			cancelled, terminated, err := s.processCandidate(ctx, candidate)
			result.Cancelled += cancelled
			result.Terminated += terminated
			if err != nil {
				sweepErrs = append(sweepErrs, fmt.Errorf(
					"stall sweep: run %s: %w", candidate.Identity.RunID, err))
			}
		}
		if len(candidates) < s.config.BatchSize {
			return result, boundedagg.Join(sweepErrs...)
		}
	}
}

func (s *StallSweeper) processCandidate(
	ctx context.Context,
	candidate StallActivity,
) (cancelled int, terminated int, err error) {
	reason, timeout, due, err := s.due(candidate)
	if err != nil || !due {
		return 0, 0, err
	}
	action, err := s.claim(ctx, candidate.Identity.RunID, reason)
	if err != nil {
		return 0, 0, err
	}
	if action.retryAt != nil && candidate.DatabaseNow.Before(*action.retryAt) {
		return 0, 0, nil
	}
	switch action.state {
	case "pending_cancel", "cancelling":
		if action.state == "pending_cancel" {
			if err := s.transition(ctx, candidate.Identity.RunID,
				"pending_cancel", "cancelling"); err != nil {
				return 0, 0, err
			}
		}
		cancelErr := s.config.Engine.Cancel(ctx, candidate.Identity.RunID)
		switch {
		case errors.Is(cancelErr, ErrNoWorker):
			terminated, err := s.beginTermination(
				ctx, candidate, reason, "cancelling", true)
			return 0, terminated, err
		case errors.Is(cancelErr, ErrEngineClosed):
			terminated, err := s.beginTermination(
				ctx, candidate, reason, "cancelling", false)
			return 0, terminated, err
		case cancelErr != nil:
			recordErr := s.recordFailure(ctx, candidate.Identity.RunID,
				"cancelling", "pending_cancel", cancelErr)
			return 0, 0, errors.Join(cancelErr, recordErr)
		default:
			if err := s.transition(ctx, candidate.Identity.RunID,
				"cancelling", "cancel_requested"); err != nil {
				return 0, 0, err
			}
			return 1, 0, nil
		}
	case "cancel_requested":
		if action.cancelRequestedAt != nil &&
			!candidate.DatabaseNow.Before(action.cancelRequestedAt.Add(timeout)) {
			terminated, err := s.beginTermination(
				ctx, candidate, reason, "cancel_requested", true)
			return 0, terminated, err
		}
		return 0, 0, nil
	case "terminating":
		if err := s.finishTermination(ctx, candidate, reason, true); err != nil {
			return 0, 0, err
		}
		return 0, 1, nil
	case "terminated":
		return 0, 0, nil
	default:
		return 0, 0, fmt.Errorf("invalid action state %q", action.state)
	}
}

func (s *StallSweeper) beginTermination(
	ctx context.Context,
	candidate StallActivity,
	reason string,
	from string,
	callEngine bool,
) (int, error) {
	if err := s.transition(
		ctx, candidate.Identity.RunID, from, "terminating"); err != nil {
		return 0, err
	}
	if err := s.finishTermination(ctx, candidate, reason, callEngine); err != nil {
		return 0, err
	}
	return 1, nil
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
	retryAt           *time.Time
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
			RETURNING state,cancel_requested_at,retry_at`,
			s.config.InstanceID, runID, reason, epoch,
		).Scan(&action.state, &action.cancelRequestedAt, &action.retryAt)
	})
	return action, err
}

func (s *StallSweeper) transition(
	ctx context.Context,
	runID string,
	from string,
	to string,
) error {
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.stall_action
			SET state=$4,
			    cancel_requested_at=CASE
			      WHEN $4='cancel_requested' THEN clock_timestamp()
			      ELSE cancel_requested_at END,
			    terminated_at=CASE
			      WHEN $4='terminated' THEN clock_timestamp()
			      ELSE terminated_at END,
			    last_error='',retry_at=NULL,owner_epoch=$3
			WHERE instance_id=$1 AND run_id=$2
			  AND state=$5 AND owner_epoch=$3`,
			s.config.InstanceID, runID, epoch, to, from,
		)
		return changed(tag.RowsAffected(), err)
	})
}

func (s *StallSweeper) recordFailure(
	ctx context.Context,
	runID string,
	from string,
	to string,
	failure error,
) error {
	return s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.stall_action
			SET state=$4,last_error=left($6,1024),
			    retry_at=clock_timestamp()+interval '30 seconds',owner_epoch=$3
			WHERE instance_id=$1 AND run_id=$2
			  AND state=$5 AND owner_epoch=$3`,
			s.config.InstanceID, runID, epoch, to, from, failure.Error(),
		)
		return changed(tag.RowsAffected(), err)
	})
}

func (s *StallSweeper) finishTermination(
	ctx context.Context,
	candidate StallActivity,
	reason string,
	callEngine bool,
) error {
	if callEngine {
		if err := s.config.Engine.Terminate(
			ctx, candidate.Identity.RunID, reason); err != nil &&
			!errors.Is(err, ErrEngineClosed) {
			recordErr := s.recordFailure(ctx, candidate.Identity.RunID,
				"terminating", "terminating", err)
			return errors.Join(err, recordErr)
		}
	}
	if err := s.config.Terminalizer.Terminalize(
		ctx, candidate.Identity, reason); err != nil {
		recordErr := s.recordFailure(ctx, candidate.Identity.RunID,
			"terminating", "terminating", err)
		return errors.Join(err, recordErr)
	}
	return s.transition(
		ctx, candidate.Identity.RunID, "terminating", "terminated")
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
