package localscheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// demandPollFailureThreshold is how many consecutive failed demand polls one
// workflow's poll may accumulate before the scheduler journals a
// workflow.starved event (#5605). Three keeps a single slow tick or a brief
// provider outage quiet, and still reports a lane whose counter has stopped
// working within three of its own polls.
const demandPollFailureThreshold = 3

// demandPollFailureKey separates a workflow's schedule, backlog and refill
// polls: a healthy backlog poll must not reset the streak of a schedule poll
// that keeps failing.
type demandPollFailureKey struct {
	identity WorkflowIdentity
	code     string
}

// demandPollFailureCode is the journal error code for a failed poll of this
// kind. It doubles as the failure-streak key.
func demandPollFailureCode(poll demandPoll) string {
	switch {
	case poll.schedule:
		return "schedule_demand_count_failed"
	case poll.refill:
		return "refill_demand_count_failed"
	default:
		return "backlog_count_failed"
	}
}

// demandPollFailed handles an EligibleCount error and returns the demand the
// tick should act on.
//
// A provider poll-budget error sheds the poll and an authentication error
// opens the workflow's auth circuit; both return 0 and are deliberate, so
// neither counts toward the failure streak. Anything else is journaled as a
// *_count_failed error, counted, and passed to failedPollDemand.
func (s *Scheduler) demandPollFailed(ctx context.Context, entry WorkflowEntry, poll demandPoll, err error) int {
	var budgetErr *ProviderPollBudgetError
	if errors.As(err, &budgetErr) {
		s.journalPollShed(entry, budgetErr.Provider, budgetErr.Remaining, budgetErr.Requested, budgetErr.ResetAt)
		return 0
	}
	if providers.IsAuthenticationError(err) {
		s.openAuthCircuit(entryIdentity(entry))
		s.journalEvent(journal.Event{
			Type:     journal.EventError,
			Workflow: entry.Workflow,
			Gaggle:   entry.Gaggle,
			Error:    &journal.ErrorDetail{Code: providers.ErrorCodeAuthFailed, Message: err.Error()},
		})
		return 0
	}
	code := demandPollFailureCode(poll)
	s.journalEvent(journal.Event{
		Type:     journal.EventError,
		Workflow: entry.Workflow,
		Gaggle:   entry.Gaggle,
		Error:    &journal.ErrorDetail{Code: code, Message: err.Error()},
	})
	s.recordDemandPollFailure(entry, code, err)
	return failedPollDemand(ctx, poll, err)
}

// failedPollDemand is the demand a failed poll stands for.
//
// A schedule poll that timed out or hit a transient provider failure fires one
// run (#5605). Treating "could not count" as "nothing to do" consumed the cron
// slot: applyDemandCount(0) dropped the pending demand, and the next slot re-ran
// the same too-slow scan. A pr-remediation counter that crossed the 45s
// demandPollTimeout on a large overlap cluster therefore never fired again, and
// the displaced PRs it existed to remediate were never picked up. One run is
// the least that keeps the lane alive. It goes through the same readiness
// admission as any other scheduled fire (maxConcurrentRuns, maxRunsPerHour,
// maxParallelRuns, idle backoff), and a start stage with nothing eligible ends
// as ordinary no-work.
//
// Everything else keeps returning 0:
//   - backlog and refill polls size fan-out, so an unknown count must not
//     guess one;
//   - a cancelled parent context is shutdown or reload, not a slow counter;
//   - a rate-limit give-up would meet the same exhausted quota in the run;
//   - a persistent failure (not-found, malformed request) is not demand.
func failedPollDemand(ctx context.Context, poll demandPoll, err error) int {
	if !poll.schedule || ctx.Err() != nil {
		return 0
	}
	var rateLimited *providers.RateLimitError
	if errors.As(err, &rateLimited) {
		return 0
	}
	if errors.Is(err, context.DeadlineExceeded) || providers.IsTransientError(err) {
		return 1
	}
	return 0
}

// recordDemandPollSuccess ends the failure streak for this poll.
func (s *Scheduler) recordDemandPollSuccess(entry WorkflowEntry, poll demandPoll) {
	key := demandPollFailureKey{identity: entryIdentity(entry), code: demandPollFailureCode(poll)}
	s.mu.Lock()
	delete(s.demandPollFailures, key)
	s.mu.Unlock()
}

// recordDemandPollFailure extends the failure streak for this poll and
// journals one workflow.starved event when it reaches
// demandPollFailureThreshold (#5605).
//
// Neither existing starvation alarm sees a counter that keeps failing:
// journalTriggerStalls (#1868) watches LastEval, which still advances on every
// due slot, and journalCapacityStarvation (#5277) needs a refused dispatch,
// which never happens when there was nothing to dispatch. Without this the
// only trace is one error line per poll.
//
// The event fires exactly once per streak, when the count reaches the
// threshold; a successful poll resets the count and so re-arms it.
func (s *Scheduler) recordDemandPollFailure(entry WorkflowEntry, code string, err error) {
	key := demandPollFailureKey{identity: entryIdentity(entry), code: code}
	s.mu.Lock()
	s.demandPollFailures[key]++
	failures := s.demandPollFailures[key]
	s.mu.Unlock()
	if failures != demandPollFailureThreshold {
		return
	}
	s.journalEvent(journal.Event{
		Type:     journal.EventWorkflowStarved,
		Workflow: entry.Workflow,
		Gaggle:   entry.Gaggle,
		Reason:   fmt.Sprintf("demand poll failed %d consecutive times (%s): %v", failures, code, err),
	})
}
