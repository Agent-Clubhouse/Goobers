package engine

import (
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// These shared fixtures compare the branch body and selected gate decisions.
// Root scheduling/terminal settlement belongs to the next controller slice;
// staticBranchBodies intentionally does not implement that policy.
func TestConformanceStaticBranchGateRouting(t *testing.T) {
	ok := succeed(map[string]interface{}{"value": "alpha"})
	failed := fail("scripted_failure", "subject failed")
	for _, tc := range []struct {
		name, pass, fail string
		script           []scriptedCall
		status           journal.BranchStatus
		terminal         string
		wantErr          bool
	}{
		{"pass-complete", wf.TerminalComplete, wf.TargetJoin, []scriptedCall{ok}, journal.BranchFailed, "", true},
		{"fail-complete", wf.TargetJoin, wf.TerminalComplete, []scriptedCall{failed}, journal.BranchFailed, "", true},
		{"pass-join", wf.TargetJoin, wf.TargetJoin, []scriptedCall{ok}, journal.BranchSucceeded, "", false},
		{"fail-join", wf.TargetJoin, wf.TargetJoin, []scriptedCall{failed}, journal.BranchFailed, "", false},
		{"pass-abort", wf.TargetAbort, wf.TargetJoin, []scriptedCall{ok}, journal.BranchFailed, wf.TargetAbort, false},
		{"fail-abort", wf.TargetJoin, wf.TargetAbort, []scriptedCall{failed}, journal.BranchFailed, wf.TargetAbort, false},
		{"pass-escalate", wf.TargetEscalate, wf.TargetJoin, []scriptedCall{ok}, journal.BranchFailed, wf.TargetEscalate, false},
		{"fail-escalate", wf.TargetJoin, wf.TargetEscalate, []scriptedCall{failed}, journal.BranchFailed, wf.TargetEscalate, false},
		{"repass-then-join", wf.TargetJoin, "alpha", []scriptedCall{failed, ok}, journal.BranchSucceeded, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := branchBodyInput("gate-conformance-"+tc.name, true, false)
			in.Run.Spec.Gates[0].Branches["pass"] = tc.pass
			in.Run.Spec.Gates[0].Branches["fail"] = tc.fail
			script := map[string][]scriptedCall{
				"seed":  {succeed(map[string]interface{}{"value": "frozen-root"})},
				"alpha": tc.script,
				"beta":  {succeed(map[string]interface{}{"value": "beta"})},
			}
			local := runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, script: script, wantLocalErr: tc.wantErr}, "local-"+tc.name)
			writer, dir := newLiveWriter(t)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Auto: gate.NewAutomatedEvaluator(), Det: newScriptedExec(script)}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(acts)
			env.ExecuteWorkflow(staticBranchBodies, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var out branchWalkTestResult
			if err := env.GetWorkflowResult(&out); err != nil {
				t.Fatal(err)
			}
			got := out.Branches[0]
			if got.Status != tc.status || got.TerminalTarget != tc.terminal || (got.Error != "") != tc.wantErr {
				t.Fatalf("engine branch = %+v; want status=%s terminal=%q error=%t", got, tc.status, tc.terminal, tc.wantErr)
			}
			if tc.wantErr && (!strings.Contains(got.Error, "completed the run instead of routing") || got.Result.Status == StatusCompleted) {
				t.Fatalf("whole-run completion did not fail closed: %+v", got)
			}
			assertLocalBranchStatus(t, local, tc.status)
			engine := liveEvents(t, dir, in.Run.RunID)
			localGates, localVisits := branchGateConformance(local)
			engineGates, engineVisits := branchGateConformance(engine)
			if len(localGates) != len(tc.script) || !slices.Equal(localGates, engineGates) {
				t.Fatalf("gate conformance mismatch:\nlocal: %+v\nengine: %+v", localGates, engineGates)
			}
			if localVisits != len(tc.script) || engineVisits != localVisits {
				t.Fatalf("subject visits local=%d engine=%d want=%d", localVisits, engineVisits, len(tc.script))
			}
		})
	}
}

func assertLocalBranchStatus(t *testing.T, events []journal.Event, want journal.BranchStatus) {
	t.Helper()
	for _, event := range events {
		if event.Type == journal.EventBranchFinished && event.Branch == 1 {
			if event.BranchStatus != want {
				t.Fatalf("local branch = %s; want %s", event.BranchStatus, want)
			}
			return
		}
	}
	t.Fatal("local branch did not settle")
}

func branchGateConformance(events []journal.Event) ([]journal.NormativeEvent, int) {
	var gates []journal.Event
	visits := 0
	for _, event := range events {
		if event.Branch != 1 {
			continue
		}
		if event.Type == journal.EventGateEvaluated {
			gates = append(gates, event)
		}
		if event.Type == journal.EventStageStarted && event.Stage == "alpha" {
			visits++
		}
	}
	return journal.ConformanceView(gates), visits
}
