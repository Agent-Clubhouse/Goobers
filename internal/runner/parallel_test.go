package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

var _ executor.BoundedArtifactRecorder = (*branchJournal)(nil)

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

func TestParallelJoinPointersAreDeclarationOrderedAndBranchTagged(t *testing.T) {
	spec := apiv1.Parallel{
		Name: "fan",
		Branches: []apiv1.Branch{
			{Name: "security", Start: "a"},
			{Name: "perf", Start: "b"},
			{Name: "coverage", Start: "c"},
		},
	}
	pointer := func(name string) apiv1.ContextPointer {
		return apiv1.ContextPointer{
			Name: name,
			Artifact: &apiv1.ArtifactPointer{
				Path:   "artifacts/" + name,
				Digest: apiv1.Digest([]byte(name)),
			},
		}
	}
	build := func(arrival []string) []apiv1.ContextPointer {
		p := newParallelExec(spec)
		byBranch := map[string][]apiv1.ContextPointer{
			"security": {pointer("security-0"), pointer("security-1")},
			"perf":     {pointer("perf-0")},
			"coverage": {pointer("coverage-0")},
		}
		for _, name := range arrival {
			p.branch(name).pointers = append(p.branch(name).pointers, byBranch[name]...)
		}
		return p.joinPointers(nil)
	}

	first := build([]string{"coverage", "security", "perf"})
	second := build([]string{"perf", "coverage", "security"})
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("join pointer JSON differs by arrival order:\n%s\n%s", firstJSON, secondJSON)
	}

	wantNames := []string{"security-0", "security-1", "perf-0", "coverage-0"}
	wantBranches := []int{1, 1, 2, 3}
	wantBranchNames := []string{"security", "security", "perf", "coverage"}
	for i := range wantNames {
		if first[i].Name != wantNames[i] || first[i].Branch != wantBranches[i] || first[i].BranchName != wantBranchNames[i] {
			t.Errorf("pointer %d = %+v, want name=%q branch=%d branchName=%q",
				i, first[i], wantNames[i], wantBranches[i], wantBranchNames[i])
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

func TestParallelCurrentStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*parallelExec)
		want    journal.BranchStatus
	}{
		{
			name: "produced outputs",
			prepare: func(p *parallelExec) {
				p.recordCurrent(map[string]any{"findings": 3}, nil)
			},
			want: journal.BranchSucceeded,
		},
		{
			name: "produced artifacts only",
			prepare: func(p *parallelExec) {
				p.recordCurrent(nil, []apiv1.ContextPointer{{Artifact: &apiv1.ArtifactPointer{Path: "report"}}})
			},
			want: journal.BranchSucceeded,
		},
		{
			name:    "settled empty",
			prepare: func(*parallelExec) {},
			want:    journal.BranchNoOutput,
		},
		{
			name: "branch-scoped no-work is a successful empty settle",
			prepare: func(p *parallelExec) {
				p.markCurrentNoOutput()
			},
			want: journal.BranchNoOutput,
		},
		{
			name: "failure",
			prepare: func(p *parallelExec) {
				p.markCurrentFailed()
			},
			want: journal.BranchFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newParallelExec(apiv1.Parallel{
				Branches: []apiv1.Branch{{Name: "branch", Start: "stage"}},
			})
			tc.prepare(p)
			if got := p.currentStatus(); got != tc.want {
				t.Errorf("currentStatus = %q, want %q", got, tc.want)
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

type parallelBlockingDeterministic struct {
	started chan string
	release <-chan struct{}
	active  atomic.Int32
	max     atomic.Int32
	calls   atomic.Int32
}

func (d *parallelBlockingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.calls.Add(1)
	if strings.HasSuffix(env.TaskID, ":collate") {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	active := d.active.Add(1)
	for current := d.max.Load(); active > current && !d.max.CompareAndSwap(current, active); current = d.max.Load() {
	}
	d.started <- env.TaskID
	<-d.release
	d.active.Add(-1)
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]any{"stage": env.TaskID},
	}, nil
}

type parallelRunResult struct {
	result Result
	err    error
}

type collateCapturingDeterministic struct {
	delegate invoke.Deterministic
	env      chan<- apiv1.InvocationEnvelope
}

func (c *collateCapturingDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if strings.HasSuffix(env.TaskID, ":collate") {
		c.env <- env
	}
	return c.delegate.Run(ctx, env, run)
}

func parallelRunnerMachine(t *testing.T, maxConcurrent int32, firstWorkspace apiv1.WorkspaceMode) *workflow.Machine {
	t.Helper()
	branchTask := func(name string, workspace apiv1.WorkspaceMode) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: workspace},
			Next: workflow.TargetJoin,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "parallel-runner-fixture", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				branchTask("lens-a", firstWorkspace),
				branchTask("lens-b", apiv1.WorkspaceScratch),
				branchTask("lens-c", apiv1.WorkspaceScratch),
				{
					Name: "collate", Type: apiv1.TaskDeterministic, Goal: "collate",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
					Next: workflow.TerminalComplete,
				},
			},
			Parallels: []apiv1.Parallel{{
				Name:                  "fan",
				FailurePolicy:         apiv1.BranchContinueOnError,
				MaxConcurrentBranches: maxConcurrent,
				Join:                  "collate",
				Branches: []apiv1.Branch{
					{Name: "a", Start: "lens-a"},
					{Name: "b", Start: "lens-b"},
					{Name: "c", Start: "lens-c"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile parallel fixture: %v", err)
	}
	return machine
}

func newParallelTestRunner(t *testing.T, newDet NewDeterministicFunc) (*Runner, string) {
	t.Helper()
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	r, err := New(Config{
		NewDeterministic: newDet,
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		ScratchDir:       t.TempDir(),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("new parallel runner: %v", err)
	}
	return r, runsDir
}

func parallelAgenticBranchGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	task := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "parallel-agentic-branch-gate", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				task("lens-a", "review-a"),
				task("lens-b", workflow.TargetJoin),
				task("collate", workflow.TerminalComplete),
			},
			Gates: []apiv1.Gate{{
				Name:      "review-a",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{
					Goober: "reviewer", Workspace: apiv1.WorkspaceScratch,
				},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         workflow.TargetJoin,
					string(apiv1.VerdictNeedsChanges): workflow.TargetJoin,
					string(apiv1.VerdictFail):         workflow.TargetAbort,
				},
			}},
			Parallels: []apiv1.Parallel{{
				Name: "fan", FailurePolicy: apiv1.BranchContinueOnError,
				MaxConcurrentBranches: 2,
				Join:                  "collate",
				Branches: []apiv1.Branch{
					{Name: "a", Start: "lens-a"},
					{Name: "b", Start: "lens-b"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile parallel agentic branch-gate fixture: %v", err)
	}
	return machine
}

func TestRunnerParallelGateVerdictContextPointerPreservesIntegrity(t *testing.T) {
	const runID = "parallel-verdict-integrity"
	results := map[string]stubTaskResult{
		runID + ":lens-a":  {status: apiv1.ResultSuccess},
		runID + ":lens-b":  {status: apiv1.ResultSuccess},
		runID + ":collate": {status: apiv1.ResultSuccess},
	}
	collateEnv := make(chan apiv1.InvocationEnvelope, 1)
	r, _ := newRerunTestRunner(t,
		func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return &fixedVerdictReviewer{verdict: apiv1.Verdict{Decision: apiv1.VerdictPass}}, nil
		},
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &collateCapturingDeterministic{
				delegate: &stubDeterministic{rec: rec, byTask: results},
				env:      collateEnv,
			}, nil
		},
	)
	r.cfg.ScratchDir = t.TempDir()

	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: parallelAgenticBranchGateMachine(t),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	var env apiv1.InvocationEnvelope
	select {
	case env = <-collateEnv:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("collate invocation was not captured")
	}
	for _, pointer := range env.ContextPointers {
		if pointer.Name != "review-a.verdict" {
			continue
		}
		if pointer.Branch != 1 || pointer.BranchName != "a" ||
			pointer.Integrity != apiv1.IntegrityDerived ||
			pointer.Artifact == nil ||
			pointer.Artifact.Integrity != apiv1.IntegrityDerived {
			t.Fatalf("verdict pointer = %+v, want derived pointer and artifact integrity from branch a", pointer)
		}
		return
	}
	t.Fatalf("collate context pointers = %+v, want review-a.verdict", env.ContextPointers)
}

func TestRunnerExecutesReadOnlyParallelWithinDeclaredBound(t *testing.T) {
	release := make(chan struct{})
	det := &parallelBlockingDeterministic{
		started: make(chan string, 4),
		release: release,
	}
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	const runID = "parallel-bounded"
	done := make(chan error, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID: runID, Gaggle: "demo",
			Machine: parallelRunnerMachine(t, 2, apiv1.WorkspaceRepoReadOnly),
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main",
			},
		})
		if err == nil && result.Phase != journal.PhaseCompleted {
			err = fmt.Errorf("phase = %q, want completed", result.Phase)
		}
		done <- err
	}()

	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-det.started:
		case err := <-done:
			t.Fatalf("parallel run ended before two branches overlapped: %v", err)
		case <-time.After(runnerTestWaitTimeout):
			t.Fatalf("two branches did not overlap (maximum observed concurrency %d)", det.max.Load())
		}
	}
	select {
	case stage := <-det.started:
		t.Fatalf("third branch %q started before a concurrency slot was released", stage)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("parallel run: %v", err)
		}
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("parallel run did not finish")
	}
	if got := det.max.Load(); got != 2 {
		t.Fatalf("maximum concurrent branches = %d, want 2", got)
	}

	rd, err := journal.OpenRead(runsDir + "/" + runID)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	wantBranch := map[string]int{"lens-a": 1, "lens-b": 2, "lens-c": 3, "collate": 0}
	for _, event := range events {
		if want, ok := wantBranch[event.Stage]; ok && event.Branch != want {
			t.Errorf("%s for stage %q has branch %d, want %d", event.Type, event.Stage, event.Branch, want)
		}
		if (event.Type == journal.EventParallelStarted || event.Type == journal.EventParallelFinished ||
			event.Type == journal.EventRunFinished) && event.Branch != 0 {
			t.Errorf("%s has branch %d, want root attribution", event.Type, event.Branch)
		}
	}
}

// A writable-repo branch workspace is now rejected by compile-time rule 9
// (internal/workflow/v_next/parallel.go) for every declared width, including
// the unset-default case — strictly earlier and stronger than this runtime
// check, so a machine built through the normal workflow.Compile path can no
// longer reach parallel_run.go's own maxConcurrentBranches>1 dispatch-time
// check with an invalid workspace. That runtime check remains as defense in
// depth against a machine compiled under an older, more lenient rule 9 (e.g.
// a cached digest predating this validation), which this test cannot
// construct without bypassing the compiler entirely. Assert the rejection at
// its actual (now earlier) point instead of pretending dispatch is reached.
func TestRunnerRejectsWritableConcurrentParallelAtCompile(t *testing.T) {
	base := parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch)
	def := base.Def
	def.Spec.Tasks[0].Run.Workspace = ""
	_, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err == nil || !strings.Contains(err.Error(), `task "lens-a" resolves to a writable repo workspace`) {
		t.Fatalf("error = %v, want writable-workspace rejection", err)
	}
}

func TestRunnerConcurrentBranchGateKeepsAttributionAndNotifiesEscalation(t *testing.T) {
	base := branchGateFanInMachine(t)
	def := base.Def
	def.Spec.Parallels[0].MaxConcurrentBranches = 2
	def.Spec.Gates[0].MaxRepasses = 1
	def.Spec.Gates[0].Branches[gate.OutcomeFail] = "review-security"
	disposition := def.Spec.Tasks[1]
	disposition.Name = "park-security"
	def.Spec.Tasks = append(def.Spec.Tasks, disposition)
	def.Spec.Gates[0].Branches[workflow.BranchEscalate] = disposition.Name
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile concurrent branch-gate fixture: %v", err)
	}
	const runID = "parallel-branch-gate"
	byTask := map[string]stubTaskResult{
		runID + ":review-security": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "review_failed", Message: "review requires gate approval"},
		},
		runID + ":review-performance": {status: apiv1.ResultSuccess},
		runID + ":park-security":      {status: apiv1.ResultSuccess},
		runID + ":collate":            {status: apiv1.ResultSuccess},
	}
	r, runsDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
	)
	commenter := &recordingCommenter{}
	r.cfg.Automated = fixedOutcomeAutomated(gate.OutcomeFail)
	r.cfg.Escalation = &gate.EscalationNotifier{Poster: commenter}

	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: machine,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main",
		},
		Item: &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	rd, err := journal.OpenRead(runsDir + "/" + runID)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	for _, event := range events {
		if event.Gate == "accept-security" && event.Branch != 1 {
			t.Errorf("%s for gate %q has branch %d, want 1", event.Type, event.Gate, event.Branch)
		}
	}
	if len(commenter.requests) != 1 || !strings.Contains(commenter.requests[0].Comment, "repass budget exhausted") {
		t.Fatalf("escalation notifications = %+v, want one canonical gate notification", commenter.requests)
	}
}

func parallelFailFastMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	base := parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch)
	def := base.Def
	def.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
	def.Spec.Parallels[0].OnFailure = workflow.TargetAbort
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile fail-fast fixture: %v", err)
	}
	return machine
}

type failFastDeterministic struct {
	bStarted   chan struct{}
	aFailed    chan struct{}
	releaseB   chan struct{}
	lensCCalls atomic.Int32
}

func (d *failFastDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	switch {
	case strings.HasSuffix(env.TaskID, ":lens-a"):
		<-d.bStarted
		close(d.aFailed)
		return apiv1.ResultEnvelope{
			Status: apiv1.ResultFailure,
			Error:  &apiv1.ErrorInfo{Code: "lens_failed", Message: "lens failed"},
		}, nil
	case strings.HasSuffix(env.TaskID, ":lens-b"):
		close(d.bStarted)
		<-d.releaseB
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"findings": 1}}, nil
	case strings.HasSuffix(env.TaskID, ":lens-c"):
		d.lensCCalls.Add(1)
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	default:
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
}

func TestRunnerConcurrentFailFastCancelsRunningAndQueuedBranches(t *testing.T) {
	det := &failFastDeterministic{
		bStarted: make(chan struct{}),
		aFailed:  make(chan struct{}),
		releaseB: make(chan struct{}),
	}
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	const runID = "parallel-fail-fast"
	done := make(chan parallelRunResult, 1)
	go func() {
		result, err := r.Start(context.Background(), StartInput{
			RunID: runID, Gaggle: "demo", Machine: parallelFailFastMachine(t),
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		})
		done <- parallelRunResult{result: result, err: err}
	}()
	select {
	case <-det.aFailed:
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("failing branch did not finish")
	}
	deadline := time.Now().Add(runnerTestWaitTimeout)
	for {
		rd, err := journal.OpenRead(runsDir + "/" + runID)
		if err == nil {
			events, readErr := rd.Events()
			if readErr != nil {
				t.Fatalf("read live journal: %v", readErr)
			}
			cancelled := false
			for _, event := range events {
				cancelled = cancelled || (event.Type == journal.EventBranchFinished &&
					event.Branch == 3 && event.BranchStatus == journal.BranchCancelled)
			}
			if cancelled {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("queued branch was not cancelled after first branch failed")
		}
		time.Sleep(10 * time.Millisecond) // Polling interval for cancellation observed by the queued branch.
	}
	close(det.releaseB)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Start: %v", got.err)
		}
		if got.result.Phase != journal.PhaseAborted {
			t.Fatalf("phase = %q, want aborted through reserved onFailure", got.result.Phase)
		}
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("fail-fast run did not finish")
	}
	if calls := det.lensCCalls.Load(); calls != 0 {
		t.Fatalf("queued lens-c calls = %d, want 0", calls)
	}

	completeness := parallelCompleteness(t, runsDir, runID)
	want := []journal.BranchStatus{
		journal.BranchFailed,
		journal.BranchCancelled,
		journal.BranchCancelled,
	}
	for i, status := range want {
		if completeness[i].Status != status {
			t.Errorf("branch %d status = %q, want %q", i+1, completeness[i].Status, status)
		}
	}
}

func TestRunnerConcurrentBlockedUsesCanonicalTerminalTransition(t *testing.T) {
	const runID = "parallel-blocked"
	// The blocker is deliberately a different issue than the driving item:
	// a self-reference is dropped before the outcome is built (#2961), which
	// would make this exercise the unattributed path instead of the named-
	// blocker propagation it is actually about.
	byTask := map[string]stubTaskResult{
		runID + ":lens-a": {
			status:    apiv1.ResultBlocked,
			errorInfo: &apiv1.ErrorInfo{Code: "DEPENDENCY_NOT_MET", Message: "issue #43 must merge first"},
			outputs:   map[string]interface{}{OutputBlockedBy: "43"},
		},
		runID + ":lens-b": {status: apiv1.ResultSuccess},
		runID + ":lens-c": {status: apiv1.ResultSuccess},
	}
	r, runsDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
	)
	var blocked *BlockedOutcome
	r.cfg.Blocked = func(_ context.Context, outcome BlockedOutcome) error {
		blocked = &outcome
		return nil
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", result.Phase)
	}
	if blocked == nil || blocked.Stage != "lens-a" || len(blocked.Blockers) != 1 || blocked.Blockers[0] != "43" {
		t.Fatalf("BlockedOutcome = %+v", blocked)
	}

	rd, err := journal.OpenRead(runsDir + "/" + runID)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	for _, event := range events {
		if (event.Type == journal.EventParallelFinished || event.Type == journal.EventRunFinished ||
			(event.Type == journal.EventError && event.Error != nil && event.Error.Code == "blocked_by_agent")) &&
			event.Branch != 0 {
			t.Errorf("%s has branch %d, want root attribution", event.Type, event.Branch)
		}
	}
}

func TestRunnerResumeConcurrentBlockedTerminalBeforeParallelFinished(t *testing.T) {
	const runID = "parallel-blocked-resume"
	machine := parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch)
	var constructions atomic.Int32
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			constructions.Add(1)
			return &countingDeterministic{}, nil
		},
	)
	var blocked *BlockedOutcome
	r.cfg.Blocked = func(_ context.Context, outcome BlockedOutcome) error {
		blocked = &outcome
		return nil
	}

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        machine.Def.Name,
		WorkflowVersion: machine.Def.Version,
		WorkflowDigest:  machine.Digest(),
		Gaggle:          "demo",
		Trigger:         journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("fan")
	for _, event := range []journal.Event{
		{
			Type: journal.EventParallelStarted, Parallel: "fan",
			Completeness: []journal.BranchOutcome{
				{Branch: 1, Name: "a"}, {Branch: 2, Name: "b"}, {Branch: 3, Name: "c"},
			},
		},
		{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "a", Stage: "lens-a"},
		{Type: journal.EventStageStarted, Branch: 1, Stage: "lens-a", Attempt: 1},
		{
			Type: journal.EventStageFinished, Branch: 1, Stage: "lens-a", Attempt: 1,
			Status:  string(apiv1.ResultBlocked),
			Outputs: map[string]any{OutputBlockedBy: "42"},
			Error:   &journal.ErrorDetail{Code: "DEPENDENCY_NOT_MET", Message: "issue #42 must merge first"},
		},
		{
			Type: journal.EventBranchFinished, Parallel: "fan", Branch: 1, BranchName: "a",
			BranchStatus: journal.BranchFailed,
		},
	} {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", result.Phase)
	}
	if got := constructions.Load(); got != 0 {
		t.Fatalf("executor constructions = %d, want no branch replay", got)
	}
	if blocked == nil || blocked.Stage != "lens-a" || len(blocked.Blockers) != 1 || blocked.Blockers[0] != "42" {
		t.Fatalf("BlockedOutcome = %+v", blocked)
	}
}

func TestRunnerConcurrentBranchesShareRunStepBudget(t *testing.T) {
	release := make(chan struct{})
	close(release)
	det := &parallelBlockingDeterministic{started: make(chan string, 3), release: release}
	r, _ := newParallelTestRunner(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil })
	r.maxSteps = 3
	_, err := r.Start(context.Background(), StartInput{
		RunID: "parallel-shared-step-budget", Gaggle: "demo",
		Machine: parallelRunnerMachine(t, 3, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if calls := det.calls.Load(); err == nil || !strings.Contains(err.Error(), "exceeded max steps (3)") || calls != 2 {
		t.Fatalf("Start error = %v, task calls = %d; want shared max-steps failure after 2 branch tasks", err, calls)
	}
}

type parallelResumeDeterministic struct {
	mu      sync.Mutex
	calls   map[string]int
	started chan string
	release <-chan struct{}
}

func (d *parallelResumeDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.mu.Lock()
	d.calls[env.TaskID]++
	d.mu.Unlock()
	if strings.HasSuffix(env.TaskID, ":collate") {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	d.started <- env.TaskID
	<-d.release
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]any{"stage": env.TaskID},
	}, nil
}

func (d *parallelResumeDeterministic) callCount(taskID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[taskID]
}

func TestRunnerResumeConcurrentParallelDoesNotRepeatFinishedStages(t *testing.T) {
	release := make(chan struct{})
	det := &parallelResumeDeterministic{
		calls:   map[string]int{},
		started: make(chan string, 8),
		release: release,
	}
	r, _ := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	const runID = "parallel-resume-finished"
	machine := parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan parallelRunResult, 1)
	go func() {
		result, err := r.Start(ctx, StartInput{
			RunID: runID, Gaggle: "demo", Machine: machine,
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		})
		done <- parallelRunResult{result: result, err: err}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-det.started:
		case <-time.After(runnerTestWaitTimeout):
			t.Fatal("parallel branches did not start before drain")
		}
	}
	cancel()
	close(release)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("drain parallel: %v", got.err)
		}
		if got.result.Phase != journal.PhaseRunning {
			t.Fatalf("drained phase = %q, want running", got.result.Phase)
		}
	case <-time.After(runnerTestWaitTimeout):
		t.Fatal("parallel drain did not return")
	}

	result, err := r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("resumed phase = %q, want completed", result.Phase)
	}
	for _, stage := range []string{"lens-a", "lens-b", "lens-c", "collate"} {
		if got := det.callCount(runID + ":" + stage); got != 1 {
			t.Errorf("%s calls = %d, want 1", stage, got)
		}
	}
}

func TestRunnerArtifactCompletenessMatchesConcurrencySetting(t *testing.T) {
	byTask := map[string]stubTaskResult{}
	for _, runID := range []string{"parallel-artifacts-one", "parallel-artifacts-two"} {
		for _, stage := range []string{"lens-a", "lens-b", "lens-c"} {
			byTask[runID+":"+stage] = stubTaskResult{
				status: apiv1.ResultSuccess, artifactName: stage + ".txt", artifactData: []byte(stage),
			}
		}
		byTask[runID+":collate"] = stubTaskResult{status: apiv1.ResultSuccess}
	}
	r, runsDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
	)
	for _, tc := range []struct {
		runID string
		max   int32
	}{
		{runID: "parallel-artifacts-one", max: 1},
		{runID: "parallel-artifacts-two", max: 2},
	} {
		result, err := r.Start(context.Background(), StartInput{
			RunID: tc.runID, Gaggle: "demo",
			Machine: parallelRunnerMachine(t, tc.max, apiv1.WorkspaceScratch),
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.runID, err)
		}
		if result.Phase != journal.PhaseCompleted {
			t.Fatalf("%s phase = %q, want completed", tc.runID, result.Phase)
		}
	}
	sequential := parallelCompleteness(t, runsDir, "parallel-artifacts-one")
	concurrent := parallelCompleteness(t, runsDir, "parallel-artifacts-two")
	for i := range sequential {
		if sequential[i].Status != journal.BranchSucceeded || sequential[i].Artifacts != 1 {
			t.Errorf("sequential branch %d = %+v, want succeeded with one artifact", i+1, sequential[i])
		}
		if concurrent[i].Status != sequential[i].Status || concurrent[i].Artifacts != sequential[i].Artifacts {
			t.Errorf("branch %d differs by concurrency: sequential=%+v concurrent=%+v", i+1, sequential[i], concurrent[i])
		}
	}
}

// slowThenFastDeterministic sleeps past the declared branchTimeoutSeconds
// budget on the branch's FIRST task, then tracks whether its second task ever
// ran. branchTimeoutSeconds is checked at the next stage boundary (never
// mid-stage — see the field's doc comment), so a single already-slow task
// still completes and reaches @join normally; the enforcement point is
// whether a NEW stage gets dispatched once the budget is already spent. That
// requires two tasks per branch: the first exceeds the budget on its own but
// still returns success, and the second (slow-b) must never be dispatched.
type slowThenFastDeterministic struct {
	slowFirstTask string
	delay         time.Duration
	secondRan     atomic.Bool
}

func (s *slowThenFastDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	switch {
	case strings.HasSuffix(env.TaskID, ":"+s.slowFirstTask):
		time.Sleep(s.delay) // Intentional branch delay exercises timeout and completion ordering.
	case strings.HasSuffix(env.TaskID, ":slow-b"):
		s.secondRan.Store(true)
	case strings.HasSuffix(env.TaskID, ":fast"):
		// A real output, so "fast" settles succeeded rather than no-output
		// (a task producing nothing is its own, distinct, legitimate status).
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"summary": "ok"}}, nil
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// branchTimeoutMachine declares a two-branch parallel with a short
// branchTimeoutSeconds budget, at the given width. The "slow" branch has two
// sequential tasks so the timeout has a real next-stage boundary to enforce
// at; "fast" is a single task that always finishes well within budget.
func branchTimeoutMachine(t *testing.T, maxConcurrent int32, policy apiv1.BranchFailurePolicy, onFailure string) *workflow.Machine {
	t.Helper()
	scratchTask := func(name, next string) apiv1.Task {
		return apiv1.Task{
			Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Next: next,
		}
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "branch-timeout-fixture", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "demo",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:    "fan",
			Tasks: []apiv1.Task{
				scratchTask("slow-a", "slow-b"),
				scratchTask("slow-b", workflow.TargetJoin),
				scratchTask("fast", workflow.TargetJoin),
				scratchTask("collate", workflow.TerminalComplete),
			},
			Parallels: []apiv1.Parallel{{
				Name:                  "fan",
				FailurePolicy:         policy,
				OnFailure:             onFailure,
				BranchTimeoutSeconds:  1,
				MaxConcurrentBranches: maxConcurrent,
				Join:                  "collate",
				Branches: []apiv1.Branch{
					{Name: "slow", Start: "slow-a"},
					{Name: "fast", Start: "fast"},
				},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile branch-timeout fixture: %v", err)
	}
	return machine
}

func testBranchTimeoutRecordsTimedOut(t *testing.T, maxConcurrent int32) {
	det := &slowThenFastDeterministic{slowFirstTask: "slow-a", delay: 1200 * time.Millisecond}
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	runID := fmt.Sprintf("branch-timeout-%d", maxConcurrent)
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: branchTimeoutMachine(t, maxConcurrent, apiv1.BranchContinueOnError, ""),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (continue_on_error tolerates the timeout)", result.Phase)
	}
	if det.secondRan.Load() {
		t.Fatal("slow-b ran after the branch already exceeded its timeout budget")
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if len(completeness) != 2 {
		t.Fatalf("completeness = %+v, want 2 entries", completeness)
	}
	if completeness[0].Name != "slow" || completeness[0].Status != journal.BranchTimedOut {
		t.Fatalf("slow branch = %+v, want status %q", completeness[0], journal.BranchTimedOut)
	}
	if completeness[1].Name != "fast" || completeness[1].Status != journal.BranchSucceeded {
		t.Fatalf("fast branch = %+v, want status %q", completeness[1], journal.BranchSucceeded)
	}
}

func TestSequentialBranchExceedingTimeoutIsRecordedTimedOut(t *testing.T) {
	testBranchTimeoutRecordsTimedOut(t, 1)
}

func TestConcurrentBranchExceedingTimeoutIsRecordedTimedOut(t *testing.T) {
	testBranchTimeoutRecordsTimedOut(t, 2)
}

// Under fail_fast at maxConcurrentBranches=1, a timed-out branch must cancel
// its still-queued sibling exactly like an ordinary ResultFailure would:
// anyFailedLocked already classes BranchTimedOut as a failure for route(),
// but the sequential path's eager cancelRemaining() call (run.go, right
// after @join) gates on par.anyFailed() rather than a literal status
// comparison, so this exercises that path end to end. (The concurrent
// orchestrator's equivalent eager-cancel check was updated too — see the
// literal `result.status == ... || result.status == journal.BranchTimedOut`
// condition in parallel_run.go — but proving it needs the sibling still
// in-flight when the timeout fires, which a 2-branch fixture with an
// instant "fast" task can't reliably arrange without its own race.)
func TestSequentialBranchTimeoutTriggersFailFastCancellation(t *testing.T) {
	det := &slowThenFastDeterministic{slowFirstTask: "slow-a", delay: 1200 * time.Millisecond}
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	const runID = "branch-timeout-failfast"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: branchTimeoutMachine(t, 1, apiv1.BranchFailFast, workflow.TargetAbort),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted through fail_fast's onFailure", result.Phase)
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if len(completeness) != 2 {
		t.Fatalf("completeness = %+v, want 2 entries", completeness)
	}
	if completeness[0].Name != "slow" || completeness[0].Status != journal.BranchTimedOut {
		t.Fatalf("slow branch = %+v, want status %q", completeness[0], journal.BranchTimedOut)
	}
	if completeness[1].Name != "fast" || completeness[1].Status != journal.BranchCancelled {
		t.Fatalf("fast branch = %+v, want status %q (fail_fast must cancel the still-queued sibling)", completeness[1], journal.BranchCancelled)
	}
}

func parallelCompleteness(t *testing.T, runsDir, runID string) []journal.BranchOutcome {
	t.Helper()
	rd, err := journal.OpenRead(runsDir + "/" + runID)
	if err != nil {
		t.Fatalf("open %s journal: %v", runID, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read %s journal: %v", runID, err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventParallelFinished {
			return events[i].Completeness
		}
	}
	t.Fatalf("%s has no parallel.finished event", runID)
	return nil
}
