// Package clustercheck defines the durable outcome of externally scheduled
// Kubernetes checks. It never runs or schedules a probe.
package clustercheck

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Result preserves the last outcome even after its freshness window expires.
// State is degraded for failure, stale for expired non-failure, warning for
// unverified/warning outcomes, and healthy only for a fresh passing check.
type Result struct {
	Check     string    `json:"check"`
	Outcome   string    `json:"outcome"`
	CheckedAt time.Time `json:"checkedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	State     string    `json:"state"`
	Stale     bool      `json:"stale"`
}

// FromEvent decodes the bounded, credential-free check identity and outcome.
func FromEvent(event journal.Event) (Result, bool) {
	if event.Type != journal.EventClusterCheckCompleted {
		return Result{}, false
	}
	check, _ := event.Runner["check"].(string)
	outcome, _ := event.Runner["outcome"].(string)
	expires, _ := event.Runner["expiresAt"].(string)
	at, err := time.Parse(time.RFC3339Nano, expires)
	if check == "" || event.Time.IsZero() || err != nil || !at.After(event.Time) || (outcome != "pass" && outcome != "warn" && outcome != "fail") {
		return Result{}, false
	}
	return Result{Check: check, Outcome: outcome, CheckedAt: event.Time, ExpiresAt: at}, true
}

// Snapshot computes freshness on each read, including when no new event arrives.
func Snapshot(latest map[string]Result, now time.Time) []Result {
	results := make([]Result, 0, len(latest))
	for _, result := range latest {
		result.Stale = !now.Before(result.ExpiresAt)
		switch {
		case result.Outcome == "fail":
			result.State = "degraded"
		case result.Stale:
			result.State = "stale"
		case result.Outcome == "warn":
			result.State = "warning"
		default:
			result.State = "healthy"
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Check < results[j].Check })
	return results
}

// WriteStatus renders every recorded check, including recovery and freshness.
func WriteStatus(w io.Writer, results []Result) {
	for _, result := range results {
		freshness := "fresh"
		if result.Stale {
			freshness = "stale"
		}
		_, _ = fmt.Fprintf(w, "Cluster check %s: %s (last=%s, %s; checked=%s, expires=%s)\n", result.Check, result.State, result.Outcome, freshness, result.CheckedAt.UTC().Format(time.RFC3339), result.ExpiresAt.UTC().Format(time.RFC3339))
	}
}
