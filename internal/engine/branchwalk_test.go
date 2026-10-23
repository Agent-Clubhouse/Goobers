package engine

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

const branchWalkTestVersion = "engine-static-branch-body-v1"

type branchWalkTestInput struct {
	Run    RunInput
	Cancel bool
}

type branchWalkTestOutcome struct {
	Status         journal.BranchStatus
	TerminalTarget string
	Result         RunResult
	Error          string
	Pointers       []apiv1.ContextPointer
}

type branchWalkTestResult struct {
	Branches   []branchWalkTestOutcome
	Projection JournalProjection
}

// This is deliberately not a production scheduler. It starts both branch bodies
// once, then joins them, exercising the real task/gate walk and commit owner.
func staticBranchBodies(ctx workflow.Context, input branchWalkTestInput) (branchWalkTestResult, error) {
	in := input.Run
	if workflow.GetVersion(ctx, branchWalkTestVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return branchWalkTestResult{}, fmt.Errorf("engine: static parallel branch execution is refused")
	}
	if !in.previewFeaturesEnabled() || !in.LiveJournal {
		return branchWalkTestResult{}, fmt.Errorf("branch fixture requires preview and live journal")
	}
	if err := wf.RefuseChildWorkflowExecution(in.Spec); err != nil {
		return branchWalkTestResult{}, err
	}
	m, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec}, wf.WithPreviewFeatures(true))
	if err != nil {
		return branchWalkTestResult{}, err
	}
	if len(in.Spec.Parallels) != 1 || in.Spec.Parallels[0].Selection != nil || len(in.Spec.Maps) != 0 {
		return branchWalkTestResult{}, fmt.Errorf("fixture requires one static parallel")
	}
	root, err := newRunJournal(ctx, in, m)
	if err != nil {
		return branchWalkTestResult{}, err
	}
	root.owner = &journalCommitOwner{root: root, mu: workflow.NewMutex(ctx)}
	root.runStarted(ctx)
	if err := root.emitPending(ctx); err != nil {
		return branchWalkTestResult{}, err
	}
	p := in.Spec.Parallels[0]
	out := branchWalkTestResult{Branches: make([]branchWalkTestOutcome, len(p.Branches))}
	seed := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: apiv1.IntegrityTrusted, Outputs: map[string]any{"value": "frozen-root"}}
	seedPointers := []apiv1.ContextPointer{{Name: "seed.artifact[0]", Integrity: apiv1.IntegrityTrusted, Artifact: &apiv1.ArtifactPointer{Path: "artifacts/seed", Digest: "sha256:" + strings.Repeat("a", 64), Integrity: apiv1.IntegrityTrusted}}}
	seedUpstream := map[string]apiv1.ResultEnvelope{"seed": seed}
	seedCompleted := newCompletedStages()
	seedCompleted.RecordCompleted(apiv1.Task{Name: "seed"}, seed)
	seedGates := map[string]bool{}
	branchCtx, cancel := workflow.WithCancel(ctx)
	defer cancel()
	if input.Cancel {
		workflow.Go(ctx, func(ctx workflow.Context) {
			if workflow.Await(ctx, func() bool { return out.Branches[1].Status != "" }) == nil {
				cancel()
			}
		})
	}
	done := 0
	for i, branch := range p.Branches {
		frame := &branchWalkFrame{
			start: branch.Start, upstream: seedUpstream,
			evaluatedGates: seedGates, pointers: seedPointers,
			completed: seedCompleted, lastStage: "seed", lastResult: seed,
		}
		if p.BranchTimeoutSeconds > 0 {
			frame.deadline = workflow.Now(ctx).Add(time.Duration(p.BranchTimeoutSeconds) * time.Second)
		}
		rec := &runJournal{owner: root.owner, branch: i + 1, branchWalk: frame, machine: m, live: true, proj: root.proj, branchRef: root.branchRef}
		rec.proj.Ops = nil
		workflow.Go(branchCtx, func(ctx workflow.Context) {
			result, err := walk(ctx, in, m, rec, nil)
			out.Branches[i] = branchWalkTestOutcome{Status: frame.status, TerminalTarget: frame.terminalTarget, Result: result, Pointers: frame.pointers}
			if err != nil {
				out.Branches[i].Error = err.Error()
			}
			done++
		})
	}
	if err := workflow.Await(ctx, func() bool { return done == len(p.Branches) }); err != nil {
		return branchWalkTestResult{}, err
	}
	// Body execution must not mutate the seed or author a whole-run terminal.
	if !maps.Equal(seed.Outputs, map[string]any{"value": "frozen-root"}) || len(seedPointers) != 1 || len(seedUpstream) != 1 || len(seedCompleted) != 1 || len(seedGates) != 0 {
		return branchWalkTestResult{}, fmt.Errorf("branch mutated frozen root")
	}
	for _, op := range root.projectionSnapshot().Ops {
		if op.Event != nil && op.Event.Type == journal.EventRunFinished {
			return branchWalkTestResult{}, fmt.Errorf("branch authored run terminal")
		}
	}
	root.append(ctx, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
	if err := root.emitPending(ctx); err != nil {
		return branchWalkTestResult{}, err
	}
	out.Projection = root.projectionSnapshot()
	return out, nil
}

func branchBodyInput(name string, gateBody, secondStage bool) branchWalkTestInput {
	seed, a, b, after, join := r9Task("seed"), r9Task("alpha"), r9Task("beta"), r9Task("after"), r9Task("join")
	seed.Next = "parallel"
	a.Next, b.Next, after.Next = wf.TargetJoin, wf.TargetJoin, wf.TargetJoin
	a.InputsFrom, b.InputsFrom = map[string]string{"root": "seed.value"}, map[string]string{"root": "seed.value"}
	a.ContextFrom = []string{"seed"}
	spec := r9Spec(seed, a, b, join)
	if secondStage {
		spec.Tasks[1].Next = "after"
		after.InputsFrom = map[string]string{"own": "alpha.value"}
		spec.Tasks = append(spec.Tasks, after)
	}
	if gateBody {
		spec.Tasks[1].Next = "review"
		spec.Gates = []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"}, Branches: map[string]string{"pass": wf.TargetJoin, "fail": wf.TargetJoin}}}
	}
	spec.Parallels = []apiv1.Parallel{{Name: "parallel", Branches: []apiv1.Branch{{Name: "alpha", Start: "alpha"}, {Name: "beta", Start: "beta"}}, Join: "join", FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrentBranches: 2}}
	in := runInput(name, spec)
	in.DSLVersion, in.LiveJournal = "3.0", true
	return branchWalkTestInput{Run: in}
}

func TestStaticBranchBodiesIsolationAndOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		result                           apiv1.ResultStatus
		gate, gatePass, tolerated, empty bool
		want                             journal.BranchStatus
	}{
		{name: "success", result: apiv1.ResultSuccess, want: journal.BranchSucceeded},
		{name: "empty-success", result: apiv1.ResultSuccess, empty: true, want: journal.BranchNoOutput},
		{name: "no-work", result: apiv1.ResultNoWork, want: journal.BranchNoOutput},
		{name: "failure", result: apiv1.ResultFailure, want: journal.BranchFailed},
		{name: "tolerated", result: apiv1.ResultFailure, tolerated: true, want: journal.BranchNoOutput},
		{name: "gate-cleared", result: apiv1.ResultFailure, gate: true, gatePass: true, want: journal.BranchSucceeded},
		{name: "gate-unresolved", result: apiv1.ResultFailure, gate: true, want: journal.BranchFailed},
		{name: "blocked", result: apiv1.ResultBlocked, want: journal.BranchFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := branchBodyInput(tc.name, tc.gate, false)
			in.Run.Spec.Tasks[1].ContinueOnError = tc.tolerated
			writer, dir := newLiveWriter(t)
			var mu sync.Mutex
			seen := map[string]apiv1.InvocationEnvelope{}
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Auto: &fixtureAuto{localOK: tc.gatePass}, Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
				mu.Lock()
				seen[stage] = env
				mu.Unlock()
				res := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage}}
				if stage == "alpha" {
					res.Status = tc.result
					if tc.empty {
						res.Outputs = nil
					}
				}
				return res, nil
			}}}
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
			if out.Branches[0].Status != tc.want || out.Branches[1].Status != journal.BranchSucceeded {
				t.Fatalf("outcomes = %+v", out.Branches)
			}
			if tc.result == apiv1.ResultBlocked && out.Branches[0].TerminalTarget != wf.TargetEscalate {
				t.Fatalf("blocked terminal = %+v", out.Branches[0])
			}
			for _, name := range []string{"alpha", "beta"} {
				inv := seen[name]
				if inv.Inputs["root"] != "frozen-root" || len(inv.ContextPointers) != 1 || inv.ContextPointers[0].Name != "seed.artifact[0]" {
					t.Fatalf("%s did not receive isolated root: %+v", name, inv)
				}
			}
			for _, event := range liveEvents(t, dir, in.Run.RunID) {
				if event.Type == journal.EventStageStarted && (event.Stage == "alpha" || event.Stage == "beta") {
					want := 1
					if event.Stage == "beta" {
						want = 2
					}
					if event.Branch != want {
						t.Fatalf("branch attribution = %+v", event)
					}
				}
			}
			if tc.tolerated && out.Branches[0].Result.Outputs["alpha"].Outputs != nil {
				t.Fatal("tolerated outputs leaked")
			}
			if _, leaked := out.Branches[1].Result.Outputs["alpha"]; leaked {
				t.Fatal("sibling result entered beta's frozen input state")
			}
			if !tc.gate && tc.result != apiv1.ResultBlocked {
				alpha := apiv1.ResultEnvelope{Status: tc.result, Outputs: map[string]any{"value": "alpha"}}
				if tc.empty {
					alpha.Outputs = nil
				}
				events := runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, script: map[string][]scriptedCall{
					"seed":  {{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "frozen-root"}}}},
					"alpha": {{result: alpha}},
					"beta":  {{result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "beta"}}}},
				}}, "local-"+tc.name)
				matched := false
				for _, event := range events {
					if event.Type != journal.EventParallelFinished {
						continue
					}
					matched = true
					if len(event.Completeness) != len(out.Branches) {
						t.Fatalf("runner completeness = %+v", event.Completeness)
					}
					for i, branch := range event.Completeness {
						if branch.Status != out.Branches[i].Status {
							t.Fatalf("runner branch %d=%s, engine=%s", i, branch.Status, out.Branches[i].Status)
						}
					}
				}
				if !matched {
					t.Fatal("runner did not settle parallel")
				}
			}
		})
	}
}

func TestStaticBranchBodyTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		nextStage, cancel bool
		want              journal.BranchStatus
	}{
		{"timeout-at-next-stage", true, false, journal.BranchTimedOut},
		{"final-stage-may-finish", false, false, journal.BranchSucceeded},
		{"cancel-active-stage", true, true, journal.BranchCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := branchBodyInput(tc.name, false, tc.nextStage)
			in.Run.Spec.Parallels[0].BranchTimeoutSeconds = 1
			in.Cancel = tc.cancel
			writer, _ := newLiveWriter(t)
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, invocation apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				if strings.HasSuffix(invocation.TaskID, ":after") {
					t.Error("branch dispatched after its deadline/cancellation")
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "done"}}, nil
			}}}
			env.RegisterActivity(acts)
			delay := 2 * time.Second
			if tc.cancel {
				delay = time.Hour
			}
			env.OnActivity(ActRunDeterministic, mock.Anything, mock.MatchedBy(func(env apiv1.InvocationEnvelope) bool { return strings.HasSuffix(env.TaskID, ":alpha") }), mock.Anything, mock.Anything, mock.Anything).
				After(delay).Return(stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "alpha"}}}, nil).Once()
			env.OnActivity(ActRunDeterministic, mock.Anything, mock.MatchedBy(func(env apiv1.InvocationEnvelope) bool { return strings.HasSuffix(env.TaskID, ":beta") }), mock.Anything, mock.Anything, mock.Anything).
				Return(stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": "beta"}}}, nil).Once()
			env.ExecuteWorkflow(staticBranchBodies, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var out branchWalkTestResult
			if err := env.GetWorkflowResult(&out); err != nil {
				t.Fatal(err)
			}
			if out.Branches[0].Status != tc.want || out.Branches[1].Status != journal.BranchSucceeded {
				t.Fatalf("outcomes = %+v", out.Branches)
			}
			env.AssertExpectations(t)
		})
	}
}

func TestStaticBranchBodyGateRepassAndOwnOutputs(t *testing.T) {
	in := branchBodyInput("branch-repass", true, false)
	in.Run.Spec.Gates[0].Branches["fail"] = "alpha"
	writer, _ := newLiveWriter(t)
	var mu sync.Mutex
	reviews := 0
	dispatches := map[string]int{}
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Auto: automatedFunc(func(_ context.Context, _ apiv1.AutomatedGate, _ apiv1.InvocationEnvelope) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		reviews++
		if reviews == 1 {
			return "fail", nil
		}
		return "pass", nil
	}), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		mu.Lock()
		defer mu.Unlock()
		stage := strings.TrimPrefix(env.TaskID, env.RunID+":")
		dispatches[stage]++
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage}}, nil
	}}}
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
	if dispatches["alpha"] != 2 || dispatches["beta"] != 1 || out.Branches[0].Status != journal.BranchSucceeded {
		t.Fatalf("dispatches=%v, outcomes=%+v", dispatches, out.Branches)
	}
	for _, op := range out.Projection.Ops {
		if op.Event != nil && op.Event.Stage == "review" && op.Event.Branch != 1 {
			t.Fatalf("gate lost branch: %+v", op.Event)
		}
	}
}

func TestStaticBranchBodiesLegacyRefusal(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion(branchWalkTestVersion, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	env.ExecuteWorkflow(staticBranchBodies, branchBodyInput("legacy", false, false))
	if env.GetWorkflowError() == nil {
		t.Fatal("legacy fanout was enabled")
	}
}
