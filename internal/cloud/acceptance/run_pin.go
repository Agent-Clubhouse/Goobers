package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	recorded, err := scanRunPins(tx.QueryRow(ctx, runPinsQuery, s.instanceID, pins.RunID))
	if err != nil {
		return err
	}
	if recorded != pins {
		return ErrRunPinsDiffer
	}
	return nil
}

const runPinsQuery = `
	SELECT run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest, goober_digest, config_generation,
	  repo_ref, branch_namespace, input_digest
	FROM control.run_pin WHERE instance_id=$1 AND run_id=$2`

func scanRunPins(row pgx.Row) (RunPins, error) {
	var pins RunPins
	var repo []byte
	if err := row.Scan(&pins.RunID, &pins.AcceptanceID, &pins.Gaggle, &pins.Workflow, &pins.WorkflowVersion, &pins.WorkflowDigest,
		&pins.GooberDigest, &pins.ConfigGeneration, &repo, &pins.BranchNamespace, &pins.InputDigest); err != nil {
		return RunPins{}, noRows(err)
	}
	if err := json.Unmarshal(repo, &pins.RepoRef); err != nil {
		return RunPins{}, err
	}
	return pins, nil
}

// RunPinsOf reads the pins the start fence recorded for runID. A run with no
// row (never started by this instance's start fence, or started before the
// pins existed) returns sql.ErrNoRows. Rows are immutable apart from
// terminal_at, so the read is unfenced.
func (s *Store) RunPinsOf(ctx context.Context, runID string) (RunPins, error) {
	return scanRunPins(s.pool.QueryRow(ctx, runPinsQuery, s.instanceID, runID))
}

// runPinSweepBatch bounds one retention pass.
const runPinSweepBatch = 64

// ErrRunStatusUnavailable means the engine could not answer for any run (it is
// unreachable or the pass ran out of time), as opposed to one run's status
// being unconfirmable. RunClosed wraps it; the sweep stops the pass on it.
var ErrRunStatusUnavailable = errors.New("cloud acceptance: run status unavailable")

// RunClosed reports whether a run's engine execution is closed. A run the
// engine no longer knows (its history retention-deleted) is closed.
type RunClosed func(ctx context.Context, runID string) (bool, error)

// RunPinSweep is one retention pass's outcome. Next is the cursor for the
// following pass; it is empty once a lap over the candidates completes.
// Checked lists the runs whose status was confirmed this pass. Unconfirmed
// holds the runs whose status could not be confirmed, and Halted why the pass
// stopped early, at run HaltedAt; their pins are kept, and both are the
// caller's to report, not to fail on.
type RunPinSweep struct {
	Marked, Deleted int64
	Next            string
	Checked         []string
	Unconfirmed     map[string]error
	Halted          error
	HaltedAt        string
}

// SweepRunPins applies run_pin's terminal-keyed retention: pins of runs that
// are older than ReplayRetention and now closed are marked terminal, and pins
// terminal for longer than ReplayRetention are deleted. A running parent's
// pins are never touched, however old. Candidates are visited in run_id order
// after the cursor, so long-running parents cannot fill every batch and starve
// the closed runs behind them. A run whose status cannot be confirmed keeps its
// pin and is listed in Unconfirmed; the rest of the batch is still applied, and
// the returned error is only the store's own (including a lost fence). The
// closed predicate runs outside any transaction, within budget for the whole
// pass; at ErrRunStatusUnavailable or the end of the budget the pass halts and
// the cursor stays before the run it stopped at. Only the marking and deletion
// are fenced, on ctx.
func (s *Store) SweepRunPins(ctx context.Context, closed RunClosed, after string, budget time.Duration) (RunPinSweep, error) {
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
	sweep := RunPinSweep{Unconfirmed: map[string]error{}}
	if len(candidates) == runPinSweepBatch {
		sweep.Next = candidates[len(candidates)-1]
	}
	statusCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var closedRuns []string
	for i, runID := range candidates {
		done, err := closed(statusCtx, runID)
		if err != nil && (errors.Is(err, ErrRunStatusUnavailable) || statusCtx.Err() != nil) {
			sweep.Halted = fmt.Errorf("run pin sweep: halted at run %s: %w", runID, err)
			sweep.HaltedAt = runID
			sweep.Next = after
			if i > 0 {
				sweep.Next = candidates[i-1]
			}
			break
		}
		switch {
		case err != nil:
			sweep.Unconfirmed[runID] = fmt.Errorf("run pin sweep: status of run %s: %w", runID, err)
			continue
		case done:
			closedRuns = append(closedRuns, runID)
		}
		sweep.Checked = append(sweep.Checked, runID)
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
		return RunPinSweep{Next: after, Unconfirmed: sweep.Unconfirmed, Halted: sweep.Halted, HaltedAt: sweep.HaltedAt}, err
	}
	return sweep, nil
}

// StartPending reports whether acceptanceID's start is still dispatching: the
// Temporal start may have happened, but the fenced confirmation that records
// the run's pins has not committed yet.
func (s *Store) StartPending(ctx context.Context, acceptanceID string) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `
		SELECT state='dispatching' FROM control.acceptance WHERE instance_id=$1 AND id=$2`,
		s.instanceID, acceptanceID).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return pending, err
}
