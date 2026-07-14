package rollup

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DigestChange is one workflow_digest transition observed in a workflow's
// run history — the moment a new definition (Tutor-authored or human) took
// effect. ChangedAt is the first run's started_at under ToDigest.
type DigestChange struct {
	Workflow   string
	FromDigest string
	ToDigest   string
	ChangedAt  time.Time
}

// DigestHistory returns every workflow_digest transition for workflow, in
// chronological order — the substrate both AssessLatestEfficacy (finding
// "the most recent change" to assess) and ChurnGuard (counting how many
// times a definition has flip-flopped) need. A run with an empty
// workflow_digest is skipped (pre-WF-016 data, or a digest that failed to
// pin) rather than treated as a real transition. Returns an empty slice,
// not an error, for a workflow with fewer than two distinct digests (no
// transition has ever happened).
func (db *DB) DigestHistory(workflow string) ([]DigestChange, error) {
	rows, err := db.sql.Query(`
		SELECT workflow_digest, started_at FROM runs
		WHERE workflow = ? AND workflow_digest IS NOT NULL AND workflow_digest != ''
		ORDER BY started_at, run_id`, workflow)
	if err != nil {
		return nil, fmt.Errorf("rollup: query digest history for %q: %w", workflow, err)
	}
	defer func() { _ = rows.Close() }()

	var changes []DigestChange
	prevDigest := ""
	for rows.Next() {
		var digest sql.NullString
		var startedAt sql.NullString
		if err := rows.Scan(&digest, &startedAt); err != nil {
			return nil, fmt.Errorf("rollup: scan digest history row: %w", err)
		}
		at, err := parseTime(startedAt)
		if err != nil {
			return nil, err
		}
		if prevDigest != "" && digest.String != prevDigest {
			changes = append(changes, DigestChange{
				Workflow:   workflow,
				FromDigest: prevDigest,
				ToDigest:   digest.String,
				ChangedAt:  at,
			})
		}
		prevDigest = digest.String
	}
	return changes, rows.Err()
}

// EfficacyVerdict is AssessEfficacy's helped/regressed/no-change/
// insufficient-data conclusion (TUT-008).
type EfficacyVerdict string

const (
	// EfficacyHelped means the after-segment's failure rate improved by at
	// least EfficacyThresholds.SignificantFailureRateDelta.
	EfficacyHelped EfficacyVerdict = "helped"
	// EfficacyRegressed means the after-segment's failure rate worsened by
	// at least EfficacyThresholds.SignificantFailureRateDelta.
	EfficacyRegressed EfficacyVerdict = "regressed"
	// EfficacyNoChange means the failure-rate delta is within the
	// significance threshold either way — not enough evidence of an effect.
	EfficacyNoChange EfficacyVerdict = "no-change"
	// EfficacyInsufficientData means one or both segments have fewer than
	// EfficacyThresholds.MinSamples terminal runs — too little evidence to
	// render any verdict, including no-change.
	EfficacyInsufficientData EfficacyVerdict = "insufficient-data"
)

// EfficacyThresholds are the config-tunable knobs AssessEfficacy checks
// against; DefaultEfficacyThresholds gives sane defaults.
type EfficacyThresholds struct {
	// MinSamples is the minimum terminal (completed+failed) run count each
	// segment (before and after) needs before a verdict is trustworthy.
	// Default 5.
	MinSamples int
	// SignificantFailureRateDelta is the minimum |after - before| failure
	// rate change (a fraction in [0, 1]) to call it helped/regressed rather
	// than no-change. Default 0.05 (5 percentage points).
	SignificantFailureRateDelta float64
}

// DefaultEfficacyThresholds returns the sane-defaults EfficacyThresholds.
func DefaultEfficacyThresholds() EfficacyThresholds {
	return EfficacyThresholds{
		MinSamples:                  5,
		SignificantFailureRateDelta: 0.05,
	}
}

// EfficacyRequest asks whether a workflow_digest transition (a merged Tutor
// PR, or any definition change) helped or regressed, comparing terminal
// runs under OldDigest ("before") against runs under NewDigest ("after"),
// both optionally bounded to Since (the #149 retention window — a
// comparison spanning a prune boundary is best-effort per the design doc's
// own T5 assumption, since pruned runs are simply absent from either
// segment, not an error).
type EfficacyRequest struct {
	Workflow   string
	OldDigest  string
	NewDigest  string
	Since      time.Time
	Thresholds EfficacyThresholds
}

// EfficacyResult is one before/after comparison — Before/After reuse
// RunStats (the same shape `goobers telemetry stats` exposes) so a diagnosis
// step or human reviewer reads the raw numbers in a familiar shape, not a
// bespoke one.
type EfficacyResult struct {
	Workflow             string
	OldDigest, NewDigest string
	Before, After        RunStats
	Verdict              EfficacyVerdict
	// FailureRateDelta is After's failure rate minus Before's — negative
	// means failures went down (improved); zero value when Verdict is
	// EfficacyInsufficientData (no meaningful delta to report).
	FailureRateDelta float64
}

// AssessEfficacy compares req.Workflow's runs under OldDigest against runs
// under NewDigest and renders a helped/regressed/no-change verdict (TUT-008,
// the metrics half — no agentic diagnosis here, just the aggregate
// comparison a Tutor's change-efficacy stage or a human reviewer consumes).
func (db *DB) AssessEfficacy(req EfficacyRequest) (EfficacyResult, error) {
	th := req.Thresholds
	if th == (EfficacyThresholds{}) {
		th = DefaultEfficacyThresholds()
	}

	before, err := db.runStatsByDigest(req.Workflow, req.OldDigest, req.Since)
	if err != nil {
		return EfficacyResult{}, fmt.Errorf("rollup: assess efficacy (before segment): %w", err)
	}
	after, err := db.runStatsByDigest(req.Workflow, req.NewDigest, req.Since)
	if err != nil {
		return EfficacyResult{}, fmt.Errorf("rollup: assess efficacy (after segment): %w", err)
	}

	result := EfficacyResult{
		Workflow:  req.Workflow,
		OldDigest: req.OldDigest,
		NewDigest: req.NewDigest,
		Before:    before,
		After:     after,
	}

	beforeTerminal := before.CompletedRuns + before.FailedRuns
	afterTerminal := after.CompletedRuns + after.FailedRuns
	if beforeTerminal < th.MinSamples || afterTerminal < th.MinSamples {
		result.Verdict = EfficacyInsufficientData
		return result, nil
	}

	beforeFailureRate := 1 - before.SuccessRate
	afterFailureRate := 1 - after.SuccessRate
	delta := afterFailureRate - beforeFailureRate
	result.FailureRateDelta = delta

	switch {
	case delta <= -th.SignificantFailureRateDelta:
		result.Verdict = EfficacyHelped
	case delta >= th.SignificantFailureRateDelta:
		result.Verdict = EfficacyRegressed
	default:
		result.Verdict = EfficacyNoChange
	}
	return result, nil
}

// AssessLatestEfficacy finds workflow's most recent digest transition (via
// DigestHistory) and assesses it — the common case of "did the last merged
// Tutor PR help or regress," without the caller needing to already know
// which two digests to compare. Returns EfficacyInsufficientData (no error)
// if the workflow has never changed digests within the observed history.
func (db *DB) AssessLatestEfficacy(workflow string, since time.Time, th EfficacyThresholds) (EfficacyResult, error) {
	changes, err := db.DigestHistory(workflow)
	if err != nil {
		return EfficacyResult{}, fmt.Errorf("rollup: assess latest efficacy for %q: %w", workflow, err)
	}
	if len(changes) == 0 {
		return EfficacyResult{Workflow: workflow, Verdict: EfficacyInsufficientData}, nil
	}
	latest := changes[len(changes)-1]
	return db.AssessEfficacy(EfficacyRequest{
		Workflow:   workflow,
		OldDigest:  latest.FromDigest,
		NewDigest:  latest.ToDigest,
		Since:      since,
		Thresholds: th,
	})
}

// runStatsByDigest is runStats (aggregates.go) narrowed to one exact
// workflow_digest instead of grouping across every digest a workflow has
// ever run under — the single-segment aggregate AssessEfficacy's before/
// after comparison needs. Returns the RunStats zero value (TotalRuns=0) for
// a digest with no runs, not an error.
func (db *DB) runStatsByDigest(workflow, digest string, since time.Time) (RunStats, error) {
	clauses := []string{"workflow = ?", "workflow_digest = ?"}
	args := []any{workflow, digest}
	if !since.IsZero() {
		clauses = append(clauses, "started_at >= ?")
		args = append(args, formatTime(since).String)
	}
	where := "WHERE " + strings.Join(clauses, " AND ")
	query := fmt.Sprintf(`
		SELECT COUNT(*),
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
			AVG(duration_ms), MIN(duration_ms), MAX(duration_ms)
		FROM runs %s`, where)
	queryArgs := append([]any{runStatusCompleted, runStatusFailed}, args...)

	var s RunStats
	var avg sql.NullFloat64
	var min, max sql.NullInt64
	err := db.sql.QueryRow(query, queryArgs...).Scan(&s.TotalRuns, &s.CompletedRuns, &s.FailedRuns, &avg, &min, &max)
	if err != nil {
		return RunStats{}, fmt.Errorf("rollup: run stats for %q@%q: %w", workflow, digest, err)
	}
	s.Workflow = workflow
	s.OtherRuns = s.TotalRuns - s.CompletedRuns - s.FailedRuns
	if terminal := s.CompletedRuns + s.FailedRuns; terminal > 0 {
		s.SuccessRate = float64(s.CompletedRuns) / float64(terminal)
	}
	s.AvgDurationMs, s.MinDurationMs, s.MaxDurationMs = avg.Float64, min.Int64, max.Int64
	return s, nil
}

// ChurnGuardRequest asks whether workflow has changed definitions too many
// times within a recent window to trust another change right now (TUT-008:
// "a definition is not repeatedly flip-flopped").
type ChurnGuardRequest struct {
	Workflow string
	// Since bounds the window changes are counted within. Zero means
	// unbounded (every recorded change, ever).
	Since time.Time
	// MaxChanges flags the workflow when the count of digest transitions
	// within the window is at or above this. Default 3 if <= 0.
	MaxChanges int
}

// ChurnGuardResult reports whether workflow is currently churn-flagged and
// the transitions that drove the count, for a diagnosis step or human
// reviewer to inspect.
type ChurnGuardResult struct {
	Workflow    string
	ChangeCount int
	Flagged     bool
	Changes     []DigestChange
}

// ChurnGuard counts req.Workflow's digest transitions within req.Since and
// flags it when the count meets or exceeds req.MaxChanges — the guard a
// Tutor change-proposal stage checks before authoring another config PR for
// a workflow that's already been flip-flopping.
func (db *DB) ChurnGuard(req ChurnGuardRequest) (ChurnGuardResult, error) {
	maxChanges := req.MaxChanges
	if maxChanges <= 0 {
		maxChanges = 3
	}
	all, err := db.DigestHistory(req.Workflow)
	if err != nil {
		return ChurnGuardResult{}, fmt.Errorf("rollup: churn guard for %q: %w", req.Workflow, err)
	}
	var inWindow []DigestChange
	for _, c := range all {
		if req.Since.IsZero() || !c.ChangedAt.Before(req.Since) {
			inWindow = append(inWindow, c)
		}
	}
	return ChurnGuardResult{
		Workflow:    req.Workflow,
		ChangeCount: len(inWindow),
		Flagged:     len(inWindow) >= maxChanges,
		Changes:     inWindow,
	}, nil
}
