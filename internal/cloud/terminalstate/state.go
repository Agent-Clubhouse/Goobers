// Package terminalstate is the cloud controller's escalationnotify.State: the
// instance state the failed and escalated terminal hooks read and update, kept
// in PostgreSQL under the controller-lease fence instead of the daemon's
// instance directory.
//
// What the cloud does differently from the daemon, deliberately:
//   - A cloud run claims items of its gaggle's one backlog repository (multi-repo
//     runs are retired), so a claimed item's repository is that backlog
//     repository, not a per-item journal annotation (#4417).
//   - Circuit-breaker parks ride the terminal_report outbox: the published effect
//     already carries the park label and is re-applied on delivery, so there is no
//     separate park outbox to reconcile.
//   - pr-remediation charges and superseded-run settlement have no cloud lane
//     (schedule-demand remediation stays disabled in cloud until #8131), so they
//     are no-ops, and no run is ever superseded.
package terminalstate

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/failurestreakpg"
	"github.com/goobers/goobers/internal/cloud/noworkstreakpg"
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/internal/nowork/settle"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/providers"
)

// The composed State must keep the embedded store's escalation observer: it is how a
// refusal latch is released only after a cycle was really escalated.
var _ parking.CycleEscalationObserver = State{}

// writeTimeout bounds one streak write (the State seam carries no context).
const writeTimeout = 30 * time.Second

// State is one epoch's, for runs of one gaggle.
type State struct {
	// State supplies ClaimedItemIDs, BacklogRepository and RecordBlock, and the
	// epoch's lease handle every write is fenced by.
	blockedrecordspg.State
	Streaks failurestreakpg.Store
	NoWork  noworkstreakpg.Store
	// Journal reads a run's journal events (the PostgreSQL journal): the terminal
	// classification and the no-work verdict come from it. A missing journal is
	// (nil, nil).
	Journal func(runID string) ([]journal.Event, error)
	// CodeRepo is the gaggle's code repository; its backlog repository is where
	// claimed items live.
	CodeRepo providers.RepositoryRef
	// RunLink is the portal link for a run, used in failure comments.
	RunLink func(runID string) (string, error)
}

var (
	_ escalationnotify.State             = State{}
	_ escalationnotify.StreakChargeGuard = State{}
)

// ClaimedItems returns the run's claimed items, each in the gaggle's backlog
// repository.
func (s State) ClaimedItems(runID string) ([]escalationnotify.Item, error) {
	ids, err := s.ClaimedItemIDs(runID)
	if err != nil {
		return nil, err
	}
	repo := s.BacklogRepository(s.CodeRepo)
	items := make([]escalationnotify.Item, 0, len(ids))
	for _, id := range ids {
		items = append(items, escalationnotify.Item{ItemID: id, Repo: repo})
	}
	return items, nil
}

// LoadFailureStreak returns an item's work-failure count. The provider is never
// consulted: a rate-limited read would be folded into the count it computes.
func (s State) LoadFailureStreak(ctx context.Context, _ gate.Commenter, repo providers.RepositoryRef, itemID string) (int, error) {
	return s.Streaks.Load(ctx, s.Handle.InstanceID, failurestreakpg.Work, repo, itemID)
}

// WriteFailureStreak persists an item's work-failure count; zero resets it.
func (s State) WriteFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	return s.write(failurestreakpg.Work, repo, itemID, count, runID, stage)
}

// LoadInfraFailureStreak returns an item's infrastructure-failure count.
func (s State) LoadInfraFailureStreak(ctx context.Context, repo providers.RepositoryRef, itemID string) (int, error) {
	return s.Streaks.Load(ctx, s.Handle.InstanceID, failurestreakpg.Infra, repo, itemID)
}

// WriteInfraFailureStreak persists an item's infrastructure-failure count.
func (s State) WriteInfraFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	return s.write(failurestreakpg.Infra, repo, itemID, count, runID, stage)
}

func (s State) write(kind failurestreakpg.Kind, repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return s.Streaks.Write(ctx, s.Handle, kind, repo, itemID, count, runID, stage)
}

// StreakChargedBy reports whether runID already advanced the item's streak, so a
// re-fired frame does not charge it twice.
func (s State) StreakChargedBy(ctx context.Context, infra bool, repo providers.RepositoryRef, itemID, runID string) (bool, error) {
	kind := failurestreakpg.Work
	if infra {
		kind = failurestreakpg.Infra
	}
	return s.Streaks.ChargedBy(ctx, s.Handle.InstanceID, kind, repo, itemID, runID)
}

// ReconcileParkOutbox does nothing: parks are terminal_report effects.
func (State) ReconcileParkOutbox(context.Context, gate.Commenter) error { return nil }

// RecordParkFailure does nothing: the park effect is already in the outbox.
func (State) RecordParkFailure(providers.RepositoryRef, string, string, string, int, error) error {
	return nil
}

// ClearParks does nothing: see ReconcileParkOutbox.
func (State) ClearParks(providers.RepositoryRef, string) error { return nil }

// VoidRemediationCharge does nothing: the cloud has no remediation lane (#8131).
func (State) VoidRemediationCharge(context.Context, gate.Commenter, string) error { return nil }

// SettleSupersededRun reports no run superseded (no remediation lane, #8131).
func (State) SettleSupersededRun(context.Context, gate.Commenter, string, string) (bool, error) {
	return false, nil
}

// SettleNoWorkStreak applies the repeated-no-work policy (settle.Settle) to a
// completed run. The cloud records no claim purpose, so no claim is treated as a
// curation claim; a run holding several items is never counted regardless.
func (s State) SettleNoWorkStreak(ctx context.Context, poster gate.Commenter, runID, finalState, runURL string) error {
	return settle.Settle(ctx, poster, settle.Deps{
		Items: func(runID string) ([]settle.Item, error) {
			claimed, err := s.ClaimedItems(runID)
			items := make([]settle.Item, 0, len(claimed))
			for _, c := range claimed {
				items = append(items, settle.Item{ItemID: c.ItemID, Repo: c.Repo})
			}
			return items, err
		},
		Terminal: func(runID, finalState string) (nowork.Terminal, bool, error) {
			if finalState == "" || s.Journal == nil {
				return nowork.Terminal{}, false, nil
			}
			events, err := s.Journal(runID)
			if err != nil {
				return nowork.Terminal{}, false, err
			}
			terminal, ok := nowork.TerminalFromEvents(events, finalState)
			return terminal, ok, nil
		},
		Increment: func(ctx context.Context, repo providers.RepositoryRef, itemID, runID string, t nowork.Terminal) (nowork.Record, nowork.Record, bool, error) {
			return s.NoWork.Increment(ctx, s.Handle, repo, itemID, runID, t)
		},
		Reset: func(ctx context.Context, repo providers.RepositoryRef, itemID, runID string) (nowork.Record, error) {
			return s.NoWork.Reset(ctx, s.Handle, repo, itemID, runID)
		},
	}, runID, finalState, runURL)
}

// RunURL is the run's portal link.
func (s State) RunURL(runID string) (string, error) {
	if s.RunLink == nil {
		return "", nil
	}
	return s.RunLink(runID)
}
