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
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/providers"
)

// writeTimeout bounds one streak write (the State seam carries no context).
const writeTimeout = 30 * time.Second

// State is one epoch's, for runs of one gaggle.
type State struct {
	// State supplies ClaimedItemIDs, BacklogRepository and RecordBlock, and the
	// epoch's lease handle every write is fenced by.
	blockedrecordspg.State
	Streaks failurestreakpg.Store
	// CodeRepo is the gaggle's code repository; its backlog repository is where
	// claimed items live.
	CodeRepo providers.RepositoryRef
	// RunLink is the portal link for a run, used in failure comments.
	RunLink func(runID string) (string, error)
}

var _ escalationnotify.State = State{}

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

// SettleNoWorkStreak is not implemented in the cloud yet (3b-2b).
func (State) SettleNoWorkStreak(context.Context, gate.Commenter, string, string, string) error {
	return nil
}

// RunURL is the run's portal link.
func (s State) RunURL(runID string) (string, error) {
	if s.RunLink == nil {
		return "", nil
	}
	return s.RunLink(runID)
}
