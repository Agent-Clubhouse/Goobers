package readmodel

import (
	"context"
	"fmt"
	"time"
)

// CalibrationSnapshot is the journal-derived input for offline projections.
// It intentionally contains observations only; it is not an authoritative
// aggregate and can always be rebuilt from the projected runs.
type CalibrationSnapshot struct {
	WindowStart time.Time
	WindowEnd   time.Time
	Runs        int
	MinSamples  int
	Outcomes    map[string]int
	Gates       map[string]map[string]int
	Nodes       map[string]NodeCalibrationObservation
}

type NodeCalibrationObservation struct {
	Samples    int
	Successes  int
	Durations  []time.Duration
	RetryWaste []float64
	Costs      []float64
}

// HarvestCalibration returns bounded stage observations from the read model.
// Cost remains owned by telemetry and is not fabricated when that projection
// is unavailable.
func (s *Store) HarvestCalibration(ctx context.Context, since, until time.Time, minSamples int) (CalibrationSnapshot, error) {
	db, release, err := s.readHandle()
	if err != nil {
		return CalibrationSnapshot{}, err
	}
	defer release()

	snapshot := CalibrationSnapshot{
		WindowStart: since.UTC(),
		WindowEnd:   until.UTC(),
		MinSamples:  minSamples,
		Outcomes:    make(map[string]int),
		Gates:       make(map[string]map[string]int),
		Nodes:       make(map[string]NodeCalibrationObservation),
	}
	var args []any
	where := ""
	if !since.IsZero() {
		where += " WHERE r.started_at >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if !until.IsZero() {
		if where == "" {
			where = " WHERE r.started_at <= ?"
		} else {
			where += " AND r.started_at <= ?"
		}
		args = append(args, until.UTC().Format(time.RFC3339Nano))
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM run r"+where, args...).Scan(&snapshot.Runs); err != nil {
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: count calibration runs: %w", err)
	}
	outcomeRows, err := db.QueryContext(ctx, "SELECT COALESCE(outcome_verdict, ''), COUNT(*) FROM run r"+where+" GROUP BY outcome_verdict", args...)
	if err != nil {
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: query calibration outcomes: %w", err)
	}
	for outcomeRows.Next() {
		var outcome string
		var count int
		if err := outcomeRows.Scan(&outcome, &count); err != nil {
			_ = outcomeRows.Close()
			return CalibrationSnapshot{}, fmt.Errorf("readmodel: scan calibration outcome: %w", err)
		}
		if outcome != "" {
			snapshot.Outcomes[outcome] = count
		}
	}
	if err := outcomeRows.Err(); err != nil {
		_ = outcomeRows.Close()
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: calibration outcomes: %w", err)
	}
	_ = outcomeRows.Close()

	gateRows, err := db.QueryContext(ctx, "SELECT name, arm, COUNT(*) FROM run_node n JOIN run r ON r.run_id = n.run_id WHERE n.kind = 'gate' AND n.arm <> ''"+whereForNode(where)+" GROUP BY name, arm", args...)
	if err != nil {
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: query calibration gates: %w", err)
	}
	for gateRows.Next() {
		var gate, outcome string
		var count int
		if err := gateRows.Scan(&gate, &outcome, &count); err != nil {
			_ = gateRows.Close()
			return CalibrationSnapshot{}, fmt.Errorf("readmodel: scan calibration gate: %w", err)
		}
		if snapshot.Gates[gate] == nil {
			snapshot.Gates[gate] = make(map[string]int)
		}
		snapshot.Gates[gate][outcome] = count
	}
	if err := gateRows.Err(); err != nil {
		_ = gateRows.Close()
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: calibration gates: %w", err)
	}
	_ = gateRows.Close()

	rows, err := db.QueryContext(ctx, `SELECT s.stage, s.attempts, s.had_success,
		s.started_at, s.finished_at, s.retry_waste, s.cost_measured
		FROM run_stage s JOIN run r ON r.run_id = s.run_id`+where, args...)
	if err != nil {
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: query calibration stages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var samples, successes, waste, costMeasured int
		var started, finished *string
		if err := rows.Scan(&name, &samples, &successes, &started, &finished, &waste, &costMeasured); err != nil {
			return CalibrationSnapshot{}, fmt.Errorf("readmodel: scan calibration stage: %w", err)
		}
		observation := snapshot.Nodes[name]
		observation.Samples += samples
		if successes != 0 {
			observation.Successes++
		}
		if started != nil && finished != nil {
			start, err := time.Parse(time.RFC3339Nano, *started)
			if err != nil {
				return CalibrationSnapshot{}, fmt.Errorf("readmodel: parse calibration start: %w", err)
			}
			end, err := time.Parse(time.RFC3339Nano, *finished)
			if err != nil {
				return CalibrationSnapshot{}, fmt.Errorf("readmodel: parse calibration finish: %w", err)
			}
			if end.After(start) {
				observation.Durations = append(observation.Durations, end.Sub(start))
			}
		}
		if waste != 0 {
			observation.RetryWaste = append(observation.RetryWaste, float64(waste))
		}
		observation.Costs = append(observation.Costs, float64(costMeasured))
		snapshot.Nodes[name] = observation
	}
	if err := rows.Err(); err != nil {
		return CalibrationSnapshot{}, fmt.Errorf("readmodel: calibration stages: %w", err)
	}
	return snapshot, nil
}

func whereForNode(runWhere string) string {
	if runWhere == "" {
		return ""
	}
	return " AND " + runWhere[6:]
}
