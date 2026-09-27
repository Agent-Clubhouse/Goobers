package fleetdiagnostics

import (
	"errors"
	"strings"
	"time"
)

// BacklogHealth reports observed matching issue work. Claim availability is
// always unknown; attention means pending work without confirmed progress,
// not proof that an issue could have been claimed or that a worker is stuck.
type BacklogHealth struct {
	State        string     `json:"state"`
	ReasonCode   string     `json:"reasonCode"`
	Coverage     string     `json:"coverage"`
	PendingCount *int64     `json:"pendingCount,omitempty"`
	ObservedAt   *time.Time `json:"observedAt,omitempty"`
}

func (f *fields) backlogHealth(observed time.Time) *BacklogHealth {
	if _, ok := f.values["backlogState"]; !ok {
		return nil
	}
	h := &BacklogHealth{State: f.text("backlogState", true), ReasonCode: f.text("backlogReasonCode", true), Coverage: f.text("backlogCoverage", true), PendingCount: f.optionalNumber("backlogPendingCount"), ObservedAt: f.optionalStamp("backlogObservedAt")}
	if !oneOf(h.State, "empty", "pending", "attention", "unknown") || !oneOf(h.Coverage, "complete", "partial", "unknown") || !oneOf(h.ReasonCode, "no_pending_work", "pending_observed", "pending_without_confirmed_progress", "observation_incomplete", "observation_stale", "operator_paused", "claimability_unknown") {
		f.err = errors.New("invalid backlog condition")
	}
	if h.ObservedAt != nil && h.ObservedAt.After(observed) {
		f.err = errors.New("future backlog observation")
	}
	if h.PendingCount != nil && *h.PendingCount == 0 && h.Coverage != "complete" {
		f.err = errors.New("incomplete backlog zero")
	}
	if h.State == "empty" && (h.Coverage != "complete" || h.PendingCount == nil || *h.PendingCount != 0 || h.ObservedAt == nil || h.ReasonCode != "no_pending_work") {
		f.err = errors.New("unproven empty backlog")
	}
	if oneOf(h.State, "pending", "attention") && (h.PendingCount == nil || *h.PendingCount <= 0 || h.ObservedAt == nil) {
		f.err = errors.New("unproven pending backlog")
	}
	if h.State == "attention" && h.ReasonCode != "pending_without_confirmed_progress" {
		f.err = errors.New("invalid pending attention reason")
	}
	return h
}
func backlogReport(source *BacklogHealth, live bool, now time.Time) *BacklogHealth {
	if source == nil {
		return nil
	}
	result := *source
	if source.PendingCount != nil {
		count := *source.PendingCount
		result.PendingCount = &count
	}
	if source.ObservedAt != nil {
		at := *source.ObservedAt
		result.ObservedAt = &at
	}
	if !live || source.ObservedAt == nil || source.ObservedAt.After(now) || now.Sub(*source.ObservedAt) > time.Minute {
		result.State, result.ReasonCode, result.Coverage, result.PendingCount = "unknown", "observation_stale", "unknown", nil
	}
	return &result
}

// DecodeBacklogHealth validates the allowlisted pending-work subset for offline
// reports. Private work content and unrecognized backlog fields are excluded.
func DecodeBacklogHealth(attrs map[string]any, observed time.Time) (*BacklogHealth, error) {
	if len(attrs) > 48 || observed.IsZero() {
		return nil, errors.New("invalid backlog observation envelope")
	}
	f := &fields{values: make(map[string]any)}
	for key, value := range attrs {
		if strings.HasPrefix(key, "backlog") {
			f.values[key] = value
		}
	}
	health := f.backlogHealth(observed)
	return health, f.finish()
}
