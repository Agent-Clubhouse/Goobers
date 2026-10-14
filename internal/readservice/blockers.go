package readservice

import (
	"context"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// OperatorBlocker is one entry of a run's blocker list with its provenance:
// what reported it, when the evidence behind it was observed, and how old that
// evidence was when this read was served (#5404). Message is the display text
// and is identical to the matching potentialBlockers entry.
type OperatorBlocker struct {
	// Code is stable per kind of blocker: the ErrorDetail code for a recorded
	// run error, otherwise one of the BlockerCode* values. A run lists at
	// most one blocker per (Source, Code).
	Code    string `json:"code"`
	Source  string `json:"source"`
	Message string `json:"message"`
	// ObservedAt is when the evidence was recorded. Absent when it is not
	// known (an unstamped event, or a read-model row projected by an older
	// build); such a blocker is never treated as superseded.
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	AgeMillis  *int64     `json:"ageMillis,omitempty"`
}

// Blocker sources: every producer of a run blocker and what retracts it
// (#5404). A blocker without a clear path stays on status after its condition
// ends; a clear path that is too broad hides a live outage. So clear paths are explicit, and a recorded run error is
// retracted by instance evidence ONLY when its code is registered in
// instanceClearedErrorCodes.
//
//   - error (persisted: the run's latest ErrorDetail, read-model LatestError):
//     superseded by a later error in the same run. A code registered in
//     instanceClearedErrorCodes is also retracted by a later
//     blocker.cleared instance-journal record for that code. Every other code
//     is the run's last recorded failure and has no automatic clear — it
//     leaves status only with the run itself.
//   - review (persisted: the latest review verdict, or its unreadable
//     rationale artifact): superseded by the next review gate evaluation;
//     cleared by a passing one.
//   - heartbeat (live: stage heartbeat older than the liveness timeout):
//     recomputed on every read; clears when a heartbeat arrives or the run
//     ends.
//   - claim (live: provider claim marker and local lease disagree):
//     recomputed on every read against the provider; clears when they agree.
const (
	BlockerSourceError     = "error"
	BlockerSourceReview    = "review"
	BlockerSourceHeartbeat = "heartbeat"
	BlockerSourceClaim     = "claim"
)

// Codes for blockers that are not a recorded run error.
const (
	BlockerCodeReviewEvidenceUnreadable = "review_evidence_unreadable"
	BlockerCodeReviewNotPassed          = "review_not_passed"
	BlockerCodeHeartbeatStale           = "stage_heartbeat_stale"
	BlockerCodeClaimMarkerDrift         = "claim_marker_drift"
)

// instanceClearedErrorCodes are the run-error codes whose condition belongs to
// the instance rather than to the run, with the evidence that retracts them.
//
// recovery_observation_failed: the observation reads the shared recovery
// inventory (#5354), and a terminal run never observes again, so only another
// run's later successful observation can show the condition has cleared. The
// runner reports every observation outcome; the daemon journals a
// blocker.cleared record on the first success of each process lifetime and
// after any failure it saw (cmd/goobers blockerclear.go). A failure recorded
// after the newest such record stays a blocker.
var instanceClearedErrorCodes = map[string]string{
	journal.ErrorCodeRecoveryObservationFailed: "a later successful post-cleanup recovery observation",
}

// addBlocker records b, replacing any earlier blocker of the same source and
// code in place so a run never lists the same blocker twice, and keeps
// PotentialBlockers in step.
func (o *OperatorRunSummary) addBlocker(b OperatorBlocker, now time.Time) {
	if b.ObservedAt != nil {
		at := *b.ObservedAt
		b.ObservedAt = &at
		age := max(now.Sub(at).Milliseconds(), 0)
		b.AgeMillis = &age
	}
	index := slices.IndexFunc(o.Blockers, func(existing OperatorBlocker) bool {
		return existing.Source == b.Source && existing.Code == b.Code
	})
	if index >= 0 {
		o.Blockers[index] = b
	} else {
		o.Blockers = append(o.Blockers, b)
	}
	o.syncPotentialBlockers()
}

// removeBlockers drops every blocker drop reports true for.
func (o *OperatorRunSummary) removeBlockers(drop func(OperatorBlocker) bool) {
	if !slices.ContainsFunc(o.Blockers, drop) {
		return
	}
	o.Blockers = slices.DeleteFunc(o.Blockers, drop)
	if len(o.Blockers) == 0 {
		o.Blockers = nil
	}
	o.syncPotentialBlockers()
}

func (o *OperatorRunSummary) syncPotentialBlockers() {
	messages := make([]string, 0, len(o.Blockers))
	for _, blocker := range o.Blockers {
		messages = append(messages, blocker.Message)
	}
	o.PotentialBlockers = messages
}

// addRecordedBlockers adds the blockers a run's own journal records — an
// unreadable latest review rationale, the latest error, a non-passing latest
// review — in the order status has always listed them. The journal-derived
// and read-model summaries share it so the two paths cannot drift: only the
// LATEST review counts on both, so a later review supersedes an earlier one's
// unreadable rationale (#5404).
func (o *OperatorRunSummary) addRecordedBlockers(reviewProblem string, reviewAt, latestErrorAt *time.Time, now time.Time) {
	if reviewProblem != "" {
		o.addBlocker(OperatorBlocker{
			Code: BlockerCodeReviewEvidenceUnreadable, Source: BlockerSourceReview,
			Message: reviewProblem, ObservedAt: reviewAt,
		}, now)
	}
	if o.LatestError != nil {
		o.addBlocker(errorBlocker(o.LatestError, latestErrorAt), now)
	}
	if blocker, ok := reviewBlocker(o.Review, reviewAt); ok {
		o.addBlocker(blocker, now)
	}
}

// errorBlocker is the blocker a recorded run error contributes.
func errorBlocker(detail *journal.ErrorDetail, observedAt *time.Time) OperatorBlocker {
	return OperatorBlocker{
		Code:       detail.Code,
		Source:     BlockerSourceError,
		Message:    detail.Code + ": " + detail.Message,
		ObservedAt: observedAt,
	}
}

// reviewBlocker is the blocker a non-passing review verdict contributes.
func reviewBlocker(review *OperatorReview, observedAt *time.Time) (OperatorBlocker, bool) {
	if review == nil || review.Verdict == "" || review.Verdict == "pass" || review.Verdict == "approve" {
		return OperatorBlocker{}, false
	}
	return OperatorBlocker{
		Code:       BlockerCodeReviewNotPassed,
		Source:     BlockerSourceReview,
		Message:    "review " + review.Verdict + ": " + review.Rationale,
		ObservedAt: observedAt,
	}, true
}

// blockerSuperseded reports whether clears retract b: b is a recorded run
// error whose code is instance-cleared, its observed-at time is known, and a
// clear for that code was recorded after it. Unknown times never clear.
func blockerSuperseded(b OperatorBlocker, clears map[string]time.Time) bool {
	if b.Source != BlockerSourceError || b.ObservedAt == nil {
		return false
	}
	if _, registered := instanceClearedErrorCodes[b.Code]; !registered {
		return false
	}
	cleared, ok := clears[b.Code]
	return ok && cleared.After(*b.ObservedAt)
}

// decorateBlockerClears retracts run blockers the instance journal has since
// recorded as cleared. Failing to read the instance journal leaves every
// blocker in place: an unreadable clear record must never hide one.
func (s *Local) decorateBlockerClears(ctx context.Context, runs []RunSummary) {
	if len(runs) == 0 || !anyInstanceClearable(runs) {
		return
	}
	projection, err := s.workflowSchedulerSnapshot(ctx)
	if err != nil || len(projection.blockerClears) == 0 {
		return
	}
	for i := range runs {
		runs[i].Operator.removeBlockers(func(b OperatorBlocker) bool {
			return blockerSuperseded(b, projection.blockerClears)
		})
	}
}

func anyInstanceClearable(runs []RunSummary) bool {
	for _, run := range runs {
		for _, blocker := range run.Operator.Blockers {
			if _, ok := instanceClearedErrorCodes[blocker.Code]; ok && blocker.Source == BlockerSourceError {
				return true
			}
		}
	}
	return false
}

// applyBlockerCleared folds a blocker.cleared record: the newest clear per code.
func (s *instanceState) applyBlockerCleared(event journal.Event) {
	if event.Type != journal.EventRunnerAnnotation ||
		runnerString(event.Runner, "kind") != journal.RunnerAnnotationBlockerCleared || event.Time.IsZero() {
		return
	}
	code := runnerString(event.Runner, "code")
	if code == "" {
		return
	}
	if s.blockerClears == nil {
		s.blockerClears = make(map[string]time.Time)
	}
	if event.Time.After(s.blockerClears[code]) {
		s.blockerClears[code] = event.Time
	}
}

// eventTimePointer is the event's timestamp, or nil for an unstamped event
// (#3774), so an unknown observed-at time is never rendered as the zero time.
func eventTimePointer(event journal.Event) *time.Time {
	if event.Time.IsZero() {
		return nil
	}
	at := event.Time
	return &at
}
