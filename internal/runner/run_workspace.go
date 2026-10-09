package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

type stageWorkspace struct {
	scratchContainer string
	scratchRunsDir   string
	path             string
	worktree         *worktree.Worktree
	// additional are read-only reference-repo checkouts (MGV-11 #1286) provisioned
	// alongside the primary worktree; torn down with it. Each carries its name for
	// the invocation envelope's AdditionalWorkspaces.
	additional []additionalCheckout
	// sparse records the repo-relative cones this workspace's checkout was
	// materialized with (project.checkout.sparse, #649) — empty for a full
	// checkout. Surfaced to the stage via the invocation envelope's
	// CheckoutCones so an agentic stage knows the tree is partial instead of
	// treating a pruned path as unexpectedly deleted.
	sparse  []string
	release func()
	// retainedChild verifies custody instead of deleting the shared child fork.
	retainedChild func(context.Context) error
	// validateReadOnly checks an immutable stage view before accepting output.
	validateReadOnly func(context.Context) error
}

// additionalWorkspaces projects a stage workspace's provisioned reference
// checkouts into the invocation envelope's AdditionalWorkspaces (MGV-11 #1286).
func additionalWorkspaces(w *stageWorkspace) []apiv1.AdditionalWorkspace {
	if w == nil || len(w.additional) == 0 {
		return nil
	}
	out := make([]apiv1.AdditionalWorkspace, 0, len(w.additional))
	for _, a := range w.additional {
		if a.worktree == nil {
			continue
		}
		out = append(out, apiv1.AdditionalWorkspace{Name: a.name, Path: a.worktree.Path})
	}
	return out
}

// checkoutCones projects a stage workspace's sparse-checkout cones into the
// invocation envelope's CheckoutCones (project.checkout.sparse, #649): ""
// for the primary Workspace, else the matching AdditionalWorkspaces[i].Name.
// A workspace with a full checkout is omitted, so the common (non-sparse)
// case publishes no field at all.
func checkoutCones(w *stageWorkspace) map[string][]string {
	if w == nil {
		return nil
	}
	cones := map[string][]string{}
	if len(w.sparse) > 0 {
		cones[""] = w.sparse
	}
	for _, a := range w.additional {
		if len(a.sparse) > 0 {
			cones[a.name] = a.sparse
		}
	}
	if len(cones) == 0 {
		return nil
	}
	return cones
}

// sparseCones returns spec's declared cones, or nil for a full checkout
// (spec nil, or Sparse empty — validation rejects an explicitly empty Sparse
// list, so an empty result here only ever means no checkout override was
// declared).
func sparseCones(spec *apiv1.CheckoutSpec) []string {
	if spec == nil {
		return nil
	}
	return spec.Sparse
}

// additionalCheckout is one provisioned read-only reference-repo worktree.
type additionalCheckout struct {
	name     string
	worktree *worktree.Worktree
	// sparse records the cones this reference checkout was materialized with
	// (project.checkout.sparse, #649) — empty for a full checkout.
	sparse []string
}

func (w *stageWorkspace) ActivateAssetPathGuard() error {
	if w.worktree == nil {
		return nil
	}
	return w.worktree.ActivateAssetPathGuard()
}

func (w *stageWorkspace) ValidateReservedPaths(ctx context.Context) error {
	if w.worktree == nil {
		return nil
	}
	return w.worktree.ValidateReservedPaths(ctx)
}

func (w *stageWorkspace) finishDispatch(ctx context.Context, preserve bool) error {
	if !preserve {
		return w.Remove(ctx)
	}
	if w.release != nil {
		w.release()
		w.release = nil
	}
	return fmt.Errorf("mutation projection failed; retained stage workspace %q", w.path)
}

func (w *stageWorkspace) Remove(ctx context.Context) error {
	defer func() {
		if w.release != nil {
			w.release()
			w.release = nil
		}
	}()
	// Tear down the read-only reference checkouts (MGV-11 #1286) first; they are
	// independent worktrees off their own mirrors, so a failure to remove one must
	// not block removing the primary worktree. Best-effort: collect the first error.
	var firstErr error
	for _, a := range w.additional {
		if a.worktree == nil {
			continue
		}
		if err := a.worktree.Remove(ctx, worktree.RemoveOptions{}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if w.worktree != nil {
		if w.retainedChild != nil {
			return errors.Join(firstErr, w.retainedChild(ctx))
		}
		if err := w.worktree.Remove(ctx, worktree.RemoveOptions{}); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	if w.scratchContainer != "" {
		return errors.Join(firstErr, removeOwnedScratch(ctx, w.scratchContainer, w.scratchRunsDir))
	}
	if err := os.RemoveAll(w.path); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// buildEnvelope provisions an isolated repository worktree or empty scratch
// directory and builds one stage attempt's invocation envelope.
func (r *Runner) buildEnvelope(ctx context.Context, in StartInput, stageName, goal string, taskInputs map[string]string, capabilities []string, limits apiv1.Limits, upstream []apiv1.ContextPointer, workspaceMode apiv1.WorkspaceMode, syncBase bool, workspaceBranch string) (apiv1.InvocationEnvelope, *stageWorkspace, error) {
	workspace, err := r.createStageWorkspace(ctx, in, stageName, workspaceMode, syncBase, workspaceBranch)
	if err != nil {
		return apiv1.InvocationEnvelope{}, nil, err
	}
	if workspaceMode == apiv1.WorkspaceScratch && slices.Contains(capabilities, string(capability.ContentsRead)) {
		workspace.additional, err = r.provisionAdditionalCheckouts(ctx, in, stageName)
		if err != nil {
			_ = workspace.Remove(ctx)
			return apiv1.InvocationEnvelope{}, nil, err
		}
	}

	inputs := make(map[string]interface{}, len(taskInputs))
	for k, v := range taskInputs {
		inputs[k] = v
	}
	baseBranch := in.configuredRepository().Branch
	if baseBranch == "" {
		baseBranch = "main"
	}
	env := apiv1.InvocationEnvelope{
		TaskID:               in.RunID + ":" + stageName,
		InstanceID:           in.instanceID,
		WorkflowID:           in.Machine.Def.Name,
		RunID:                in.RunID,
		TriggerRef:           in.Trigger.Ref,
		Gaggle:               in.Gaggle,
		BranchNamespace:      r.branchNamespaceFor(in.Gaggle),
		BaseBranch:           baseBranch,
		Goal:                 goal,
		GooberDigest:         in.GooberDigest,
		ConfigGeneration:     in.configGeneration,
		Workspace:            workspace.path,
		RepoRef:              in.RepoRef.EnvelopeRef(),
		WorkspaceRevision:    in.workspaceRevision.DeepCopy(),
		AdditionalWorkspaces: additionalWorkspaces(workspace),
		CheckoutCones:        checkoutCones(workspace),
		Item:                 in.Item,
		ContextPointers:      upstream,
		Capabilities:         capabilities,
		Limits:               limits,
		Inputs:               inputs,
	}
	return env, workspace, nil
}

// createStageWorkspace provisions this stage attempt's workspace. workspaceBranch
// is the run-scoped branch rebinding (WorkspaceBranchOutput, #392): empty — the
// normal case — means the run's own branch, providers.BranchName.
func (r *Runner) createStageWorkspace(ctx context.Context, in StartInput, stageName string, mode apiv1.WorkspaceMode, syncBase bool, workspaceBranch string) (*stageWorkspace, error) {
	if in.heldChildWorkspace != nil {
		return in.heldChildWorkspace, nil
	}
	if in.ChildWorkspace != nil && mode != apiv1.WorkspaceScratch {
		return r.createChildStageWorkspace(ctx, in, stageName, mode, syncBase, workspaceBranch)
	}
	if err := selectedWorkspaceUnsupported(in, mode); err != nil {
		return nil, err
	}
	switch mode {
	case apiv1.WorkspaceScratch:
		if syncBase {
			return nil, fmt.Errorf("create scratch workspace: syncBase requires a repo workspace")
		}

		if r.cfg.ScratchDir == "" && in.pinnedWorkspace != nil {
			in.pinnedStage.Lock()
			if err := r.preparePinnedStage(ctx, in, false, workspaceBranch); err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			if err := verifyWorkspaceBranchSHA(ctx, in, in.pinnedWorkspace, workspaceBranch); err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			return &stageWorkspace{path: in.pinnedWorkspace.Path, worktree: in.pinnedWorkspace, release: in.pinnedStage.Unlock}, nil
		}
		if r.cfg.ScratchDir == "" {
			return nil, fmt.Errorf("create scratch workspace: runner ScratchDir is required")
		}
		if err := os.MkdirAll(r.cfg.ScratchDir, 0o700); err != nil {
			return nil, fmt.Errorf("create scratch workspace root: %w", err)
		}
		return createOwnedScratch(r.cfg.ScratchDir, r.cfg.RunsDir, in.RunID)
	case apiv1.WorkspaceRepoReadOnly:
		if in.pinnedWorkspace != nil {
			if syncBase {
				return nil, fmt.Errorf("create read-only workspace: syncBase requires a writable repo workspace")
			}
			if workspaceBranch != "" {
				return nil, readOnlyRebindError(in, stageName, workspaceBranch)
			}
			in.pinnedStage.Lock()
			if err := r.preparePinnedStage(ctx, in, false, ""); err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			additional, err := r.provisionAdditionalCheckouts(ctx, in, stageName)
			if err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			return &stageWorkspace{path: in.pinnedWorkspace.Path, worktree: in.pinnedWorkspace, additional: additional, release: in.pinnedStage.Unlock}, nil
		}
		// A detached checkout at the pinned base revision: no branch name, so
		// two of these can coexist for one run. That is the whole point —
		// every writable repo workspace is created on ONE run branch and git
		// refuses to check one branch out in two worktrees, so concurrent
		// repo-backed branch stages would otherwise collide outright
		// (docs/design/static-fan-out-fan-in.md §6.5).
		if syncBase {
			return nil, fmt.Errorf("create read-only workspace: syncBase requires a writable repo workspace")
		}
		if workspaceBranch != "" {
			return nil, readOnlyRebindError(in, stageName, workspaceBranch)
		}
		repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
		if err != nil {
			return nil, err
		}
		baseRef := in.RepoRef.Branch
		if baseRef == "" {
			baseRef = "main"
		}
		sparse := sparseCones(in.RepoRef.Checkout)
		wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      in.RunID + "-" + stageName,
			OwnerRunID: in.RunID,
			BaseRef:    baseRef,
			// Branch deliberately empty — a detached checkout, the same shape
			// provisionAdditionalCheckouts already uses for reference repos.
			Branch: "",
			Sparse: sparse,
		})
		if err != nil {
			return nil, fmt.Errorf("create read-only worktree: %w", err)
		}
		additional, err := r.provisionAdditionalCheckouts(ctx, in, stageName)
		if err != nil {
			_ = wt.Remove(ctx, worktree.RemoveOptions{})
			return nil, err
		}
		return &stageWorkspace{path: wt.Path, worktree: wt, additional: additional, sparse: sparse}, nil
	case "", apiv1.WorkspaceRepo:
		if in.pinnedWorkspace != nil {
			in.pinnedStage.Lock()
			if err := r.preparePinnedStage(ctx, in, syncBase, workspaceBranch); err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			if err := verifyWorkspaceBranchSHA(ctx, in, in.pinnedWorkspace, workspaceBranch); err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			additional, err := r.provisionAdditionalCheckouts(ctx, in, stageName)
			if err != nil {
				in.pinnedStage.Unlock()
				return nil, err
			}
			return &stageWorkspace{path: in.pinnedWorkspace.Path, worktree: in.pinnedWorkspace, additional: additional, release: in.pinnedStage.Unlock}, nil
		}
		repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
		if err != nil {
			return nil, err
		}

		baseRef := in.RepoRef.Branch
		if baseRef == "" {
			baseRef = "main"
		}
		branch := providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
		if workspaceBranch != "" {
			branch = workspaceBranch
		}
		sparse := sparseCones(in.RepoRef.Checkout)
		wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      in.RunID + "-" + stageName,
			OwnerRunID: in.RunID,
			BaseRef:    baseRef,
			Branch:     branch,
			SyncBase:   syncBase,
			// A rebound branch names work that already exists; creating it
			// from base instead would hand the stage a pristine checkout
			// wearing the PR's branch name. Fail loudly instead.
			RequireExistingBranch: workspaceBranch != "",
			AcquireRemoteBranch:   workspaceBranch != "",
			Sparse:                sparse,
		})
		if err != nil {
			return nil, fmt.Errorf("create worktree: %w", err)
		}
		if err := verifyWorkspaceBranchSHA(ctx, in, wt, workspaceBranch); err != nil {
			_ = wt.Remove(ctx, worktree.RemoveOptions{})
			return nil, err
		}
		additional, err := r.provisionAdditionalCheckouts(ctx, in, stageName)
		if err != nil {
			// Best-effort teardown of the primary worktree so a failed
			// reference-repo checkout doesn't leak the stage's main worktree.
			_ = wt.Remove(ctx, worktree.RemoveOptions{})
			return nil, err
		}

		return &stageWorkspace{path: wt.Path, worktree: wt, additional: additional, sparse: sparse}, nil
	default:
		return nil, fmt.Errorf("unknown workspace mode %q", mode)
	}
}

func repoRefPtr(ref apiv1.RepoRef) *apiv1.RepoRef {
	repository := ref
	return &repository
}

func verifyWorkspaceBranchSHA(ctx context.Context, in StartInput, wt *worktree.Worktree, branch string) error {
	expected := strings.TrimSpace(in.WorkspaceBranchSHA)
	if strings.TrimSpace(branch) == "" || expected == "" {
		return nil
	}
	actual, err := wt.HeadSHA(ctx)
	if err != nil {
		return fmt.Errorf("verify workspace branch %q commit: %w", branch, err)
	}
	if !strings.EqualFold(strings.TrimSpace(actual), expected) {
		return fmt.Errorf("workspace branch %q advanced to %q, expected %q", branch, strings.TrimSpace(actual), expected)
	}
	return nil
}

func (r *Runner) preparePinnedStage(ctx context.Context, in StartInput, syncBase bool, workspaceBranch string) error {
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	branch := providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	if workspaceBranch != "" {
		branch = workspaceBranch
	}
	if err := in.pinnedWorkspace.PreparePinned(ctx, worktree.PinnedPrepareOptions{
		BaseRef:               baseRef,
		Branch:                branch,
		SyncBase:              syncBase,
		RequireExistingBranch: workspaceBranch != "",
	}); err != nil {
		return fmt.Errorf("prepare pinned workspace: %w", err)
	}
	return nil
}

func (r *Runner) acquirePinnedWorkspace(ctx context.Context, jr executionJournal, in *StartInput) (*worktree.PinnedLease, error) {
	if in.Child != nil {
		// Child admission owns its separate managed fork. Pure scratch child
		// machines need no project lease either.
		return nil, nil
	}
	if !r.cfg.PinnedWorkspace {
		return nil, nil
	}
	if err := selectedWorkspaceUnsupported(*in, apiv1.WorkspaceRepo); err != nil {
		return nil, err
	}
	if len(r.cfg.AdditionalRepos) > 0 {
		return nil, fmt.Errorf("runner: pinned project workspaces cannot provision additional repository worktrees")
	}
	r.pinnedMu.Lock()
	owned := r.pinnedRuns[in.RunID]
	r.pinnedMu.Unlock()
	if owned != nil {
		in.pinnedWorkspace = owned.Worktree
		in.pinnedStage = &sync.Mutex{}
		return owned, nil
	}
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	branch := providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	lease, err := r.cfg.Worktrees.AcquirePinned(stalledAttemptContext(ctx), worktree.PinnedOptions{
		RepoURL:     repoURL,
		RunID:       in.RunID,
		BaseRef:     baseRef,
		Branch:      branch,
		CleanPolicy: worktree.PinnedCleanPolicy(r.cfg.PinnedCleanPolicy),
		OnQueuePosition: func(position int) error {
			return jr.Append(journal.Event{
				Type: journal.EventRunnerAnnotation,
				Runner: map[string]any{
					"workspaceMode": "pinned",
					"queuePosition": position,
				},
			})
		},
		OnQueueWait: jr.ObserveActivity,
	})
	if err != nil {
		return nil, fmt.Errorf("runner: acquire pinned workspace for run %q: %w", in.RunID, err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"workspaceMode": "pinned",
			"queuePosition": 0,
		},
	}); err != nil {
		_ = lease.Release()
		return nil, fmt.Errorf("runner: journal pinned workspace acquisition for run %q: %w", in.RunID, err)
	}
	in.pinnedWorkspace = lease.Worktree
	in.pinnedStage = &sync.Mutex{}
	r.pinnedMu.Lock()
	r.pinnedRuns[in.RunID] = lease
	r.pinnedMu.Unlock()
	return lease, nil
}

func (r *Runner) releasePinnedWorkspace(runID string) error {
	r.pinnedMu.Lock()
	lease := r.pinnedRuns[runID]
	delete(r.pinnedRuns, runID)
	r.pinnedMu.Unlock()
	return lease.Release()
}

// provisionAdditionalCheckouts materializes a read-only checkout of each of the
// gaggle's reference repos (Config.AdditionalRepos, MGV-11 #1286) for a
// repo-workspace stage. Each is a detached worktree (Branch:"") off its own
// mirror at the repo's default branch — the stage may READ it, but the worktree
// Manager's git-auth resolver only ever holds that repo's contents:read token
// (MGV-10), so there is no credential to push. A gaggle with no AdditionalRepos
// returns nil and provisions nothing (byte-identical to prior behavior). A
// failure to provision any reference repo tears down the ones already created
// and fails the stage — a stage must not start with a partial reference set.
func (r *Runner) provisionAdditionalCheckouts(ctx context.Context, in StartInput, stageName string) ([]additionalCheckout, error) {
	if len(r.cfg.AdditionalRepos) == 0 {
		return nil, nil
	}
	checkouts := make([]additionalCheckout, 0, len(r.cfg.AdditionalRepos))
	for _, repo := range r.cfg.AdditionalRepos {
		repoURL, err := r.cfg.RepoCloneURL(repo)
		if err != nil {
			r.teardownCheckouts(ctx, checkouts)
			return nil, fmt.Errorf("resolve reference repo %q clone URL: %w", repo.Name, err)
		}
		baseRef := repo.Branch
		if baseRef == "" {
			baseRef = "main"
		}
		sparse := sparseCones(repo.Checkout)
		wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      in.RunID + "-" + stageName + "-ref-" + sanitizeRefName(repo.Name),
			OwnerRunID: in.RunID,
			BaseRef:    baseRef,
			// Detached, read-only: no run branch, never pushed.
			Branch: "",
			Sparse: sparse,
		})
		if err != nil {
			r.teardownCheckouts(ctx, checkouts)
			return nil, fmt.Errorf("checkout reference repo %q: %w", repo.Name, err)
		}
		checkouts = append(checkouts, additionalCheckout{name: repo.Name, worktree: wt, sparse: sparse})
	}
	return checkouts, nil
}

// teardownCheckouts best-effort removes already-provisioned reference checkouts
// after a mid-provision failure, so a partial reference set never leaks.
func (r *Runner) teardownCheckouts(ctx context.Context, checkouts []additionalCheckout) {
	for _, c := range checkouts {
		if c.worktree != nil {
			_ = c.worktree.Remove(ctx, worktree.RemoveOptions{})
		}
	}
}

// sanitizeRefName reduces a reference repo's name to a single safe path segment
// for use in the worktree RunID (which must be one segment, no "/" or ".."). Any
// run of non-alphanumeric characters collapses to a single '-'.
func sanitizeRefName(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "ref"
	}
	return out
}

// worktreeWarningEvent builds the runner.annotation journal event that surfaces
// a provisioned worktree's non-fatal warnings (#643 symlink flattening today),
// returning ok=false when there is nothing to report. The payload lives under
// Runner, which is excluded from conformance, so recording it never perturbs a
// run's normative event stream — it is purely an operator-visible note. Split
// out from dispatchTask so the event shape is unit-testable without a live
// worktree provision.
func worktreeWarningEvent(stage string, wt *worktree.Worktree) (journal.Event, bool) {
	if wt == nil || len(wt.Warnings) == 0 {
		return journal.Event{}, false
	}
	return journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Stage:  stage,
		Runner: map[string]any{"kind": "worktree.warnings", "warnings": wt.Warnings},
	}, true
}

func (w *stageWorkspace) ValidateAfterInvocation(ctx context.Context, invocation *gooberInvocation) error {
	var err error
	if w.validateReadOnly != nil {
		err = w.validateReadOnly(ctx)
	}
	if invocation != nil && invocation.materializedAssets() {
		err = errors.Join(err, w.ValidateReservedPaths(ctx))
	}
	return err
}
