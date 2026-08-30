package readmodel

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestParseSourceAppliedIsStrict pins that a malformed header is an error.
//
// The failure mode this prevents is the worst available: a header that parsed
// leniently into a truncated position would report "already applied" for a
// bound nobody could read, turning a read-your-write guarantee into its
// opposite.
func TestParseSourceAppliedIsStrict(t *testing.T) {
	position, err := ParseSourceApplied("run-abc:47")
	if err != nil {
		t.Fatalf("a well-formed value must parse: %v", err)
	}
	if position.RunID != "run-abc" || position.JournalSeq != 47 {
		t.Errorf("parsed %+v, want run-abc:47", position)
	}

	for _, raw := range []string{
		"run-abc",         // no separator
		"run-abc:",        // no sequence
		":47",             // no run id
		"run-abc:xyz",     // non-numeric sequence
		"run-abc:47:junk", // trailing junk
		"run-abc:-1",      // negative
		"",
	} {
		if _, err := ParseSourceApplied(raw); err == nil {
			t.Errorf("%q parsed without error; a malformed bound must never read as satisfied", raw)
		}
	}
}

// TestSourceAppliedIsAbsentNotAnErrorForAnUnprojectedRun pins the case
// If-Source-Applied exists for.
//
// A run mutated microseconds ago is legitimately not projected yet. Treating
// that as an error would make the header fail exactly when it is needed.
func TestSourceAppliedIsAbsentNotAnErrorForAnUnprojectedRun(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	_, found, err := store.SourceApplied(ctx, fmt.Sprintf("%032x", 99))
	if err != nil {
		t.Errorf("an unprojected run returned an error: %v", err)
	}
	if found {
		t.Error("the store claims a position for a run it has never seen")
	}

	satisfied, err := store.SatisfiesSourceApplied(ctx, SourcePosition{
		RunID: fmt.Sprintf("%032x", 99), JournalSeq: 1,
	})
	if err != nil {
		t.Fatalf("satisfies: %v", err)
	}
	if satisfied {
		t.Error("an unprojected run reported its position as satisfied; a client would " +
			"proceed believing its write was visible")
	}
}

// TestSatisfiesSourceAppliedComparesPositions pins the comparison itself.
func TestSatisfiesSourceAppliedComparesPositions(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	runID := fmt.Sprintf("%032x", 5)

	startedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertRun(ctx, Projection{Run: RunRow{
		RunID: runID, Gaggle: "alpha", Workflow: "wf",
		Phase: journal.PhaseRunning, StartedAt: startedAt, LastSeq: 40,
	}}); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		required uint64
		want     bool
	}{{39, true}, {40, true}, {41, false}} {
		got, err := store.SatisfiesSourceApplied(ctx, SourcePosition{RunID: runID, JournalSeq: testCase.required})
		if err != nil {
			t.Fatal(err)
		}
		if got != testCase.want {
			t.Errorf("projected at 40, required %d: satisfied=%v want %v",
				testCase.required, got, testCase.want)
		}
	}
}

// TestReadStateReportsNoSweepAsDegraded pins the condition that is easiest to
// omit and most consequential.
//
// Until one sweep cycle has completed, NOTHING bounds the un-watermarked case —
// a run whose journal advanced but whose intake write was lost is undiscovered
// and undiscoverable. Reporting a small lagSeconds in that state would be a lie
// with a number attached.
func TestReadStateReportsNoSweepAsDegraded(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	state, err := store.ReadState(ctx, ReadStateInput{})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.LastSweepCompletedAt != nil {
		t.Fatal("a fresh store reported a completed sweep cycle")
	}
	if !slices.Contains(state.Degraded, DegradedNoSweepCompleted) {
		t.Errorf("degraded = %v, want it to contain %q; without a completed cycle nothing "+
			"bounds the un-watermarked staleness case", state.Degraded, DegradedNoSweepCompleted)
	}
	if state.Completeness != CompletenessComplete {
		t.Errorf("completeness = %q; degraded is not the same as incomplete — the answer is "+
			"still every row the store holds", state.Completeness)
	}
}

// TestReadStateLagTakesTheWorstBound pins that lag is a max, not a pick.
//
// Neither term alone is sufficient. Pending-intake age misses runs whose
// watermark was never written; sweep age misses runs that ARE watermarked but
// not yet projected. Reporting either one alone would understate staleness in
// the case the other covers.
func TestReadStateLagTakesTheWorstBound(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })

	// A completed sweep 120s ago, and a pending marker 300s old.
	cursor, err := store.SweepCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursor.LastCycleCompletedAt = now.Add(-120 * time.Second)
	if err := store.SaveSweepCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}

	state, err := store.ReadState(ctx, ReadStateInput{
		PendingIntake:   1,
		OldestPendingAt: now.Add(-300 * time.Second),
	})
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.LagSeconds < 300 {
		t.Errorf("lagSeconds = %.0f, want at least 300 (the older of the two bounds); "+
			"reporting the sweep's 120s alone would understate staleness for a run whose "+
			"watermark is still pending", state.LagSeconds)
	}

	// And the reverse: a stale sweep with no pending intake still reports lag.
	cursor.LastCycleCompletedAt = now.Add(-900 * time.Second)
	if err := store.SaveSweepCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadState(ctx, ReadStateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if state.LagSeconds < 900 {
		t.Errorf("lagSeconds = %.0f with no pending intake, want at least 900; the sweep's "+
			"age is the only bound on runs whose watermark was never written",
			state.LagSeconds)
	}
}

// TestReadStateFlagsIntakeWriteFailures pins that the residual window is
// observable rather than assumed away.
//
// Every failed intake write is a run whose freshness depends on the sweep rather
// than the projector. §7.2 is explicit that this must be surfaced: the journal
// append and the intake write are in different files and cannot be atomic, so
// the gap is real and permanent, and the only honest response is to count it.
func TestReadStateFlagsIntakeWriteFailures(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	state, err := store.ReadState(ctx, ReadStateInput{IntakeWriteFailures: 3})
	if err != nil {
		t.Fatal(err)
	}
	if state.IntakeWriteFailures != 3 {
		t.Errorf("intakeWriteFailures = %d, want 3", state.IntakeWriteFailures)
	}
	if !slices.Contains(state.Degraded, DegradedIntakeWriteFailure) {
		t.Errorf("degraded = %v, want it to contain %q", state.Degraded, DegradedIntakeWriteFailure)
	}
}

// TestReadStateCarriesEpochAndRetentionFloor pins the two fields a live client
// needs to decide whether its cursor is still valid.
func TestReadStateCarriesEpochAndRetentionFloor(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	state, err := store.ReadState(ctx, ReadStateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch == "" {
		t.Error("readState carries no epoch; a client could not detect a rebuild and would " +
			"wait forever for a sequence that will never arrive")
	}
	stored, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != stored.Epoch {
		t.Errorf("readState epoch %q does not match the store's %q", state.Epoch, stored.Epoch)
	}
	if state.MinChangeSeq != stored.MinChangeSeq {
		t.Errorf("readState minChangeSeq %d does not match the store's %d",
			state.MinChangeSeq, stored.MinChangeSeq)
	}
}

// TestDegradedIsNeverNil pins that the field serialises as [] rather than null.
//
// A client rendering `readState.degraded.length` gets a TypeError on null, and
// the portal renders this on every page — so a nil slice would break every view
// the moment anything degraded.
func TestDegradedIsNeverNil(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	state, err := store.ReadState(ctx, ReadStateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if state.Degraded == nil {
		t.Error("degraded is nil; it serialises as null and a client reading .length crashes")
	}
}

// TestReadStateReportsPartialOnAKnownProjectionGap is #2843's core fix.
//
// A run the projector failed to apply is missing from every list served from
// the projection, and the envelope used to report "complete" regardless —
// `CompletenessPartial` was defined and never assigned, so the portal showed a
// silently truncated runs list with a clean bill of health.
func TestReadStateReportsPartialOnAKnownProjectionGap(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	state, err := store.ReadState(ctx, ReadStateInput{ProjectFailures: 2})
	if err != nil {
		t.Fatal(err)
	}
	if state.Completeness != CompletenessPartial {
		t.Errorf("completeness = %q, want %q: runs the projector could not apply are "+
			"absent from the answer", state.Completeness, CompletenessPartial)
	}
	if !slices.Contains(state.Degraded, DegradedProjectFailure) {
		t.Errorf("degraded = %v, want it to contain %q", state.Degraded, DegradedProjectFailure)
	}
	if len(state.Missing) != 1 {
		t.Fatalf("missing = %+v, want exactly one named partition", state.Missing)
	}
	// §7.2: partial without a named partition and an expiry is silent omission
	// renamed.
	missing := state.Missing[0]
	if missing.Name != MissingProjectedRuns || missing.Reason == "" || missing.ExpectedBy.IsZero() {
		t.Errorf("missing partition = %+v, want a name, a reason and an expiry", missing)
	}
}

// TestReadStateStaysCompleteWithoutAKnownGap pins that lag alone is not
// partiality: a backlog waiting to be projected is stale, not missing, and
// flagging it partial would make the signal wallpaper.
func TestReadStateStaysCompleteWithoutAKnownGap(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	state, err := store.ReadState(ctx, ReadStateInput{PendingIntake: 4})
	if err != nil {
		t.Fatal(err)
	}
	if state.Completeness != CompletenessComplete {
		t.Errorf("completeness = %q, want %q", state.Completeness, CompletenessComplete)
	}
	if len(state.Missing) != 0 {
		t.Errorf("missing = %+v, want none", state.Missing)
	}
}

// TestReadStateLagCoversProjectionLag pins that a projector which has stopped
// widens the reported bound even when nothing is pending — the drained-intake
// case that a pending count alone cannot see.
func TestReadStateLagCoversProjectionLag(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	state, err := store.ReadState(ctx, ReadStateInput{ProjectionLagSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	if state.LagSeconds < 600 {
		t.Errorf("lagSeconds = %v, want at least the reported projection lag of 600",
			state.LagSeconds)
	}
}
