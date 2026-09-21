package fleetdiagnostics

import (
	"errors"
	"strings"
	"time"
)

// WorkerHealth is recent polling evidence for the shared engine queue only.
// It does not establish process liveness or dispatch pod availability.
type WorkerHealth struct {
	Observation  string    `json:"observation"`
	Coverage     string    `json:"coverage"`
	ObservedAt   time.Time `json:"observedAt"`
	MissingCount *int64    `json:"missingCount,omitempty"`
}

func (f *fields) workerHealth(observed time.Time, count *int64) *WorkerHealth {
	if _, ok := f.values["workerObservation"]; !ok {
		return nil
	}
	w := &WorkerHealth{Observation: f.text("workerObservation", true), Coverage: f.text("workerCoverage", true), ObservedAt: f.stamp("workerObservedAt", true), MissingCount: count}
	valid := w.Coverage == "engine_workflow_activity_queue" && !w.ObservedAt.IsZero() && !w.ObservedAt.After(observed) && observed.Sub(w.ObservedAt) <= 5*time.Second
	switch w.Observation {
	case "unknown", "not_required":
		valid = valid && count == nil
	case "recent_poller":
		valid = valid && count != nil && *count == 0
	case "no_recent_poller":
		valid = valid && count != nil && *count == 1
	default:
		valid = false
	}
	if !valid {
		f.err = errors.New("invalid worker observation")
	}
	return w
}

// DecodeWorkerHealth validates only the closed worker subset of an offline
// observation; unrelated diagnostic payloads are never copied.
func DecodeWorkerHealth(attrs map[string]any, observed time.Time) (*WorkerHealth, error) {
	f := &fields{values: make(map[string]any)}
	for key, value := range attrs {
		if strings.HasPrefix(key, "worker") || key == "missingWorkerCount" {
			f.values[key] = value
		}
	}
	count := f.optionalNumber("missingWorkerCount")
	health := f.workerHealth(observed, count)
	return health, f.finish()
}

func workerReport(source *WorkerHealth, live bool) *WorkerHealth {
	if source == nil {
		return nil
	}
	result := *source
	if source.MissingCount != nil {
		count := *source.MissingCount
		result.MissingCount = &count
	}
	if !live {
		result.Observation = "unknown"
		result.MissingCount = nil
	}
	return &result
}
