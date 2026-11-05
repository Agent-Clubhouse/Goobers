package runsemantics

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func compileImplReviewMachine(t *testing.T, spec apiv1.WorkflowSpec) *workflow.Machine {
	t.Helper()
	spec.Gaggle = "acme-web"
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}
	m, err := workflow.Compile(workflow.Definition{Name: "impl-review-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return m
}

func reviewGate(name, onPass, onChanges string, ws apiv1.WorkspaceMode) apiv1.Gate {
	return apiv1.Gate{
		Name:      name,
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Workspace: ws},
		Branches: map[string]string{
			"pass":          onPass,
			"needs-changes": onChanges,
			"fail":          workflow.TargetAbort,
		},
	}
}

// implementCheckReviewMachine is the #5414 shape: an agentic implementer, a
// deterministic stage between it and the review, and a reviewer that
// declared a workspace other than the writable run branch.
func implementCheckReviewMachine(t *testing.T, reviewerWorkspace apiv1.WorkspaceMode) *workflow.Machine {
	t.Helper()
	return compileImplReviewMachine(t, apiv1.WorkflowSpec{
		Start: "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff", Next: "check"},
			{Name: "check", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{reviewGate("review", workflow.TerminalComplete, "implement", reviewerWorkspace)},
	})
}

func TestReviewsImplementationClassifiesByDominance(t *testing.T) {
	t.Run("review behind an agentic implementer and a deterministic stage", func(t *testing.T) {
		m := implementCheckReviewMachine(t, apiv1.WorkspaceRepoReadOnly)
		if !ReviewsImplementation(m, "review") {
			t.Fatal("review gate after implement->check must classify as implementation review")
		}
		if !RequiresReviewerDiff(m, "review", "check") {
			t.Fatal("a deterministic subject must not disable the diff requirement of an implementation review")
		}
	})

	t.Run("spec check reachable before the implementer", func(t *testing.T) {
		m := compileImplReviewMachine(t, apiv1.WorkflowSpec{
			Start: "plan",
			Tasks: []apiv1.Task{
				{Name: "plan", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "spec-review"},
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff", Next: "review"},
			},
			Gates: []apiv1.Gate{
				reviewGate("spec-review", "implement", "plan", apiv1.WorkspaceRepoReadOnly),
				reviewGate("review", workflow.TerminalComplete, "implement", ""),
			},
		})
		if ReviewsImplementation(m, "spec-review") {
			t.Fatal("a gate reachable before any implementer must not classify as implementation review")
		}
		if !ReviewsImplementation(m, "review") {
			t.Fatal("the post-implement review must classify as implementation review")
		}
	})

	t.Run("deterministic-only subject (merge-review shape)", func(t *testing.T) {
		m := agenticGateMachine(t)
		if ReviewsImplementation(m, "review") || RequiresReviewerDiff(m, "review", "implement") {
			t.Fatal("a review fed only by deterministic stages must keep its historical behaviour")
		}
	})

	t.Run("read-only agentic task does not implement", func(t *testing.T) {
		m := compileImplReviewMachine(t, apiv1.WorkflowSpec{
			Start: "research",
			Tasks: []apiv1.Task{
				{Name: "research", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "read", Workspace: apiv1.WorkspaceRepoReadOnly, Next: "review"},
			},
			Gates: []apiv1.Gate{reviewGate("review", workflow.TerminalComplete, "research", apiv1.WorkspaceRepoReadOnly)},
		})
		if ReviewsImplementation(m, "review") {
			t.Fatal("a repo-readonly agentic task commits nothing and must not make its review an implementation review")
		}
	})

}

// TestGraphReachesJoinOnlyThroughEveryBranch is #5414's static fan-in rule: a
// join runs only after EVERY branch settles, so a deterministic sibling
// branch must not make the review after the join look reachable around an
// implementer in another branch. (Compile rule 9 forbids writable-repo branch
// stages today, so the graph is built by hand.)
func TestGraphReachesJoinOnlyThroughEveryBranch(t *testing.T) {
	graph := workflow.Graph{
		Start: "fanout",
		Nodes: []workflow.GraphNode{
			{ID: "fanout", Kind: workflow.GraphNodeParallel},
			{ID: "left", Kind: workflow.GraphNodeAgentic},
			{ID: "right", Kind: workflow.GraphNodeDeterministic},
			{ID: "join", Kind: workflow.GraphNodeDeterministic},
			{ID: "review", Kind: workflow.GraphNodeGate},
		},
		Edges: []workflow.GraphEdge{
			{Source: "fanout", Target: "left", Branch: "left"},
			{Source: "fanout", Target: "right", Branch: "right"},
			{Source: "left", Target: "join"},
			{Source: "right", Target: "join"},
			{Source: "join", Target: "review"},
		},
	}
	parallel := func(name string) (apiv1.Parallel, bool) {
		if name != "fanout" {
			return apiv1.Parallel{}, false
		}
		return apiv1.Parallel{Name: "fanout", Join: "join", Branches: []apiv1.Branch{{Name: "left", Start: "left"}, {Name: "right", Start: "right"}}}, true
	}
	if !GraphReaches(parallel, graph, "review", nil) {
		t.Fatal("review after the join must be reachable when nothing is blocked")
	}
	if GraphReaches(parallel, graph, "review", map[string]bool{"left": true}) {
		t.Fatal("the join waits for the implementer branch, so the review must not be reachable around it")
	}
	if !GraphReaches(parallel, graph, "right", map[string]bool{"left": true}) {
		t.Fatal("a sibling branch's own stages stay reachable while another branch is blocked")
	}
}
