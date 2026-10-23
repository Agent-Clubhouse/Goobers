package runsemantics

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
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
	return GraphReaches(m.Parallel, graph, gateName, nil) && !GraphReaches(m.Parallel, graph, gateName, implementers)
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

// GraphReaches reports whether target is reachable from the graph's start
// without entering any node in blocked.
//
// A parallel's join is not entered by any single branch: the runtime waits for
// every branch to settle, so the join is reachable only when EVERY branch can
// reach it without entering blocked. Otherwise a deterministic sibling branch
// would make a join look reachable around an implementer in another branch
// that the join in fact always waits for.
func GraphReaches(parallel func(string) (apiv1.Parallel, bool), graph workflow.Graph, target string, blocked map[string]bool) bool {
	r := graphReachability{parallel: parallel, next: make(map[string][]string, len(graph.Nodes)), joins: map[string]bool{}, active: map[string]bool{}}
	for _, e := range graph.Edges {
		r.next[e.Source] = append(r.next[e.Source], e.Target)
	}
	for _, node := range graph.Nodes {
		if node.Kind != workflow.GraphNodeParallel {
			continue
		}
		if p, ok := parallel(node.ID); ok && p.Join != "" {
			r.joins[p.Join] = true
		}
	}
	return r.visit(graph.Start, blocked)[target]
}

type graphReachability struct {
	parallel func(string) (apiv1.Parallel, bool)
	next     map[string][]string
	joins    map[string]bool
	active   map[string]bool // parallels on the current recursion stack
}

// visit returns every node reachable from start without entering blocked.
func (r graphReachability) visit(start string, blocked map[string]bool) map[string]bool {
	seen := map[string]bool{}
	if start == "" || blocked[start] {
		return seen
	}
	seen[start] = true
	queue := []string{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		targets := r.next[node]
		if join, ok := r.joinAfterAllBranches(node, blocked); ok {
			targets = append(append([]string(nil), targets...), join)
		}
		for _, to := range targets {
			if to == "" || seen[to] || blocked[to] {
				continue
			}
			if r.joins[to] && !r.isJoinOf(node, to) {
				// Branch terminals' converging edges: only the parallel itself
				// admits its join, once every branch reaches it.
				continue
			}
			seen[to] = true
			queue = append(queue, to)
		}
	}
	return seen
}

// joinAfterAllBranches returns node's join when node is a parallel every one
// of whose branches reaches that join without entering blocked.
func (r graphReachability) joinAfterAllBranches(node string, blocked map[string]bool) (string, bool) {
	p, ok := r.parallel(node)
	if !ok || p.Join == "" || r.active[node] {
		return "", false
	}
	r.active[node] = true
	defer delete(r.active, node)
	for _, branch := range p.Branches {
		if !r.branchReachesJoin(branch.Start, p.Join, blocked) {
			return "", false
		}
	}
	return p.Join, true
}

func (r graphReachability) branchReachesJoin(start, join string, blocked map[string]bool) bool {
	for node := range r.visit(start, blocked) {
		for _, to := range r.next[node] {
			if to == join {
				return true
			}
		}
	}
	return false
}

func (r graphReachability) isJoinOf(node, join string) bool {
	p, ok := r.parallel(node)
	return ok && p.Join == join
}
