package workerhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// WorktreeWorkspaces is the production engine.WorkspaceProvisioner: a fresh,
// isolated, disposable working copy per stage attempt, provisioned exactly as
// the local runner's createStageWorkspace does — a git worktree on the run
// branch for repo mode, a detached worktree at the pinned base for
// repo-readonly, an empty temp directory for scratch mode. Every mode the
// DSL enum admits has an arm here: the engine threads a gate's or agentic
// task's declared workspace through unchanged, so a mode this switch did not
// know would fail a run that compiled clean. Without a wired provisioner the
// engine fails every workspace-needing stage closed (#621), so this is what
// makes `goobers worker` able to execute real stages.
//
// It is also the worker's half of mode-3 workspace continuity (#3803): a
// request carrying a WorkspaceDelta is landed on the run branch in the
// mirror BEFORE the worktree is cut (Manager.ApplyBundle, fast-forward-only),
// and a writable repo workspace publishes what its stage committed when the
// engine asks (worktreeWorkspace.PublishDelta). Both go through Store — the
// fleet-wide blob store the daemon's blob plane serves pods from — which is
// what lets a pod's commit reach a self-placed stage and vice versa.
type WorktreeWorkspaces struct {
	// Manager provisions repo-mode worktrees (mirror clone + git worktree add).
	Manager *worktree.Manager
	// ScratchDir roots scratch-mode workspaces.
	ScratchDir string
	// CloneURL derives the git remote for a RepoRef. Nil applies the local
	// runner's own derivation (runner.DefaultRepoCloneURL).
	CloneURL func(apiv1.RepoRef) (string, error)
	// Store is the content-addressed store workspace-delta bundles travel
	// through (the worker's --blob-store, the RWX volume the daemon's blob
	// plane serves pods from). Nil is the mode-1/2 self-only posture: nothing
	// is published, and a request that CARRIES a delta is refused rather than
	// provisioned at base — a stage that runs believing it continues from its
	// predecessor while it does not is the silent wrong result #3803 names.
	Store blobstore.Store
	// Log receives apply/publish diagnostics (the keep arm's two SHAs, the
	// applied tip) — the worker's own log is the far-side record of these
	// decisions. Nil writes to os.Stderr.
	Log io.Writer
}

func (p *WorktreeWorkspaces) log() io.Writer {
	if p.Log != nil {
		return p.Log
	}
	return os.Stderr
}

// Provision implements engine.WorkspaceProvisioner.
func (p *WorktreeWorkspaces) Provision(ctx context.Context, req engine.WorkspaceRequest) (engine.Workspace, error) {
	switch req.Mode {
	case apiv1.WorkspaceScratch:
		if req.SyncBase {
			// Compilation rejects the combination (v_current checks); this
			// guards a request constructed without it, same as the local
			// runner's createStageWorkspace.
			return nil, fmt.Errorf("workerhost: scratch workspace for stage %q: syncBase requires a repo workspace", req.Stage)
		}
		if req.WorkspaceDelta != "" {
			return nil, fmt.Errorf("workerhost: scratch workspace for stage %q was handed workspace delta %s; a scratch workspace has no run branch to land it on", req.Stage, req.WorkspaceDelta)
		}
		if p.ScratchDir == "" {
			return nil, fmt.Errorf("workerhost: scratch workspace for stage %q: no scratch dir configured", req.Stage)
		}
		if err := os.MkdirAll(p.ScratchDir, 0o700); err != nil {
			return nil, fmt.Errorf("workerhost: create scratch root: %w", err)
		}
		path, err := os.MkdirTemp(p.ScratchDir, "goobers-scratch-*")
		if err != nil {
			return nil, fmt.Errorf("workerhost: create scratch workspace: %w", err)
		}
		return scratchWorkspace(path), nil
	case apiv1.WorkspaceRepoReadOnly:
		// A detached checkout at the pinned base revision, the local runner's
		// createStageWorkspace arm exactly: no branch name, so it never
		// collides with the run branch another worktree holds and never
		// enters the continuity record (no branch, nothing to bundle). A
		// read-only stage reads base by definition, so a rebound branch, a
		// base sync or a delta are each a contradiction — refused, never
		// silently provisioned as the writable branch.
		if req.SyncBase {
			return nil, fmt.Errorf("workerhost: read-only workspace for stage %q: syncBase requires a writable repo workspace", req.Stage)
		}
		if req.WorkspaceBranch != "" {
			return nil, fmt.Errorf("workerhost: read-only workspace for stage %q: a rebound branch %q requires a writable repo workspace", req.Stage, req.WorkspaceBranch)
		}
		if req.WorkspaceDelta != "" {
			return nil, fmt.Errorf("workerhost: read-only workspace for stage %q was handed workspace delta %s; a read-only stage reads the pinned base and has no branch to land it on", req.Stage, req.WorkspaceDelta)
		}
		repoURL, baseRef, err := p.repoTarget(req)
		if err != nil {
			return nil, err
		}
		wt, err := p.Manager.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      req.RunID + "-" + req.Stage,
			OwnerRunID: req.RunID,
			BaseRef:    baseRef,
			Branch:     "",
			Sparse:     sparseCones(req.RepoRef.Checkout),
		})
		if err != nil {
			return nil, fmt.Errorf("workerhost: create read-only worktree for stage %q: %w", req.Stage, err)
		}
		return &worktreeWorkspace{wt: wt, manager: p.Manager, log: p.log(), repoURL: repoURL, base: baseRef}, nil
	case "", apiv1.WorkspaceRepo:
		repoURL, baseRef, err := p.repoTarget(req)
		if err != nil {
			return nil, err
		}
		branch := providers.BranchNameIn(
			providers.NormalizeBranchNamespace(req.BranchNamespace),
			req.Workflow, req.RunID,
		)
		if req.WorkspaceBranch != "" {
			namespace := providers.NormalizeBranchNamespace(req.BranchNamespace)
			if !strings.HasPrefix(req.WorkspaceBranch, namespace) {
				return nil, fmt.Errorf("workerhost: selected workspace branch %q for stage %q is outside namespace %q", req.WorkspaceBranch, req.Stage, namespace)
			}
			branch = req.WorkspaceBranch
		}
		if req.WorkspaceDelta != "" {
			if err := p.applyDelta(ctx, req, repoURL, branch, baseRef); err != nil {
				return nil, err
			}
		}
		wt, err := p.Manager.Create(ctx, worktree.CreateOptions{
			RepoURL:               repoURL,
			RunID:                 req.RunID + "-" + req.Stage,
			OwnerRunID:            req.RunID,
			BaseRef:               baseRef,
			Branch:                branch,
			SyncBase:              req.SyncBase,
			RequireExistingBranch: req.WorkspaceBranch != "",
			AcquireRemoteBranch:   req.WorkspaceBranch != "",
			Sparse:                sparseCones(req.RepoRef.Checkout),
		})
		if err != nil {
			return nil, fmt.Errorf("workerhost: create worktree for stage %q: %w", req.Stage, err)
		}
		// The publish baseline is HEAD as HANDED to the stage — after the
		// existing-branch checkout and after any syncBase merge — not the
		// worktree's own starting ref (which Create captures before the
		// merge). PublishDelta must answer "did this stage commit", and a
		// syncBase that fast-forwarded the branch onto an advanced base moves
		// HEAD without the stage having done anything: measured against the
		// pre-merge ref that read as "moved", and on a branch the sync
		// landed exactly at base the bundle range was empty and git refused
		// it — a hard stage failure for a stage that did nothing wrong.
		publishBase, err := wt.HeadSHA(ctx)
		if err != nil {
			_ = wt.Remove(ctx, worktree.RemoveOptions{})
			return nil, fmt.Errorf("workerhost: resolve provisioned HEAD for stage %q: %w", req.Stage, err)
		}
		return &worktreeWorkspace{
			wt: wt, manager: p.Manager, store: p.Store, log: p.log(),
			repoURL: repoURL, branch: branch, base: baseRef, publishBase: publishBase,
		}, nil
	default:
		return nil, fmt.Errorf("workerhost: unknown workspace mode %q for stage %q", req.Mode, req.Stage)
	}
}

// repoTarget resolves the clone URL and base ref a repo-backed request
// (writable or read-only) is provisioned from.
func (p *WorktreeWorkspaces) repoTarget(req engine.WorkspaceRequest) (repoURL, baseRef string, err error) {
	if p.Manager == nil {
		return "", "", fmt.Errorf("workerhost: repo workspace for stage %q: no worktree manager configured", req.Stage)
	}
	cloneURL := p.CloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	repoURL, err = cloneURL(req.RepoRef)
	if err != nil {
		return "", "", err
	}
	baseRef = req.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	return repoURL, baseRef, nil
}

// applyDelta is the worker's APPLY half (#3803 option 2): fetch the bundle
// the engine named from the shared store, verify it, and land it on the run
// branch in the mirror under the per-repo lock. Every failure is fatal to the
// provision — continuing without the delta would hand the stage base and let
// it report success on a workspace that silently dropped its predecessor's
// commits.
func (p *WorktreeWorkspaces) applyDelta(ctx context.Context, req engine.WorkspaceRequest, repoURL, branch, baseRef string) error {
	if p.Store == nil {
		return fmt.Errorf("workerhost: stage %q was handed workspace delta %s but this worker has no blob store (--blob-store / GOOBERS_BLOB_STORE); refusing to provision a workspace that would silently omit earlier stages' commits", req.Stage, req.WorkspaceDelta)
	}
	data, err := p.Store.Get(ctx, req.WorkspaceDelta)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return fmt.Errorf("workerhost: workspace delta %s for stage %q is not in the blob store %s; the producing stage's bundle never reached the store this worker reads", req.WorkspaceDelta, req.Stage, p.Store.Describe())
		}
		return fmt.Errorf("workerhost: fetch workspace delta %s for stage %q: %w", req.WorkspaceDelta, req.Stage, err)
	}
	bundle, err := workspacedelta.Load(data, req.WorkspaceDelta)
	if err != nil {
		return fmt.Errorf("workerhost: stage %q: %w", req.Stage, err)
	}
	outcome, err := p.Manager.ApplyBundle(ctx, worktree.ApplyBundleOptions{
		RepoURL:             repoURL,
		Branch:              branch,
		BaseRef:             baseRef,
		OwnerRunID:          req.RunID,
		AcquireRemoteBranch: req.WorkspaceBranch != "",
	}, bundle, p.log())
	if err != nil {
		return fmt.Errorf("workerhost: apply workspace delta %s for stage %q: %w", req.WorkspaceDelta, req.Stage, err)
	}
	_, _ = fmt.Fprintf(p.log(), "workspace delta: applied %s to %s for stage %s (%s: %s -> %s)\n",
		req.WorkspaceDelta, branch, req.Stage, outcome.Outcome, shortSHA(outcome.Before), shortSHA(outcome.After))
	return nil
}

func shortSHA(sha string) string {
	if sha == "" {
		return "(absent)"
	}
	return sha
}

// sparseCones returns spec's declared cones, or nil for a full checkout
// (mirrors internal/runner's own copy — a 4-line helper duplicated rather
// than shared, the same tradeoff internal/worktree's doc.go already accepts
// for validRunID).
func sparseCones(spec *apiv1.CheckoutSpec) []string {
	if spec == nil {
		return nil
	}
	return spec.Sparse
}

type scratchWorkspace string

func (w scratchWorkspace) Path() string { return string(w) }

func (w scratchWorkspace) Remove(context.Context) error { return os.RemoveAll(string(w)) }

type worktreeWorkspace struct {
	wt      *worktree.Worktree
	manager *worktree.Manager
	store   blobstore.Store
	log     io.Writer
	repoURL string
	branch  string
	base    string
	// publishBase is the commit HEAD was at when the workspace was handed to
	// the stage (post-checkout, post-syncBase); PublishDelta measures the
	// stage's OWN commits against it. Empty for a read-only workspace, which
	// never publishes.
	publishBase string
}

func (w *worktreeWorkspace) Path() string { return w.wt.Path }

func (w *worktreeWorkspace) Remove(ctx context.Context) error {
	return w.wt.Remove(ctx, worktree.RemoveOptions{})
}

// PublishDelta implements engine.DeltaPublisher: bundle base..<run branch>
// from the mirror and put it in the store, so a pod (or another worker) can
// continue from what this stage committed (#3803, reverse direction).
//
// Publishes only when the stage ITSELF committed — HEAD carries commits not
// reachable from the HEAD it was handed (publishBase) — and only when the
// branch then carries something beyond base to bundle. Ref movement alone is
// not a publication: a syncBase merge advances the branch without the stage
// having produced anything, and treating that as "this stage published"
// both failed the stage outright when the merge landed exactly at base (git
// refuses an empty bundle) and made a non-producer the record's newest
// entry, which the engine's WF022 runtime arm would then refuse a declared
// 3.0 consumer over (the implementation lane's local-ci shape). An unchanged
// branch reports Unchanged, which the engine journals explicitly rather than
// leaving "no digest" ambiguous with "not a repo stage". With no Store
// configured (mode 1/2, no pods) nothing is published and nothing is
// reported — the shared mirror is continuity there, exactly as before.
func (w *worktreeWorkspace) PublishDelta(ctx context.Context) (engine.WorkspaceDeltaPublication, error) {
	if w.store == nil || w.wt.Branch == "" || w.publishBase == "" {
		return engine.WorkspaceDeltaPublication{}, nil
	}
	committed, err := w.wt.HasCommitsAheadOf(ctx, w.publishBase)
	if err != nil {
		return engine.WorkspaceDeltaPublication{}, fmt.Errorf("workerhost: publish workspace delta: %w", err)
	}
	if !committed {
		return engine.WorkspaceDeltaPublication{Unchanged: true}, nil
	}
	// The stage committed relative to what it was handed, but the bundle is
	// cut base..branch; a stage that reset its branch back onto base has
	// nothing in that range, and git refuses to create an empty bundle.
	carries, err := w.wt.HasCommitsAheadOf(ctx, w.base)
	if err != nil {
		return engine.WorkspaceDeltaPublication{}, fmt.Errorf("workerhost: publish workspace delta: %w", err)
	}
	if !carries {
		_, _ = fmt.Fprintf(w.log, "workspace delta: %s committed but carries nothing beyond %s; nothing to publish\n", w.branch, w.base)
		return engine.WorkspaceDeltaPublication{Unchanged: true}, nil
	}
	bundle, err := w.manager.BundleRunBranch(ctx, w.repoURL, w.branch, w.base)
	if err != nil {
		return engine.WorkspaceDeltaPublication{}, fmt.Errorf("workerhost: publish workspace delta: %w", err)
	}
	if err := w.store.Put(ctx, bundle.Digest, bundle.Data); err != nil {
		return engine.WorkspaceDeltaPublication{}, fmt.Errorf("workerhost: publish workspace delta %s (%d bytes) to %s: %w", bundle.Digest, len(bundle.Data), w.store.Describe(), err)
	}
	_, _ = fmt.Fprintf(w.log, "workspace delta: published %s (%d bytes) carrying %s..%s on %s\n", bundle.Digest, len(bundle.Data), bundle.Base, bundle.Tip, w.branch)
	return engine.WorkspaceDeltaPublication{Digest: bundle.Digest, Base: bundle.Base, Tip: bundle.Tip}, nil
}
