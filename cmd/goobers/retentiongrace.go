package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

// retentionGraceWindow is how long a retention policy reports what it would
// delete before it deletes anything, the first time it finds real candidates
// (#3056's safe first-enable, #4253). It matches the telemetry side's window:
// an operator who upgrades into an opt-out default gets a week to see the
// candidate list and object before anything is removed.
const retentionGraceWindow = 7 * 24 * time.Hour

// retentionGraceState records where a retention policy is in its first-enable
// grace window, and what its last pass did. The policy projection is derived;
// PendingTelemetryPass is an authoritative deletion manifest until its
// bounded summary reaches the instance journal.
//
// Both worktree and telemetry retention persist this common shape. The final
// fields are telemetry-only status details; omitempty keeps them out of the
// worktree state document.
type retentionGraceState struct {
	Schema string `json:"schema"`
	// DetectedAt/EnforceAt stay zero until the first pass that actually finds
	// data exceeding policy starts the window; EnforceAt is when real deletion
	// begins (DetectedAt + retentionGraceWindow).
	DetectedAt     time.Time `json:"detectedAt,omitempty"`
	EnforceAt      time.Time `json:"enforceAt,omitempty"`
	LastPassAt     time.Time `json:"lastPassAt"`
	LastPassDryRun bool      `json:"lastPassDryRun"`
	CandidateCount int       `json:"candidateCount"`
	PrunedCount    int       `json:"prunedCount"`
	// TotalRuns/OldestRetainedAt are telemetry retention's last-pass view,
	// used to report the effective history cutoff rather than only a count.
	TotalRuns        int       `json:"totalRuns,omitempty"`
	OldestRetainedAt time.Time `json:"oldestRetainedAt,omitempty"`
	// EnforceAcknowledged and LargeFirstEnforceBlocked belong to telemetry's
	// additional large-first-enforcement safety gate.
	EnforceAcknowledged      bool `json:"enforceAcknowledged,omitempty"`
	LargeFirstEnforceBlocked bool `json:"largeFirstEnforceBlocked,omitempty"`
	// PendingTelemetryPass is the durable outbox and exact deletion manifest
	// for an automatic telemetry prune whose bounded instance-journal summary
	// has not been acknowledged. Worktree retention never sets it.
	PendingTelemetryPass *telemetryRetentionPass `json:"pendingTelemetryPass,omitempty"`
}

type telemetryRetentionPass struct {
	ID               string             `json:"id"`
	Phase            string             `json:"phase"`
	At               time.Time          `json:"at"`
	DryRun           bool               `json:"dryRun"`
	CandidateCount   int                `json:"candidateCount"`
	EnforceAt        time.Time          `json:"enforceAt,omitempty"`
	PolicyWindow     time.Duration      `json:"policyWindow,omitempty"`
	PolicyMaxRuns    int                `json:"policyMaxRuns,omitempty"`
	TotalRuns        int                `json:"totalRuns,omitempty"`
	OldestRetainedAt time.Time          `json:"oldestRetainedAt,omitempty"`
	Candidates       []retention.Result `json:"candidates,omitempty"`
}

// normalizeRetentionGraceState rejects clocks that cannot have been produced
// by recordRetentionPass. Resetting the window from now is the conservative
// repair: malformed or future state can delay deletion, but can never make it
// happen earlier than a fresh first-enable grace window.
func normalizeRetentionGraceState(state retentionGraceState, now time.Time) (retentionGraceState, bool) {
	detectedMissing := state.DetectedAt.IsZero()
	enforceMissing := state.EnforceAt.IsZero()
	valid := detectedMissing == enforceMissing
	if !detectedMissing {
		valid = valid && !state.DetectedAt.After(now) && state.EnforceAt.Equal(state.DetectedAt.Add(retentionGraceWindow))
	}
	if valid {
		return state, false
	}
	state.DetectedAt = now
	state.EnforceAt = now.Add(retentionGraceWindow)
	return state, true
}

func retentionGraceStatePath(layout instance.Layout, file string) string {
	return filepath.Join(layout.SchedulerDir(), file)
}

// readRetentionGraceState returns ok=false (zero state, nil error) when no
// pass has ever recorded one — a fresh instance, or one that predates the file.
func readRetentionGraceState(layout instance.Layout, file string) (state retentionGraceState, ok bool, err error) {
	data, err := os.ReadFile(retentionGraceStatePath(layout, file))
	if os.IsNotExist(err) {
		return retentionGraceState{}, false, nil
	}
	if err != nil {
		return retentionGraceState{}, false, fmt.Errorf("retention: read state %s: %w", file, err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return retentionGraceState{}, false, fmt.Errorf("retention: decode state %s: %w", file, err)
	}
	return state, true, nil
}

func writeRetentionGraceState(layout instance.Layout, file, schema string, state retentionGraceState) error {
	state.Schema = schema
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("retention: encode state %s: %w", file, err)
	}
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		return fmt.Errorf("retention: create scheduler dir: %w", err)
	}
	if err := journal.WriteFileAtomic(retentionGraceStatePath(layout, file), data, 0o644); err != nil {
		return fmt.Errorf("retention: write state %s: %w", file, err)
	}
	return nil
}

// retentionPassIsDryRun decides whether this pass reports or deletes.
//
// It keys off EnforceAt rather than "does a state file exist". The state file
// is written on every pass, including a dry pass that found zero candidates,
// so gating on file existence lets one harmless empty pass permanently satisfy
// the "first pass" check — and the next pass to find real candidates, however
// much later, then enforces with no grace period at all. That was the bug
// #4801 fixed on the telemetry side; this side is built with it already fixed.
//
// An instance with nothing to prune therefore stays harmlessly dry forever
// (a real pass would do nothing differently), and only starts the window the
// first time it finds real candidates.
func retentionPassIsDryRun(state retentionGraceState, immediate bool, now time.Time) bool {
	if immediate {
		return false
	}
	withinGrace := !state.EnforceAt.IsZero() && now.Before(state.EnforceAt)
	return withinGrace || state.EnforceAt.IsZero()
}

// recordRetentionPass advances the grace window and the last-pass record.
// Starting the window is what a dry pass that found real candidates does.
func recordRetentionPass(state retentionGraceState, dryRun, immediate bool, candidates int, now time.Time) retentionGraceState {
	if dryRun && !immediate && state.EnforceAt.IsZero() && candidates > 0 {
		state.DetectedAt = now
		state.EnforceAt = now.Add(retentionGraceWindow)
	}
	state.LastPassAt = now
	state.LastPassDryRun = dryRun
	state.CandidateCount = candidates
	if !dryRun {
		state.PrunedCount = candidates
	}
	return state
}
