package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// This file is FO-8's `make test-conformance` corpus for the static
// fan-out/fan-in construct (docs/design/static-fan-out-fan-in.md §9). Every
// TestConformance* function here corresponds to one of that section's
// required fixtures. Each fixture asserts a branch's own seq-ordered
// NormativeEvent sequence and the completeness record — never absolute seq
// across branches, and never cross-branch interleaving (§6.2).

// §9 fixture 1: a 3-branch parallel where every branch succeeds.
func TestConformanceParallelAllBranchesSucceed(t *testing.T) {
	byTask := map[string]stubTaskResult{
		"conformance-all-succeed:lens-a":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 0}},
		"conformance-all-succeed:lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 1}},
		"conformance-all-succeed:lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
		"conformance-all-succeed:collate": {status: apiv1.ResultSuccess},
	}
	r, runsDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
	)
	const runID = "conformance-all-succeed"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if len(completeness) != 3 {
		t.Fatalf("completeness = %+v, want 3 entries", completeness)
	}
	for i, name := range []string{"a", "b", "c"} {
		if completeness[i].Name != name || completeness[i].Status != journal.BranchSucceeded {
			t.Errorf("branch %d = %+v, want %q succeeded", i, completeness[i], name)
		}
	}
	branches := conformanceBranches(t, runsDir, runID)
	for branchID, name := range map[int]string{1: "a", 2: "b", 3: "c"} {
		seq := branches[branchID]
		if len(seq) == 0 {
			t.Fatalf("branch %d (%s) has no normative events", branchID, name)
		}
		for _, e := range seq {
			if e.Branch != branchID {
				t.Errorf("branch %d event %+v carries a different branch id", branchID, e)
			}
		}
	}
}

// §9 fixture 2: one branch failing under each of the three failure policies,
// asserting both the routing decision and whether the join ran.
func TestConformanceFailurePolicyRoutingAndJoinParticipation(t *testing.T) {
	for _, tc := range []struct {
		policy       apiv1.BranchFailurePolicy
		wantPhase    journal.RunPhase
		wantJoinRan  bool
		wantSiblings journal.BranchStatus // status of the never-failing sibling
	}{
		{policy: apiv1.BranchContinueOnError, wantPhase: journal.PhaseCompleted, wantJoinRan: true, wantSiblings: journal.BranchSucceeded},
		{policy: apiv1.BranchAllOrNothing, wantPhase: journal.PhaseAborted, wantJoinRan: false, wantSiblings: journal.BranchSucceeded},
		{policy: apiv1.BranchFailFast, wantPhase: journal.PhaseAborted, wantJoinRan: false, wantSiblings: journal.BranchCancelled},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			runID := "conformance-policy-" + string(tc.policy)
			byTask := map[string]stubTaskResult{
				runID + ":lens-a":  {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "lens_failed", Message: "lens failed"}},
				runID + ":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 1}},
				runID + ":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
				runID + ":collate": {status: apiv1.ResultSuccess},
			}
			r, runsDir := newParallelTestRunner(t,
				func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
					return &stubDeterministic{rec: rec, byTask: byTask}, nil
				},
			)
			machine := parallelPolicyMachine(t, tc.policy)
			result, err := r.Start(context.Background(), StartInput{
				RunID: runID, Gaggle: "demo", Machine: machine,
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if result.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", result.Phase, tc.wantPhase)
			}
			joinRan := stageRan(t, runsDir, runID, "collate")
			if joinRan != tc.wantJoinRan {
				t.Fatalf("collate (join) ran = %v, want %v", joinRan, tc.wantJoinRan)
			}
			completeness := parallelCompleteness(t, runsDir, runID)
			if len(completeness) != 3 {
				t.Fatalf("completeness = %+v, want 3 entries", completeness)
			}
			if completeness[0].Name != "a" || completeness[0].Status != journal.BranchFailed {
				t.Fatalf("lens-a = %+v, want failed", completeness[0])
			}
			if completeness[1].Name != "b" || completeness[1].Status != tc.wantSiblings {
				t.Fatalf("lens-b = %+v, want %q", completeness[1], tc.wantSiblings)
			}
		})
	}
}

// §9 fixture 3a: a branch exceeding branchTimeoutSeconds (see
// parallel_test.go's TestSequential/ConcurrentBranchExceedingTimeoutIsRecordedTimedOut
// for the dedicated width-1/width>1 pair this fixture's normative-sequence
// assertion complements).
func TestConformanceBranchExceedingTimeoutIsRecordedTimedOut(t *testing.T) {
	det := &slowThenFastDeterministic{slowFirstTask: "slow-a", delay: 1200 * time.Millisecond}
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	const runID = "conformance-branch-timeout"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: branchTimeoutMachine(t, 1, apiv1.BranchContinueOnError, ""),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if completeness[0].Name != "slow" || completeness[0].Status != journal.BranchTimedOut {
		t.Fatalf("slow branch = %+v, want timed-out", completeness[0])
	}
	branches := conformanceBranches(t, runsDir, runID)
	if seq := branches[1]; len(seq) == 0 || seq[len(seq)-1].Type != journal.EventBranchFinished ||
		seq[len(seq)-1].BranchStatus != journal.BranchTimedOut {
		t.Fatalf("slow branch's normative sequence ends %+v, want a branch.finished timed-out entry", seq)
	}
}

// §9 fixture 3b: a branch whose stage RETRIES push it over the budget — the
// budget is exceeded by cumulative attempts, not by any single attempt (or
// any single stage's own TimeoutSeconds) alone.
func TestConformanceBranchRetriesPushOverTimeoutBudget(t *testing.T) {
	det := &retryUntilBudgetExceededDeterministic{
		retryTask: "slow-a", attemptDelay: 400 * time.Millisecond, failAttempts: 2,
	}
	r, runsDir := newParallelTestRunner(t,
		func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
	)
	machine := branchTimeoutMachine(t, 1, apiv1.BranchContinueOnError, "")
	def := machine.Def
	def.Spec.Tasks[0].Retry = &apiv1.RetryPolicy{MaxAttempts: 3, BackoffSeconds: 0}
	compiled, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile retry-timeout fixture: %v", err)
	}
	const runID = "conformance-retry-timeout"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: compiled,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if completeness[0].Name != "slow" || completeness[0].Status != journal.BranchTimedOut {
		t.Fatalf("slow branch = %+v, want timed-out once cumulative retries exceeded the budget", completeness[0])
	}
}

// §9 fixture 4: a branch stage returning no-work asserts the BRANCH
// completes and the RUN does NOT (§5.3) — the one place this design changes
// an existing status's meaning (outside a parallel, no-work ends the run).
func TestConformanceNoWorkBranchCompletesWithoutEndingRun(t *testing.T) {
	byTask := map[string]stubTaskResult{
		"conformance-no-work:lens-a":  {status: apiv1.ResultNoWork},
		"conformance-no-work:lens-b":  {status: apiv1.ResultSuccess},
		"conformance-no-work:lens-c":  {status: apiv1.ResultSuccess},
		"conformance-no-work:collate": {status: apiv1.ResultSuccess},
	}
	r, runsDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
	)
	const runID = "conformance-no-work"
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo",
		Machine: parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch),
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — a branch's no-work must not end the run", result.Phase)
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if completeness[0].Name != "a" || completeness[0].Status != journal.BranchNoOutput {
		t.Fatalf("lens-a = %+v, want no-output", completeness[0])
	}
	if !stageRan(t, runsDir, runID, "collate") {
		t.Fatal("collate (join) did not run after a branch-scoped no-work")
	}
}

// §9 fixture 5: a branch containing a gate that routes to @abort asserts the
// whole run aborts and sibling branches are recorded cancelled.
func TestConformanceAbortGateInBranchAbortsRunAndCancelsSiblings(t *testing.T) {
	const runID = "conformance-abort-gate"
	byTask := map[string]stubTaskResult{
		runID + ":lens-a":  {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "lens_failed", Message: "lens failed"}},
		runID + ":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 1}},
		runID + ":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
		runID + ":collate": {status: apiv1.ResultSuccess},
	}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		ScratchDir:   t.TempDir(),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// width 1 (sequential): lens-a fails and its gate routes to @abort before
	// lens-b/lens-c ever start, so they are cancelled rather than run —
	// exactly the "siblings recorded cancelled" the fixture asks for, without
	// needing to race a concurrent branch's completion against the abort.
	machine := parallelAbortGateMachine(t)
	if width := machine.Def.Spec.Parallels[0].MaxConcurrentBranches; width > 1 {
		t.Fatalf("parallelAbortGateMachine width = %d, want <= 1 for this fixture", width)
	}
	result, err := r.Start(context.Background(), StartInput{
		RunID: runID, Gaggle: "demo", Machine: machine,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", result.Phase)
	}
	completeness := parallelCompleteness(t, runsDir, runID)
	if len(completeness) != 3 {
		t.Fatalf("completeness = %+v, want 3 entries", completeness)
	}
	if completeness[0].Name != "a" || completeness[0].Status != journal.BranchFailed {
		t.Fatalf("lens-a = %+v, want failed (its gate routed to @abort)", completeness[0])
	}
	for _, sibling := range completeness[1:] {
		if sibling.Status != journal.BranchCancelled {
			t.Errorf("sibling %+v, want cancelled once the run aborted", sibling)
		}
	}
}

// §9 fixture 6: a run crashing and recovering mid-parallel with branches at
// different depths asserts the resumed run reaches the same terminal and the
// same completeness record as an uninterrupted one. "Crash" is simulated the
// same way internal/runner's own resume tests do (e.g.
// resume_from_terminal_test.go): hand-author the journal a real crash would
// leave on disk, then Resume — rather than racing a live goroutine.
func TestConformanceResumeMidParallelReachesSameTerminalAsUninterrupted(t *testing.T) {
	byTask := map[string]stubTaskResult{
		":lens-a":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 0}},
		":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 1}},
		":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
		":collate": {status: apiv1.ResultSuccess},
	}
	byTaskFor := func(runID string) map[string]stubTaskResult {
		out := make(map[string]stubTaskResult, len(byTask))
		for suffix, res := range byTask {
			out[runID+suffix] = res
		}
		return out
	}

	// Uninterrupted baseline: the same workflow run start to finish in one go.
	const baselineID = "conformance-resume-baseline"
	baselineRunner, baselineDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTaskFor(baselineID)}, nil
		},
	)
	machine := parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch)
	baseline, err := baselineRunner.Start(context.Background(), StartInput{
		RunID: baselineID, Gaggle: "demo", Machine: machine,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil {
		t.Fatalf("baseline Start: %v", err)
	}
	if baseline.Phase != journal.PhaseCompleted {
		t.Fatalf("baseline phase = %q, want completed", baseline.Phase)
	}
	baselineCompleteness := parallelCompleteness(t, baselineDir, baselineID)

	// Interrupted run: hand-author a journal that stops mid-parallel — branch
	// 1 (lens-a) fully finished and settled, branch 2 (lens-b) STARTED but
	// never finished (simulating a crash mid-attempt), branch 3 (lens-c)
	// never started at all. Different depths per branch, as the fixture asks.
	const resumeID = "conformance-resume-interrupted"
	resumeRunner, resumeDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTaskFor(resumeID)}, nil
		},
	)
	jr, err := journal.Create(resumeDir, journal.RunIdentity{
		RunID: resumeID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
		{Branch: 1, Name: "a"}, {Branch: 2, Name: "b"}, {Branch: 3, Name: "c"},
	}}); err != nil {
		t.Fatalf("append parallel.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventBranchStarted, Branch: 1, Parallel: "fan", BranchName: "a", Stage: "lens-a"}); err != nil {
		t.Fatalf("append branch 1 started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "lens-a", Branch: 1, Attempt: 1}); err != nil {
		t.Fatalf("append lens-a started: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "lens-a", Branch: 1, Attempt: 1,
		Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"findings": 0},
	}); err != nil {
		t.Fatalf("append lens-a finished: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventBranchFinished, Branch: 1, Parallel: "fan", BranchName: "a", BranchStatus: journal.BranchSucceeded}); err != nil {
		t.Fatalf("append branch 1 finished: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventBranchStarted, Branch: 2, Parallel: "fan", BranchName: "b", Stage: "lens-b"}); err != nil {
		t.Fatalf("append branch 2 started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "lens-b", Branch: 2, Attempt: 1}); err != nil {
		t.Fatalf("append lens-b started: %v", err)
	}
	// lens-b's OWN attempt finished successfully, but the process died before
	// the branch-settle bookkeeping (branch.finished) ran — a shallower depth
	// than branch 1 (fully settled) and a deeper one than branch 3 (never
	// started), without needing a retry budget to resume through.
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "lens-b", Branch: 2, Attempt: 1,
		Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"findings": 1},
	}); err != nil {
		t.Fatalf("append lens-b finished: %v", err)
	}
	// No branch.finished/run.finished at all: a real crash just stops
	// mid-stream — Recover reopens exactly this shape.
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resumed, err := resumeRunner.Resume(context.Background(), ResumeInput{RunID: resumeID, Machine: machine})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Phase != baseline.Phase {
		t.Fatalf("resumed phase = %q, want the same terminal as the uninterrupted run (%q)", resumed.Phase, baseline.Phase)
	}
	resumedCompleteness := parallelCompleteness(t, resumeDir, resumeID)
	if len(resumedCompleteness) != len(baselineCompleteness) {
		t.Fatalf("resumed completeness = %+v, want the same shape as the baseline %+v", resumedCompleteness, baselineCompleteness)
	}
	for i := range baselineCompleteness {
		if resumedCompleteness[i].Name != baselineCompleteness[i].Name || resumedCompleteness[i].Status != baselineCompleteness[i].Status {
			t.Errorf("resumed branch %d = %+v, want %+v", i, resumedCompleteness[i], baselineCompleteness[i])
		}
	}
}

// countingStubDeterministic is stubDeterministic plus a per-task call
// counter, for tests that must assert a specific task was never (re)dispatched.
type countingStubDeterministic struct {
	stubDeterministic
	mu    sync.Mutex
	calls map[string]int
}

func (s *countingStubDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, det apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[env.TaskID]++
	s.mu.Unlock()
	return s.stubDeterministic.Run(ctx, env, det)
}

func (s *countingStubDeterministic) callsFor(taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[taskID]
}

// TestParallelResumeRefusesToRedispatchBranchAttemptThatAlreadyMutated is
// #3637's parallel-branch acceptance scenario: parallel_run.go's own resume
// path (independent of the sequential walk's stepTask) must apply the same
// guard. A branch task's mutation (e.g. a PR create) can succeed and be
// journaled as ref.touched before the crash cuts off that attempt's own
// stage.finished write. Resuming must not blindly continue the branch at
// attempt+1 — that would redispatch the executor and duplicate the
// already-succeeded mutation.
func TestParallelResumeRefusesToRedispatchBranchAttemptThatAlreadyMutated(t *testing.T) {
	machine := parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch)
	const runID = "conformance-resume-mutated"
	det := &countingStubDeterministic{stubDeterministic: stubDeterministic{byTask: map[string]stubTaskResult{
		runID + ":lens-a":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 0}},
		runID + ":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 1}},
		runID + ":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
		runID + ":collate": {status: apiv1.ResultSuccess},
	}}}
	resumeRunner, resumeDir := newParallelTestRunner(t,
		func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			det.rec = rec
			return det, nil
		},
	)
	jr, err := journal.Create(resumeDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
		{Branch: 1, Name: "a"}, {Branch: 2, Name: "b"}, {Branch: 3, Name: "c"},
	}}); err != nil {
		t.Fatalf("append parallel.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventBranchStarted, Branch: 1, Parallel: "fan", BranchName: "a", Stage: "lens-a"}); err != nil {
		t.Fatalf("append branch 1 started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "lens-a", Branch: 1, Attempt: 1}); err != nil {
		t.Fatalf("append lens-a started: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "lens-a", Branch: 1, Attempt: 1,
		Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"findings": 0},
	}); err != nil {
		t.Fatalf("append lens-a finished: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventBranchFinished, Branch: 1, Parallel: "fan", BranchName: "a", BranchStatus: journal.BranchSucceeded}); err != nil {
		t.Fatalf("append branch 1 finished: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventBranchStarted, Branch: 2, Parallel: "fan", BranchName: "b", Stage: "lens-b"}); err != nil {
		t.Fatalf("append branch 2 started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "lens-b", Branch: 2, Attempt: 1}); err != nil {
		t.Fatalf("append lens-b started: %v", err)
	}
	// The mutation itself (e.g. a PR create) already succeeded and was
	// journaled before the crash cut off this attempt's stage.finished write.
	if err := jr.Append(journal.Event{
		Type: journal.EventRefTouched, Stage: "lens-b", Branch: 2, Attempt: 1,
		ExternalRef: &journal.ExternalRef{Provider: string(apiv1.ProviderGitHub), Kind: "pr", ID: "42"},
	}); err != nil {
		t.Fatalf("append lens-b ref.touched: %v", err)
	}
	// No stage.finished, no branch.finished/run.finished: a real crash just
	// stops mid-stream, right after the mutation but before it could be
	// recorded as this attempt's own outcome.
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := resumeRunner.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine}); err == nil {
		t.Fatal("Resume: want an error — lens-b's interrupted attempt already touched an external mutation and must not be redispatched")
	}
	if calls := det.callsFor(runID + ":lens-b"); calls != 0 {
		t.Fatalf("lens-b executor called %d times, want 0 — redispatching would duplicate the already-succeeded mutation", calls)
	}
}

// §9 fixture 7: a continue_on_error parallel executed at maxConcurrentBranches
// 1 vs > 1 must produce the same per-branch NormativeEvent projection — with
// one branch FAILING, since that is where width-dependent divergence would
// actually show up (a fail-fast/all-or-nothing comparison is deliberately
// excluded per the design's own §9 note: those policies are expected to
// differ by width, since fail_fast's eager cancellation depends on timing).
func TestConformanceContinueOnErrorWidthInvariance(t *testing.T) {
	byTask := func(runID string) map[string]stubTaskResult {
		return map[string]stubTaskResult{
			runID + ":lens-a":  {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "lens_failed", Message: "lens failed"}},
			runID + ":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 1}},
			runID + ":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]any{"findings": 2}},
			runID + ":collate": {status: apiv1.ResultSuccess},
		}
	}
	results := map[int32][]journal.NormativeEvent{}
	dirs := map[int32]string{}
	for _, width := range []int32{1, 2} {
		runID := "conformance-width-invariance"
		r, runsDir := newParallelTestRunner(t,
			func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
				return &stubDeterministic{rec: rec, byTask: byTask(runID)}, nil
			},
		)
		result, err := r.Start(context.Background(), StartInput{
			RunID: runID, Gaggle: "demo",
			Machine: parallelRunnerMachine(t, width, apiv1.WorkspaceScratch),
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
		})
		if err != nil {
			t.Fatalf("width %d: Start: %v", width, err)
		}
		if result.Phase != journal.PhaseCompleted {
			t.Fatalf("width %d: phase = %q, want completed (continue_on_error)", width, result.Phase)
		}
		dirs[width] = runsDir
		rd, err := journal.OpenRead(runsDir + "/" + runID)
		if err != nil {
			t.Fatalf("width %d: open journal: %v", width, err)
		}
		events, err := rd.Events()
		if err != nil {
			t.Fatalf("width %d: read journal: %v", width, err)
		}
		branches := journal.ConformanceBranches(events)
		var flattened []journal.NormativeEvent
		for _, branchID := range []int{1, 2, 3} {
			flattened = append(flattened, branches[branchID]...)
		}
		results[width] = flattened
	}
	seq1, seq2 := results[1], results[2]
	if len(seq1) != len(seq2) {
		t.Fatalf("normative event count differs by width: width=1 has %d, width>1 has %d", len(seq1), len(seq2))
	}
	for i := range seq1 {
		if seq1[i] != seq2[i] {
			t.Errorf("event %d differs by width: width=1 %+v, width>1 %+v", i, seq1[i], seq2[i])
		}
	}
}

// --- shared conformance-corpus fixtures and helpers ---

func conformanceBranches(t *testing.T, runsDir, runID string) map[int][]journal.NormativeEvent {
	t.Helper()
	rd, err := journal.OpenRead(runsDir + "/" + runID)
	if err != nil {
		t.Fatalf("open %s journal: %v", runID, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read %s journal: %v", runID, err)
	}
	return journal.ConformanceBranches(events)
}

func stageRan(t *testing.T, runsDir, runID, stage string) bool {
	t.Helper()
	rd, err := journal.OpenRead(runsDir + "/" + runID)
	if err != nil {
		t.Fatalf("open %s journal: %v", runID, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read %s journal: %v", runID, err)
	}
	for _, e := range events {
		if e.Type == journal.EventStageStarted && e.Stage == stage {
			return true
		}
	}
	return false
}

// parallelPolicyMachine declares the same 3-branch shape as
// parallelRunnerMachine at the given failure policy, with an onFailure target
// for the two policies that need one.
func parallelPolicyMachine(t *testing.T, policy apiv1.BranchFailurePolicy) *workflow.Machine {
	t.Helper()
	base := parallelRunnerMachine(t, 1, apiv1.WorkspaceScratch)
	def := base.Def
	def.Spec.Parallels[0].FailurePolicy = policy
	if policy != apiv1.BranchContinueOnError {
		def.Spec.Parallels[0].OnFailure = workflow.TargetAbort
	}
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile %s policy fixture: %v", policy, err)
	}
	return machine
}

// parallelAbortGateMachine gives lens-a a gate that routes to @abort on
// failure, instead of lens-a itself being the terminal task.
func parallelAbortGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	base := parallelPolicyMachine(t, apiv1.BranchFailFast)
	def := base.Def
	def.Spec.Tasks[0].Next = "verdict-a" // lens-a
	def.Spec.Gates = append(def.Spec.Gates, apiv1.Gate{
		Name:      "verdict-a",
		Evaluator: apiv1.EvaluatorAutomated,
		// "status-equals" defaults its equals param to "success", so lens-a's
		// ResultFailure status routes to the fail branch with no params.
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  map[string]string{"pass": workflow.TargetJoin, "fail": workflow.TargetAbort},
	})
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile abort-gate fixture: %v", err)
	}
	return machine
}

// retryUntilBudgetExceededDeterministic fails retryTask failAttempts times
// (each attempt taking attemptDelay), then succeeds — so the branch exceeds
// its timeout budget through cumulative retry attempts rather than through
// any single attempt or the stage's own TimeoutSeconds.
type retryUntilBudgetExceededDeterministic struct {
	retryTask    string
	attemptDelay time.Duration
	failAttempts int
	calls        int
}

func (d *retryUntilBudgetExceededDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if !strings.HasSuffix(env.TaskID, ":"+d.retryTask) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	d.calls++
	time.Sleep(d.attemptDelay) // Intentional attempt delay makes parallel retry ordering observable.
	if d.calls <= d.failAttempts {
		// A dispatch error (not a ResultFailure status) is what Task.Retry
		// governs — an executor-level failure, retried up to MaxAttempts;
		// a returned ResultFailure status is a task-level business outcome
		// that flows to gate routing instead and is never auto-retried here.
		return apiv1.ResultEnvelope{}, fmt.Errorf("transient executor failure")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
