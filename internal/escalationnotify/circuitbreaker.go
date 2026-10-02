package escalationnotify

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/providers"
)

// FailureStreakThreshold is the number of consecutive non-completed terminals
// on one item after which the circuit breaker parks it goobers:needs-human.
const FailureStreakThreshold = 3

// applyCircuitBreaker increments the failure streak for each claimed item and
// parks the issue (needs-human + remove ready) once the threshold is reached.
// Shared by Failed (PhaseFailed) and TerminalNotifier
// (PhaseEscalated/PhaseAborted) so that ALL non-completed terminals count
// toward the same streak.
//
// Each claimed item is routed to ITS OWN recorded repository (#4417), not a
// single repo shared across every item this run holds: on a multi-repo
// instance a run can hold claims against different repos (e.g. a
// decomposition parent from GaggleSpec.AdditionalRepos), and applying one
// item's repo to another's provider call silently mutates the wrong issue.
// State.ClaimedItems fails closed before this function makes any provider
// call if an item's identity was never recorded.
//
// A park whose provider mutation fails is persisted to the circuit-breaker
// outbox (#3646) and retried here on the next terminal: discarding it left the
// item goobers:ready with no durable evidence the protection had been
// attempted, so unhealthy work kept churning.
func (p *Policy) applyCircuitBreaker(ctx context.Context, runID, stage, runURL string) error {
	var errs []error
	if err := p.State.ReconcileParkOutbox(ctx, p.Poster); err != nil {
		errs = append(errs, err)
	}
	items, err := p.State.ClaimedItems(runID)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	if len(items) == 0 {
		return errors.Join(errs...)
	}
	for _, item := range items {
		repoRef, itemID := item.Repo, item.ItemID
		prevCount, loadErr := p.State.LoadFailureStreak(ctx, p.Poster, repoRef, itemID)
		if loadErr != nil {
			errs = append(errs, fmt.Errorf("load failure streak state on %s#%s: %w", repoRef.Name, itemID, loadErr))
			continue
		}
		count := prevCount + 1

		// The authoritative update happens FIRST (Goobers#3025): the streak
		// that gates the circuit breaker must not depend on a provider
		// comment write succeeding. The comment is posted after, as a
		// best-effort projection — its failure is still reported, but it
		// never blocks or rolls back the persisted count.
		if err := p.State.WriteFailureStreak(repoRef, itemID, count, runID, stage); err != nil {
			errs = append(errs, fmt.Errorf("persist failure streak state on %s#%s: %w", repoRef.Name, itemID, err))
			continue
		}
		if err := gate.UpsertFailureComment(ctx, p.Poster, repoRef, itemID, count, stage, runID, runURL); err != nil {
			errs = append(errs, fmt.Errorf("upsert failure comment on %s#%s: %w", repoRef.Name, itemID, err))
		}

		if count >= FailureStreakThreshold {
			if _, err := p.Poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository:   repoRef,
				ID:           itemID,
				AddLabels:    []string{providers.LabelNeedsHuman},
				RemoveLabels: []string{providers.LabelReady},
			}); err != nil {
				errs = append(errs, fmt.Errorf("apply circuit breaker on %s#%s: %w", repoRef.Name, itemID, err))
				if rerr := p.State.RecordParkFailure(repoRef, itemID, runID, stage, count, err); rerr != nil {
					errs = append(errs, fmt.Errorf("persist circuit breaker park for %s#%s: %w", repoRef.Name, itemID, rerr))
				}
			} else if cerr := p.State.ClearParks(repoRef, itemID); cerr != nil {
				errs = append(errs, fmt.Errorf("clear circuit breaker park for %s#%s: %w", repoRef.Name, itemID, cerr))
			}
		}
	}
	return errors.Join(errs...)
}

// resetCircuitBreaker mirrors applyCircuitBreaker's per-item repository
// routing (#4417) for the completed-terminal reset path.
func (p *Policy) resetCircuitBreaker(ctx context.Context, runID, runURL string) error {
	items, err := p.State.ClaimedItems(runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range items {
		repoRef, itemID := item.Repo, item.ItemID
		if err := p.State.WriteFailureStreak(repoRef, itemID, 0, runID, ""); err != nil {
			errs = append(errs, fmt.Errorf("reset failure streak state on %s#%s: %w", repoRef.Name, itemID, err))
		}
		if err := gate.ResetFailureComment(ctx, p.Poster, repoRef, itemID, runID, runURL); err != nil {
			errs = append(errs, fmt.Errorf("reset failure streak on %s#%s: %w", repoRef.Name, itemID, err))
		}
		// A completed run resets the streak that motivated any still-pending
		// park for this item, so the outbox entry is moot rather than owed
		// (#3646).
		if err := p.State.ClearParks(repoRef, itemID); err != nil {
			errs = append(errs, fmt.Errorf("clear circuit breaker park on %s#%s: %w", repoRef.Name, itemID, err))
		}
	}
	return errors.Join(errs...)
}
