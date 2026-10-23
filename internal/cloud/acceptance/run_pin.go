package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrRunPinsDiffer means a start confirmation carried different pins than the
// ones already recorded for that run.
var ErrRunPinsDiffer = errors.New("cloud acceptance: run pins differ from the recorded start")

// RunPins are a started engine run's immutable identity as the start fence
// resolved it: the RunInput the Temporal workflow runs, identified by its
// start-owner input digest. Control-side checks compare the run identity the
// worker journals against these, never the other way round.
type RunPins struct {
	RunID, AcceptanceID          string
	Gaggle, Workflow             string
	WorkflowVersion              int
	WorkflowDigest, GooberDigest string
	ConfigGeneration             string
	RepoRef                      apiv1.RepoRef
	BranchNamespace, InputDigest string
}

// RunPinsFor derives the pins of the start in.
func RunPinsFor(in engine.RunInput) (RunPins, error) {
	digest, err := startfence.InputDigest(in)
	if err != nil {
		return RunPins{}, err
	}
	pins := RunPins{RunID: in.RunID, AcceptanceID: in.AcceptanceID, Gaggle: in.Gaggle, Workflow: in.WorkflowName, WorkflowVersion: in.Version,
		WorkflowDigest: in.WorkflowDigest, GooberDigest: in.GooberDigest, ConfigGeneration: in.ConfigGeneration, RepoRef: in.RepoRef,
		BranchNamespace: in.BranchNamespace, InputDigest: digest}
	// Only the start identity is required. Pins are evidence for later
	// control-side checks, never a reason to refuse a start: a consumer that
	// needs a pin refuses its own operation when that pin is empty.
	if pins.RunID == "" || pins.AcceptanceID == "" {
		return RunPins{}, fmt.Errorf("%w: start input has no run identity", triggerqueue.ErrTransition)
	}
	return pins, nil
}

// recordRunPins writes pins once, inside the fenced transaction that confirms
// or adopts the start. A repeated confirmation of the same start is a no-op;
// different pins for a recorded run refuse the confirmation.
func (s *Store) recordRunPins(ctx context.Context, tx pgx.Tx, pins RunPins, epoch int64) error {
	repo, err := json.Marshal(pins.RepoRef)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
		  goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (instance_id, run_id) DO NOTHING`,
		s.instanceID, pins.RunID, pins.AcceptanceID, pins.Gaggle, pins.Workflow, pins.WorkflowVersion, pins.WorkflowDigest,
		pins.GooberDigest, pins.ConfigGeneration, repo, pins.BranchNamespace, pins.InputDigest, epoch); err != nil {
		return err
	}
	var recorded RunPins
	var recordedRepo []byte
	err = tx.QueryRow(ctx, `
		SELECT run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest, goober_digest, config_generation,
		  repo_ref, branch_namespace, input_digest
		FROM control.run_pin WHERE instance_id=$1 AND run_id=$2`, s.instanceID, pins.RunID,
	).Scan(&recorded.RunID, &recorded.AcceptanceID, &recorded.Gaggle, &recorded.Workflow, &recorded.WorkflowVersion, &recorded.WorkflowDigest,
		&recorded.GooberDigest, &recorded.ConfigGeneration, &recordedRepo, &recorded.BranchNamespace, &recorded.InputDigest)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(recordedRepo, &recorded.RepoRef); err != nil {
		return err
	}
	if recorded != pins {
		return ErrRunPinsDiffer
	}
	return nil
}

// runPinSweepBatch bounds one retention pass.
const runPinSweepBatch = 64

// RunClosed reports whether a run's engine execution is closed. A run the
// engine no longer knows (its history retention-deleted) is closed.
type RunClosed func(ctx context.Context, runID string) (bool, error)

// RunPinSweep is one retention pass's outcome. Next is the cursor for the
// following pass; it is empty once a lap over the candidates completes.
// Unconfirmed holds the runs whose status could not be confirmed; their pins
// are kept, and the error is the caller's to report, not to fail on.
type RunPinSweep struct {
	Marked, Deleted int64
	Next            string
	Unconfirmed     []error
}

// SweepRunPins applies run_pin's terminal-keyed retention: pins of runs that
// are older than ReplayRetention and now closed are marked terminal, and pins
// terminal for longer than ReplayRetention are deleted. A running parent's
// pins are never touched, however old. Candidates are visited in run_id order
// after the cursor, so long-running parents cannot fill every batch and starve
// the closed runs behind them. A run whose status cannot be confirmed keeps its
// pin and is listed in Unconfirmed; the rest of the batch is still applied, and
// the returned error is only the store's own (including a lost fence). The
// closed predicate runs outside any transaction; only the marking and deletion
// are fenced.
func (s *Store) SweepRunPins(ctx context.Context, closed RunClosed, after string) (RunPinSweep, error) {
	if closed == nil {
		return RunPinSweep{Next: after}, errors.New("cloud acceptance: run pin sweep requires a run status")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT run_id FROM control.run_pin
		WHERE instance_id=$1 AND terminal_at IS NULL AND created_at<clock_timestamp()-$2*interval '1 second' AND run_id>$3
		ORDER BY run_id LIMIT $4`,
		s.instanceID, triggerqueue.ReplayRetention.Seconds(), after, runPinSweepBatch)
	if err != nil {
		return RunPinSweep{Next: after}, err
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return RunPinSweep{Next: after}, err
	}
	sweep := RunPinSweep{}
	if len(candidates) == runPinSweepBatch {
		sweep.Next = candidates[len(candidates)-1]
	}
	var closedRuns []string
	for _, runID := range candidates {
		done, err := closed(ctx, runID)
		switch {
		case err != nil:
			sweep.Unconfirmed = append(sweep.Unconfirmed, fmt.Errorf("run pin sweep: status of run %s: %w", runID, err))
		case done:
			closedRuns = append(closedRuns, runID)
		}
	}
	err = s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, _ int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.run_pin SET terminal_at=clock_timestamp()
			WHERE instance_id=$1 AND run_id=ANY($2) AND terminal_at IS NULL`, s.instanceID, closedRuns)
		if err != nil {
			return err
		}
		sweep.Marked = tag.RowsAffected()
		tag, err = tx.Exec(txCtx, `
			DELETE FROM control.run_pin
			WHERE instance_id=$1 AND terminal_at<clock_timestamp()-$2*interval '1 second'`,
			s.instanceID, triggerqueue.ReplayRetention.Seconds())
		sweep.Deleted = tag.RowsAffected()
		return err
	})
	if err != nil {
		return RunPinSweep{Next: after, Unconfirmed: sweep.Unconfirmed}, err
	}
	return sweep, nil
}
