package rollup

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CurationStats is the per-window rollup of durable curation action records.
type CurationStats struct {
	Runs         int `json:"runs"`
	ReportedRuns int `json:"reportedRuns"`
	Ready        int `json:"ready"`
	NeedsHuman   int `json:"needsHuman"`
	Closed       int `json:"closed"`
	Deduped      int `json:"deduped"`
	Split        int `json:"split"`
	Stale        int `json:"stale"`
	Reconciled   int `json:"reconciled"`
	Milestoned   int `json:"milestoned"`
	Bounced      int `json:"bounced"`
}

// ReadyPoolHealth combines the latest backlog snapshot with windowed ready
// transitions and implementation demand.
type ReadyPoolHealth struct {
	HasSample                 bool      `json:"-"`
	ObservedAt                time.Time `json:"observedAt,omitempty"`
	Depth                     int       `json:"depth"`
	AverageAgeSeconds         float64   `json:"averageAgeSeconds"`
	OldestAgeSeconds          float64   `json:"oldestAgeSeconds"`
	Starved                   bool      `json:"starved"`
	ClaimAgeSamples           int       `json:"claimAgeSamples"`
	AverageClaimAgeSeconds    float64   `json:"averageClaimAgeSeconds"`
	BounceRate                float64   `json:"bounceRate"`
	HasBounceRate             bool      `json:"-"`
	ForwardCurationThroughput int       `json:"forwardCurationThroughput"`
	ImplementationDemand      int       `json:"implementationDemand"`
}

func (db *DB) curationStats(req StatsRequest, transitions []storedReadyLabelTransition) (CurationStats, error) {
	if agentStatsActive(req) || (req.Workflow != "" && req.Workflow != "backlog-curation") {
		return CurationStats{}, nil
	}
	clauses := []string{"r.workflow = 'backlog-curation'"}
	args := make([]any, 0, 3)
	if req.Gaggle != "" {
		clauses = append(clauses, "r.gaggle = ?")
		args = append(args, req.Gaggle)
	}
	if !req.Since.IsZero() {
		clauses = append(clauses, "ca.occurred_at >= ?")
		args = append(args, formatTime(req.Since).String)
	}
	if !req.Until.IsZero() {
		clauses = append(clauses, "ca.occurred_at <= ?")
		args = append(args, formatTime(req.Until).String)
	}
	query := `
		SELECT COUNT(*), COALESCE(SUM(ca.reported), 0),
			COALESCE(SUM(ca.ready_count), 0),
			COALESCE(SUM(ca.needs_human_count), 0),
			COALESCE(SUM(ca.closed_count), 0),
			COALESCE(SUM(ca.deduped_count), 0),
			COALESCE(SUM(ca.split_count), 0),
			COALESCE(SUM(ca.stale_count), 0),
			COALESCE(SUM(ca.reconciled_count), 0),
			COALESCE(SUM(ca.milestoned_count), 0),
			COALESCE(SUM(ca.bounced_count), 0)
		FROM curation_actions ca
		JOIN runs r ON r.run_id = ca.run_id
		WHERE ` + strings.Join(clauses, " AND ")
	var out CurationStats
	if err := db.sql.QueryRow(query, args...).Scan(
		&out.Runs, &out.ReportedRuns, &out.Ready, &out.NeedsHuman,
		&out.Closed, &out.Deduped, &out.Split, &out.Stale,
		&out.Reconciled, &out.Milestoned, &out.Bounced,
	); err != nil {
		return CurationStats{}, fmt.Errorf("rollup: query curation stats: %w", err)
	}
	out.Bounced = countTransitionsInWindow(transitions, "not-ready", req)
	return out, nil
}

func (db *DB) readyPoolHealth(
	req StatsRequest,
	curation CurationStats,
	transitions []storedReadyLabelTransition,
) (ReadyPoolHealth, error) {
	if agentStatsActive(req) || req.Workflow != "" {
		return ReadyPoolHealth{}, nil
	}
	var out ReadyPoolHealth
	sampleClauses := []string{"r.workflow = 'backlog-curation'"}
	sampleArgs := make([]any, 0, 3)
	if req.Gaggle != "" {
		sampleClauses = append(sampleClauses, "r.gaggle = ?")
		sampleArgs = append(sampleArgs, req.Gaggle)
	}
	if !req.Since.IsZero() {
		sampleClauses = append(sampleClauses, "s.observed_at >= ?")
		sampleArgs = append(sampleArgs, formatTime(req.Since).String)
	}
	if !req.Until.IsZero() {
		sampleClauses = append(sampleClauses, "s.observed_at <= ?")
		sampleArgs = append(sampleArgs, formatTime(req.Until).String)
	}
	var observedAt sql.NullString
	err := db.sql.QueryRow(`
		SELECT s.observed_at, s.depth, s.average_age_seconds, s.oldest_age_seconds
		FROM ready_pool_samples s
		JOIN runs r ON r.run_id = s.run_id
		WHERE `+strings.Join(sampleClauses, " AND ")+`
		ORDER BY s.observed_at DESC, s.run_id
		LIMIT 1`, sampleArgs...).Scan(
		&observedAt, &out.Depth, &out.AverageAgeSeconds, &out.OldestAgeSeconds,
	)
	if err != nil && err != sql.ErrNoRows {
		return ReadyPoolHealth{}, fmt.Errorf("rollup: query ready-pool sample: %w", err)
	}
	if err == nil {
		parsed, parseErr := parseTime(observedAt)
		if parseErr != nil {
			return ReadyPoolHealth{}, parseErr
		}
		out.HasSample = true
		out.ObservedAt = parsed
		out.Starved = out.Depth == 0
	}

	claimClauses := []string{"r.workflow = 'implementation'"}
	claimArgs := make([]any, 0, 3)
	if req.Gaggle != "" {
		claimClauses = append(claimClauses, "r.gaggle = ?")
		claimArgs = append(claimArgs, req.Gaggle)
	}
	if !req.Since.IsZero() {
		claimClauses = append(claimClauses, "rc.claimed_at >= ?")
		claimArgs = append(claimArgs, formatTime(req.Since).String)
	}
	if !req.Until.IsZero() {
		claimClauses = append(claimClauses, "rc.claimed_at <= ?")
		claimArgs = append(claimArgs, formatTime(req.Until).String)
	}
	if err := db.sql.QueryRow(`
		SELECT COUNT(*), COALESCE(AVG(rc.ready_age_seconds), 0)
		FROM ready_claims rc
		JOIN runs r ON r.run_id = rc.run_id
		WHERE `+strings.Join(claimClauses, " AND "), claimArgs...).Scan(
		&out.ClaimAgeSamples, &out.AverageClaimAgeSeconds,
	); err != nil {
		return ReadyPoolHealth{}, fmt.Errorf("rollup: query ready claim ages: %w", err)
	}

	demandClauses := []string{"r.workflow = 'implementation'", "pm.kind = 'issue'", "pm.operation = 'claim'"}
	demandArgs := make([]any, 0, 3)
	if req.Gaggle != "" {
		demandClauses = append(demandClauses, "r.gaggle = ?")
		demandArgs = append(demandArgs, req.Gaggle)
	}
	if !req.Since.IsZero() {
		demandClauses = append(demandClauses, "pm.occurred_at >= ?")
		demandArgs = append(demandArgs, formatTime(req.Since).String)
	}
	if !req.Until.IsZero() {
		demandClauses = append(demandClauses, "pm.occurred_at <= ?")
		demandArgs = append(demandArgs, formatTime(req.Until).String)
	}
	if err := db.sql.QueryRow(`
		SELECT COUNT(*)
		FROM provider_mutations pm
		JOIN runs r ON r.run_id = pm.run_id
		WHERE `+strings.Join(demandClauses, " AND "), demandArgs...).Scan(&out.ImplementationDemand); err != nil {
		return ReadyPoolHealth{}, fmt.Errorf("rollup: query implementation demand: %w", err)
	}
	out.ForwardCurationThroughput = curation.Ready
	bounced, cohort := bounceCohort(transitions, req)
	if cohort > 0 {
		out.BounceRate = float64(bounced) / float64(cohort)
		out.HasBounceRate = true
	}
	return out, nil
}

type storedReadyLabelTransition struct {
	EventID    int64
	ItemID     string
	Transition string
	OccurredAt time.Time
}

func (db *DB) readyLabelTransitions(req StatsRequest) ([]storedReadyLabelTransition, error) {
	clauses := []string{"r.workflow = 'backlog-curation'"}
	var args []any
	if req.Gaggle != "" {
		clauses = append(clauses, "r.gaggle = ?")
		args = append(args, req.Gaggle)
	}
	rows, err := db.sql.Query(`
		SELECT rt.event_id, MIN(rt.item_id), MIN(rt.transition), MIN(rt.occurred_at)
		FROM ready_label_transitions rt
		JOIN runs r ON r.run_id = rt.run_id
		WHERE `+strings.Join(clauses, " AND ")+`
		GROUP BY rt.event_id
		ORDER BY MIN(rt.occurred_at), rt.event_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query ready-label transitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var transitions []storedReadyLabelTransition
	for rows.Next() {
		var transition storedReadyLabelTransition
		var occurredAt string
		if err := rows.Scan(&transition.EventID, &transition.ItemID, &transition.Transition, &occurredAt); err != nil {
			return nil, fmt.Errorf("rollup: scan ready-label transition: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("rollup: parse ready-label transition time: %w", err)
		}
		transition.OccurredAt = parsed
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup: iterate ready-label transitions: %w", err)
	}
	sort.Slice(transitions, func(i, j int) bool {
		if transitions[i].OccurredAt.Equal(transitions[j].OccurredAt) {
			return transitions[i].EventID < transitions[j].EventID
		}
		return transitions[i].OccurredAt.Before(transitions[j].OccurredAt)
	})
	return transitions, nil
}

func countTransitionsInWindow(transitions []storedReadyLabelTransition, kind string, req StatsRequest) int {
	count := 0
	for _, transition := range transitions {
		if transition.Transition == kind && inStatsWindow(transition.OccurredAt, req) {
			count++
		}
	}
	return count
}

func bounceCohort(transitions []storedReadyLabelTransition, req StatsRequest) (bounced, cohort int) {
	active := map[string]int64{}
	inCohort := map[int64]bool{}
	for _, transition := range transitions {
		if !req.Until.IsZero() && transition.OccurredAt.After(req.Until) {
			break
		}
		switch transition.Transition {
		case "ready":
			if _, ok := active[transition.ItemID]; ok {
				continue
			}
			active[transition.ItemID] = transition.EventID
			if inStatsWindow(transition.OccurredAt, req) {
				inCohort[transition.EventID] = true
				cohort++
			}
		case "not-ready":
			readyEvent, ok := active[transition.ItemID]
			if !ok {
				continue
			}
			delete(active, transition.ItemID)
			if inCohort[readyEvent] {
				bounced++
			}
		}
	}
	return bounced, cohort
}

func inStatsWindow(at time.Time, req StatsRequest) bool {
	return (req.Since.IsZero() || !at.Before(req.Since)) &&
		(req.Until.IsZero() || !at.After(req.Until))
}
