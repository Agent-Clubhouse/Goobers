package main

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func ptr(t time.Time) *time.Time { return &t }

// An unapproved commenter can move stale/ageDays/lastMeaningfulActivityAt. The
// resulting signal must therefore carry that commenter's provenance, not the
// work item's maintainer grade, or the artifact gets admitted as maintainer
// input on the strength of arbitrary comment text (TBH-4).
func TestStalenessSignalCarriesWeakestContributingGrade(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	item := providers.WorkItem{
		ID:        "42",
		CreatedAt: ptr(now.Add(-90 * 24 * time.Hour)),
		Integrity: apiv1.IntegrityMaintainer,
	}
	comments := []providers.Comment{{
		Author: "stranger", CreatedAt: ptr(now.Add(-1 * time.Hour)),
		Integrity: apiv1.IntegrityUnapproved,
	}}

	signal, err := calculateBacklogStaleness(item, comments, "goobersbot", now,
		backlogStalenessPolicy{thresholdDays: 30})
	if err != nil {
		t.Fatalf("calculateBacklogStaleness: %v", err)
	}
	if signal.Integrity != apiv1.IntegrityUnapproved {
		t.Errorf("integrity = %q, want %q — the unapproved comment moved the signal",
			signal.Integrity, apiv1.IntegrityUnapproved)
	}
	if signal.Integrity.Meets(apiv1.IntegrityMaintainer) {
		t.Error("a signal moved by an unapproved comment must not satisfy a maintainer minimum")
	}
	// The comment really did drive the outcome, so the grade is not incidental.
	if signal.Stale {
		t.Error("recent unapproved comment should have reset staleness")
	}
}

// With only maintainer-graded inputs the signal stays maintainer — the check has
// to discriminate rather than collapse everything to unapproved.
func TestStalenessSignalKeepsMaintainerWhenAllInputsAre(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	item := providers.WorkItem{
		ID: "42", CreatedAt: ptr(now.Add(-90 * 24 * time.Hour)),
		Integrity: apiv1.IntegrityMaintainer,
	}
	signal, err := calculateBacklogStaleness(item, nil, "goobersbot", now,
		backlogStalenessPolicy{thresholdDays: 30})
	if err != nil {
		t.Fatalf("calculateBacklogStaleness: %v", err)
	}
	if signal.Integrity != apiv1.IntegrityMaintainer {
		t.Errorf("integrity = %q, want %q", signal.Integrity, apiv1.IntegrityMaintainer)
	}
	if !signal.Stale {
		t.Error("a 90-day-old item past a 30-day threshold should be stale")
	}
}

// An unlabeled comment collapses the signal rather than being skipped.
func TestStalenessSignalFailsClosedOnUnlabeledComment(t *testing.T) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	item := providers.WorkItem{
		ID: "42", CreatedAt: ptr(now.Add(-90 * 24 * time.Hour)),
		Integrity: apiv1.IntegrityMaintainer,
	}
	comments := []providers.Comment{{Author: "stranger", CreatedAt: ptr(now.Add(-1 * time.Hour))}}
	signal, err := calculateBacklogStaleness(item, comments, "goobersbot", now,
		backlogStalenessPolicy{thresholdDays: 30})
	if err != nil {
		t.Fatalf("calculateBacklogStaleness: %v", err)
	}
	if signal.Integrity != apiv1.IntegrityUnapproved {
		t.Errorf("integrity = %q, want %q for an unlabeled contributor",
			signal.Integrity, apiv1.IntegrityUnapproved)
	}
}
