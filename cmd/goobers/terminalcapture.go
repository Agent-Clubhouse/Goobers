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
	plan := &terminalCapturePlan{ctx: ctx, layout: l, manager: wtMgr, runID: runID, events: events, startedAt: startedAt}
	_, err := wtMgr.WithRunBranchCheckout(ctx, branch, plan.worthCapturing, plan.publish)
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
	if journal.PhaseFromEvents(events) == journal.PhaseCompleted {
		// A run that completed delivered its work through its own outputs —
		// it pushed its branch, opened its PR, closed its item. The run branch
		// is emphatically not the last copy of anything, and #5355 is about
		// runs that end WITHOUT delivering. Capturing here would spend a slot
		// in a scarce instance-wide inventory, on every successful run, to
		// protect work nobody will ever restore — crowding out exactly the
		// failed runs the inventory exists for. It would also pay for a
		// checkout inside terminal finalization on the hot path.
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

// terminalCapturePlan carries what the two phases of one capture share: the
// run's own evidence going in, and the retention request the decision phase
// built going out. Splitting the work this way is what keeps the expensive
// checkout off the path of every terminal run whose branch needs nothing.
type terminalCapturePlan struct {
	ctx       context.Context
	layout    instance.Layout
	manager   *worktree.Manager
	runID     string
	events    []journal.Event
	startedAt time.Time

	request     recovery.RetentionRequest
	publication recovery.PublicationJournal
}

// worthCapturing decides, from the bare mirror alone, whether this run branch
// needs a recovery snapshot — and builds the retention request if it does.
// Nothing here checks anything out.
//
// Three conditions suppress the capture, in increasing cost order. A branch
// already contained in its base carries nothing to protect. A branch some
// record retained for this run was already captured from is protected already.
// SkipEmpty (set by recoveryCleanupRequest) is the final backstop during
// publication, for a tip whose cumulative implementation turns out to be empty
// for a reason the containment test could not see.
func (p *terminalCapturePlan) worthCapturing(managedKey, mirror, tip string) (bool, error) {
	cfg, err := instance.LoadConfig(p.layout.ConfigFile())
	if err != nil {
		return false, fmt.Errorf("load terminal recovery configuration: %w", err)
	}
	key, baseRef, ok, err := terminalCaptureIdentity(p.layout, cfg, managedKey)
	if err != nil || !ok {
		// A mirror no configured repository claims cannot be attributed to a
		// repository identity, and a record without one is unusable. There is
		// no partial publication to make here.
		return false, err
	}
	contained, err := recovery.CommitContainedIn(p.ctx, mirror, tip, baseRef)
	if err != nil || contained {
		return false, err
	}
	captureAt, err := recoveryWindowTime(p.events, p.startedAt)
	if err != nil {
		return false, err
	}
	publication := recoveryCleanupJournal{directory: p.layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
	// Repository is filled in by publish: the request is built here, where the
	// policy and inventory resolve once, but the snapshot is captured from the
	// checkout that only exists if this function says yes.
	request, err := recoveryCleanupRequest(p.layout, cfg, p.manager.Root, p.manager, key, "", p.runID, captureAt, publication)
	if err != nil {
		return false, err
	}
	request.BaseRef = baseRef
	covered, err := terminalCaptureCovered(p.ctx, request, mirror, tip)
	if err != nil || covered {
		return false, err
	}
	p.request, p.publication = request, publication
	return true, nil
}

// publish captures the branch tip from the throwaway checkout through the same
// Retain path a stage capture uses.
func (p *terminalCapturePlan) publish(path, _ string) error {
	request := p.request
	request.Repository = path
	_, _, err := recovery.Retain(p.ctx, request, p.publication)
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
