package readmodel

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

// MonitorMetric is a rate measured for an attributed node.
type MonitorMetric string

const (
	MonitorFailure    MonitorMetric = "failure"
	MonitorRetryWaste MonitorMetric = "retry-waste"
)

// MonitorPoint is one time bucket for one node and metric. Value is a rate in
// [0,1], while Samples is the denominator used to calculate it.
type MonitorPoint struct {
	At       time.Time
	Gaggle   string
	Workflow string
	Kind     string
	Node     string
	Identity string
	Metric   MonitorMetric
	Value    float64
	Samples  int
}

// MonitorOptions controls the read-only node time-series query.
type MonitorOptions struct {
	Gaggle, Workflow string
	Since, Until     time.Time
}

// NodeMetrics returns daily failure and retry-waste rates from node buckets. It
// reads only the read model and does not mutate the projection.
func (s *Store) NodeMetrics(ctx context.Context, options MonitorOptions) ([]MonitorPoint, error) {
	predicates := []string{"1 = 1"}
	args := []any{}
	if options.Gaggle != "" {
		predicates = append(predicates, "gaggle = ?")
		args = append(args, options.Gaggle)
	}
	if options.Workflow != "" {
		predicates = append(predicates, "workflow = ?")
		args = append(args, options.Workflow)
	}
	if !options.Since.IsZero() {
		predicates = append(predicates, "day >= ?")
		args = append(args, options.Since.UTC().Format(dayFormat))
	}
	if !options.Until.IsZero() {
		predicates = append(predicates, "day <= ?")
		args = append(args, options.Until.UTC().Format(dayFormat))
	}

	query := `SELECT day, gaggle, workflow, kind, name, identity,
		SUM(runs), SUM(failures), SUM(retry_waste)
		FROM bucket_node_day WHERE ` + strings.Join(predicates, " AND ") + `
		GROUP BY day, gaggle, workflow, kind, name, identity
		ORDER BY day, gaggle, workflow, kind, name, identity`

	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}

	defer release()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("readmodel: node monitor metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var points []MonitorPoint
	for rows.Next() {
		var day, gaggle, workflow, kind, node, identity string
		var samples, failures, retryWaste int
		if err := rows.Scan(&day, &gaggle, &workflow, &kind, &node, &identity,
			&samples, &failures, &retryWaste); err != nil {
			return nil, fmt.Errorf("readmodel: scan node monitor metric: %w", err)
		}
		at, err := time.Parse(dayFormat, day)
		if err != nil {
			return nil, fmt.Errorf("readmodel: parse node monitor day %q: %w", day, err)
		}
		base := MonitorPoint{At: at, Gaggle: gaggle, Workflow: workflow, Kind: kind,
			Node: node, Identity: identity, Samples: samples}
		base.Metric = MonitorFailure
		base.Value = float64(failures) / float64(samples)
		points = append(points, base)
		base.Metric = MonitorRetryWaste
		base.Value = float64(retryWaste) / float64(samples)
		points = append(points, base)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: node monitor metrics rows: %w", err)
	}
	return points, nil
}

// Nomination is the provider-neutral approval-gated work request for a drift.
type Nomination struct {
	Drift
	Marker           string
	DedupeKey        string
	Title            string
	Body             string
	Labels           []string
	RequiresApproval bool
}

// Improvement confirms that a previously nominated change reduced the metric.
type Improvement struct {
	Drift
	Verification     string
	NominationMarker string
	RequiresApproval bool
}

// MonitorResult contains provider-neutral outputs. The sink is responsible for
// applying SEC-047's approval gate through the configured backlog provider.
type MonitorResult struct {
	Drifts       []Drift
	Nominations  []Nomination
	Improvements []Improvement
}

type MonitorSink interface {
	OpenNomination(context.Context, Nomination) error
	ConfirmImprovement(context.Context, Improvement) error
}

// Monitor detects drift and emits at most one nomination per node/metric
// regression episode. Claims are persisted before calling the sink so repeated
// polls cannot file the same provider-side nomination.
func (s *Store) Monitor(ctx context.Context, options MonitorOptions, config MonitorConfig, sink MonitorSink) (MonitorResult, error) {
	points, err := s.NodeMetrics(ctx, options)
	if err != nil {
		return MonitorResult{}, err
	}
	result := MonitorResult{Drifts: DetectDrift(points, config)}
	for _, drift := range result.Drifts {
		if drift.Direction == "regression" {
			nomination := Nomination{
				Drift: drift,
				Marker: fmt.Sprintf("goobers:drift:%s:%s:%s:%s:%s:%s:%s",
					drift.Gaggle, drift.Workflow, drift.Kind, drift.Node, drift.Identity, drift.Metric,
					drift.StartedAt.UTC().Format(time.RFC3339)),
				DedupeKey: "",
				Title:     fmt.Sprintf("Regression detected for %s %s", drift.Node, drift.Metric),
				Body: fmt.Sprintf("Changepoint started %s; baseline %.4f, current %.4f, magnitude %.4f.",
					drift.StartedAt.UTC().Format(time.RFC3339), drift.Baseline, drift.Current, drift.Magnitude),
				Labels:           []string{providers.LabelNominated, providers.LabelNeedsHuman},
				RequiresApproval: true,
			}
			nomination.DedupeKey = nomination.Marker
			if sink == nil {
				result.Nominations = append(result.Nominations, nomination)
				continue
			}
			claimed, err := s.claimMonitorNomination(ctx, nomination.Marker)
			if err != nil {
				return MonitorResult{}, err
			}
			if !claimed {
				continue
			}
			if err := sink.OpenNomination(ctx, nomination); err != nil {
				if releaseErr := s.releaseMonitorNomination(ctx, nomination.Marker); releaseErr != nil {
					return MonitorResult{}, fmt.Errorf("readmodel: open drift nomination: %w; release claim: %v",
						err, releaseErr)
				}
				return MonitorResult{}, fmt.Errorf("readmodel: open drift nomination: %w", err)
			}
			result.Nominations = append(result.Nominations, nomination)
		} else {
			marker, err := s.lookupMonitorNominationMarker(ctx, drift.Gaggle, drift.Workflow,
				drift.Kind, drift.Node, drift.Identity, drift.Metric)
			if err != nil {
				return MonitorResult{}, err
			}
			if marker == "" {
				continue
			}
			improvement := Improvement{
				Drift: drift, Verification: "did-it-help",
				NominationMarker: marker,
				RequiresApproval: true,
			}
			if sink != nil {
				claimed, err := s.claimMonitorImprovement(ctx, drift.Gaggle, drift.Workflow, drift.Kind,
					drift.Node, drift.Identity, drift.Metric)
				if err != nil {
					return MonitorResult{}, err
				}
				if !claimed {
					continue
				}
				if err := sink.ConfirmImprovement(ctx, improvement); err != nil {
					if releaseErr := s.releaseMonitorImprovementClaim(ctx, marker); releaseErr != nil {
						return MonitorResult{}, fmt.Errorf("readmodel: confirm improvement: %w; release claim: %v",
							err, releaseErr)
					}
					return MonitorResult{}, fmt.Errorf("readmodel: confirm improvement: %w", err)
				}
				if err := s.releaseMonitorNomination(ctx, marker); err != nil {
					return MonitorResult{}, fmt.Errorf("readmodel: release drift nomination: %w", err)
				}
			}
			result.Improvements = append(result.Improvements, improvement)
		}
	}
	return result, nil
}

func (s *Store) claimMonitorNomination(ctx context.Context, marker string) (bool, error) {
	db, release, err := s.writeHandle()
	if err != nil {
		return false, err
	}
	defer release()
	result, err := db.ExecContext(ctx,
		`INSERT INTO monitor_nomination (marker, claimed_at) VALUES (?, ?) ON CONFLICT(marker) DO NOTHING`,
		marker, formatTime(s.now()))
	if err != nil {
		return false, fmt.Errorf("readmodel: claim drift nomination: %w", err)
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *Store) releaseMonitorNomination(ctx context.Context, marker string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	_, err = db.ExecContext(ctx, `DELETE FROM monitor_nomination WHERE marker = ?`, marker)
	return err
}

func (s *Store) claimMonitorImprovement(ctx context.Context, gaggle, workflow, kind, node, identity string, metric MonitorMetric) (bool, error) {
	db, release, err := s.writeHandle()
	if err != nil {
		return false, err
	}
	defer release()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("readmodel: begin improvement claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	marker, err := monitorNominationMarker(ctx, tx, gaggle, workflow, kind, node, identity, metric)
	if err != nil {
		return false, err
	}
	if marker == "" {
		return false, nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE monitor_nomination SET improvement_claimed_at = ?
		 WHERE marker = ? AND improvement_claimed_at IS NULL`,
		formatTime(s.now()), marker)
	if err != nil {
		return false, fmt.Errorf("readmodel: claim improvement: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("readmodel: claim improvement rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("readmodel: commit improvement claim: %w", err)
	}
	return affected == 1, nil
}

func (s *Store) releaseMonitorImprovementClaim(ctx context.Context, marker string) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	_, err = db.ExecContext(ctx,
		`UPDATE monitor_nomination SET improvement_claimed_at = NULL WHERE marker = ?`,
		marker)
	return err
}

// lookupMonitorNominationMarker returns the claimed marker for a given node/metric,
// if one exists. Returns empty string if no marker is claimed.
func (s *Store) lookupMonitorNominationMarker(ctx context.Context, gaggle, workflow, kind, node, identity string, metric MonitorMetric) (string, error) {
	db, release, err := s.readHandle()
	if err != nil {
		return "", err
	}
	defer release()

	prefix := monitorNominationPrefix(gaggle, workflow, kind, node, identity, metric)

	var marker string
	err = db.QueryRowContext(ctx,
		`SELECT marker FROM monitor_nomination WHERE marker LIKE ? ESCAPE '\' LIMIT 1`,
		prefix).Scan(&marker)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("readmodel: lookup monitor nomination marker: %w", err)
	}
	return marker, nil
}

type monitorNominationQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func monitorNominationMarker(ctx context.Context, db monitorNominationQuerier, gaggle, workflow, kind, node, identity string, metric MonitorMetric) (string, error) {
	var marker string
	err := db.QueryRowContext(ctx,
		`SELECT marker FROM monitor_nomination
		 WHERE marker LIKE ? ESCAPE '\' AND improvement_claimed_at IS NULL LIMIT 1`,
		monitorNominationPrefix(gaggle, workflow, kind, node, identity, metric)).Scan(&marker)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("readmodel: lookup monitor nomination marker: %w", err)
	}
	return marker, nil
}

func monitorNominationPrefix(gaggle, workflow, kind, node, identity string, metric MonitorMetric) string {
	escape := func(value string) string {
		return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
	}
	return fmt.Sprintf("goobers:drift:%s:%s:%s:%s:%s:%s:%%",
		escape(gaggle), escape(workflow), escape(kind), escape(node), escape(identity), escape(string(metric)))
}

// MonitorConfig controls CUSUM sensitivity and its minimum evidence.
type MonitorConfig struct {
	// BaselinePoints is the number of observations used to establish a node's
	// baseline. Sensitivity is the number of standard deviations for an alarm.
	BaselinePoints     int
	MinSamples         int
	Sensitivity        float64
	SeasonalPeriod     int
	DisableSeasonality bool
	// WorkloadTolerance suppresses alarms when bucket volume changes by more
	// than this fraction relative to the baseline.
	WorkloadTolerance float64
}

func (c MonitorConfig) withDefaults() MonitorConfig {
	if c.BaselinePoints <= 0 {
		c.BaselinePoints = 7
	}
	if c.MinSamples <= 0 {
		c.MinSamples = 5
	}
	if c.Sensitivity <= 0 {
		c.Sensitivity = 3
	}
	if c.DisableSeasonality {
		c.SeasonalPeriod = 0
	} else if c.SeasonalPeriod <= 0 {
		c.SeasonalPeriod = 7
	}
	if c.WorkloadTolerance <= 0 {
		c.WorkloadTolerance = 0.5
	}
	return c
}

// Drift is a confirmed changepoint. Direction is "regression" for an upward
// shift and "improvement" for a downward shift.
type Drift struct {
	Gaggle, Workflow, Kind, Node, Identity string
	Metric                                 MonitorMetric
	Direction                              string
	StartedAt                              time.Time
	Baseline                               float64
	Current                                float64
	Magnitude                              float64
}

// DetectDrift applies a two-sided CUSUM independently to each node and metric.
// It emits only the first alarm in an episode; a new episode requires the
// cumulative score to return to zero, which provides deterministic debounce.
func DetectDrift(points []MonitorPoint, config MonitorConfig) []Drift {
	config = config.withDefaults()
	grouped := make(map[string][]MonitorPoint)
	for _, point := range points {
		if point.Samples < config.MinSamples {
			continue
		}
		key := strings.Join([]string{point.Gaggle, point.Workflow, point.Kind,
			point.Node, point.Identity, string(point.Metric)}, "\x00")
		grouped[key] = append(grouped[key], point)
	}
	var result []Drift
	for _, series := range grouped {
		sort.SliceStable(series, func(i, j int) bool { return series[i].At.Before(series[j].At) })
		result = append(result, detectSeries(series, config)...)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].StartedAt.Equal(result[j].StartedAt) {
			return result[i].Node < result[j].Node
		}
		return result[i].StartedAt.Before(result[j].StartedAt)
	})
	return result
}

func detectSeries(series []MonitorPoint, config MonitorConfig) []Drift {
	if len(series) <= config.BaselinePoints {
		return nil
	}
	initialBaseline := append([]MonitorPoint(nil), series[:config.BaselinePoints]...)
	baseline := initialBaseline
	mean, deviation := stats(values(baseline))
	if deviation < 0.01 {
		deviation = 0.01
	}
	limit := config.Sensitivity * deviation
	positive, negative := 0.0, 0.0
	armed := true
	var events []Drift
	for index, point := range series[config.BaselinePoints:] {
		if config.SeasonalPeriod > 1 {
			baseline = seasonalBaseline(series, index+config.BaselinePoints, config)
			if len(baseline) < config.BaselinePoints {
				// Use the established baseline until enough complete seasonal
				// periods exist to make a same-slot comparison meaningful.
				baseline = initialBaseline
			}
			mean, deviation = stats(values(baseline))
			if deviation < 0.01 {
				deviation = 0.01
			}
			limit = config.Sensitivity * deviation
		}
		if workloadShifted(point, baseline, config.WorkloadTolerance) {
			positive, negative = 0, 0
			continue
		}
		delta := point.Value - mean
		k := limit / (2 * float64(config.BaselinePoints))
		positive = math.Max(0, positive+delta-k)
		negative = math.Min(0, negative+delta+k)
		if !armed {
			if positive == 0 && negative == 0 {
				armed = true
			}

			continue
		}
		direction := ""
		if positive >= limit {
			direction = "regression"
		} else if -negative >= limit {
			direction = "improvement"
		}
		if direction != "" {
			events = append(events, Drift{
				Gaggle: point.Gaggle, Workflow: point.Workflow, Kind: point.Kind,
				Node: point.Node, Identity: point.Identity, Metric: point.Metric,
				Direction: direction, StartedAt: point.At, Baseline: mean,
				Current: point.Value, Magnitude: math.Abs(point.Value - mean),
			})
			armed = false
			positive, negative = 0, 0
		}
	}
	return events
}

func workloadShifted(point MonitorPoint, baseline []MonitorPoint, tolerance float64) bool {
	if len(baseline) == 0 {
		return true
	}
	var average float64
	for _, sample := range baseline {
		average += float64(sample.Samples)
	}
	average /= float64(len(baseline))
	if average == 0 {
		return true
	}
	ratio := float64(point.Samples) / average
	return ratio < 1-tolerance || ratio > 1+tolerance
}

func seasonalBaseline(series []MonitorPoint, end int, config MonitorConfig) []MonitorPoint {
	period := config.SeasonalPeriod
	want := (series[end].At.Unix() / 86400) % int64(period)
	out := make([]MonitorPoint, 0, config.BaselinePoints)
	for i := end - 1; i >= 0 && len(out) < config.BaselinePoints; i-- {
		if (series[i].At.Unix()/86400)%int64(period) == want {
			out = append(out, series[i])
		}
	}
	return out
}

func values(points []MonitorPoint) []float64 {
	out := make([]float64, len(points))
	for i := range points {
		out[i] = points[i].Value
	}
	return out
}

func stats(values []float64) (float64, float64) {
	var mean float64
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	var variance float64
	for _, value := range values {
		variance += (value - mean) * (value - mean)
	}
	return mean, math.Sqrt(variance / float64(len(values)))
}
