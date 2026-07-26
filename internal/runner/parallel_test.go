package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestNewParallelExecAssignsIdsByDeclarationOrder(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name: "fan",
		Branches: []apiv1.Branch{
			{Name: "security", Start: "a"},
			{Name: "perf", Start: "b"},
			{Name: "coverage", Start: "c"},
		},
	})
	for i, want := range []struct {
		id   int
		name string
	}{{1, "security"}, {2, "perf"}, {3, "coverage"}} {
		got := p.branches[i]
		if got.id != want.id || got.name != want.name {
			t.Errorf("branch %d = {id:%d name:%q}, want {id:%d name:%q}; declaration order assigns ids and 0 is the root",
				i, got.id, got.name, want.id, want.name)
		}
	}
}

func TestParallelExecAdvancesThroughEveryBranch(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name:     "fan",
		Branches: []apiv1.Branch{{Name: "a", Start: "a1"}, {Name: "b", Start: "b1"}},
	})

	if cur := p.current(); cur.name != "a" {
		t.Fatalf("first branch = %q, want a", cur.name)
	}
	next, more := p.advance(journal.BranchSucceeded)
	if !more || next.name != "b" {
		t.Fatalf("advance = (%v, %v), want branch b", next, more)
	}
	if _, more := p.advance(journal.BranchFailed); more {
		t.Fatal("advance past the last branch should report no more branches")
	}

	record := p.completeness()
	if len(record) != 2 {
		t.Fatalf("completeness = %+v, want one entry per declared branch", record)
	}
	if record[0].Status != journal.BranchSucceeded || record[1].Status != journal.BranchFailed {
		t.Errorf("completeness statuses = %v/%v, want succeeded/failed", record[0].Status, record[1].Status)
	}
}

// Every DECLARED branch gets a record entry, even one that never ran — a
// missing branch must be visible rather than silently absent.
func TestCompletenessCoversBranchesThatNeverRan(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name:     "fan",
		Branches: []apiv1.Branch{{Name: "a", Start: "a1"}, {Name: "b", Start: "b1"}},
	})
	p.advance(journal.BranchSucceeded) // settles a, moves to b; b never settles

	record := p.completeness()
	if len(record) != 2 {
		t.Fatalf("completeness = %+v, want 2 entries", record)
	}
	if record[1].Status != journal.BranchCancelled {
		t.Errorf("unrun branch status = %q, want cancelled", record[1].Status)
	}
}

func TestBranchStatusFor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    apiv1.ResultEnvelope
		artifacts int
		want      journal.BranchStatus
	}{
		{
			name:      "produced outputs",
			result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"findings": 3}},
			artifacts: 0,
			want:      journal.BranchSucceeded,
		},
		{
			name:      "produced artifacts only",
			result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
			artifacts: 2,
			want:      journal.BranchSucceeded,
		},
		{
			// The distinction the four original statuses could not express.
			name:      "settled empty",
			result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
			artifacts: 0,
			want:      journal.BranchNoOutput,
		},
		{
			name:      "branch-scoped no-work is a successful empty settle",
			result:    apiv1.ResultEnvelope{Status: apiv1.ResultNoWork},
			artifacts: 0,
			want:      journal.BranchNoOutput,
		},
		{
			name:      "failure",
			result:    apiv1.ResultEnvelope{Status: apiv1.ResultFailure},
			artifacts: 0,
			want:      journal.BranchFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := branchStatusFor(tc.result, tc.artifacts); got != tc.want {
				t.Errorf("branchStatusFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEveryDeclaredFailurePolicyExecutes(t *testing.T) {
	for _, policy := range []apiv1.BranchFailurePolicy{
		apiv1.BranchContinueOnError, apiv1.BranchFailFast, apiv1.BranchAllOrNothing,
	} {
		if err := supportedFailurePolicy(policy); err != nil {
			t.Errorf("policy %q must execute: %v", policy, err)
		}
	}
	if err := supportedFailurePolicy("nonsense"); err == nil {
		t.Error("an unknown policy must fail closed rather than default to permissive")
	}
}

// When nothing fails, all three policies behave identically: the join runs.
// They differ ONLY on failure.
func TestRouteRunsJoinWhenNoBranchFailed(t *testing.T) {
	for _, policy := range []apiv1.BranchFailurePolicy{
		apiv1.BranchContinueOnError, apiv1.BranchFailFast, apiv1.BranchAllOrNothing,
	} {
		p := newParallelExec(apiv1.Parallel{
			Name: "fan", FailurePolicy: policy, Join: "collate", OnFailure: "@escalate",
			Branches: []apiv1.Branch{{Name: "a", Start: "a1"}, {Name: "b", Start: "b1"}},
		})
		p.advance(journal.BranchSucceeded)
		p.advance(journal.BranchNoOutput)
		target, runJoin := p.route()
		if !runJoin || target != "collate" {
			t.Errorf("policy %q with no failure routed to (%q, join=%v), want collate/true", policy, target, runJoin)
		}
	}
}

func TestRouteOnFailureByPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy     apiv1.BranchFailurePolicy
		wantTarget string
		wantJoin   bool
	}{
		// The join owns the decision via the completeness record.
		{apiv1.BranchContinueOnError, "collate", true},
		{apiv1.BranchFailFast, "park", false},
		{apiv1.BranchAllOrNothing, "park", false},
	} {
		p := newParallelExec(apiv1.Parallel{
			Name: "fan", FailurePolicy: tc.policy, Join: "collate", OnFailure: "park",
			Branches: []apiv1.Branch{{Name: "a", Start: "a1"}, {Name: "b", Start: "b1"}},
		})
		p.advance(journal.BranchFailed)
		p.advance(journal.BranchSucceeded)
		target, runJoin := p.route()
		if target != tc.wantTarget || runJoin != tc.wantJoin {
			t.Errorf("policy %q routed to (%q, join=%v), want (%q, %v)", tc.policy, target, runJoin, tc.wantTarget, tc.wantJoin)
		}
	}
}

// no-output is a SUCCESSFUL settle — a research lens that found nothing must
// not trip a failure policy.
func TestNoOutputIsNotAFailure(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name: "fan", FailurePolicy: apiv1.BranchFailFast, Join: "collate", OnFailure: "park",
		Branches: []apiv1.Branch{{Name: "a", Start: "a1"}},
	})
	p.advance(journal.BranchNoOutput)
	if p.anyFailed() {
		t.Error("a no-output branch must not count as failed")
	}
	if target, runJoin := p.route(); !runJoin || target != "collate" {
		t.Errorf("routed to (%q, join=%v), want the join to run", target, runJoin)
	}
}

func TestCancelRemainingSettlesUnstartedBranches(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name: "fan", FailurePolicy: apiv1.BranchFailFast, Join: "collate", OnFailure: "park",
		Branches: []apiv1.Branch{{Name: "a", Start: "a1"}, {Name: "b", Start: "b1"}, {Name: "c", Start: "c1"}},
	})
	p.advance(journal.BranchFailed) // a fails, b becomes active
	cancelled := p.cancelRemaining()
	if len(cancelled) != 2 {
		t.Fatalf("cancelled %d branches, want b and c", len(cancelled))
	}
	record := p.completeness()
	if record[0].Status != journal.BranchFailed {
		t.Errorf("branch a = %q, want failed", record[0].Status)
	}
	for _, i := range []int{1, 2} {
		if record[i].Status != journal.BranchCancelled {
			t.Errorf("branch %d = %q, want cancelled", i, record[i].Status)
		}
	}
}

func TestParallelCursorsProjectLivePositions(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name:     "fan",
		Branches: []apiv1.Branch{{Name: "a", Start: "a1"}, {Name: "b", Start: "b1"}},
	})
	cursors := p.cursors()
	if len(cursors) != 2 {
		t.Fatalf("cursors = %+v, want 2", cursors)
	}
	if cursors[0].MachineState != "a1" || cursors[0].Parallel != "fan" || cursors[0].Branch != 1 {
		t.Errorf("cursor 0 = %+v, want branch 1 of fan at a1", cursors[0])
	}

	p.advance(journal.BranchSucceeded)
	cursors = p.cursors()
	if cursors[0].MachineState != "" || cursors[0].Status != journal.BranchSucceeded {
		t.Errorf("settled cursor = %+v, want no resume position and a terminal status", cursors[0])
	}
}
