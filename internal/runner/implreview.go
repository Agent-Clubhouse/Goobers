package runner

import (
	"context"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// ReviewsImplementation reports whether gateName is an implementation-review
// gate (#5414): an agentic gate that no path from the workflow's start can
// reach without first passing an agentic task that commits to the run branch
// (a writable repo workspace, the default for an agentic task). Every
// evaluation of such a gate follows committed implementation work, so its
// reviewer is owed the run's committed diff whatever workspace it declared
// and whatever deterministic stages sit between it and the implementer, and
// an empty diff there is a fail-closed condition rather than something a
// reviewer should be asked to judge on the base branch.
//
// Dominance rather than plain reachability is the point: a gate that the
// workflow can also reach BEFORE any implementer runs (a spec check that a
// later repass loop happens to revisit) reviews something other than the
// implementation and keeps its historical behaviour.
//
// Shared by the local runner and the Temporal engine so the two substrates
// classify every gate identically.
func ReviewsImplementation(m *workflow.Machine, gateName string) bool {
	if m == nil {
		return false
	}
	g, ok := m.Gate(gateName)
	if !ok || g.Evaluator != apiv1.EvaluatorAgentic {
		return false
	}
	graph := m.Graph()
	implementers := make(map[string]bool)
	for _, node := range graph.Nodes {
		if node.Kind != workflow.GraphNodeAgentic {
			continue
		}
		if t, ok := m.Task(node.ID); ok && commitsToRunBranch(t) {
			implementers[node.ID] = true
		}
	}
	if len(implementers) == 0 {
		return false
	}
	return graphReaches(graph, gateName, nil) && !graphReaches(graph, gateName, implementers)
}

// RequiresReviewerDiff reports whether an agentic gate evaluating subjectStage
// must see a non-empty run diff: its subject is an agentic task (#415), or
// the gate reviews implementation work (#5414). An empty diff for such a gate
// is fast-failed instead of being handed to the reviewer.
func RequiresReviewerDiff(m *workflow.Machine, gateName, subjectStage string) bool {
	if m == nil {
		return false
	}
	if t, ok := m.Task(subjectStage); ok && t.Type == apiv1.TaskAgentic {
		return true
	}
	return ReviewsImplementation(m, gateName)
}

func commitsToRunBranch(t apiv1.Task) bool {
	if t.Type != apiv1.TaskAgentic {
		return false
	}
	mode := t.EffectiveWorkspace()
	return mode == "" || mode.IsWritableRepo()
}

// graphReaches reports whether target is reachable from the graph's start
// without entering any node in blocked.
func graphReaches(graph workflow.Graph, target string, blocked map[string]bool) bool {
	if graph.Start == "" || blocked[graph.Start] {
		return false
	}
	next := make(map[string][]string, len(graph.Nodes))
	for _, e := range graph.Edges {
		next[e.Source] = append(next[e.Source], e.Target)
	}
	seen := map[string]bool{graph.Start: true}
	queue := []string{graph.Start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if node == target {
			return true
		}
		for _, to := range next[node] {
			if to == "" || seen[to] || blocked[to] {
				continue
			}
			seen[to] = true
			queue = append(queue, to)
		}
	}
	return false
}

// reviewerDiff reads the patch an agentic gate's reviewer is handed as
// evidence (#301). A reviewer on the run branch reads it from its own
// worktree, exactly as before. An implementation-review gate whose reviewer
// was NOT given the run branch — a detached repo-readonly checkout at base,
// or a scratch directory with no checkout at all — reads the same
// base...<run branch> patch from the managed mirror its worktrees share
// (#5414); otherwise the reviewer silently judged the base branch. Returns
// nil when there is nothing to read.
func (r *Runner) reviewerDiff(ctx context.Context, in StartInput, gateName, workspaceBranch string, wt *worktree.Worktree) ([]byte, error) {
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	if wt != nil && (wt.Branch != "" || in.pinnedWorkspace != nil) {
		return wt.Diff(ctx, baseRef)
	}
	if wt == nil && ReviewsImplementation(in.Machine, gateName) {
		// A scratch reviewer in a run whose branch lives in its own worktree
		// (a pinned project workspace, or a child run's admitted fork) reads
		// it there, under the same lock its stages hold.
		if own, lock := onBranchRunWorktree(in); own != nil {
			lock.Lock()
			defer lock.Unlock()
			return own.Diff(ctx, baseRef)
		}
	}
	if !r.readsRunBranchFromMirror(in, gateName) {
		if wt == nil {
			return nil, nil
		}
		return wt.Diff(ctx, baseRef)
	}
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	branch := workspaceBranch
	if branch == "" {
		branch = providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
	}
	return r.cfg.Worktrees.RunBranchDiff(ctx, repoURL, baseRef, branch)
}

// onBranchRunWorktree returns the run's own on-branch worktree when the run
// branch lives outside the shared mirror's per-stage worktrees, with the lock
// that serializes the run's stages on it.
func onBranchRunWorktree(in StartInput) (*worktree.Worktree, sync.Locker) {
	if in.pinnedWorkspace != nil && in.pinnedStage != nil {
		return in.pinnedWorkspace, in.pinnedStage
	}
	if in.childWorkspace != nil && in.childWorkspace.worktree != nil {
		return in.childWorkspace.worktree, &in.childWorkspace.stage
	}
	return nil, nil
}

// readsRunBranchFromMirror reports whether reviewerDiff may read the run
// branch from the shared mirror: only for an implementation-review gate, and
// only where the run branch lives in this runner's own managed mirror (not a
// pinned project workspace or a child run's separate fork, which reviewerDiff
// reads in place).
func (r *Runner) readsRunBranchFromMirror(in StartInput, gateName string) bool {
	if r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return false
	}
	if in.pinnedWorkspace != nil || in.ChildWorkspace != nil || in.heldChildWorkspace != nil {
		return false
	}
	return ReviewsImplementation(in.Machine, gateName)
}
