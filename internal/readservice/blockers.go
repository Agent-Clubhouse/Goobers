package readservice

import (
	"context"
	"slices"
	"strings"
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
// ends; a clear path that is too broad hides a live outage. So clear paths are
// explicit, and a recorded run error is retracted by later evidence ONLY when
// its code is registered in clearedErrorCodes.
//
//   - error (persisted: the run's latest ErrorDetail, read-model LatestError):
//     superseded by a later error in the same run. A code registered in
//     clearedErrorCodes is also retracted by a later blocker.cleared
//     instance-journal record of that code's scope. Every other code is the
//     run's last recorded failure and has no automatic clear — it leaves
//     status only with the run itself.
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

// Scopes of a blocker.cleared record.
const (
	clearScopeInstance = "instance"
	clearScopeRun      = "run"
)

// clearedErrorCodes are the run-error codes a blocker.cleared record can
// retract, and the scope whose evidence retracts each. A record is honoured
// only for the scope registered here.
//
// recovery_observation_failed (instance): the shared recovery inventory could
// not be read (#5354). A terminal run never observes again, but any other
// run's later successful observation reads the same inventory, so it shows
// the condition cleared. The daemon journals an instance clear on the first
// success of each process lifetime, on the first success after an inventory
// failure it saw, and periodically (internal/blockerclear).
//
// recovery_observation_run_failed (run): the inventory was readable but this
// run's own records, its count or ownership checks, or its journal append
// failed. Another run's success never looks at any of those, so only a later
// successful observation of the SAME run clears it; a run that never
// observes again keeps the blocker.
var clearedErrorCodes = map[string]string{
	journal.ErrorCodeRecoveryObservationFailed:    clearScopeInstance,
	journal.ErrorCodeRecoveryObservationRunFailed: clearScopeRun,
}

// blockerClearKey is the fold key for a clear: the code alone for instance
// scope, the code and run for run scope.
func blockerClearKey(code, scope, runID string) string {
	if scope == clearScopeRun {
		return code + "\x00" + runID
	}
	return code
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

// blockerSuperseded reports whether clears retract b on run runID: b is a
// recorded run error whose code is registered, its observed-at time is known,
// and a clear of the registered scope was observed after it. The clear's time
// is when its successful observation STARTED, so a success that read the
// inventory before a concurrent failure never retracts that failure, however
// late its record was written. Unknown times never clear.
func blockerSuperseded(b OperatorBlocker, runID string, clears map[string]time.Time) bool {
	if b.Source != BlockerSourceError || b.ObservedAt == nil {
		return false
	}
	scope, registered := clearedErrorCodes[b.Code]
	if !registered {
		return false
	}
	cleared, ok := clears[blockerClearKey(b.Code, scope, runID)]
	return ok && cleared.After(*b.ObservedAt)
}

// decorateBlockerClears retracts run blockers the instance journal has since
// recorded as cleared. Failing to read the instance journal leaves every
// blocker in place: an unreadable clear record must never hide one.
func (s *Local) decorateBlockerClears(ctx context.Context, runs []RunSummary) {
	if len(runs) == 0 || !anyClearable(runs) {
		return
	}
	projection, err := s.workflowSchedulerSnapshot(ctx)
	if err != nil || len(projection.blockerClears) == 0 {
		return
	}
	for i := range runs {
		runs[i].Operator.removeBlockers(func(b OperatorBlocker) bool {
			return blockerSuperseded(b, runs[i].ID, projection.blockerClears)
		})
	}
}

func anyClearable(runs []RunSummary) bool {
	for _, run := range runs {
		for _, blocker := range run.Operator.Blockers {
			if _, ok := clearedErrorCodes[blocker.Code]; ok && blocker.Source == BlockerSourceError {
				return true
			}
		}
	}
	return false
}

// applyBlockerCleared folds a blocker.cleared record: the newest observedAt
// per code (instance scope) or per code and run (run scope). A record without
// a parseable observedAt, or of a scope its code is not registered for, is
// ignored — it can prove nothing about when the condition cleared.
func (s *instanceState) applyBlockerCleared(event journal.Event) {
	if event.Type != journal.EventRunnerAnnotation ||
		runnerString(event.Runner, "kind") != journal.RunnerAnnotationBlockerCleared {
		return
	}
	code, scope := runnerString(event.Runner, "code"), runnerString(event.Runner, "scope")
	if registered, ok := clearedErrorCodes[code]; !ok || registered != scope ||
		(scope == clearScopeRun && event.RunID == "") {
		return
	}
	observedAt, err := time.Parse(time.RFC3339Nano, runnerString(event.Runner, "observedAt"))
	if err != nil {
		return
	}
	if s.blockerClears == nil {
		s.blockerClears = make(map[string]time.Time)
	}
	key := blockerClearKey(code, scope, event.RunID)
	if observedAt.After(s.blockerClears[key]) {
		s.blockerClears[key] = observedAt
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

// BlockerLine renders each blocker once with when it was observed and how long
// before now (#5404), so a blocker that stopped recurring reads as old rather
// than as a current outage. A summary from a reader that predates structured
// blockers falls back to the bare display text.
func (o OperatorRunSummary) BlockerLine(now time.Time) string {
	if len(o.Blockers) == 0 {
		return strings.Join(o.PotentialBlockers, "; ")
	}
	parts := make([]string, 0, len(o.Blockers))
	for _, blocker := range o.Blockers {
		observed := "observed at an unknown time"
		if blocker.ObservedAt != nil {
			age := max(now.Sub(*blocker.ObservedAt), 0)
			observed = "observed " + blocker.ObservedAt.UTC().Format(time.RFC3339) + ", " + age.Truncate(time.Second).String() + " ago"
		}
		parts = append(parts, blocker.Message+" ("+observed+")")
	}
	return strings.Join(parts, "; ")
}
