package main

import (
	"testing"
	"time"
)

func TestRetentionGraceDecisionIsSharedByBothDeletionPaths(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		state     retentionGraceState
		immediate bool
		want      bool
	}{
		{name: "not started", want: true},
		{name: "immediate before start", immediate: true, want: false},
		{name: "inside window", state: retentionGraceState{EnforceAt: now.Add(time.Minute)}, want: true},
		{name: "exact boundary", state: retentionGraceState{EnforceAt: now}, want: false},
		{name: "after window", state: retentionGraceState{EnforceAt: now.Add(-time.Minute)}, want: false},
		{name: "immediate inside window", state: retentionGraceState{EnforceAt: now.Add(time.Minute)}, immediate: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktree := retentionPassIsDryRun(tt.state, tt.immediate, now)
			telemetry := telemetryRetentionPassIsDryRun(tt.state, tt.immediate, now)
			if worktree != tt.want || telemetry != tt.want {
				t.Fatalf("dry-run decisions = worktree %v telemetry %v, want %v", worktree, telemetry, tt.want)
			}
		})
	}
}

func TestNormalizeRetentionGraceStateRepairsUnsafeClocks(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	validDetected := now.Add(-time.Hour)
	tests := []struct {
		name     string
		state    retentionGraceState
		repaired bool
	}{
		{name: "not started"},
		{name: "valid", state: retentionGraceState{DetectedAt: validDetected, EnforceAt: validDetected.Add(retentionGraceWindow)}},
		{name: "detected missing", state: retentionGraceState{EnforceAt: now.Add(time.Hour)}, repaired: true},
		{name: "enforcement missing", state: retentionGraceState{DetectedAt: validDetected}, repaired: true},
		{name: "wrong duration", state: retentionGraceState{DetectedAt: validDetected, EnforceAt: validDetected.Add(time.Hour)}, repaired: true},
		{name: "future detection", state: retentionGraceState{DetectedAt: now.Add(time.Hour), EnforceAt: now.Add(time.Hour).Add(retentionGraceWindow)}, repaired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.state.TotalRuns = 17
			got, repaired := normalizeRetentionGraceState(tt.state, now)
			if repaired != tt.repaired {
				t.Fatalf("repaired = %v, want %v", repaired, tt.repaired)
			}
			if got.TotalRuns != 17 {
				t.Fatalf("repair discarded subsystem state: %#v", got)
			}
			if repaired && (!got.DetectedAt.Equal(now) || !got.EnforceAt.Equal(now.Add(retentionGraceWindow))) {
				t.Fatalf("repair window = (%s, %s), want fresh window from %s", got.DetectedAt, got.EnforceAt, now)
			}
		})
	}
}
