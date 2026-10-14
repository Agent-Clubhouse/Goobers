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

// InfraFailureStreakThreshold is the number of infrastructure-classified
// terminals on one item, since its last completed run, after which the
// circuit breaker parks it (#5638). Infrastructure terminals are kept off the
// genuine/work streak because they are not evidence about the item — but an
// environment that fails the same way every time (a CI suite that always
// hangs past its timeout, which the failure-class gate judges retryable and
// therefore infra) must still stop retrying eventually. A separate, higher
// threshold bounds that loop without letting ordinary weather park items at
// the work rate.
const InfraFailureStreakThreshold = 2 * FailureStreakThreshold

// streak is one of the breaker's two counters: the genuine/work streak, or the
// infrastructure streak counted separately at a higher threshold.
type streak struct {
	threshold int
	load      func(ctx context.Context, repo providers.RepositoryRef, itemID string) (int, error)
	write     func(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error
	comment   func(ctx context.Context, repo providers.RepositoryRef, itemID string, count int, stage, runID, runURL string, applying []string) error
	reset     func(ctx context.Context, repo providers.RepositoryRef, itemID, runID, runURL string) error
}

func (p *Policy) workStreak() streak {
	return streak{
		threshold: FailureStreakThreshold,
		load: func(ctx context.Context, repo providers.RepositoryRef, itemID string) (int, error) {
			return p.State.LoadFailureStreak(ctx, p.Poster, repo, itemID)
		},
		write: p.State.WriteFailureStreak,
		comment: func(ctx context.Context, repo providers.RepositoryRef, itemID string, count int, stage, runID, runURL string, applying []string) error {
			return gate.UpsertFailureComment(ctx, p.Poster, repo, itemID, count, stage, runID, runURL, applying)
		},
		reset: func(ctx context.Context, repo providers.RepositoryRef, itemID, runID, runURL string) error {
			return gate.ResetFailureComment(ctx, p.Poster, repo, itemID, runID, runURL)
		},
	}
}

func (p *Policy) infraStreak() streak {
	return streak{
		threshold: InfraFailureStreakThreshold,
		load:      p.State.LoadInfraFailureStreak,
		write:     p.State.WriteInfraFailureStreak,
		comment: func(ctx context.Context, repo providers.RepositoryRef, itemID string, count int, stage, runID, runURL string, applying []string) error {
			return gate.UpsertInfraFailureComment(ctx, p.Poster, repo, itemID, count, InfraFailureStreakThreshold, stage, runID, runURL, applying)
		},
		reset: func(ctx context.Context, repo providers.RepositoryRef, itemID, runID, runURL string) error {
			return gate.ResetInfraFailureComment(ctx, p.Poster, repo, itemID, runID, runURL)
		},
	}
}

// applyCircuitBreaker charges a genuine/work terminal to the work streak.
// Shared by Failed (PhaseFailed) and TerminalNotifier
// (PhaseEscalated/PhaseAborted) so that ALL non-completed terminals count
// toward the same streak.
func (p *Policy) applyCircuitBreaker(ctx context.Context, runID, stage, runURL string) error {
	return p.chargeStreak(ctx, p.workStreak(), runID, stage, runURL)
}

// applyInfraCircuitBreaker charges an infrastructure-classified terminal to
// the separate infrastructure streak (#5638).
func (p *Policy) applyInfraCircuitBreaker(ctx context.Context, runID, stage, runURL string) error {
	return p.chargeStreak(ctx, p.infraStreak(), runID, stage, runURL)
}

// chargeStreak increments s for each claimed item and parks the issue
// (needs-human + remove ready) once s's threshold is reached.
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
func (p *Policy) chargeStreak(ctx context.Context, s streak, runID, stage, runURL string) error {
	var errs []error
	if err := p.State.ReconcileParkOutbox(ctx, p.Poster); err != nil {
		errs = append(errs, err)
	}
	items, err := p.State.ClaimedItems(runID)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	for _, item := range items {
		if err := p.chargeItem(ctx, s, item, runID, stage, runURL); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Policy) chargeItem(ctx context.Context, s streak, item Item, runID, stage, runURL string) error {
	repoRef, itemID := item.Repo, item.ItemID
	prevCount, loadErr := s.load(ctx, repoRef, itemID)
	if loadErr != nil {
		return fmt.Errorf("load failure streak state on %s#%s: %w", repoRef.Name, itemID, loadErr)
	}
	count := prevCount + 1

	// The authoritative update happens FIRST (Goobers#3025): the streak
	// that gates the circuit breaker must not depend on a provider
	// comment write succeeding. The comment is posted after, as a
	// best-effort projection — its failure is still reported, but it
	// never blocks or rolls back the persisted count.
	if err := s.write(repoRef, itemID, count, runID, stage); err != nil {
		return fmt.Errorf("persist failure streak state on %s#%s: %w", repoRef.Name, itemID, err)
	}
	var errs []error
	var applying []string
	if count >= s.threshold {
		applying = []string{providers.LabelNeedsHuman}
	}
	if err := s.comment(ctx, repoRef, itemID, count, stage, runID, runURL, applying); err != nil {
		errs = append(errs, fmt.Errorf("upsert failure comment on %s#%s: %w", repoRef.Name, itemID, err))
	}
	if count < s.threshold {
		return errors.Join(errs...)
	}
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
	return errors.Join(errs...)
}

// resetCircuitBreaker mirrors chargeStreak's per-item repository routing
// (#4417) for the completed-terminal reset path. A completed run resets both
// streaks.
func (p *Policy) resetCircuitBreaker(ctx context.Context, runID, runURL string) error {
	items, err := p.State.ClaimedItems(runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, item := range items {
		repoRef, itemID := item.Repo, item.ItemID
		errs = append(errs, resetStreak(ctx, p.workStreak(), repoRef, itemID, runID, runURL))
		// The infrastructure streak is touched only when it is running, so
		// an item that never had an infrastructure terminal gets no extra
		// state write or comment on every completed run.
		if count, err := p.State.LoadInfraFailureStreak(ctx, repoRef, itemID); err != nil {
			errs = append(errs, fmt.Errorf("load infrastructure failure streak state on %s#%s: %w", repoRef.Name, itemID, err))
		} else if count > 0 {
			errs = append(errs, resetStreak(ctx, p.infraStreak(), repoRef, itemID, runID, runURL))
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

func resetStreak(ctx context.Context, s streak, repoRef providers.RepositoryRef, itemID, runID, runURL string) error {
	var errs []error
	if err := s.write(repoRef, itemID, 0, runID, ""); err != nil {
		errs = append(errs, fmt.Errorf("reset failure streak state on %s#%s: %w", repoRef.Name, itemID, err))
	}
	if err := s.reset(ctx, repoRef, itemID, runID, runURL); err != nil {
		errs = append(errs, fmt.Errorf("reset failure streak on %s#%s: %w", repoRef.Name, itemID, err))
	}
	return errors.Join(errs...)
}
