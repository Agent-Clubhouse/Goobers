// Package stallsweep is the cloud (PostgreSQL, lease-fenced) stall and
// maximum-run-duration sweep. It lives under internal/cloud so the local
// runtime never reaches pgx or the cloud lease.
package stallsweep

import (
	"context"
	"encoding/json"
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

// maxEventClockSkew bounds how far a writer's event clock may sit outside the
// database's [startedAt, now] window before max-duration accounting refuses the
// run's history. It applies only to histories with events stored before the
// journal recorded its own commit clock (EventRecord.RecordedAt).
const maxEventClockSkew = 5 * time.Minute

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
	Pool       *pgxpool.Pool
	InstanceID string
	Manager    *cloudlease.Manager
	Handle     *cloudlease.Handle
	Source     StallSource
	// Events reads one run's journal records. It is called only for candidates
	// whose timing already makes them due, so the read is bounded by the page.
	// A record's RecordedAt (the journal's database commit clock), when every
	// record has one, replaces the writer clock in duration accounting.
	Events func(ctx context.Context, runID string) ([]journal.EventRecord, error)
	// Report receives non-fatal findings (a run history the sweep refuses to
	// credit); nil drops them.
	Report         func(error)
	Engine         EngineControl
	Terminalizer   RunTerminalizer
	StalledTimeout time.Duration
	MaxRunDuration time.Duration
	BatchSize      int
}

func (s *StallSweeper) report(err error) {
	if s.config.Report != nil {
		s.config.Report(err)
	}
}

// StallSweeper applies cloud stall and maximum-duration policy.
type StallSweeper struct{ config StallConfig }

// NewStallSweeper validates and constructs a cloud sweep.
func NewStallSweeper(config StallConfig) (*StallSweeper, error) {
	if config.Pool == nil || config.InstanceID == "" ||
		config.Manager == nil || config.Handle == nil ||
		config.Handle.InstanceID != config.InstanceID ||
		config.Source == nil || config.Events == nil || config.Engine == nil || config.Terminalizer == nil ||
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

// Result reports external actions admitted by this pass.
type Result struct {
	Cancelled  int
	Terminated int
}

// Sweep processes a bounded stable page. Controller downtime is deliberately
// absent: cloud stages continue on workers while the controller is unavailable.
func (s *StallSweeper) Sweep(ctx context.Context) (Result, error) {
	var result Result
	var after string
	var sweepErrs []error
	retried, retryErrs := s.retryTerminating(ctx)
	result.Terminated += retried
	sweepErrs = append(sweepErrs, retryErrs...)
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

func (s *StallSweeper) retryTerminating(
	ctx context.Context,
) (int, []error) {
	rows, err := s.config.Pool.Query(ctx, `
		SELECT run_id,identity,reason
		FROM control.stall_action
		WHERE instance_id=$1 AND state='terminating'
		  AND (retry_at IS NULL OR retry_at<=clock_timestamp())
		ORDER BY COALESCE(retry_at,detected_at),run_id
		LIMIT $2`,
		s.config.InstanceID, s.config.BatchSize,
	)
	if err != nil {
		return 0, []error{err}
	}
	type pending struct {
		runID, reason string
		identity      journal.RunIdentity
	}
	var actions []pending
	for rows.Next() {
		var action pending
		var raw []byte
		if err := rows.Scan(&action.runID, &raw, &action.reason); err != nil {
			rows.Close()
			return 0, []error{err}
		}
		if err := json.Unmarshal(raw, &action.identity); err != nil {
			rows.Close()
			return 0, []error{err}
		}
		actions = append(actions, action)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, []error{err}
	}
	var (
		completed int
		errs      []error
	)
	for _, action := range actions {
		claimed, err := s.claimTerminating(ctx, action.runID)
		if err != nil {
			errs = append(errs, fmt.Errorf("retry claim %s: %w", action.runID, err))
			continue
		}
		if !claimed {
			continue
		}
		candidate := StallActivity{Identity: action.identity}
		if err := s.finishTermination(
			ctx, candidate, action.reason, true); err != nil {
			errs = append(errs, fmt.Errorf("retry termination %s: %w", action.runID, err))
			continue
		}
		completed++
	}
	return completed, errs
}

func (s *StallSweeper) claimTerminating(
	ctx context.Context,
	runID string,
) (bool, error) {
	claimed := false
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		tag, err := tx.Exec(txCtx, `
			UPDATE control.stall_action
			SET owner_epoch=$3,retry_at=NULL
			WHERE instance_id=$1 AND run_id=$2
			  AND state='terminating'
			  AND (retry_at IS NULL OR retry_at<=clock_timestamp())
			  AND owner_epoch<=$3`,
			s.config.InstanceID, runID, epoch,
		)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	return claimed, err
}

func (s *StallSweeper) processCandidate(
	ctx context.Context,
	candidate StallActivity,
) (cancelled int, terminated int, err error) {
	reason, timeout, due, err := s.due(ctx, candidate)
	if err != nil || !due {
		return 0, 0, err
	}
	action, err := s.claim(ctx, candidate.Identity, reason)
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
	ctx context.Context,
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
	// Cheap timing screen first: only runs that could be due have their events
	// read, so a page of healthy runs costs no journal reads.
	pastDuration := maxDuration > 0 &&
		!candidate.DatabaseNow.Before(candidate.Identity.StartedAt.Add(maxDuration))
	pastStall := !candidate.DatabaseNow.Before(candidate.LastAppendAt.Add(stallTimeout))
	if !pastDuration && !pastStall {
		return "", stallTimeout, false, nil
	}
	records, err := s.config.Events(ctx, candidate.Identity.RunID)
	if err != nil {
		return "", 0, false, fmt.Errorf("read run events: %w", err)
	}
	events := make([]journal.Event, len(records))
	for i, record := range records {
		events[i] = record.Event
	}
	if pastDuration && s.creditedElapsed(records, events, candidate) > maxDuration {
		return "max_duration", stallTimeout, true, nil
	}
	if !pastStall {
		return "", stallTimeout, false, nil
	}
	// A run parked at a human gate or on a child is waiting, not stalled; the
	// local sweep's one exemption (journal.ParkedAtGate / ParkedOnChild). It
	// holds even when something appended after the pause.
	if journal.ParkedAtGate(events) || journal.ParkedOnChild(events) {
		return "", stallTimeout, false, nil
	}
	return "stalled", stallTimeout, true, nil
}

// creditedElapsed is the run's age less its durable child waits (design 10.3,
// the same journal.ChildExecutionElapsed the local sweep uses), all on the
// database clock. When every record carries the journal's commit clock the
// waits are measured on it and writer skew is irrelevant. Across a database
// failover those stamps come from a different primary's clock and may step
// slightly backwards; if ChildExecutionElapsed rejects the resulting history,
// the waits get no credit and the run is measured by its raw age, which can
// only end it sooner, never extend it. Otherwise (events
// stored before the journal recorded it) the writer clocks are used inside a
// window widened by at most maxEventClockSkew, failing CLOSED: an event
// stamped further outside it, or an unreadable wait history, gives the child
// waits no credit and the run is measured by its raw database-clock age.
func (s *StallSweeper) creditedElapsed(records []journal.EventRecord, events []journal.Event, candidate StallActivity) time.Duration {
	startedAt, now := candidate.Identity.StartedAt, candidate.DatabaseNow
	elapsed := now.Sub(startedAt)
	if recorded, ok := recordedClockEvents(records); ok {
		credited, err := journal.ChildExecutionElapsed(recorded, startedAt, now, nil)
		if err != nil {
			s.report(fmt.Errorf("read run execution clock: %w; child waits get no credit", err))
			return elapsed
		}
		return credited
	}
	if len(events) > 0 {
		if first := events[0].Time; first.Before(startedAt) {
			if startedAt.Sub(first) > maxEventClockSkew {
				s.report(fmt.Errorf("run event clock is %s before the run start; child waits get no credit", startedAt.Sub(first).Round(time.Second)))
				return elapsed
			}
			startedAt = first
		}
		if last := events[len(events)-1].Time; last.After(now) {
			if last.Sub(now) > maxEventClockSkew {
				s.report(fmt.Errorf("run event clock is %s ahead of the database; child waits get no credit", last.Sub(now).Round(time.Second)))
				return elapsed
			}
			now = last
		}
	}
	credited, err := journal.ChildExecutionElapsed(events, startedAt, now, nil)
	if err != nil {
		s.report(fmt.Errorf("read run execution clock: %w; child waits get no credit", err))
		return elapsed
	}
	return credited
}

// recordedClockEvents returns the events timed by the journal's commit clock,
// or false when any record lacks it: one history is never measured on a mix
// of database and writer clocks.
func recordedClockEvents(records []journal.EventRecord) ([]journal.Event, bool) {
	if len(records) == 0 {
		return nil, false
	}
	events := make([]journal.Event, len(records))
	for i, record := range records {
		if record.RecordedAt.IsZero() {
			return nil, false
		}
		events[i] = record.Event
		events[i].Time = record.RecordedAt
	}
	return events, true
}

type stallAction struct {
	state             string
	cancelRequestedAt *time.Time
	retryAt           *time.Time
}

func (s *StallSweeper) claim(
	ctx context.Context,
	identity journal.RunIdentity,
	reason string,
) (stallAction, error) {
	var action stallAction
	rawIdentity, err := json.Marshal(identity)
	if err != nil {
		return action, err
	}
	err = s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		return tx.QueryRow(txCtx, `
			INSERT INTO control.stall_action (
				instance_id,run_id,identity,reason,state,detected_at,owner_epoch
			) VALUES ($1,$2,$3,$4,'pending_cancel',clock_timestamp(),$5)
			ON CONFLICT (instance_id,run_id) DO UPDATE
			SET owner_epoch=excluded.owner_epoch,
			    identity=excluded.identity,
			    reason=CASE
			      WHEN control.stall_action.state='pending_cancel'
			      THEN excluded.reason ELSE control.stall_action.reason END
			WHERE control.stall_action.instance_id=$1
			  AND control.stall_action.owner_epoch<=$5
			RETURNING state,cancel_requested_at,retry_at`,
			s.config.InstanceID, identity.RunID, rawIdentity, reason, epoch,
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
