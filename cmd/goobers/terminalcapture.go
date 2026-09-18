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

// terminalCapture is the resolved identity one terminal branch capture needs:
// which mirror to look in, which branch, and the same base, key and window a
// stage capture of the same run would have recorded.
type terminalCapture struct {
	repoURL       string
	repositoryKey string
	branch        string
	baseRef       string
	runID         string
	captureAt     time.Time
}

// captureTerminalRunBranch publishes a recovery snapshot of runID's local run
// branch when the branch carries work that nothing retained already covers. It
// runs on every terminal finalization; the cheap checks (no configured repo, no
// journaled branch, no such branch in the mirror, nothing to capture, already
// covered) all short-circuit before any checkout happens.
//
// Failure is returned, never swallowed: the caller folds it into the same
// deferred-cleanup error a refused terminal handoff produces, so the run stays
// active and finalization is retried rather than acknowledging a terminal run
// whose only implementation was never published.
func captureTerminalRunBranch(l instance.Layout, wtMgr *worktree.Manager, runID string) error {
	if wtMgr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	capture, cfg, ok, err := planTerminalCapture(ctx, l, runID)
	if err != nil || !ok {
		return err
	}
	_, err = wtMgr.WithRunBranchCheckout(ctx, capture.repoURL, capture.branch, func(path, tip string) error {
		return retainTerminalRunBranch(ctx, l, cfg, wtMgr, capture, path, tip)
	})
	return err
}

// planTerminalCapture resolves the capture identity from the run's own durable
// record, reporting ok=false when this run can have nothing to capture.
func planTerminalCapture(ctx context.Context, l instance.Layout, runID string) (terminalCapture, *instance.Config, bool, error) {
	if _, err := recovery.RefForRun(runID); err != nil {
		return terminalCapture{}, nil, false, err
	}
	reader, err := journal.OpenReadOnly(filepath.Join(l.RunsDir(), runID))
	if err != nil {
		return terminalCapture{}, nil, false, fmt.Errorf("terminal recovery run journal unavailable: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return terminalCapture{}, nil, false, fmt.Errorf("terminal recovery run identity unavailable: %w", err)
	}
	if identity.RunID != runID || identity.StartedAt.IsZero() {
		return terminalCapture{}, nil, false, fmt.Errorf("terminal recovery run identity does not match the finalizing run")
	}
	events, err := reader.Events()
	if err != nil {
		return terminalCapture{}, nil, false, fmt.Errorf("terminal recovery run evidence unavailable: %w", err)
	}
	branch := journaledRunBranch(events)
	if branch == "" {
		// A run with no recorded branch never had a local run branch to
		// advance, so there is nothing on the mirror to rescue.
		return terminalCapture{}, nil, false, nil
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return terminalCapture{}, nil, false, fmt.Errorf("load terminal recovery configuration: %w", err)
	}
	if len(cfg.Repos) == 0 {
		return terminalCapture{}, cfg, false, nil
	}
	captureAt, err := recoveryCaptureTime(ctx, reader, identity.StartedAt, true)
	if err != nil {
		return terminalCapture{}, nil, false, err
	}
	repoURL, key, baseRef, err := terminalCaptureRepository(l, cfg)
	if err != nil {
		return terminalCapture{}, nil, false, err
	}
	return terminalCapture{
		repoURL: repoURL, repositoryKey: key, branch: branch,
		baseRef: baseRef, runID: runID, captureAt: captureAt,
	}, cfg, true, nil
}

// terminalCaptureRepository resolves the clone URL, canonical repository key
// and base ref of the repository the finalizing run's branch belongs to. It
// reuses the gaggle-project resolution terminal branch cleanup already applies,
// and reproduces the runner's own base-ref rule (the project's branch, "main"
// when unset) so the published record names the base a stage capture would.
func terminalCaptureRepository(l instance.Layout, cfg *instance.Config) (string, string, string, error) {
	project, err := terminalGaggleProject(l)
	if err != nil {
		return "", "", "", err
	}
	configured := cfg.Repos[0]
	if project.Owner != "" && project.Name != "" {
		if match, ok := configuredRepoForProject(cfg, project); ok {
			configured = match
		}
	}
	cloneURL := repoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	repoURL, err := cloneURL(apiv1.RepoRef{
		Provider: apiv1.Provider(configured.Provider), BaseURL: configured.BaseURL,
		Owner: configured.Owner, Project: configured.Project, Name: configured.Name,
	})
	if err != nil {
		return "", "", "", err
	}
	key := providers.RepositoryRef{
		Provider: providers.ProviderKind(configured.Provider), URL: configured.BaseURL,
		Owner: configured.Owner, Project: configured.Project, Name: configured.Name,
	}.CanonicalKey()
	baseRef := project.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	return repoURL, key, baseRef, nil
}

// journaledRunBranch returns the run branch the runner recorded for this run.
// The journal is the single source consulted: deriving the name from the
// namespace, workflow and run ID would reproduce a convention rather than read
// the branch the run actually used.
func journaledRunBranch(events []journal.Event) string {
	for i := range events {
		ref := events[i].ExternalRef
		if ref != nil && ref.Kind == "branch" && ref.ID != "" {
			return ref.ID
		}
	}
	return ""
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
func retainTerminalRunBranch(ctx context.Context, l instance.Layout, cfg *instance.Config, wtMgr *worktree.Manager, capture terminalCapture, path, tip string) error {
	publication := recoveryCleanupJournal{directory: l.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
	request, err := recoveryCleanupRequest(l, cfg, wtMgr.Root, wtMgr, capture.repositoryKey, path, capture.runID, capture.captureAt, publication)
	if err != nil {
		return err
	}
	request.BaseRef = capture.baseRef
	covered, err := terminalCaptureCovered(ctx, request, path, tip)
	if err != nil || covered {
		return err
	}
	_, _, err = recovery.Retain(ctx, request, publication)
	return err
}

// terminalCaptureCovered reports whether some record already retained for this
// run protects the branch tip, so terminal capture does not spend a second
// scarce inventory slot on work that is already published.
func terminalCaptureCovered(ctx context.Context, request recovery.RetentionRequest, path, tip string) (bool, error) {
	entries, _, err := recovery.ReadInventoryTolerant(ctx, request.InventoryRoot, request.MaxSnapshots)
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
