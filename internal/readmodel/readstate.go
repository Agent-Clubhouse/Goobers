package readmodel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The readState envelope (#1927, design §7.2, §7.4).
//
// # Why freshness is reported rather than guaranteed
//
// The journal append and the intake write land in different files and cannot be
// made atomic. A writer killed between them leaves a run whose journal has
// advanced with no watermark to say so — discoverable only by the repair sweep.
//
// So the honest bound on staleness is not "the projector is caught up". It is:
//
//	max(age of the oldest pending intake marker, now - lastSweepCompletedAt)
//
// The second term is what covers the un-watermarked case, and it is why the
// sweep's cycle completion is reported rather than kept internal. IntakeWriteFailures
// makes the residual window observable instead of assumed away.
//
// # Why this is additive
//
// No existing field, ordering, or cursor semantic changes. A client that ignores
// readState behaves exactly as before, which is what makes it deployable ahead
// of the UI that renders it (#1928).

// Completeness says whether a response covers everything it claims to.
type Completeness string

const (
	// CompletenessComplete means every partition the request covers was
	// answered.
	CompletenessComplete Completeness = "complete"
	// CompletenessPartial means a NAMED partition is missing, with an expiry
	// expectation. §7.2: partial without a named partition and an expiry is
	// silent omission renamed, and becomes wallpaper the user learns to ignore.
	CompletenessPartial Completeness = "partial"
)

// MissingPartition names something a partial response could not include.
//
// Both fields are required by the contract test, not by convention: "partial"
// with no name tells a user nothing, and with no expiry gives them no reason to
// look again.
type MissingPartition struct {
	// Name identifies what is missing, e.g. "analytics" or "runs-root-2".
	Name string `json:"name"`
	// Reason is human-facing.
	Reason string `json:"reason"`
	// ExpectedBy is when it should be available. Required: a partial that never
	// resolves is a defect, and without a deadline nothing can detect one.
	ExpectedBy time.Time `json:"expectedBy"`
}

// SourcePosition is a run's position in its journal.
//
// This is what read-your-write compares against, NOT change.seq. A mutation
// cannot return a change sequence: the projector allocates it asynchronously
// after the mutation returns, and making the mutation wait would couple write
// availability to the derived store — exactly the dependency §3.1 removes.
type SourcePosition struct {
	RunID      string `json:"runId"`
	JournalSeq uint64 `json:"journalSeq"`
}

// ReadState is the freshness envelope carried by every read response.
type ReadState struct {
	Epoch      string `json:"epoch"`
	AppliedSeq uint64 `json:"appliedSeq"`
	// SourceApplied is the newest source position the projection has applied,
	// when one is known.
	SourceApplied *SourcePosition `json:"sourceApplied,omitempty"`
	ObservedAt    time.Time       `json:"observedAt"`
	// LagSeconds is the honest staleness bound described above.
	LagSeconds float64 `json:"lagSeconds"`
	// PendingIntake is how many watermarks are waiting to be projected.
	PendingIntake int `json:"pendingIntake"`
	// OldestPendingSourceAgeSeconds is how long the oldest of them has waited.
	OldestPendingSourceAgeSeconds float64 `json:"oldestPendingSourceAge"`
	// IntakeWriteFailures counts watermarks that could not be recorded. Each one
	// is a run whose freshness depends on the sweep rather than the projector.
	IntakeWriteFailures int `json:"intakeWriteFailures"`
	// LastSweepCompletedAt is when repair last finished a full cycle. Nil means
	// no cycle has completed since start, which is itself a degraded condition:
	// until one has, nothing bounds the un-watermarked case.
	LastSweepCompletedAt *time.Time   `json:"lastSweepCompletedAt,omitempty"`
	MinChangeSeq         uint64       `json:"minChangeSeq"`
	Completeness         Completeness `json:"completeness"`
	// Missing names the partitions a partial response omits.
	Missing []MissingPartition `json:"missing,omitempty"`
	// Degraded names conditions that do not make the answer wrong but do make it
	// less trustworthy than usual.
	Degraded []string `json:"degraded"`
}

// Degraded condition names. Stable strings: the UI keys off them, and a
// renamed condition silently stops rendering.
const (
	DegradedNoSweepCompleted   = "no_sweep_completed"
	DegradedIntakeWriteFailure = "intake_write_failure"
	DegradedProjectionLag      = "projection_lag"
	DegradedSweepStale         = "sweep_stale"
	// DegradedProjectFailure reports that the projector could not apply one or
	// more runs. Unlike lag, this is not a matter of waiting: those runs are
	// absent from the projection until repair rediscovers them.
	DegradedProjectFailure = "project_failure"
)

// MissingProjectedRuns names the partition a projection gap omits.
//
// One stable name rather than a name per run: the response cannot enumerate
// runs it does not have, and the operator question ("is the list missing
// anything?") is answered by the partition, not by the identities.
const MissingProjectedRuns = "projected-runs"

// sweepStaleAfter is how long since a completed sweep cycle before that itself
// becomes a degraded condition.
//
// Repair walks continuously at a fixed rate, so cycle time scales with history
// (H / rate) — a large instance legitimately takes a while. This threshold is
// not "the sweep is broken", it is "the un-watermarked staleness bound is now
// wide enough that you should know".
const sweepStaleAfter = 30 * time.Minute

// ReadStateInput is the runtime data the store cannot know by itself.
type ReadStateInput struct {
	PendingIntake        int
	OldestPendingAt      time.Time
	IntakeWriteFailures  int
	ProjectionLagSeconds float64
	// ProjectFailures counts runs the projector tried and failed to apply and
	// has NOT projected since. Each one is a KNOWN gap in the projection, which
	// is what makes the response partial rather than merely lagging. It must be
	// the open set rather than a lifetime total: a gap the repair sweep has
	// closed is no longer a gap, and a signal that never clears stops carrying
	// information.
	ProjectFailures int
}

// ReadState builds the envelope.
func (s *Store) ReadState(ctx context.Context, input ReadStateInput) (ReadState, error) {
	state, err := s.State(ctx)
	if err != nil {
		return ReadState{}, err
	}
	latest, err := s.LatestChangeSeq(ctx)
	if err != nil {
		return ReadState{}, err
	}
	cursor, err := s.SweepCursor(ctx)
	if err != nil {
		return ReadState{}, err
	}

	now := s.now()
	out := ReadState{
		Epoch:               state.Epoch,
		AppliedSeq:          latest,
		ObservedAt:          now,
		PendingIntake:       input.PendingIntake,
		IntakeWriteFailures: input.IntakeWriteFailures,
		MinChangeSeq:        state.MinChangeSeq,
		Completeness:        CompletenessComplete,
		Degraded:            []string{},
	}
	if !cursor.LastCycleCompletedAt.IsZero() {
		completed := cursor.LastCycleCompletedAt
		out.LastSweepCompletedAt = &completed
	}
	if !input.OldestPendingAt.IsZero() {
		out.OldestPendingSourceAgeSeconds = now.Sub(input.OldestPendingAt).Seconds()
	}

	// The lag bound. Both terms matter and neither alone is sufficient:
	// pending-intake age misses runs whose watermark was never written, and
	// sweep age misses runs that ARE watermarked but not yet projected.
	out.LagSeconds = maxFloat(out.OldestPendingSourceAgeSeconds, input.ProjectionLagSeconds)
	if out.LastSweepCompletedAt != nil {
		out.LagSeconds = maxFloat(out.LagSeconds, now.Sub(*out.LastSweepCompletedAt).Seconds())
	}

	if out.LastSweepCompletedAt == nil {
		// No cycle has completed since start, so nothing bounds the
		// un-watermarked case at all. This is reported rather than folded into
		// LagSeconds as an infinity, because a number a client renders must stay
		// a number.
		out.Degraded = append(out.Degraded, DegradedNoSweepCompleted)
	} else if now.Sub(*out.LastSweepCompletedAt) > sweepStaleAfter {
		out.Degraded = append(out.Degraded, DegradedSweepStale)
	}
	if input.IntakeWriteFailures > 0 {
		out.Degraded = append(out.Degraded, DegradedIntakeWriteFailure)
	}
	if input.PendingIntake > 0 {
		out.Degraded = append(out.Degraded, DegradedProjectionLag)
	}
	if input.ProjectFailures > 0 {
		out.Degraded = append(out.Degraded, DegradedProjectFailure)
	}
	// A known gap makes the answer partial, not merely stale: runs the projector
	// failed to apply, and runs whose watermark was never recorded, are absent
	// from every list until repair rediscovers them. Reporting "complete" here
	// is what let a silently truncated runs list look healthy.
	//
	// Both terms are open counts, so this clears itself: once the failing runs
	// project and the watermark writes succeed, completeness returns to
	// "complete". IntakeWriteFailures currently has no producer in readservice —
	// it is carried for callers that track watermark write errors themselves, so
	// in the daemon today the gap is the projector's open failures alone.
	if gap := input.ProjectFailures + input.IntakeWriteFailures; gap > 0 {
		out.Completeness = CompletenessPartial
		out.Missing = append(out.Missing, MissingPartition{
			Name: MissingProjectedRuns,
			Reason: fmt.Sprintf(
				"%d run(s) are not projected: %d failed to apply, %d have no recorded watermark; the repair sweep will rediscover them",
				gap, input.ProjectFailures, input.IntakeWriteFailures),
			// The repair sweep is the mechanism that closes this, so its own
			// staleness threshold is the honest deadline: past it, the gap is a
			// defect rather than work in progress.
			ExpectedBy: now.Add(sweepStaleAfter),
		})
	}
	return out, nil
}

// SourceApplied reports the projected source position for a run.
//
// This is what If-Source-Applied is compared against. Absent means the
// projection has not seen the run at all, which a caller must treat as "not yet
// applied" rather than as an error — a run mutated microseconds ago is legitimately
// not projected yet, and that is the entire case the header exists for.
func (s *Store) SourceApplied(ctx context.Context, runID string) (SourcePosition, bool, error) {
	row, found, err := s.GetRun(ctx, runID)
	if err != nil || !found {
		return SourcePosition{}, false, err
	}
	return SourcePosition{RunID: runID, JournalSeq: row.LastSeq}, true, nil
}

// SatisfiesSourceApplied reports whether the projection has caught up to a
// required source position.
func (s *Store) SatisfiesSourceApplied(ctx context.Context, required SourcePosition) (bool, error) {
	applied, found, err := s.SourceApplied(ctx, required.RunID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return applied.JournalSeq >= required.JournalSeq, nil
}

// ParseSourceApplied decodes an If-Source-Applied header value, "<runID>:<seq>".
//
// Split on the LAST colon rather than the first, and parsed strictly: a
// malformed header must be an error, never a silently truncated position that
// reads as "already applied". That failure mode would turn a read-your-write
// guarantee into its opposite — the client asks for a bound and is told it was
// met by a value nobody could parse.
//
// (Written by hand rather than with Sscanf: "%31s:%d" does not work, because
// %s is greedy and consumes the colon along with the run id, so even a
// well-formed value fails to parse. Found by probing the parser rather than
// trusting the format string.)
func ParseSourceApplied(raw string) (SourcePosition, error) {
	malformed := func() (SourcePosition, error) {
		return SourcePosition{}, fmt.Errorf(
			"readmodel: malformed source position %q, want <runID>:<seq>", raw)
	}
	colon := strings.LastIndex(raw, ":")
	if colon <= 0 || colon == len(raw)-1 {
		return malformed()
	}
	seq, err := strconv.ParseUint(raw[colon+1:], 10, 64)
	if err != nil {
		return malformed()
	}
	return SourcePosition{RunID: raw[:colon], JournalSeq: seq}, nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
