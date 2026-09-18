package main

import (
	"context"
	"fmt"
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

// Terminal capture closes the gap between the two cleanup guards. Stage
// worktrees are torn down through the NONTERMINAL guard, which cannot know the
// run is about to end, and the terminal guard only ever sees worktrees that
// still exist when FinalizeRun walks them. A run that commits its
// implementation, exhausts its budget before a PR exists and then turns
// terminal therefore reaches termination with no worktree left to capture
// from — while the work itself survives on the mirror's local run branch,
// which `git worktree remove` never deletes and which the retention sweep only
// prunes once its tip is an ancestor of a base branch. Nothing in the recovery
// contract can reach a branch, so without this capture that implementation is
// retained by accident and published nowhere.
//
// The checks are ordered so that everything which can answer "this run has
// nothing to protect" runs BEFORE anything that can fail for a reason of its
// own. Most terminal runs — every branch-less one, every run on an instance
// with no mirror for its branch — must finalize exactly as they did before
// this capture existed, including on an instance whose configuration cannot be
// loaded at all. Only a run whose branch really is sitting in a managed mirror
// is allowed to make finalization fail, and then only because protecting real
// work genuinely failed. This is the same rule installTerminalRecoveryGuard
// already applies by resolving configuration only once an owned worktree
// actually needs cleaning up.

// captureTerminalRunBranch publishes a recovery snapshot of runID's local run
// branch when the branch carries work that nothing retained already covers.
//
// It is a no-op, never an error, when there is nothing it could protect: no
// worktree manager, an unreadable run journal, no run branch recorded for the
// run, or no managed mirror holding that branch (which is also every pinned
// run — a pinned workspace hands its state off through handoffPinnedState, and
// its branch is not in a managed mirror).
//
// Once the branch IS found in a mirror, failure is returned rather than
// swallowed: the caller folds it into the same deferred-cleanup error a
// refused terminal handoff produces, so the run stays active and finalization
// is retried rather than acknowledging a terminal run whose only
// implementation was never published.
func captureTerminalRunBranch(l instance.Layout, wtMgr *worktree.Manager, runID string) error {
	if wtMgr == nil {
		return nil
	}
	if _, err := recovery.RefForRun(runID); err != nil {
		return nil
	}
	branch, events, startedAt, ok := terminalCaptureBranch(l, runID)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Everything below here only runs for a run whose branch was actually
	// found in a managed mirror, so a failure means real work is at risk.
	_, err := wtMgr.WithRunBranchCheckout(ctx, branch, func(key, path, tip string) error {
		return retainTerminalRunBranch(ctx, l, wtMgr, terminalCaptureSite{
			runID: runID, managedKey: key, events: events, startedAt: startedAt,
		}, path, tip)
	})
	return err
}

// terminalCaptureBranch reads the run branch the runner recorded for this run.
// The journal is the single source consulted: deriving the name from the
// namespace, workflow and run ID would reproduce a convention rather than read
// the branch the run actually used.
//
// Every failure reports "no branch", not an error. A run whose journal cannot
// be read or carries no branch reference has nothing on disk for this capture
// to protect, and terminal finalization of such a run must not start failing
// because a capture could not read a journal it did not need.
func terminalCaptureBranch(l instance.Layout, runID string) (string, []journal.Event, time.Time, bool) {
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return "", nil, time.Time{}, false
	}
	identity, err := reader.Identity()
	if err != nil || identity.RunID != runID || identity.StartedAt.IsZero() {
		return "", nil, time.Time{}, false
	}
	events, err := reader.Events()
	if err != nil {
		return "", nil, time.Time{}, false
	}
	for i := range events {
		ref := events[i].ExternalRef
		if ref != nil && ref.Kind == "branch" && ref.ID != "" {
			return ref.ID, events, identity.StartedAt, true
		}
	}
	return "", nil, time.Time{}, false
}

// terminalCaptureSite is what the mirror visit already knows about the run
// whose branch it found.
type terminalCaptureSite struct {
	runID      string
	managedKey string
	events     []journal.Event
	startedAt  time.Time
}

// retainTerminalRunBranch publishes the branch tip through the same Retain path
// a stage capture uses, from a throwaway detached checkout of that tip.
//
// Two conditions suppress it, in increasing cost order. SkipEmpty (set by
// recoveryCleanupRequest) drops a capture whose cumulative implementation
// against the base is empty, which is exactly the "tip carries nothing the base
// does not" case — expressed as the net difference rather than as SHA equality,
// so a tip that merely merged or reverted back to the base is also skipped.
// terminalCaptureCovered then drops a capture whose work an existing retained
// record already protects.
func retainTerminalRunBranch(ctx context.Context, l instance.Layout, wtMgr *worktree.Manager, site terminalCaptureSite, path, tip string) error {
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return fmt.Errorf("load terminal recovery configuration: %w", err)
	}
	key, baseRef, ok, err := terminalCaptureIdentity(l, cfg, site.managedKey)
	if err != nil || !ok {
		// A mirror no configured repository claims cannot be attributed to a
		// repository identity, and a record without one is unusable. There is
		// no partial publication to make here.
		return err
	}
	captureAt, err := recoveryWindowTime(site.events, site.startedAt)
	if err != nil {
		return err
	}
	publication := recoveryCleanupJournal{directory: l.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
	request, err := recoveryCleanupRequest(l, cfg, wtMgr.Root, wtMgr, key, path, site.runID, captureAt, publication)
	if err != nil {
		return err
	}
	request.BaseRef = baseRef
	covered, err := terminalCaptureCovered(ctx, request, path, tip)
	if err != nil || covered {
		return err
	}
	_, _, err = recovery.Retain(ctx, request, publication)
	return err
}

// terminalCaptureIdentity attributes the managed mirror the branch was found in
// back to a configured repository, returning that repository's canonical key
// and the base ref a stage capture of the same run would have recorded. It
// reuses the gaggle-project resolution terminal branch cleanup already applies
// and reproduces the runner's own base-ref rule (the project's branch, "main"
// when unset).
func terminalCaptureIdentity(l instance.Layout, cfg *instance.Config, managedKey string) (string, string, bool, error) {
	if len(cfg.Repos) == 0 {
		return "", "", false, nil
	}
	project, err := terminalGaggleProject(l)
	if err != nil {
		return "", "", false, err
	}
	cloneURL := repoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	for _, repo := range cfg.Repos {
		url, err := cloneURL(apiv1.RepoRef{
			Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL,
			Owner: repo.Owner, Project: repo.Project, Name: repo.Name,
		})
		if err != nil {
			return "", "", false, err
		}
		if worktree.RepositoryKey(url) != managedKey {
			continue
		}
		key := providers.RepositoryRef{
			Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL,
			Owner: repo.Owner, Project: repo.Project, Name: repo.Name,
		}.CanonicalKey()
		baseRef := project.Branch
		if baseRef == "" {
			baseRef = "main"
		}
		return key, baseRef, true, nil
	}
	return "", "", false, nil
}

// terminalCaptureCovered reports whether some record already retained for this
// run protects the branch tip, so terminal capture does not spend a second
// scarce inventory slot on work that is already published.
func terminalCaptureCovered(ctx context.Context, request recovery.RetentionRequest, path, tip string) (bool, error) {
	entries, err := recovery.ReadInventory(ctx, request.InventoryRoot, request.MaxSnapshots)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Record.RunID != request.RunID {
			continue
		}
		// The checkout shares the mirror's object store, so a stage capture's
		// snapshot commit and its parent are both readable from here.
		covered, err := recovery.SnapshotCoversCommit(ctx, path, entry.Record.SnapshotSHA, tip)
		if err != nil {
			return false, err
		}
		if covered {
			return true, nil
		}
	}
	return false, nil
}
