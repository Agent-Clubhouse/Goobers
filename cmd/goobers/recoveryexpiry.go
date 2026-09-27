package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// windowDryRun is the retention pass's combined resolved decision (operator
// retention.dryRun or an unelapsed first-enable grace window, #4253). It
// governs the landing-proof retirement path: retiring a snapshot with landed
// content deletes its refs, so that path has to observe the same window the
// worktree sweep does — otherwise a grace period that only reports worktrees
// would still be destroying recovery snapshots underneath it.
//
// operatorDryRun and graceActive are the same decision split apart (#5354):
// a contentless retirement (recoveryContentlessJustification below) holds no
// recoverable content, so it is gated by operatorDryRun alone, and graceActive
// is passed through only so a successful deletion can note when the grace
// window would otherwise have held it.
func retireExpiredRecovery(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, windowDryRun, operatorDryRun, graceActive bool, stdout, stderr io.Writer) error {
	root := filepath.Join(layout.Root, "recovery")
	// #5092: the sweep that RECLAIMS capacity must resolve the same cap the
	// writers enforce, or it decides retention against a limit no writer
	// agrees with. The cap governs the retirement decisions below; it no
	// longer bounds the read (see the read itself).
	policy, origin := resolveRecoveryPolicy(layout, setup.Config)
	// setup.InstanceLog is a typed nil on the retention CLI path, which an
	// interface-valued nil check cannot see — take the address of a live log
	// only when there is one.
	if setup.InstanceLog != nil {
		journalRecoveryPolicyFallback(setup.InstanceLog, origin, policy, root)
	}
	// Tolerant, and this is the fix for a hard instance wedge. The sweep is the
	// ONLY path that reclaims capacity by retention policy, and it read the
	// inventory all-or-nothing: one crashed publish leaves a reservation
	// holding only lock files, the strict read then failed the whole scan, and
	// from that moment nothing was ever retired again. The inventory climbed to
	// its cap, every worktree cleanup needing a recovery handoff was refused
	// with ErrInventoryFull, worktree reuse stopped, and stages died at
	// "create worktree". #5092 fixed exactly this for the eviction path and
	// left the sweep strict.
	//
	// Tolerance is safe HERE specifically because this sweep never infers
	// absence: it decides each entry on that entry's own evidence and retires
	// it individually, so not seeing a broken neighbour cannot make retiring
	// this one wrong. ReadInventory stays all-or-nothing for callers that ask
	// whether recovery state exists at all, which is the distinction its doc
	// draws. The unreadable set is reported rather than dropped, so an
	// incomplete scan is never mistaken for a clean one.
	//
	// The limit passed here is the structural ceiling, not policy.
	// MaxSnapshotsEffective(): #5300 moved this read onto the cap the writers
	// enforce, which fixed reading BELOW the cap and still refused ABOVE it.
	// An inventory holding more entries than the cap — what any operator gets
	// by lowering maxSnapshots on a full inventory, and the shape of the
	// production wedge — made the whole sweep return "inventory is full"
	// before a single entry was considered, so retention, including the
	// contentless purge that ignores the grace window, never ran on exactly
	// the inventories that needed it (#5354). The cap governs whether a new
	// reservation may be WRITTEN; it is resolved above and still governs
	// every retirement decision below.
	entries, unreadable, err := recovery.ReadInventoryTolerant(ctx, root, recovery.MaxInventoryEntries)
	if err != nil {
		return err
	}
	var failures error
	for _, broken := range unreadable {
		pf(stderr, "warning: recovery inventory reservation %q is unreadable and still consumes a slot: %v\n", broken.Name, broken.Err)
		failures = errors.Join(failures, fmt.Errorf("unreadable recovery reservation %s: %w", broken.Name, broken.Err))
	}
	if len(entries) == 0 {
		return failures
	}
	operatorEvents, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		return errors.Join(failures, err)
	}
	entries = prioritizeAbandonedRecovery(entries, operatorEvents, &failures)
	// retained is the full set the pass read, fixed for the whole sweep.
	// recoveryContentlessJustification needs it to decide supersession, and
	// deciding every candidate against one fixed snapshot (rather than
	// re-scanning after each retirement) is what keeps a superseded
	// duplicate from ever being retired ahead of the newer entry that
	// protects its content in the same pass.
	retained := retainedEvictionRecords(entries)
	for _, entry := range entries {
		err := retireExpiredRecoveryEntry(ctx, root, setup, policy, managers, runsByRoot, entry, retained, operatorEvents, windowDryRun, operatorDryRun, graceActive, stdout, layout.SchedulerDir())
		if err != nil {
			pf(stderr, "warning: recovery retention failed run=%q ref=%q: %v\n", entry.Record.RunID, entry.Record.Ref, err)
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// prioritizeAbandonedRecovery puts operator-abandoned entries first so the
// slots an operator has already released are reclaimed before anything else.
//
// An entry whose record cannot be read is now SKIPPED (recorded into failures)
// rather than aborting the whole ordering. It was the second of two places
// where one damaged entry stopped every other entry from ever being retired:
// ordering is a preference, so failing to classify one entry must not deny the
// other 127 their retention policy. A skipped entry keeps its slot and is
// reported, so the problem stays visible instead of becoming a silent leak.
func prioritizeAbandonedRecovery(
	entries []recovery.InventoryEntry,
	operatorEvents []journal.Event,
	failures *error,
) []recovery.InventoryEntry {
	prioritized := make([]recovery.InventoryEntry, 0, len(entries))
	remaining := make([]recovery.InventoryEntry, 0, len(entries))
	for _, entry := range entries {
		current, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			*failures = errors.Join(*failures, fmt.Errorf("classify recovery entry %s: %w", entry.RecordPath, err))
			continue
		}
		abandoned, err := recovery.ExplicitlyAbandoned(operatorEvents, current)
		if err != nil {
			*failures = errors.Join(*failures, fmt.Errorf("classify recovery entry %s: %w", entry.RecordPath, err))
			continue
		}
		if abandoned {
			prioritized = append(prioritized, entry)
		} else {
			remaining = append(remaining, entry)
		}
	}
	return append(prioritized, remaining...)
}

func retireExpiredRecoveryEntry(ctx context.Context, root string, setup *schedulerSetup, recoveryCfg instance.RecoverySnapshotConfig, managers []*worktree.Manager, runsByRoot map[string]string, entry recovery.InventoryEntry, retained []recovery.Record, operatorEvents []journal.Event, windowDryRun, operatorDryRun, graceActive bool, stdout io.Writer, schedulerDir string) error {
	manager, runDir, err := recoveryRetentionOwner(entry.Record.RunID, managers, runsByRoot)
	if err != nil {
		return err
	}
	retainWindow, err := recoveryCfg.RetainWindowEffective()
	if err != nil {
		return err
	}
	_, err = journal.WithIdleRunReader(ctx, runDir, func(reader *journal.Reader) error {
		record, err := recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return err
		}
		eligible, err := recoveryRetirementEligible(reader, record, time.Now().UTC(), operatorEvents, retainWindow)
		if err != nil {
			return err
		}
		url, err := recoveryRetentionCloneURL(setup.Config, record.RepositoryKey)
		if err != nil {
			return err
		}
		found, err := manager.WithRecoveryRepositories(ctx, url, func(repositories []string) error {
			// recoveryContentlessJustification decides categories 2-4 from
			// retained content alone: no landing proof, no operator decision,
			// no elapsed retain-until window (#5359 follow-up left by #5366).
			// It runs FIRST, ahead of landing proof, because it needs neither
			// a terminal run phase nor a configured landing route, and a
			// non-empty result makes the entry eligible even inside the
			// retain floor: it holds nothing worth keeping.
			justification := ""
			if !eligible {
				justification = recoveryContentlessJustification(ctx, record, repositories, retained, operatorEvents, true)
				eligible = justification != ""
			}
			if !eligible {
				landed, err := recoverySweepLandedHeads(ctx, setup, reader, runDir, record)
				if err != nil || len(landed) == 0 {
					return err
				}
				eligible, err = verifyRecoveryLandingRepositories(ctx, repositories, record, landed, recoveryCfg.MaxArchiveBytesEffective())
				if err != nil || !eligible {
					return err
				}
			}
			rule := "recovery-policy"
			if justification != "" {
				rule = justification
			}
			// A contentless retirement holds nothing reviewable, so it is
			// gated by the operator's explicit dryRun alone (#5354); every
			// other retirement (landing proof) keeps observing the combined
			// window dry-run exactly as before.
			effectiveDryRun := windowDryRun
			if justification != "" {
				effectiveDryRun = operatorDryRun
			}
			if effectiveDryRun {
				pf(stdout, "retention candidate kind=recovery rule=%s run=%q ref=%q\n", rule, record.RunID, record.Ref)
				return nil
			}
			_, err := recovery.RetireSnapshot(ctx, root, record, func(current recovery.Record) error {
				for _, repository := range repositories {
					if err := recovery.DeleteSnapshotRef(ctx, repository, current); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if justification != "" {
				journalSweepReclamation(schedulerDir, record, justification)
				if graceActive {
					pf(stdout, "retention deleted rule=%s kind=recovery run=%q ref=%q (grace window does not apply: no recoverable content)\n", justification, record.RunID, record.Ref)
				}
			}
			return nil
		})
		if err == nil && !found {
			return fmt.Errorf("recovery retention requires an existing managed repository")
		}
		return err
	})
	return err
}

// recoverySweepLandedHeads gates the sweep's original #4823 landing-proof
// path behind a terminal run phase and a configured landing route, unchanged
// from before recoveryContentlessJustification was folded in above it.
func recoverySweepLandedHeads(ctx context.Context, setup *schedulerSetup, reader *journal.Reader, runDir string, record recovery.Record) ([]recovery.LandedHead, error) {
	phase, err := reader.PhaseBounded(ctx)
	if err != nil || !terminalRunPhase(phase) {
		return nil, err
	}
	project, err := recoveryConfiguredProject(setup.Config, record.RepositoryKey)
	if err != nil {
		return nil, err
	}
	route, err := recoveryLandingRoute(project)
	if err != nil {
		return nil, err
	}
	return recoveryLandingHeads(ctx, filepath.Dir(runDir), record, route)
}

// journalSweepReclamation records the periodic sweep's contentless retirement
// the same way the on-demand eviction hook does
// (recoveryEvictionScope.journalReclamation), so a snapshot holding nothing
// worth keeping is equally explainable from the instance log regardless of
// which path retired it.
func journalSweepReclamation(schedulerDir string, record recovery.Record, justification string) {
	event, err := recovery.RetainedEvent(record)
	if err != nil {
		return
	}
	event.Runner["operation"] = "recovery-reclaimed"
	event.Runner["recoveryReclamationJustification"] = justification
	log := recoveryCleanupJournal{directory: schedulerDir, scrubber: journal.NewRegistryScrubber()}
	_ = log.Append(event)
}

// A receiving commit may exist only in the pinned clone, not the mirror (or
// vice versa). One complete content proof suffices; an absent object in another
// managed copy must not hide that proof. Cleanup still checks every owned ref.
func verifyRecoveryLandingRepositories(ctx context.Context, repositories []string, record recovery.Record, landed []recovery.LandedHead, maxArchiveBytes int64) (bool, error) {
	var failures error
	for _, repository := range repositories {
		for _, head := range landed {
			verified, err := recovery.VerifyLandedRestoration(ctx, repository, record, head, maxArchiveBytes)
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			if verified {
				return true, nil
			}
		}
	}
	return false, failures
}

func recoveryRetentionOwner(runID string, managers []*worktree.Manager, runsByRoot map[string]string) (*worktree.Manager, string, error) {
	var owner *worktree.Manager
	var runDir string
	for _, manager := range managers {
		runsRoot, configured := runsByRoot[manager.Root]
		if !configured || runsRoot == "" {
			return nil, "", fmt.Errorf("recovery retention requires configured run directory mapping")
		}
		candidate := filepath.Join(runsRoot, runID)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if !info.IsDir() || owner != nil {
			return nil, "", fmt.Errorf("recovery retention requires unambiguous real run directory")
		}
		owner, runDir = manager, candidate
	}
	if owner == nil {
		return nil, "", fmt.Errorf("recovery retention requires owning run journal")
	}
	return owner, runDir, nil
}

func recoveryRetirementEligible(reader *journal.Reader, record recovery.Record, now time.Time, operatorEvents []journal.Event, retainWindow time.Duration) (bool, error) {
	identity, err := reader.Identity()
	if err != nil {
		return false, err
	}
	if identity.RunID != record.RunID || identity.StartedAt.IsZero() {
		return false, fmt.Errorf("recovery retention run identity mismatch")
	}
	events, err := reader.Events()
	if err != nil {
		return false, err
	}
	if !terminalRunPhase(journal.PhaseFromEvents(events)) {
		return false, nil
	}
	if len(operatorEvents) > 0 {
		abandoned, err := recovery.ExplicitlyAbandoned(operatorEvents, record)
		if err != nil || abandoned {
			return abandoned, err
		}
	}
	finished, err := recoveryWindowTime(events, identity.StartedAt)
	if err != nil {
		return false, err
	}
	// Stage capture may be older than terminal renewal. Never prune in the
	// terminal window just because renewal has not yet acknowledged its sidecar.
	return !now.Before(record.RetainUntil) && !now.Before(finished.Add(retainWindow)), nil
}

func recoveryRetentionCloneURL(cfg *instance.Config, key string) (string, error) {
	clone := repoCloneURL
	if clone == nil {
		clone = runner.DefaultRepoCloneURL
	}
	var url string
	for _, repo := range cfg.Repos {
		identity := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		if identity.CanonicalKey() != key {
			continue
		}
		if url != "" {
			return "", fmt.Errorf("ambiguous recovery repository configuration")
		}
		var err error
		url, err = clone(apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name})
		if err != nil {
			return "", err
		}
	}
	if url == "" {
		return "", fmt.Errorf("recovery repository is not configured")
	}
	return url, nil
}
