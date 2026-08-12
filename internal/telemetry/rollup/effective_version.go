package rollup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EffectiveVersion is the version-segmented cohort key from the Tutor v2
// redesign (docs/design/tutor-redesign.md §4.1): everything that determines
// a run's behaviour, not just its workflow structure. Two runs pool into the
// same efficacy cohort only when every axis — workflow definition,
// participating goobers' resolved specs, model, and harness version — is
// identical.
type EffectiveVersion struct {
	WorkflowDigest string `json:"workflowDigest"`
	GooberDigest   string `json:"gooberDigest"`
	Model          string `json:"model"`
	HarnessVersion string `json:"harnessVersion"`
}

// Hash is the EffectiveVersion's stable content identity, in the same
// "sha256:<hex>" shape as WorkflowDigest/GooberDigest so all three compose
// uniformly in logs and artifacts.
func (v EffectiveVersion) Hash() string {
	// Marshal error is unreachable: EffectiveVersion is four plain strings.
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// effectiveVersionRun is one run's cohort assignment. Excluded is true when
// the run cannot be assigned a defined EffectiveVersion: no workflow_digest
// (pre-WF-016 data), or the run's agentic spans disagree on model/harness
// version (a run cannot straddle two cohorts). Excluded runs are dropped
// entirely from cohort history and efficacy comparisons — the design doc's
// "partial-overlap cohorts... never pooled" rule, applied at the run level:
// a run with undefined provenance is neither "before" nor "after" evidence
// for any cohort.
type effectiveVersionRun struct {
	RunID      string
	StartedAt  time.Time
	Status     string
	DurationMs sql.NullInt64
	Version    EffectiveVersion
	Hash       string
	Excluded   bool
}

// agentProvenance is the reduction of a run's agent_invocations rows: the
// single (model, harness_version) pair every agentic span in the run agrees
// on. distinctPairs > 1 means the run's agentic spans disagree — a mixed
// run, whose EffectiveVersion is undefined. distinctPairs == 0 means the run
// has no agentic spans at all (a purely deterministic workflow), which is a
// well-defined, non-agentic cohort (empty Model/HarnessVersion).
type agentProvenance struct {
	distinctPairs  int
	model          string
	harnessVersion string
}

func (db *DB) agentProvenanceByRunForGaggle(ctx context.Context, gaggle, workflow string) (map[string]agentProvenance, error) {
	query := `
		SELECT ai.run_id, ai.model, ai.harness_version
		FROM agent_invocations ai
		JOIN runs r ON r.run_id = ai.run_id
		WHERE r.workflow = ?`
	args := []any{workflow}
	if gaggle != "" {
		query += " AND r.gaggle = ?"
		args = append(args, gaggle)
	}
	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query agent provenance for %q: %w", workflow, err)
	}
	defer func() { _ = rows.Close() }()

	type pair struct{ model, harness string }
	seen := map[string]map[pair]struct{}{}
	for rows.Next() {
		var runID, model, harness string
		if err := rows.Scan(&runID, &model, &harness); err != nil {
			return nil, fmt.Errorf("rollup: scan agent provenance row: %w", err)
		}
		set, ok := seen[runID]
		if !ok {
			set = map[pair]struct{}{}
			seen[runID] = set
		}
		set[pair{model, harness}] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]agentProvenance, len(seen))
	for runID, set := range seen {
		if len(set) != 1 {
			out[runID] = agentProvenance{distinctPairs: len(set)}
			continue
		}
		for p := range set {
			out[runID] = agentProvenance{distinctPairs: 1, model: p.model, harnessVersion: p.harness}
		}
	}
	return out, nil
}

func (db *DB) effectiveVersionRowsForGaggle(ctx context.Context, gaggle, workflow string, since time.Time) ([]effectiveVersionRun, error) {
	provenance, err := db.agentProvenanceByRunForGaggle(context.Background(), gaggle, workflow)
	if err != nil {
		return nil, err
	}

	clauses := []string{"r.workflow = ?"}
	args := []any{workflow}
	if gaggle != "" {
		clauses = append(clauses, "r.gaggle = ?")
		args = append(args, gaggle)
	}
	if !since.IsZero() {
		clauses = append(clauses, "r.started_at >= ?")
		args = append(args, formatTime(since).String)
	}
	query := fmt.Sprintf(`
		SELECT r.run_id, r.started_at, r.status, r.duration_ms, r.workflow_digest, COALESCE(g.goober_digest, '')
		FROM runs r LEFT JOIN run_goober_digests g ON g.run_id = r.run_id
		WHERE %s
		ORDER BY r.started_at, r.run_id`, strings.Join(clauses, " AND "))
	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query effective version rows for %q: %w", workflow, err)
	}
	defer func() { _ = rows.Close() }()

	var out []effectiveVersionRun
	for rows.Next() {
		var runID string
		var startedAt, status, workflowDigest sql.NullString
		var gooberDigest string
		var durationMs sql.NullInt64
		if err := rows.Scan(&runID, &startedAt, &status, &durationMs, &workflowDigest, &gooberDigest); err != nil {
			return nil, fmt.Errorf("rollup: scan effective version row: %w", err)
		}
		at, err := parseTime(startedAt)
		if err != nil {
			return nil, err
		}
		prov := provenance[runID]
		excluded := workflowDigest.String == "" || prov.distinctPairs > 1
		version := EffectiveVersion{
			WorkflowDigest: workflowDigest.String,
			GooberDigest:   gooberDigest,
			Model:          prov.model,
			HarnessVersion: prov.harnessVersion,
		}
		out = append(out, effectiveVersionRun{
			RunID:      runID,
			StartedAt:  at,
			Status:     status.String,
			DurationMs: durationMs,
			Version:    version,
			Hash:       version.Hash(),
			Excluded:   excluded,
		})
	}
	return out, rows.Err()
}

// EffectiveVersionChange is one EffectiveVersion transition observed in a
// workflow's run history — DigestHistory's cohort-aware counterpart. Runs
// with an undefined EffectiveVersion (effectiveVersionRun.Excluded) are
// invisible to transition detection: they neither close out the prior
// cohort nor open a new one.
type EffectiveVersionChange struct {
	Workflow    string
	FromVersion EffectiveVersion
	ToVersion   EffectiveVersion
	FromHash    string
	ToHash      string
	ChangedAt   time.Time
}

// EffectiveVersionCohort is one contiguous observed cohort for a workflow.
type EffectiveVersionCohort struct {
	Version   EffectiveVersion
	Hash      string
	StartedAt time.Time
	Stats     RunStats
}

// DigestHistoryByEffectiveVersion returns every EffectiveVersion transition
// for workflow, in chronological order, mirroring DigestHistory but keyed on
// the full cohort (workflow + goober + model + harness) rather than
// workflow_digest alone.
func (db *DB) DigestHistoryByEffectiveVersion(ctx context.Context, workflow string) ([]EffectiveVersionChange, error) {
	return db.DigestHistoryByEffectiveVersionForGaggle(ctx, "", workflow)
}

// DigestHistoryByEffectiveVersionForGaggle is the gaggle-scoped form used by
// Tutor holdouts. Workflow names are only unique within a gaggle, so a
// cross-run verifier must never pool same-named workflows from other silos.
func (db *DB) DigestHistoryByEffectiveVersionForGaggle(ctx context.Context, gaggle, workflow string) ([]EffectiveVersionChange, error) {
	rows, err := db.effectiveVersionRowsForGaggle(ctx, gaggle, workflow, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("rollup: effective version history for %q: %w", workflow, err)
	}

	var changes []EffectiveVersionChange
	var prev *effectiveVersionRun
	for i := range rows {
		row := &rows[i]
		if row.Excluded {
			continue
		}
		if prev != nil && row.Hash != prev.Hash {
			changes = append(changes, EffectiveVersionChange{
				Workflow:    workflow,
				FromVersion: prev.Version,
				ToVersion:   row.Version,
				FromHash:    prev.Hash,
				ToHash:      row.Hash,
				ChangedAt:   row.StartedAt,
			})
		}
		prev = row
	}
	return changes, nil
}

// FirstEffectiveVersionCohortForGaggle returns the first contiguous cohort
// observed after since with the requested configuration axes. An empty digest
// is a wildcard. Model and harness remain part of the exact cohort boundary.
func (db *DB) FirstEffectiveVersionCohortForGaggle(ctx context.Context,
	gaggle, workflow, workflowDigest, gooberDigest string,
	since time.Time,
) (*EffectiveVersionCohort, error) {
	rows, err := db.effectiveVersionRowsForGaggle(ctx, gaggle, workflow, since)
	if err != nil {
		return nil, fmt.Errorf("rollup: effective version cohort for %q: %w", workflow, err)
	}
	var selected []effectiveVersionRun
	for _, row := range rows {
		if row.Excluded {
			continue
		}
		if len(selected) == 0 {
			if (workflowDigest != "" && row.Version.WorkflowDigest != workflowDigest) ||
				(gooberDigest != "" && row.Version.GooberDigest != gooberDigest) {
				continue
			}
			selected = append(selected, row)
			continue
		}
		if row.Hash != selected[0].Hash {
			break
		}
		selected = append(selected, row)
	}
	if len(selected) == 0 {
		return nil, nil
	}
	return &EffectiveVersionCohort{
		Version:   selected[0].Version,
		Hash:      selected[0].Hash,
		StartedAt: selected[0].StartedAt,
		Stats:     aggregateRunStats(workflow, selected),
	}, nil
}

// aggregateRunStats reduces a set of effectiveVersionRun rows (already
// filtered to one cohort) into a RunStats, mirroring runStatsByDigest's SQL
// aggregation in Go since the source rows are already loaded in memory.
func aggregateRunStats(workflow string, rows []effectiveVersionRun) RunStats {
	s := RunStats{Workflow: workflow}
	var durSum float64
	var durCount int
	var min, max int64
	for _, r := range rows {
		s.TotalRuns++
		switch r.Status {
		case runStatusCompleted:
			s.CompletedRuns++
		case runStatusFailed:
			s.FailedRuns++
		}
		if r.DurationMs.Valid {
			d := r.DurationMs.Int64
			durSum += float64(d)
			if durCount == 0 || d < min {
				min = d
			}
			if durCount == 0 || d > max {
				max = d
			}
			durCount++
		}
	}
	s.OtherRuns = s.TotalRuns - s.CompletedRuns - s.FailedRuns
	if terminal := s.CompletedRuns + s.FailedRuns; terminal > 0 {
		s.SuccessRate = float64(s.CompletedRuns) / float64(terminal)
	}
	if durCount > 0 {
		s.AvgDurationMs = durSum / float64(durCount)
		s.MinDurationMs = min
		s.MaxDurationMs = max
	}
	return s
}

func (db *DB) runStatsByEffectiveVersionForGaggle(
	gaggle, workflow, hash string,
	since, before time.Time,
) (RunStats, error) {
	rows, err := db.effectiveVersionRowsForGaggle(context.Background(), gaggle, workflow, since)
	if err != nil {
		return RunStats{}, err
	}
	var matched []effectiveVersionRun
	for _, r := range rows {
		if r.Excluded || r.Hash != hash || (!before.IsZero() && !r.StartedAt.Before(before)) {
			continue
		}
		matched = append(matched, r)
	}
	return aggregateRunStats(workflow, matched), nil
}

// EffectiveVersionEfficacyRequest asks whether an EffectiveVersion
// transition helped or regressed, comparing terminal runs under OldVersion
// ("before") against runs under NewVersion ("after") — AssessEfficacy's
// cohort-aware counterpart (TUT-P3).
type EffectiveVersionEfficacyRequest struct {
	Gaggle      string
	Workflow    string
	OldVersion  EffectiveVersion
	NewVersion  EffectiveVersion
	Since       time.Time
	BeforeSince time.Time
	AfterSince  time.Time
	BeforeUntil time.Time
	AfterUntil  time.Time
	Thresholds  EfficacyThresholds
}

// EffectiveVersionEfficacyResult is one before/after EffectiveVersion
// comparison, mirroring EfficacyResult but carrying the full cohort key
// (not just a workflow_digest) on each side.
type EffectiveVersionEfficacyResult struct {
	Workflow                       string
	OldVersion, NewVersion         EffectiveVersion
	OldVersionHash, NewVersionHash string
	Before, After                  RunStats
	Verdict                        EfficacyVerdict
	FailureRateDelta               float64
}

// AssessEfficacyByEffectiveVersion compares req.Workflow's runs under
// OldVersion against runs under NewVersion and renders a helped/regressed/
// no-change verdict, identically to AssessEfficacy except the cohort key is
// the full EffectiveVersion (workflow + goober + model + harness) rather
// than workflow_digest alone — so a model or harness change starts its own
// cohort instead of being silently pooled with runs before that change.
func (db *DB) AssessEfficacyByEffectiveVersion(ctx context.Context, req EffectiveVersionEfficacyRequest) (EffectiveVersionEfficacyResult, error) {
	th := req.Thresholds
	if th == (EfficacyThresholds{}) {
		th = DefaultEfficacyThresholds()
	}
	oldHash := req.OldVersion.Hash()
	newHash := req.NewVersion.Hash()

	beforeSince := req.Since
	if !req.BeforeSince.IsZero() {
		beforeSince = req.BeforeSince
	}
	afterSince := req.Since
	if !req.AfterSince.IsZero() {
		afterSince = req.AfterSince
	}
	before, err := db.runStatsByEffectiveVersionForGaggle(req.Gaggle, req.Workflow, oldHash, beforeSince, req.BeforeUntil)
	if err != nil {
		return EffectiveVersionEfficacyResult{}, fmt.Errorf("rollup: assess effective-version efficacy (before segment): %w", err)
	}
	after, err := db.runStatsByEffectiveVersionForGaggle(req.Gaggle, req.Workflow, newHash, afterSince, req.AfterUntil)
	if err != nil {
		return EffectiveVersionEfficacyResult{}, fmt.Errorf("rollup: assess effective-version efficacy (after segment): %w", err)
	}

	result := EffectiveVersionEfficacyResult{
		Workflow:       req.Workflow,
		OldVersion:     req.OldVersion,
		NewVersion:     req.NewVersion,
		OldVersionHash: oldHash,
		NewVersionHash: newHash,
		Before:         before,
		After:          after,
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

// AssessLatestEfficacyByEffectiveVersion finds workflow's most recent
// EffectiveVersion transition (via DigestHistoryByEffectiveVersion) and
// assesses it, mirroring AssessLatestEfficacy. Returns
// EfficacyInsufficientData (no error) if the workflow has never observed an
// EffectiveVersion transition within the recorded history.
func (db *DB) AssessLatestEfficacyByEffectiveVersion(ctx context.Context, workflow string, since time.Time, th EfficacyThresholds) (EffectiveVersionEfficacyResult, error) {
	return db.AssessLatestEfficacyByEffectiveVersionForGaggle(ctx, "", workflow, since, th)
}

// AssessLatestEfficacyByEffectiveVersionForGaggle is the gaggle-scoped form
// for callers that resolve a workflow definition within one gaggle.
func (db *DB) AssessLatestEfficacyByEffectiveVersionForGaggle(ctx context.Context, gaggle, workflow string, since time.Time, th EfficacyThresholds) (EffectiveVersionEfficacyResult, error) {
	changes, err := db.DigestHistoryByEffectiveVersionForGaggle(ctx, gaggle, workflow)
	if err != nil {
		return EffectiveVersionEfficacyResult{}, fmt.Errorf("rollup: assess latest effective-version efficacy for %q: %w", workflow, err)
	}
	if len(changes) == 0 {
		return EffectiveVersionEfficacyResult{Workflow: workflow, Verdict: EfficacyInsufficientData}, nil
	}
	latest := changes[len(changes)-1]
	return db.AssessEfficacyByEffectiveVersion(ctx, EffectiveVersionEfficacyRequest{
		Gaggle:     gaggle,
		Workflow:   workflow,
		OldVersion: latest.FromVersion,
		NewVersion: latest.ToVersion,
		Since:      since,
		Thresholds: th,
	})
}
