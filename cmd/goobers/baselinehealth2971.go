package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/baseline"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

// baselineStateFileName holds the instance's cached baseline observations and
// its durable shared blockers. It is instance-scoped, not run-scoped: the whole
// point of #2971 is that the SECOND run affected by a red base pays nothing to
// learn what the first one measured.
const baselineStateFileName = "baseline-health.json"

// baselineHealthAdapter satisfies runner.BaselineHealth for the daemon: it
// resolves the target branch's current commit from the managed mirror and
// measures a baseline in a disposable, base-pinned worktree.
type baselineHealthAdapter struct {
	manager   *worktree.Manager
	evaluator *baseline.Evaluator
}

// buildBaselineHealth wires shared-baseline-failure detection, or returns nil
// when the instance has not opted in (baselineHealth.enabled) or has no
// worktree manager to measure with — in which case every CI failure keeps its
// pre-existing attribution.
func buildBaselineHealth(l instance.Layout, cfg *instance.Config, wtMgr *worktree.Manager) (runner.BaselineHealth, error) {
	if !cfg.BaselineHealthEnabled() || wtMgr == nil {
		return nil, nil
	}
	store, err := baseline.OpenStore(filepath.Join(l.Root, baselineStateFileName))
	if err != nil {
		return nil, err
	}
	adapter := &baselineHealthAdapter{manager: wtMgr}
	adapter.evaluator = &baseline.Evaluator{
		Store:        store,
		Prober:       &baseline.CommandProber{Checkout: adapter},
		ProbeTimeout: cfg.BaselineProbeTimeout(),
		RepairLane:   cfg.SharedRepairLaneEnabled(),
	}
	return adapter, nil
}

// BaseSHA reports the commit the repository's target branch currently points
// at, read from the managed mirror the run's own worktrees branch from.
func (a *baselineHealthAdapter) BaseSHA(ctx context.Context, repo apiv1.RepoRef, repoURL string) (string, error) {
	dir, err := a.manager.WorkingCopy(ctx, repoURL)
	if err != nil {
		return "", fmt.Errorf("baseline: working copy for %s/%s: %w", repo.Owner, repo.Name, err)
	}
	branch := repo.Branch
	if branch == "" {
		branch = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", branch)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("baseline: resolve %s in %s: %w", branch, dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Classify implements runner.BaselineHealth.
func (a *baselineHealthAdapter) Classify(ctx context.Context, req baseline.Request) (baseline.Decision, error) {
	return a.evaluator.Classify(ctx, req)
}

// ReleaseReady implements runner.BaselineHealth: it resolves the repository's
// current target-branch commit and un-parks every subject whose shared baseline
// failure was measured at an older base (or has since gone green). The base SHA
// comes from the managed mirror, so a release costs no provider call.
func (a *baselineHealthAdapter) ReleaseReady(ctx context.Context, repo apiv1.RepoRef, repoURL string) ([]baseline.Waiter, error) {
	baseSHA, err := a.BaseSHA(ctx, repo, repoURL)
	if err != nil {
		return nil, err
	}
	return a.evaluator.ReleaseReady(baseline.RepoKey(repo.Owner, repo.Name), baseSHA)
}

// Materialize implements baseline.Checkout with a disposable detached worktree
// at the pinned base commit — the target branch exactly as the affected run
// synced it, with none of that run's own commits.
//
// The directory is unique per probe rather than derived from the base SHA: two
// runs measuring the same base must never share one worktree, or the first to
// finish removes the tree the second is still running CI in. Concurrent
// measurement of the same baseline is separately collapsed by the evaluator's
// per-baseline probe lock, so this is the second line of defence.
func (a *baselineHealthAdapter) Materialize(ctx context.Context, target baseline.ProbeTarget) (string, func(), error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, fmt.Errorf("baseline: probe id: %w", err)
	}
	runID := fmt.Sprintf("baseline-%s-%s", baselineShortSHA(target.BaseSHA), hex.EncodeToString(nonce))
	wt, err := a.manager.Create(ctx, worktree.CreateOptions{
		RepoURL: target.RepoURL,
		RunID:   runID,
		BaseRef: target.BaseSHA,
	})
	if err != nil {
		return "", nil, err
	}
	release := func() {
		// A leaked probe worktree would pin disk for every red base ever
		// measured; removal failures are not the run's problem, so they are
		// deliberately not propagated into the classification.
		_ = wt.Remove(context.WithoutCancel(ctx), worktree.RemoveOptions{})
	}
	return wt.Path, release, nil
}

// baselineShortSHA keeps a probe worktree name readable; the nonce, not the
// commit, is what makes it unique.
func baselineShortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
