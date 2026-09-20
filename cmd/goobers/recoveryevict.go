package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// recoveryEvictFunc returns the eviction hook (#4823) invoked when a durable
// recovery handoff is refused for capacity. It retires the first inventory
// entry it can justify retiring, and every justification names why the entry
// held nothing worth keeping (#5354 acceptance: no reclamation discards
// agent-authored work). It never returns an error for an individual candidate
// that turns out ineligible or broken — a single bad entry must not block
// eviction of a later, genuinely reclaimable one, or fail the cleanup that is
// waiting on capacity.
//
// manager must be the SAME manager whose repository lock for key is already
// held by the caller (true of every wiring in this package: a cleanup-guard
// callback, or a request handler inside WithRecoveryMirror) — candidates for
// key itself therefore use WithRecoveryRepositoriesLocked, never
// WithRecoveryRepositories, because re-acquiring that lock here would deadlock
// on it; candidates for any OTHER repository use TryWithRecoveryRepositories,
// which takes that repository's own lock without ever waiting for it.
func recoveryEvictFunc(layout instance.Layout, cfg *instance.Config, manager *worktree.Manager, key string) recovery.EvictFunc {
	// The operator cap the caller was refused against is deliberately unused:
	// see the read below.
	return func(ctx context.Context, root string, _ int) (bool, error) {
		// Tolerant, for the same reason this function never fails on an
		// individual ineligible candidate: one unreadable reservation must
		// not stop a later, genuinely reclaimable one from being evicted. A
		// crashed publish leaves a directory holding only lock files, and a
		// strict read turned that into eviction failing FOREVER — capacity
		// was never reclaimed again and the inventory grew without bound
		// (#5092). A broken reservation is never an eviction candidate
		// itself, so skipping it loses nothing; it is reported rather than
		// discarded so an operator can see debris that no path can reclaim.
		//
		// Read at the structural ceiling, not at limit. The cap is a WRITE
		// limit — whether another reservation may be created — and a read
		// bounded by it refuses outright once the directory already holds
		// MORE entries than the cap ("75 of 8 slots used"). That is exactly
		// the state this hook exists to clear: the refusal happened before
		// any rule was evaluated, so superseded duplicates and landed work
		// stayed put and every publish fell through to the overflow tier
		// (#5354). Nothing here infers absence from the scan; each candidate
		// is judged on its own evidence, so seeing more than the cap allows
		// cannot make retiring one of them wrong.
		entries, unreadable, err := recovery.ReadInventoryTolerant(ctx, root, recovery.MaxInventoryEntries)
		if err != nil {
			return false, err
		}
		ownURL, _ := recoveryRetentionCloneURL(cfg, key)
		scope := recoveryEvictionScope{layout: layout, cfg: cfg, manager: manager, key: key, root: root, ownURL: ownURL}
		scope.reportUnreadable(unreadable)
		return scope.evict(ctx, entries)
	}
}

// recoveryEvictionScope carries the one cleanup's identity through the
// ordered reclamation rules, so each rule stays a predicate rather than
// re-deriving configuration and lock ownership for itself.
type recoveryEvictionScope struct {
	layout  instance.Layout
	cfg     *instance.Config
	manager *worktree.Manager
	key     string
	root    string
	// ownURL is the clone URL for key. The manager's repository lock is keyed
	// by clone URL, so an identical URL — not an equal repository key — is
	// what decides whether this caller already holds the lock.
	ownURL string
}

// recoveryEvictionRule is one ordered reclamation justification.
type recoveryEvictionRule struct {
	// sameRepositoryOnly restricts candidates to the cleanup's own repository,
	// for a rule whose evidence needs that repository's managed clone.
	sameRepositoryOnly bool
	// needsTerminalRun gates the rule behind the owning run's journal showing
	// a terminal phase: a live run's snapshot is never reclaimable, whatever
	// its content.
	needsTerminalRun bool
	// reclaim names the justification for retiring record, or "" when the
	// entry must be kept. repositories is the locked managed mirror and
	// pinned clone set for record's OWN repository.
	reclaim func(ctx context.Context, record recovery.Record, repositories []string) (string, error)
}

func (s recoveryEvictionScope) evict(ctx context.Context, entries []recovery.InventoryEntry) (bool, error) {
	records := retainedEvictionRecords(entries)
	if len(records) == 0 {
		return false, nil
	}
	operatorEvents, _ := journal.ReadInstanceLog(s.layout.SchedulerDir())
	for _, rule := range s.reclamationRules(records, operatorEvents) {
		for _, record := range records {
			if rule.sameRepositoryOnly && record.RepositoryKey != s.key {
				continue
			}
			if retired, _ := s.retire(ctx, record, rule); retired {
				return true, nil
			}
		}
	}
	return false, nil
}

// reclamationRules orders reclamation from the cheapest and least ambiguous
// justification to the most expensive. Ordering matters for cost, not for
// safety: every rule independently establishes that the entry it retires
// protects no agent-authored work.
func (s recoveryEvictionScope) reclamationRules(records []recovery.Record, operatorEvents []journal.Event) []recoveryEvictionRule {
	return []recoveryEvictionRule{
		// The operator already decided. `goobers recovery-abandon` marks an
		// exact record, but only the periodic sweep acted on it - and that
		// sweep is report-only for the whole first-enable grace window, so an
		// abandonment freed no capacity for a week (#5110).
		{
			needsTerminalRun: true,
			reclaim: func(_ context.Context, record recovery.Record, _ []string) (string, error) {
				abandoned, err := recovery.ExplicitlyAbandoned(operatorEvents, record)
				if err != nil || !abandoned {
					return "", err
				}
				return reclaimOperatorAbandoned, nil
			},
		},
		// Content the inventory never needed to hold: no diff at all (#5095),
		// Goobers' own bookkeeping (#5119), or bytes a newer entry already
		// protects (#5110). Terminality is not required - none of these hold
		// anything the owning run could still be extending.
		{
			reclaim: func(ctx context.Context, record recovery.Record, repositories []string) (string, error) {
				own := record.RepositoryKey == s.key
				return recoveryContentlessJustification(ctx, record, repositories, records, operatorEvents, own), nil
			},
		},
		// Landing proof, unchanged (#4823): the work is already on the target
		// branch. Needs that repository's managed clone to verify.
		{
			sameRepositoryOnly: true,
			needsTerminalRun:   true,
			reclaim:            s.landingProvenReclaim,
		},
	}
}

// retainedEvictionRecords re-reads each entry's record under its retained
// sidecar so a renewal is never raced, and drops entries it cannot read: a
// record that cannot be read is never a candidate, and must not stop the next
// one from being considered.
//
// The result is ordered oldest capture first. Inventory order is a directory
// hash, so without this the entry reclaimed from a set of equally reclaimable
// ones would be arbitrary; taking the oldest keeps the survivor the freshest
// copy, which is the one a restore would want.
func retainedEvictionRecords(entries []recovery.InventoryEntry) []recovery.Record {
	records := make([]recovery.Record, 0, len(entries))
	for _, entry := range entries {
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return newerRetainedEntry(records[j], records[i])
	})
	return records
}

func (s recoveryEvictionScope) retire(ctx context.Context, record recovery.Record, rule recoveryEvictionRule) (bool, error) {
	if !rule.needsTerminalRun {
		return s.retireWithinRepositories(ctx, record, rule)
	}
	// The run journal is opened OUTSIDE the repository lock, matching the
	// nesting every other recovery path uses; reversing it would introduce a
	// second lock order between the two.
	runDir := filepath.Join(s.layout.RunsDir(), record.RunID)
	var retired bool
	_, err := journal.WithIdleRunReader(ctx, runDir, func(reader *journal.Reader) error {
		identity, err := reader.Identity()
		if err != nil || identity.RunID != record.RunID {
			return err
		}
		phase, err := reader.PhaseBounded(ctx)
		if err != nil || !terminalRunPhase(phase) {
			return err
		}
		retired, err = s.retireWithinRepositories(ctx, record, rule)
		return err
	})
	return retired, err
}

func (s recoveryEvictionScope) retireWithinRepositories(ctx context.Context, record recovery.Record, rule recoveryEvictionRule) (bool, error) {
	url, err := recoveryRetentionCloneURL(s.cfg, record.RepositoryKey)
	if err != nil {
		return false, err
	}
	var retired bool
	visit := func(repositories []string) error {
		justification, err := rule.reclaim(ctx, record, repositories)
		if err != nil || justification == "" {
			return err
		}
		if err := retireSnapshotEverywhere(ctx, s.root, record, repositories); err != nil {
			return err
		}
		retired = true
		s.journalReclamation(record, justification)
		return nil
	}
	if url == s.ownURL {
		_, err = s.manager.WithRecoveryRepositoriesLocked(ctx, url, visit)
	} else {
		_, err = s.manager.TryWithRecoveryRepositories(ctx, url, visit)
	}
	return retired, err
}

func retireSnapshotEverywhere(ctx context.Context, root string, record recovery.Record, repositories []string) error {
	_, err := recovery.RetireSnapshot(ctx, root, record, func(current recovery.Record) error {
		for _, repository := range repositories {
			if err := recovery.DeleteSnapshotRef(ctx, repository, current); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// landingProvenReclaim keeps #4823's original semantics exactly, including
// its deliberate independence from the periodic sweep's dry-run, first-enable
// grace window and retain-until floor: those gate a background policy decision
// made with time to spare, while this runs only when a cleanup is already
// blocked on capacity.
func (s recoveryEvictionScope) landingProvenReclaim(ctx context.Context, record recovery.Record, repositories []string) (string, error) {
	project, err := recoveryConfiguredProject(s.cfg, record.RepositoryKey)
	if err != nil {
		return "", err
	}
	route, err := recoveryLandingRoute(project)
	if err != nil {
		return "", err
	}
	landed, err := recoveryLandingHeads(ctx, s.layout.RunsDir(), record, route)
	if err != nil || len(landed) == 0 {
		return "", err
	}
	verified, err := verifyRecoveryLandingRepositories(ctx, repositories, record, landed, s.cfg.Retention.RecoveryEffective().MaxArchiveBytesEffective())
	if err != nil || !verified {
		return "", err
	}
	return reclaimLandingProven, nil
}

// journalReclamation records WHY a slot was freed. A reclamation that cannot
// state why the content it removed was not worth keeping is exactly what
// #5354's non-goals rule out, so the justification is durable evidence rather
// than a code comment.
func (s recoveryEvictionScope) journalReclamation(record recovery.Record, justification string) {
	event, err := recovery.RetainedEvent(record)
	if err != nil {
		return
	}
	event.Runner["operation"] = "recovery-reclaimed"
	event.Runner["recoveryReclamationJustification"] = justification
	_ = s.instanceLog().Append(event)
}

// reportUnreadable names the debris a tolerant scan skipped. #5354 records
// that ReadInventoryTolerant's single caller discarded this list, so
// reservations that count against the cap and that no path can reclaim were
// invisible until an operator went looking on disk.
func (s recoveryEvictionScope) reportUnreadable(unreadable []recovery.UnreadableEntry) {
	if len(unreadable) == 0 {
		return
	}
	const named = 8
	names := make([]string, 0, named)
	for _, entry := range unreadable {
		if len(names) == named {
			break
		}
		names = append(names, entry.Name)
	}
	message := fmt.Sprintf(
		"recovery inventory holds %d unreadable reservation(s) in %s that count toward capacity and no reclamation path can free: %s",
		len(unreadable), s.root, strings.Join(names, ", "),
	)
	if len(unreadable) > len(names) {
		message += fmt.Sprintf(" (and %d more)", len(unreadable)-len(names))
	}
	_ = s.instanceLog().Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "recovery_inventory_unreadable", Message: message},
	})
}

// instanceLog carries eviction's own observations to the instance log. Both
// callers discard its error deliberately: a reporting failure must never fail
// the cleanup that is waiting on capacity.
func (s recoveryEvictionScope) instanceLog() recoveryCleanupJournal {
	return recoveryCleanupJournal{directory: s.layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
}
