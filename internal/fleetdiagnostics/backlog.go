package fleetdiagnostics

import (
	"errors"
	"strings"
	"time"
)

// BacklogHealth reports observed matching issue work. ClaimableCount, when
// present, is a verified lower bound of polled pending items that the
// authoritative claim sources showed admissible at ObservedAt; zero is
// reported only when every pending item was observed held or waiting. It is
// evidence, not an authorization guarantee. Attention means pending work
// without confirmed progress, not proof that a worker is stuck.
type BacklogHealth struct {
	State          string     `json:"state"`
	ReasonCode     string     `json:"reasonCode"`
	Coverage       string     `json:"coverage"`
	PendingCount   *int64     `json:"pendingCount,omitempty"`
	ClaimableCount *int64     `json:"claimableCount,omitempty"`
	ObservedAt     *time.Time `json:"observedAt,omitempty"`
}

func (f *fields) backlogHealth(observed time.Time) *BacklogHealth {
	if _, ok := f.values["backlogState"]; !ok {
		return nil
	}
	h := &BacklogHealth{State: f.text("backlogState", true), ReasonCode: f.text("backlogReasonCode", true), Coverage: f.text("backlogCoverage", true), PendingCount: f.optionalNumber("backlogPendingCount"), ClaimableCount: f.optionalNumber("backlogClaimableCount"), ObservedAt: f.optionalStamp("backlogObservedAt")}
	if !oneOf(h.State, "empty", "pending", "attention", "unknown") || !oneOf(h.Coverage, "complete", "partial", "unknown") || !oneOf(h.ReasonCode, "no_pending_work", "pending_observed", "pending_without_confirmed_progress", "observation_incomplete", "observation_stale", "operator_paused", "claimability_unknown", "claimable_observed", "claimable_without_confirmed_progress", "pending_held") {
		f.err = errors.New("invalid backlog condition")
	}
	if err := backlogClaimabilityError(h); err != nil {
		f.err = err
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
	if h.State == "attention" && !oneOf(h.ReasonCode, "pending_without_confirmed_progress", "claimable_without_confirmed_progress") {
		f.err = errors.New("invalid pending attention reason")
	}
	return h
}

// backlogClaimabilityError keeps claimability evidence tied to its reason: a
// positive count only with a claimable reason, zero only for a fully held
// backlog, and never more claimable items than pending ones.
func backlogClaimabilityError(h *BacklogHealth) error {
	claimable := h.ClaimableCount
	switch h.ReasonCode {
	case "claimable_observed", "claimable_without_confirmed_progress":
		if claimable == nil || *claimable <= 0 {
			return errors.New("unproven claimable backlog")
		}
		if h.ReasonCode == "claimable_observed" && h.State != "pending" || h.ReasonCode != "claimable_observed" && h.State != "attention" {
			return errors.New("invalid claimable backlog state")
		}
	case "pending_held":
		if claimable == nil || *claimable != 0 || h.State != "pending" || h.Coverage != "complete" {
			return errors.New("unproven held backlog")
		}
	default:
		if claimable != nil {
			return errors.New("claimable count without claimability evidence")
		}
	}
	if claimable != nil && (h.PendingCount == nil || *claimable < 0 || *claimable > *h.PendingCount) {
		return errors.New("invalid claimable count")
	}
	return nil
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
	if source.ClaimableCount != nil {
		claimable := *source.ClaimableCount
		result.ClaimableCount = &claimable
	}
	if source.ObservedAt != nil {
		at := *source.ObservedAt
		result.ObservedAt = &at
	}
	if !live || source.ObservedAt == nil || source.ObservedAt.After(now) || now.Sub(*source.ObservedAt) > time.Minute {
		result.State, result.ReasonCode, result.Coverage, result.PendingCount, result.ClaimableCount = "unknown", "observation_stale", "unknown", nil, nil
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
