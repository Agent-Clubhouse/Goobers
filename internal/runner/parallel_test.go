package runner

import (
	"strings"
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

// FO-5 executes continue_on_error only. The cancelling policies must fail
// CLOSED rather than silently behaving like continue_on_error, which would let
// a workflow believe it had fail-fast semantics it does not have.
func TestUnsupportedFailurePoliciesFailClosed(t *testing.T) {
	if err := supportedFailurePolicy(apiv1.BranchContinueOnError); err != nil {
		t.Errorf("continue_on_error must execute: %v", err)
	}
	for _, policy := range []apiv1.BranchFailurePolicy{apiv1.BranchFailFast, apiv1.BranchAllOrNothing} {
		err := supportedFailurePolicy(policy)
		if err == nil {
			t.Errorf("policy %q must fail closed until FO-6 implements cancellation", policy)
			continue
		}
		if !strings.Contains(err.Error(), "not yet implemented") {
			t.Errorf("policy %q error = %q, want it to say the policy is unimplemented", policy, err)
		}
	}
	if err := supportedFailurePolicy("nonsense"); err == nil {
		t.Error("an unknown policy must fail closed")
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
