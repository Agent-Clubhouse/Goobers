package engine

import (
	"errors"
	"sync"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/telemetry"
)

// AttemptUsage carries trusted adapter usage through activity history,
// including failed attempts. Unavailable marks known execution whose trusted
// usage could not be recovered. Envelope metrics are agent-authored and never
// charged.
type AttemptUsage struct {
	Metrics     map[string]float64 `json:"metrics,omitempty"`
	Reported    bool               `json:"reported,omitempty"`
	Unavailable bool               `json:"unavailable,omitempty"`
}
type activityUsageCollector struct {
	mu    sync.Mutex
	usage AttemptUsage
}

func (c *activityUsageCollector) report(metrics map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage.Reported = true
	if c.usage.Metrics == nil {
		c.usage.Metrics = make(map[string]float64)
	}
	for name, value := range metrics {
		if telemetry.IsCanonicalAgentUsageMetric(name) {
			c.usage.Metrics[name] = value
		}
	}
}
func (c *activityUsageCollector) snapshot() AttemptUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := AttemptUsage{Reported: c.usage.Reported, Metrics: make(map[string]float64)}
	for name, value := range c.usage.Metrics {
		out.Metrics[name] = value
	}
	return out
}

// Keep retry-at at detail zero (the existing infrastructure error contract);
// usage occupies detail one and survives Temporal's error serialization.
func agenticUsageError(err error, usage AttemptUsage) error {
	var app *temporal.ApplicationError
	if !errors.As(err, &app) {
		return err
	}
	var retryAt time.Time
	if app.HasDetails() {
		_ = app.Details(&retryAt)
	}
	return temporal.NewApplicationErrorWithOptions(app.Message(), app.Type(), temporal.ApplicationErrorOptions{
		NonRetryable: app.NonRetryable(), Cause: app.Unwrap(), Details: []interface{}{retryAt, usage},
	})
}
func dispatchFailureUsage(err error) AttemptUsage {
	for current := err; current != nil; current = errors.Unwrap(current) {
		var app *temporal.ApplicationError
		if !errors.As(current, &app) || !app.HasDetails() {
			continue
		}
		var retryAt time.Time
		var evidence dispatchFailureEvidence
		if readDispatchFailureDetails(app, &retryAt, &evidence) == nil &&
			evidence.SchemaVersion == dispatchFailureEvidenceVersion {
			if evidence.Usage != nil {
				return *evidence.Usage
			}
			return AttemptUsage{Unavailable: evidence.UsageUnavailable}
		}
		var usage AttemptUsage
		if readDispatchFailureDetails(app, &retryAt, &usage) == nil {
			return usage
		}
	}
	return AttemptUsage{}
}
