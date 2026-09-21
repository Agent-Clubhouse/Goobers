package fleetdiagnostics

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type fields struct {
	values map[string]any
	err    error
}

func newFields(values map[string]any) *fields {
	f := &fields{}
	if len(values) > 48 {
		f.err = errors.New("too many diagnostic fields")
		return f
	}
	f.values = make(map[string]any, len(values))
	for k, v := range values {
		f.values[k] = v
	}
	if f.number("schemaVersion", true) != SchemaVersion {
		f.err = errors.New("unsupported diagnostic schema")
	}
	if _, ok := f.values["goobers.diagnostics.schema_version"]; ok && f.number("goobers.diagnostics.schema_version", true) != SchemaVersion {
		f.err = errors.New("unsupported transport schema")
	}
	return f
}
func (f *fields) take(key string, required bool) any {
	v, ok := f.values[key]
	delete(f.values, key)
	if !ok && required && f.err == nil {
		f.err = fmt.Errorf("missing %s", key)
	}
	return v
}
func (f *fields) text(key string, required bool) string {
	value := f.take(key, required)
	if value == nil && !required {
		return ""
	}
	s, ok := value.(string)
	if !ok || len(s) > 256 || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 || required && s == "" {
		f.err = fmt.Errorf("invalid %s", key)
	}
	return s
}
func (f *fields) number(key string, required bool) int64 {
	value := f.take(key, required)
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || v > 1<<53 {
			f.err = fmt.Errorf("invalid %s", key)
			return 0
		}
		n = int64(v)
	default:
		f.err = fmt.Errorf("invalid %s", key)
	}
	if n < 0 {
		f.err = fmt.Errorf("negative %s", key)
	}
	return n
}
func (f *fields) optionalNumber(key string) *int64 {
	if _, ok := f.values[key]; !ok {
		return nil
	}
	v := f.number(key, true)
	return &v
}
func (f *fields) stamp(key string, required bool) time.Time {
	raw := f.text(key, required)
	if raw == "" && !required {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.IsZero() {
		f.err = fmt.Errorf("invalid %s", key)
	}
	return t.UTC()
}
func (f *fields) optionalStamp(key string) *time.Time {
	if _, ok := f.values[key]; !ok {
		return nil
	}
	t := f.stamp(key, true)
	return &t
}
func (f *fields) identity() Identity {
	return Identity{Organization: f.text("organization", false), Environment: f.text("environment", false), DeploymentID: f.text("deploymentId", true), InstanceID: f.text("instanceId", true), GaggleID: f.text("gaggleId", false), OwnerRef: f.text("ownerRef", false), Component: f.text("component", true)}
}
func (f *fields) window() Window {
	w := Window{BootID: f.text("bootId", true), BootStartedAt: f.stamp("bootStartedAt", true), Sequence: f.number("sequence", true), ObservedAt: f.stamp("observedAt", true), WindowStart: f.stamp("windowStart", true), Coverage: f.text("windowCoverage", true)}
	if !oneOf(w.Coverage, "complete", "partial", "unknown") || w.Sequence < 1 || w.WindowStart.Before(w.BootStartedAt) || w.ObservedAt.Before(w.WindowStart) {
		f.err = errors.New("invalid observation window")
	}
	return w
}
func (f *fields) finish() error {
	if f.err != nil {
		return f.err
	}
	if len(f.values) > 0 {
		return errors.New("unknown diagnostic fields")
	}
	return nil
}
func oneOf(v string, choices ...string) bool {
	for _, choice := range choices {
		if v == choice {
			return true
		}
	}
	return false
}

// DecodeHeartbeat validates the closed scalar wire contract. Unknown fields or
// versions fail closed rather than silently presenting a partial observation.
func DecodeHeartbeat(attrs map[string]any) (Heartbeat, error) {
	f := newFields(attrs)
	h := Heartbeat{Identity: f.identity(), Window: f.window(), State: f.text("state", true), ReasonCode: f.text("reasonCode", true), LastUsefulProgressAt: f.optionalStamp("lastUsefulProgressAt"), OldestEligibleAt: f.optionalStamp("oldestEligibleAt"), EligibleCount: f.optionalNumber("eligibleCount"), InflightCount: f.optionalNumber("inflightCount"), AdmissionLimit: f.optionalNumber("admissionLimit"), MissingWorkerCount: f.optionalNumber("missingWorkerCount"), RetryCount: f.optionalNumber("retryCount"), NoWorkCount: f.optionalNumber("noWorkCount"), Version: f.text("version", false), BuildCommit: f.text("buildCommit", false), Channel: f.text("channel", false), Platform: f.text("platform", false)}
	if !oneOf(h.State, "productive", "idle", "paused", "waiting", "backoff", "stalled", "unknown") {
		f.err = errors.New("invalid health state")
	}
	if !oneOf(h.ReasonCode, "progress_observed", "no_eligible_work", "operator_paused", "waiting_for_gate", "retry_backoff", "no_progress", "worker_unavailable", "admission_saturated", "repeated_no_work", "provider_throttled", "cleanup_failure", "storage_failure", "observation_incomplete", "startup", "observation_unavailable", "clock_skew", "observation_stale", "stage_within_deadline", "stage_deadline_unknown", "work_eligibility_unknown", "progress_window_unknown", "eligible_within_threshold", "progress_unconfirmed") {
		f.err = errors.New("invalid health reason")
	}
	for _, t := range []*time.Time{h.LastUsefulProgressAt, h.OldestEligibleAt} {
		if t != nil && t.After(h.ObservedAt) {
			f.err = errors.New("evidence timestamp exceeds observation")
		}
	}
	h.RequiredMCP = f.mcpHealth(h.ObservedAt)
	h.Worker = f.workerHealth(h.ObservedAt, h.MissingWorkerCount)
	return h, f.finish()
}

// DecodeFeatureUsage validates a bounded feature identifier and absolute window.
func DecodeFeatureUsage(attrs map[string]any) (FeatureUsage, error) {
	f := newFields(attrs)
	u := FeatureUsage{Identity: f.identity(), Window: f.window(), WindowEnd: f.stamp("windowEnd", true), FeatureID: f.text("featureId", true), Count: f.optionalNumber("count")}
	v := f.take("configured", true)
	var ok bool
	u.Configured, ok = v.(bool)
	if !ok || !oneOf(u.FeatureID, FeatureIDs()...) || u.WindowEnd.Before(u.WindowStart) || u.WindowEnd.After(u.ObservedAt) {
		f.err = errors.New("invalid feature observation")
	}
	return u, f.finish()
}
