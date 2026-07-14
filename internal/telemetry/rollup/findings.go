package rollup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FindingKind classifies a candidate finding's detection family (TUT-010).
// These names anchor the shape issue #148 will formalize as a versioned
// JSON schema under api/schemas/ — T2 owns the detection logic and this
// Go-level shape, giving #148 a stable interface to build the
// `telemetry-query`/candidate-findings connector around, rather than each
// landing in lockstep.
type FindingKind string

const (
	// FindingStageFailureRate flags a stage whose failure rate meets or
	// exceeds Thresholds.MaxFailureRate over at least Thresholds.MinSamples
	// attempts (TUT-010 failure-patterns family).
	FindingStageFailureRate FindingKind = "stage-failure-rate"
	// FindingErrorSignature flags an (code, error_class) pattern recurring
	// at least Thresholds.MinErrorSignatureCount times across runs
	// (TUT-010 failure-patterns family — error-code clustering).
	FindingErrorSignature FindingKind = "error-signature"
	// FindingGateNeverFails flags a gate that has evaluated at least
	// Thresholds.MinGateEvaluations times and never once returned a
	// non-pass verdict — a gate providing no signal (TUT-010 gate-noise
	// family).
	FindingGateNeverFails FindingKind = "gate-never-fails"
	// FindingGateRepassChurn flags a gate whose escalation rate (repass
	// budget exhausted, #89) meets or exceeds
	// Thresholds.MaxGateEscalationRate — the reviewer keeps repeating a
	// non-pass verdict without the run ever resolving cleanly (TUT-010
	// gate-noise family — reviewer-verdict repetition).
	FindingGateRepassChurn FindingKind = "gate-repass-churn"
	// FindingWorkflowUntriggered flags a workflow named in a
	// CoverageRequest with zero runs in the request's window (TUT-010
	// coverage-gaps family).
	FindingWorkflowUntriggered FindingKind = "workflow-untriggered"
	// FindingStageUnreached flags a (workflow, stage) pair named in a
	// CoverageRequest with zero stage attempts in the request's window
	// (TUT-010 coverage-gaps family).
	FindingStageUnreached FindingKind = "stage-unreached"
)

// JournalPointer names a flagged run whose journal a diagnosis step can
// resolve for evidence. T2 only needs to name the run; T3 builds the
// agentic read surface that actually resolves it (cross-run journal:read,
// #121-extended).
type JournalPointer struct {
	RunID string `json:"runId"`
}

// Finding is one detected candidate — Subject names what was flagged (a
// stage, gate, or workflow name, or an error code), Metrics carries the
// raw numbers a diagnosis step or a human reviewer needs to judge the
// finding, and FlaggedRuns are example runs exhibiting it (bounded, newest
// first; empty for a coverage gap, since the whole point is that no run
// exists to point at).
type Finding struct {
	Kind        FindingKind        `json:"kind"`
	Subject     string             `json:"subject"`
	Metrics     map[string]float64 `json:"metrics"`
	Threshold   float64            `json:"threshold"`
	FlaggedRuns []JournalPointer   `json:"flaggedRuns"`
}

// Thresholds are the config-tunable detection knobs a Tutor goober
// definition sets (OQ-2); DefaultThresholds gives sane defaults for a
// caller that doesn't override them. Every rate is a fraction in [0, 1].
type Thresholds struct {
	// MinSamples is the minimum attempt/run count before a failure-rate
	// finding is trustworthy — avoids flagging a stage on a single failed
	// attempt. Default 5.
	MinSamples int
	// MaxFailureRate flags a stage whose failure rate (failed / (succeeded
	// + failed)) is at or above this fraction. Default 0.3.
	MaxFailureRate float64
	// MinErrorSignatureCount flags a recurring (code, error_class) pattern
	// occurring at least this many times. Default 5.
	MinErrorSignatureCount int
	// MinGateEvaluations is the minimum evaluation count before a gate-noise
	// finding (never-fails or repass-churn) is trustworthy. Default 5.
	MinGateEvaluations int
	// MaxGateEscalationRate flags a gate whose escalation rate (escalated /
	// total evaluations) is at or above this fraction. Default 0.2.
	MaxGateEscalationRate float64
	// MaxFlaggedRuns bounds how many example runs each finding carries.
	// Default 10.
	MaxFlaggedRuns int
}

// DefaultThresholds returns the sane-defaults Thresholds a Tutor goober
// definition can override selectively (OQ-2).
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinSamples:             5,
		MaxFailureRate:         0.3,
		MinErrorSignatureCount: 5,
		MinGateEvaluations:     5,
		MaxGateEscalationRate:  0.2,
		MaxFlaggedRuns:         10,
	}
}

// CoverageRequest names the workflow/stage universe a coverage-gap
// detection pass checks telemetry against — data the rollup cannot derive
// on its own (it has no view into workflow definitions, only what ran), so
// the caller (the tutor connector stage, once #148 wires the CLI around
// this) supplies it, typically from the compiled workflow registry.
// Workflows maps a workflow name to its expected stage names; a workflow
// with no stages listed is checked for triggering only, not stage reach.
type CoverageRequest struct {
	StatsRequest
	Workflows map[string][]string
}

// DetectRequest bundles the window/filter Detect's aggregate queries share
// with the coverage-gap universe and the thresholds each family checks
// against.
type DetectRequest struct {
	StatsRequest
	Coverage   CoverageRequest
	Thresholds Thresholds
}

// Detect runs every TUT-010 detection family Detect supports — failure
// patterns (stage failure rate, error-code clustering), gate noise
// (never-fails, repass churn), and coverage gaps (untriggered workflows,
// unreached stages) — against req's window/filter and Thresholds, and
// returns candidate Findings sorted by (Kind, Subject, Stage) for a
// deterministic result given a fixed telemetry.db snapshot. The waste
// family (duration/token/cost percentiles, retry waste) is deferred to
// #144 (usage accounting), which this package does not yet have data for.
func (db *DB) Detect(req DetectRequest) ([]Finding, error) {
	th := req.Thresholds
	if th == (Thresholds{}) {
		th = DefaultThresholds()
	}
	if th.MaxFlaggedRuns <= 0 {
		th.MaxFlaggedRuns = 10
	}

	var findings []Finding

	stageFindings, err := db.detectStageFailureRate(req.StatsRequest, th)
	if err != nil {
		return nil, err
	}
	findings = append(findings, stageFindings...)

	errFindings, err := db.detectErrorSignatures(req.StatsRequest, th)
	if err != nil {
		return nil, err
	}
	findings = append(findings, errFindings...)

	gateFindings, err := db.detectGateNoise(req.StatsRequest, th)
	if err != nil {
		return nil, err
	}
	findings = append(findings, gateFindings...)

	if len(req.Coverage.Workflows) > 0 {
		coverageFindings, err := db.detectCoverageGaps(req.Coverage)
		if err != nil {
			return nil, err
		}
		findings = append(findings, coverageFindings...)
	}

	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Subject < b.Subject
	})
	return findings, nil
}

// detectStageFailureRate flags stages whose failure rate meets or exceeds
// th.MaxFailureRate over at least th.MinSamples terminal (success/failure)
// attempts — reuses stageStats, the same aggregate `goobers telemetry
// stats` already exposes, so this adds threshold-based flagging on top
// rather than a second query engine.
func (db *DB) detectStageFailureRate(req StatsRequest, th Thresholds) ([]Finding, error) {
	stages, err := db.stageStats(req)
	if err != nil {
		return nil, fmt.Errorf("rollup: detect stage failure rate: %w", err)
	}
	var out []Finding
	for _, s := range stages {
		terminal := s.SucceededAttempts + s.FailedAttempts
		if terminal < th.MinSamples {
			continue
		}
		failureRate := 1 - s.SuccessRate
		if failureRate < th.MaxFailureRate {
			continue
		}
		runs, err := db.stageFailingRuns(req, s.Stage, th.MaxFlaggedRuns)
		if err != nil {
			return nil, err
		}
		out = append(out, Finding{
			Kind:    FindingStageFailureRate,
			Subject: s.Stage,
			Metrics: map[string]float64{
				"failureRate":    failureRate,
				"totalAttempts":  float64(terminal),
				"failedAttempts": float64(s.FailedAttempts),
			},
			Threshold:   th.MaxFailureRate,
			FlaggedRuns: runs,
		})
	}
	return out, nil
}

// stageFailingRuns returns the runs (newest first, bounded by limit) whose
// stage attempt for the named stage failed, matching req's filters.
func (db *DB) stageFailingRuns(req StatsRequest, stage string, limit int) ([]JournalPointer, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "r.started_at", req)
	clause := "sa.stage = ? AND sa.status = ?"
	if where != "" {
		clause = strings.TrimPrefix(where, "WHERE ") + " AND " + clause
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT sa.run_id, r.started_at FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		WHERE %s
		ORDER BY r.started_at DESC, sa.run_id DESC
		LIMIT ?`, clause)
	args = append(append([]any{}, args...), stage, stageStatusFailure, limit)
	return queryRunIDs(db, query, args)
}

// detectErrorSignatures flags recurring (code, error_class) patterns
// occurring at least th.MinErrorSignatureCount times — reuses
// TopErrorSignatures, adding threshold-based flagging and a bounded list
// of contributing runs (TopErrorSignatures itself only names one example).
func (db *DB) detectErrorSignatures(req StatsRequest, th Thresholds) ([]Finding, error) {
	sigs, err := db.TopErrorSignatures(req, 0)
	if err != nil {
		return nil, fmt.Errorf("rollup: detect error signatures: %w", err)
	}
	var out []Finding
	for _, sig := range sigs {
		if sig.Count < th.MinErrorSignatureCount {
			continue
		}
		runs, err := db.errorSignatureRuns(req, sig.Code, sig.ErrorClass, th.MaxFlaggedRuns)
		if err != nil {
			return nil, err
		}
		out = append(out, Finding{
			Kind:    FindingErrorSignature,
			Subject: sig.Code,
			Metrics: map[string]float64{
				"count": float64(sig.Count),
			},
			Threshold:   float64(th.MinErrorSignatureCount),
			FlaggedRuns: runs,
		})
	}
	return out, nil
}

func (db *DB) errorSignatureRuns(req StatsRequest, code, errorClass string, limit int) ([]JournalPointer, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "e.occurred_at", req)
	clause := "e.code = ? AND e.error_class = ?"
	if where != "" {
		clause = strings.TrimPrefix(where, "WHERE ") + " AND " + clause
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT e.run_id, e.occurred_at FROM run_errors e
		JOIN runs r ON r.run_id = e.run_id
		WHERE %s
		ORDER BY e.occurred_at DESC, e.run_id DESC
		LIMIT ?`, clause)
	args = append(append([]any{}, args...), code, errorClass, limit)
	return queryRunIDs(db, query, args)
}

// gateOutcomePass mirrors internal/gate.OutcomePass's wire value ("pass") —
// not imported, same decoupling rationale as mirror.go: this package reads
// journal-derived rollup text, it does not depend on the gate package.
const gateOutcomePass = "pass"

// gateAggregate is one gate's evaluation counts, accumulated in Go from raw
// gate_verdicts rows since escalated/repassAttempt live inside runner_json
// text, not a dedicated column — matching the package's existing convention
// of parsing runner_json only when a caller needs its structured fields
// (GateVerdict's own doc comment).
type gateAggregate struct {
	total, nonPass, escalated int
}

// detectGateNoise flags gates that never fail (FindingGateNeverFails) and
// gates whose escalation rate is high (FindingGateRepassChurn) — both
// require the per-gate evaluation counts gate_verdicts' runner_json carries
// (repassAttempt/escalated, #128), so this parses every matching row rather
// than aggregating in SQL (kept portable across the pure-Go sqlite driver,
// no JSON1 dependency).
func (db *DB) detectGateNoise(req StatsRequest, th Thresholds) ([]Finding, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "g.occurred_at", req)
	query := fmt.Sprintf(`
		SELECT g.gate, g.verdict, g.runner_json FROM gate_verdicts g
		JOIN runs r ON r.run_id = g.run_id
		%s`, where)
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: detect gate noise: %w", err)
	}
	defer func() { _ = rows.Close() }()

	agg := map[string]*gateAggregate{}
	var order []string
	for rows.Next() {
		var gate string
		var verdict, runnerJSON sql.NullString
		if err := rows.Scan(&gate, &verdict, &runnerJSON); err != nil {
			return nil, fmt.Errorf("rollup: scan gate_verdict: %w", err)
		}
		a, ok := agg[gate]
		if !ok {
			a = &gateAggregate{}
			agg[gate] = a
			order = append(order, gate)
		}
		a.total++
		if verdict.String != gateOutcomePass {
			a.nonPass++
		}
		if runnerJSON.Valid && gateEscalated(runnerJSON.String) {
			a.escalated++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Finding
	for _, gate := range order {
		a := agg[gate]
		if a.total < th.MinGateEvaluations {
			continue
		}
		if a.nonPass == 0 {
			runs, err := db.gateRuns(req, gate, "", th.MaxFlaggedRuns)
			if err != nil {
				return nil, err
			}
			out = append(out, Finding{
				Kind:    FindingGateNeverFails,
				Subject: gate,
				Metrics: map[string]float64{
					"totalEvaluations": float64(a.total),
				},
				Threshold:   float64(th.MinGateEvaluations),
				FlaggedRuns: runs,
			})
		}
		escalationRate := float64(a.escalated) / float64(a.total)
		if escalationRate >= th.MaxGateEscalationRate {
			runs, err := db.gateRuns(req, gate, "escalated", th.MaxFlaggedRuns)
			if err != nil {
				return nil, err
			}
			out = append(out, Finding{
				Kind:    FindingGateRepassChurn,
				Subject: gate,
				Metrics: map[string]float64{
					"escalationRate":   escalationRate,
					"totalEvaluations": float64(a.total),
					"escalatedCount":   float64(a.escalated),
				},
				Threshold:   th.MaxGateEscalationRate,
				FlaggedRuns: runs,
			})
		}
	}
	return out, nil
}

// gateEscalated reports whether a gate_verdicts.runner_json blob carries
// "escalated":true (#89's repass-budget-exhausted signal).
func gateEscalated(runnerJSON string) bool {
	var m struct {
		Escalated bool `json:"escalated"`
	}
	if err := json.Unmarshal([]byte(runnerJSON), &m); err != nil {
		return false
	}
	return m.Escalated
}

// gateRuns returns the runs (newest first, bounded by limit) where gate
// evaluated, optionally filtered to only escalated evaluations (mode ==
// "escalated"); mode == "" returns every evaluating run.
func (db *DB) gateRuns(req StatsRequest, gate, mode string, limit int) ([]JournalPointer, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "g.occurred_at", req)
	clause := "g.gate = ?"
	if where != "" {
		clause = strings.TrimPrefix(where, "WHERE ") + " AND " + clause
	}
	query := fmt.Sprintf(`
		SELECT g.run_id, g.occurred_at, g.runner_json FROM gate_verdicts g
		JOIN runs r ON r.run_id = g.run_id
		WHERE %s
		ORDER BY g.occurred_at DESC, g.run_id DESC`, clause)
	rows, err := db.sql.Query(query, append(append([]any{}, args...), gate)...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query gate runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}
	var out []JournalPointer
	for rows.Next() {
		var runID, occurredAt sql.NullString
		var runnerJSON sql.NullString
		if err := rows.Scan(&runID, &occurredAt, &runnerJSON); err != nil {
			return nil, fmt.Errorf("rollup: scan gate run: %w", err)
		}
		if mode == "escalated" && (!runnerJSON.Valid || !gateEscalated(runnerJSON.String)) {
			continue
		}
		if seen[runID.String] {
			continue
		}
		seen[runID.String] = true
		out = append(out, JournalPointer{RunID: runID.String})
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// detectCoverageGaps flags workflows named in req.Workflows with zero runs,
// and (workflow, stage) pairs with zero stage attempts, within req's
// window/gaggle filter.
func (db *DB) detectCoverageGaps(req CoverageRequest) ([]Finding, error) {
	var out []Finding
	for _, workflow := range sortedKeys(req.Workflows) {
		count, err := db.workflowRunCount(req.StatsRequest, workflow)
		if err != nil {
			return nil, fmt.Errorf("rollup: detect coverage gap for workflow %q: %w", workflow, err)
		}
		if count == 0 {
			out = append(out, Finding{
				Kind:        FindingWorkflowUntriggered,
				Subject:     workflow,
				Metrics:     map[string]float64{"runCount": 0},
				Threshold:   0,
				FlaggedRuns: nil,
			})
			continue // no runs at all means no stage could have been reached either
		}
		for _, stage := range req.Workflows[workflow] {
			stageCount, err := db.workflowStageAttemptCount(req.StatsRequest, workflow, stage)
			if err != nil {
				return nil, fmt.Errorf("rollup: detect coverage gap for %s/%s: %w", workflow, stage, err)
			}
			if stageCount == 0 {
				out = append(out, Finding{
					Kind:        FindingStageUnreached,
					Subject:     workflow + "/" + stage,
					Metrics:     map[string]float64{"attemptCount": 0},
					Threshold:   0,
					FlaggedRuns: nil,
				})
			}
		}
	}
	return out, nil
}

func (db *DB) workflowRunCount(req StatsRequest, workflow string) (int, error) {
	where, args := statsWhere("workflow", "gaggle", "started_at", req)
	clause := "workflow = ?"
	if where != "" {
		clause = strings.TrimPrefix(where, "WHERE ") + " AND " + clause
	}
	var count int
	err := db.sql.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM runs WHERE %s`, clause),
		append(append([]any{}, args...), workflow)...).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (db *DB) workflowStageAttemptCount(req StatsRequest, workflow, stage string) (int, error) {
	where, args := statsWhere("r.workflow", "r.gaggle", "r.started_at", req)
	clause := "r.workflow = ? AND sa.stage = ?"
	if where != "" {
		clause = strings.TrimPrefix(where, "WHERE ") + " AND " + clause
	}
	var count int
	err := db.sql.QueryRow(fmt.Sprintf(`
		SELECT COUNT(*) FROM stage_attempts sa
		JOIN runs r ON r.run_id = sa.run_id
		WHERE %s`, clause),
		append(append([]any{}, args...), workflow, stage)...).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// queryRunIDs runs query/args and collects distinct run ids in result order
// into JournalPointers — the shared tail every per-finding "which runs"
// lookup above uses.
func queryRunIDs(db *DB, query string, args []any) ([]JournalPointer, error) {
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query run ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []JournalPointer
	for rows.Next() {
		var runID string
		var sortKey sql.NullString
		if err := rows.Scan(&runID, &sortKey); err != nil {
			return nil, fmt.Errorf("rollup: scan run id: %w", err)
		}
		out = append(out, JournalPointer{RunID: runID})
	}
	return out, rows.Err()
}

// sortedKeys returns m's keys sorted ascending — Detect's coverage pass
// iterates workflows in a stable order so its Findings are deterministic
// for a fixed input (T2's own test-plan requirement) even though Go map
// iteration order is not.
func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
