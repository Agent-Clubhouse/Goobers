package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/temporaltest"
)

// fakeStageDispatcher is a scripted StageDispatcher: it records every
// Dispatch call and answers with the scripted report/error.
type fakeStageDispatcher struct {
	mu       sync.Mutex
	attempts []dispatcher.Attempt
	eligible [][]dispatcher.RunnerSpec
	report   dispatcher.Report
	err      error
	calls    atomic.Int64
}

func (f *fakeStageDispatcher) Dispatch(_ context.Context, attempt dispatcher.Attempt, eligible []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, attempt)
	f.eligible = append(f.eligible, append([]dispatcher.RunnerSpec(nil), eligible...))
	return f.report, f.err
}

func (f *fakeStageDispatcher) recorded() ([]dispatcher.Attempt, [][]dispatcher.RunnerSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dispatcher.Attempt(nil), f.attempts...), append([][]dispatcher.RunnerSpec(nil), f.eligible...)
}

func surrenderStore(t *testing.T) *dispatcher.SurrenderDir {
	t.Helper()
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return plane
}

// putSurrendered stores a surrendered result exactly as the in-pod runtime
// would: marshalled and Put under the attempt identity, through the same
// production SurrenderDir the worker wires.
func putSurrendered(t *testing.T, plane *dispatcher.SurrenderDir, runID, stage string, attempt int, result dispatcher.SurrenderedResult) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.Put(context.Background(), runID, stage, attempt, data); err != nil {
		t.Fatal(err)
	}
}

func remoteEligible() []dispatcher.RunnerSpec {
	return []dispatcher.RunnerSpec{{
		Name: "win-ci", OS: "windows", HostKind: "image", Host: "ghcr.io/example/win:v1",
		Memory: "16Gi", Restrictions: []string{"tmp:ephemeral"},
	}}
}

func dispatchInput(runID, stage string, attempt int) DispatchStageInput {
	return DispatchStageInput{
		Envelope: apiv1.InvocationEnvelope{
			TaskID:     runID + ":" + stage,
			WorkflowID: "wf",
			RunID:      runID,
			Gaggle:     "web",
			Attempt:    int32(attempt),
			Limits:     apiv1.Limits{MaxDurationSeconds: 90},
		},
		Placement: PinnedPlacement{
			Stage: stage, Queue: dispatcher.QueueName("web", "win-ci"),
			Eligible:       remoteEligible(),
			LedgerTouching: true,
			CPU:            "2", Memory: "4Gi", Disk: "10Gi",
			Restrictions: []string{"tmp:ephemeral"},
		},
	}
}

// The surrendered blob marshals back into the exact stageActivityResult the
// in-process activities produce: envelope, mutation facts, and mutation
// issues — the #3588 parity acceptance.
func TestDispatchStageMarshalsSurrenderedResult(t *testing.T) {
	store := surrenderStore(t)
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Pod: "goobers-run-1", Phase: corev1.PodSucceeded,
		SurrenderConfirmed: true, Disposed: true,
	}}
	putSurrendered(t, store, "run-1", "build", 2, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Summary: "built remotely",
			Outputs: map[string]interface{}{"artifact": "bin/app.exe"},
		},
		Mutations:      []dispatcher.SurrenderedMutation{{Provider: "github", Kind: "pr", ID: "7", Operation: "open"}},
		MutationIssues: []string{"line 3: provider, kind, and id are required"},
	})

	a := &Activities{Dispatcher: fake, Surrenders: store}
	result, err := a.DispatchStage(context.Background(), dispatchInput("run-1", "build", 2))
	if err != nil {
		t.Fatalf("DispatchStage error: %v", err)
	}
	if result.Status != apiv1.ResultSuccess || result.Summary != "built remotely" {
		t.Fatalf("result envelope = %+v, want the surrendered envelope", result.ResultEnvelope)
	}
	if got := result.Outputs["artifact"]; got != "bin/app.exe" {
		t.Fatalf("outputs = %v, want the surrendered outputs", result.Outputs)
	}
	if len(result.Mutations) != 1 || result.Mutations[0] != (mutationFact{Provider: "github", Kind: "pr", ID: "7", Operation: "open"}) {
		t.Fatalf("mutations = %+v, want the surrendered mutation fact converted field for field", result.Mutations)
	}
	if len(result.MutationIssues) != 1 {
		t.Fatalf("mutation issues = %v, want carried through", result.MutationIssues)
	}

	attempts, eligible := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(attempts))
	}
	got := attempts[0]
	want := dispatcher.Attempt{
		RunID: "run-1", Gaggle: "web", Workflow: "wf", Stage: "build", Number: 2,
		LedgerTouching: true, CPU: "2", Memory: "4Gi", Disk: "10Gi",
		Restrictions: []string{"tmp:ephemeral"},
		Timeout:      90 * time.Second,
	}
	if got.RunID != want.RunID || got.Stage != want.Stage || got.Number != want.Number ||
		got.Gaggle != want.Gaggle || got.Workflow != want.Workflow ||
		got.LedgerTouching != want.LedgerTouching || got.CPU != want.CPU ||
		got.Memory != want.Memory || got.Disk != want.Disk || got.Timeout != want.Timeout {
		t.Fatalf("attempt = %+v, want %+v (built purely from activity input)", got, want)
	}
	if len(eligible[0]) != 1 || eligible[0][0].Name != "win-ci" {
		t.Fatalf("eligible = %+v, want the pinned set passed through untouched", eligible[0])
	}
}

// ErrStageFailed arrives with surrender confirmed, so the pod's own
// ResultFailure envelope is the business outcome — envelope, not error —
// with parity to the local executor's failure handling.
func TestDispatchStageStageFailedReturnsSurrenderedEnvelope(t *testing.T) {
	store := surrenderStore(t)
	fake := &fakeStageDispatcher{
		report: dispatcher.Report{Runner: "win-ci", Pod: "p", Phase: corev1.PodFailed, SurrenderConfirmed: true, Disposed: true},
		err:    fmt.Errorf("%w (run run-1 stage build attempt 1, pod p)", dispatcher.ErrStageFailed),
	}
	putSurrendered(t, store, "run-1", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{
			Status: apiv1.ResultFailure,
			Error:  &apiv1.ErrorInfo{Code: "tests_failed", Message: "3 tests failed"},
		},
	})
	a := &Activities{Dispatcher: fake, Surrenders: store}
	result, err := a.DispatchStage(context.Background(), dispatchInput("run-1", "build", 1))
	if err != nil {
		t.Fatalf("a stage-failed pod with a surrendered envelope is a business outcome, got error: %v", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "tests_failed" {
		t.Fatalf("result = %+v, want the surrendered failure envelope", result.ResultEnvelope)
	}
}

// Defense-in-depth for #3588: a dispatcher error that is NOT ErrStageFailed
// but arrives with surrender CONFIRMED — e.g. a post-surrender dispose fault —
// must still project the pod's authoritative surrendered envelope, never be
// bypassed into an infra retry that discards the result and re-dispatches an
// already-settled (possibly MUTATING) stage. Keyed off report.SurrenderConfirmed.
func TestDispatchStageSurrenderConfirmedErrorReadsEnvelope(t *testing.T) {
	store := surrenderStore(t)
	fake := &fakeStageDispatcher{
		report: dispatcher.Report{
			Runner: "win-ci", Pod: "p", Phase: corev1.PodSucceeded,
			SurrenderConfirmed: true, Disposed: false,
			DisposeErr: errors.New("dispatcher: dispose pod ns/p: apiserver conflict"),
		},
		// A generic (non-typed, non-ErrStageFailed) dispatcher error that would
		// classify as infra if it bypassed the envelope.
		err: errors.New("dispatcher: dispose pod ns/p: apiserver conflict"),
	}
	putSurrendered(t, store, "run-d", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "surrendered before the dispose fault"},
	})
	a := &Activities{Dispatcher: fake, Surrenders: store}
	result, err := a.DispatchStage(context.Background(), dispatchInput("run-d", "build", 1))
	if err != nil {
		t.Fatalf("a confirmed surrender must project the envelope despite a dispose fault, got error: %v", err)
	}
	if result.Status != apiv1.ResultSuccess || result.Summary != "surrendered before the dispose fault" {
		t.Fatalf("result = %+v, want the surrendered envelope", result.ResultEnvelope)
	}
}

// Dispatch-plane failures classify per the design of record: deterministic
// refusals are policy-classed (identical redispatch reproduces them), and
// everything else — capacity, pod loss, unconfirmed surrender — is an infra
// attempt retried on a fresh pod (architecture §5 item 8).
func TestDispatchStageErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"surrender unconfirmed is infra", fmt.Errorf("%w (run r stage s attempt 1, pod p)", dispatcher.ErrSurrenderUnconfirmed), FailureTypeInfrastructure},
		{"capacity timeout is infra", &dispatcher.CapacityTimeoutError{Runner: "win-ci", OS: "windows", Waited: time.Minute, Bound: time.Minute}, FailureTypeInfrastructure},
		{"pod create failure is infra", errors.New("dispatcher: create pod ns/p: connection refused"), FailureTypeInfrastructure},
		{"selection refusal is policy", &dispatcher.SelectionError{Diagnostic: "eligible set empty"}, FailureTypeStage},
		{"skew refusal is policy", &dispatcher.SkewError{Image: "img:aaa", Reason: "tag mismatch"}, FailureTypeStage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Activities{
				Dispatcher: &fakeStageDispatcher{err: tc.err},
				Surrenders: surrenderStore(t),
			}
			_, err := a.DispatchStage(context.Background(), dispatchInput("run-c", "build", 1))
			if err == nil {
				t.Fatal("want a classified error")
			}
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) {
				t.Fatalf("error %v is not a typed application error", err)
			}
			if appErr.Type() != tc.want {
				t.Fatalf("failure type = %q, want %q", appErr.Type(), tc.want)
			}
		})
	}
}

// A confirmed surrender whose result blob is unreadable is a data-plane
// fault: infra-classed so the attempt retries on a fresh pod, never burning
// the policy budget.
func TestDispatchStageMissingSurrenderedResultIsInfra(t *testing.T) {
	a := &Activities{
		Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}},
		Surrenders: surrenderStore(t),
	}
	_, err := a.DispatchStage(context.Background(), dispatchInput("run-m", "build", 1))
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != FailureTypeInfrastructure {
		t.Fatalf("error = %v, want an infra-classed application error", err)
	}
}

func TestDispatchStageFailsClosed(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		a := &Activities{}
		_, err := a.DispatchStage(context.Background(), dispatchInput("run-n", "build", 1))
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("error = %v, want the not-configured refusal", err)
		}
	})
	t.Run("self placement refused", func(t *testing.T) {
		a := &Activities{Dispatcher: &fakeStageDispatcher{}, Surrenders: surrenderStore(t)}
		input := dispatchInput("run-s", "build", 1)
		input.Placement.Self = true
		_, err := a.DispatchStage(context.Background(), input)
		if err == nil || !strings.Contains(err.Error(), "local path") {
			t.Fatalf("error = %v, want the self-placement refusal", err)
		}
	})
	t.Run("local report refused", func(t *testing.T) {
		a := &Activities{
			Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "self", Local: true}},
			Surrenders: surrenderStore(t),
		}
		_, err := a.DispatchStage(context.Background(), dispatchInput("run-l", "build", 1))
		if err == nil || !strings.Contains(err.Error(), "disagree") {
			t.Fatalf("error = %v, want the pin/selection disagreement refusal", err)
		}
	})
	t.Run("statusless surrender refused", func(t *testing.T) {
		store := surrenderStore(t)
		putSurrendered(t, store, "run-e", "build", 1, dispatcher.SurrenderedResult{})
		a := &Activities{
			Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}},
			Surrenders: store,
		}
		_, err := a.DispatchStage(context.Background(), dispatchInput("run-e", "build", 1))
		if err == nil || !strings.Contains(err.Error(), "no status") {
			t.Fatalf("error = %v, want the partial-envelope refusal", err)
		}
	})
}

// #3699: the pod actually needs to know what to run. DispatchStageInput
// carries the pinned DeterministicRun through to the activity, and
// DispatchStage lands it on the dispatcher.Attempt the pod spec is rendered
// from.
func TestDispatchStagePopulatesAttemptFromRun(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "run-r", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true,
	}}
	a := &Activities{Dispatcher: fake, Surrenders: store}

	input := dispatchInput("run-r", "build", 1)
	input.Run = &apiv1.DeterministicRun{
		Script:    "curl -sf https://example.invalid",
		Env:       map[string]string{"GOOBERS_PROBE_TARGET": "8.8.8.8"},
		Workspace: apiv1.WorkspaceScratch,
	}
	if _, err := a.DispatchStage(context.Background(), input); err != nil {
		t.Fatalf("DispatchStage error: %v", err)
	}
	attempts, _ := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(attempts))
	}
	got := attempts[0]
	if got.Script != "curl -sf https://example.invalid" {
		t.Fatalf("attempt.Script = %q, want the pinned Run's script", got.Script)
	}
	if got.Env["GOOBERS_PROBE_TARGET"] != "8.8.8.8" {
		t.Fatalf("attempt.Env = %+v, want the pinned Run's env", got.Env)
	}
	if len(got.Command) != 0 {
		t.Fatalf("attempt.Command = %v, want empty (Script was declared, not Command)", got.Command)
	}
}

// #3699 v1 scope: DispatchStage re-asserts the same guards
// dispatchRemoteTask applies before ever reaching the activity (belt and
// suspenders, matching this file's self-placement re-assertion) — a
// non-scratch workspace, declared capabilities, or a goobers-CLI/provider-
// builtin command must never reach the dispatcher, because the in-pod
// executor cannot honor any of them yet.
func TestDispatchStageRefusesV1UnsupportedRun(t *testing.T) {
	for name, mutate := range map[string]func(*DispatchStageInput){
		// repo and scratch are both provisioned in-pod now (pod-side checkout);
		// what remains refused is a mode this substrate has never had, because
		// running it as if it were scratch would hand the stage the wrong
		// workspace silently.
		"workspace mode the pod cannot provision": func(in *DispatchStageInput) {
			in.Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceMode("shared-nfs")}
		},
		// Narrow, measured replacement for the blanket goobers-CLI refusal:
		// telemetry-query reads the instance CONFIG DIRECTORY (the workflow
		// definitions), which a stage pod does not have. Every other CLI stage
		// this instance's workflows invoke reaches its repo and credential
		// through the environment and now dispatches.
		"goobers command reading the config dir": func(in *DispatchStageInput) {
			in.Run = &apiv1.DeterministicRun{Command: []string{"goobers", "telemetry-query"}, Workspace: apiv1.WorkspaceScratch}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
			a := &Activities{Dispatcher: fake, Surrenders: surrenderStore(t)}
			input := dispatchInput("run-u", "build", 1)
			mutate(&input)
			if _, err := a.DispatchStage(context.Background(), input); err == nil {
				t.Fatal("expected a v1-scope refusal, got none")
			}
			if fake.calls.Load() != 0 {
				t.Fatal("an unsupported Run reached the dispatcher instead of being refused first")
			}
		})
	}
}

// A mode-3 stage executes through the dispatch activity with everything —
// eligible set, attempt facts, queue — read from RunInput's pinned
// placements, while an unpinned stage in the same run keeps the local arm.
func TestModeThreeStageExecutesThroughDispatchActivity(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "build",
		Tasks: []apiv1.Task{
			{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
				Run:  &apiv1.DeterministicRun{Command: []string{"build.cmd"}, Workspace: apiv1.WorkspaceScratch},
				Next: "report"},
			{Name: "report", Type: apiv1.TaskDeterministic, Goal: "report",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}},
		},
	}
	in := runInput("mode-three", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "build", Queue: dispatcher.QueueName("web", "win-ci"),
		Eligible: remoteEligible(), Memory: "4Gi",
	}}

	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "surrendered over the blob plane"},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Pod: "pod-1", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true,
	}}
	var localStages []string
	var mu sync.Mutex
	det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		mu.Lock()
		defer mu.Unlock()
		_, stage, _ := strings.Cut(env.TaskID, ":")
		localStages = append(localStages, stage)
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if got := result.Outputs["build"].Summary; got != "surrendered over the blob plane" {
		t.Fatalf("build output = %+v, want the surrendered envelope in the run outputs", result.Outputs["build"])
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want exactly the pinned stage", fake.calls.Load())
	}
	attempts, eligible := fake.recorded()
	if attempts[0].Stage != "build" || len(eligible[0]) != 1 || eligible[0][0].Name != "win-ci" {
		t.Fatalf("dispatch got attempt %+v eligible %+v, want the pinned placement data", attempts[0], eligible[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(localStages) != 1 || localStages[0] != "report" {
		t.Fatalf("local stages = %v, want only the unpinned stage", localStages)
	}
}

// A repo-workspace stage now DISPATCHES: the pod provisions the checkout
// itself. This replaces the refusal that used to live here.
//
// It matters far beyond one test. Production workflows declare no `workspace:`
// at all, so every stage in them defaults to repo — meaning the old refusal
// blocked EVERY stage of every real workflow from mode 3, and only stages
// explicitly authored as scratch could run in a pod.
//
// The DSL validator rejects an unknown workspace mode before a workflow ever
// runs (`unknown workspace "x" (want repo, scratch, or repo-readonly)`), and
// all three of those are provisionable in-pod now — so the unsupported-mode
// guard that remains in dispatchRemoteTask is unreachable via valid DSL and
// exists only for a version-skewed or hand-built input.
func TestModeThreeDispatchesRepoWorkspaceStage(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "build",
		Tasks: []apiv1.Task{
			// No workspace declared: defaults to repo, exactly like every
			// production workflow stage.
			{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
				Run: &apiv1.DeterministicRun{Command: []string{"build.cmd"}}},
		},
	}
	in := runInput("mode-three-repo-ws", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "build", Queue: dispatcher.QueueName("web", "win-ci"),
		Eligible: remoteEligible(), Memory: "4Gi",
	}}
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenderStore(t)})
	env.ExecuteWorkflow(Run, in)

	if fake.calls.Load() == 0 {
		t.Fatal("a repo-workspace stage must now reach the dispatcher; the pod provisions its own checkout")
	}
	if len(fake.attempts) == 0 {
		t.Fatal("no attempt recorded")
	}
	// The pod cannot check anything out without being told which repository and
	// which workspace mode, so both must ride the attempt.
	got := fake.attempts[0]
	if got.Workspace != string(apiv1.WorkspaceRepo) && got.Workspace != "" {
		t.Fatalf("attempt workspace = %q, want the declared repo mode", got.Workspace)
	}
	if got.RunContext[executor.RepoOwnerEnvVar] == "" && got.RunContext[executor.RepoNameEnvVar] == "" {
		t.Fatalf("attempt carries no repository for the checkout: %v", got.RunContext)
	}
}

// Self placements — and absent placements — leave the existing arms
// untouched: the dispatcher seam is never consulted (zero-declaration
// invariance, architecture §11 item 1).
func TestSelfAndAbsentPlacementsKeepLocalArms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		placements []PinnedPlacement
	}{
		{"no placements", nil},
		{"self placement", []PinnedPlacement{{Stage: "build", Self: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle:   "web",
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
				Start:    "build",
				Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}}},
			}
			in := runInput("self-invariance", spec)
			in.Placements = tc.placements
			fake := &fakeStageDispatcher{}
			var localCalls atomic.Int64
			det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				localCalls.Add(1)
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			var ts testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&ts)
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenderStore(t)})
			env.ExecuteWorkflow(Run, in)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("workflow error: %v", err)
			}
			if localCalls.Load() != 1 {
				t.Fatalf("local executor calls = %d, want 1", localCalls.Load())
			}
			if fake.calls.Load() != 0 {
				t.Fatalf("dispatcher calls = %d, want the seam never consulted", fake.calls.Load())
			}
		})
	}
}

// remotePlacementFor is the whole of the workflow's routing decision: pure
// data from RunInput, no solve, no config, no I/O.
func TestRemotePlacementFor(t *testing.T) {
	in := RunInput{Placements: []PinnedPlacement{
		{Stage: "local", Self: true},
		{Stage: "win", Queue: "goobers-dispatch.web.win-ci"},
	}}
	if _, remote := remotePlacementFor(in, "local"); remote {
		t.Fatal("a self placement must keep the local arms")
	}
	if _, remote := remotePlacementFor(in, "unpinned"); remote {
		t.Fatal("an unpinned stage must keep the local arms")
	}
	placement, remote := remotePlacementFor(in, "win")
	if !remote || placement.Queue != "goobers-dispatch.web.win-ci" {
		t.Fatalf("placement = %+v remote = %v, want the pinned queue routed", placement, remote)
	}
}

// The end-to-end determinism and routing proof on a real Temporal server: the
// workflow queue's worker serves NO dispatcher seam, the dispatch queue's
// worker does — the run completes only if the pinned queue routed the
// activity — and the recorded history replays cleanly, which is the
// replay-determinism guarantee for the placement-from-RunInput design.
func TestModeThreeQueueRoutingAndHistoryReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})

	const workflowQueue = "mode-three-routing"
	dispatchQueue := dispatcher.QueueName("web", "win-ci")
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "build",
		Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
			Run: &apiv1.DeterministicRun{Command: []string{"build.cmd"}, Workspace: apiv1.WorkspaceScratch}}},
	}
	in := runInput("mode-three-routing", spec)
	in.Placements = []PinnedPlacement{{Stage: "build", Queue: dispatchQueue, Eligible: remoteEligible()}}

	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "routed"},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "win-ci", Pod: "pod-r", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true,
	}}

	temporalClient := server.Client()
	// The workflow queue's worker has NO dispatcher: if the dispatch activity
	// were mis-routed here it would fail "not configured" instead of
	// completing the run.
	workflowWorker := temporalworker.New(temporalClient, workflowQueue, temporalworker.Options{})
	RegisterWith(workflowWorker, &Activities{Workspaces: testWorkspaces(t)})
	if err := workflowWorker.Start(); err != nil {
		t.Fatalf("start workflow-queue worker: %v", err)
	}
	t.Cleanup(workflowWorker.Stop)
	dispatchWorker := temporalworker.New(temporalClient, dispatchQueue, temporalworker.Options{})
	RegisterWith(dispatchWorker, &Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	if err := dispatchWorker.Start(); err != nil {
		t.Fatalf("start dispatch-queue worker: %v", err)
	}
	t.Cleanup(dispatchWorker.Stop)

	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "mode-three-routing",
		TaskQueue: workflowQueue,
	}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.Status != StatusCompleted || result.Outputs["build"].Summary != "routed" {
		t.Fatalf("result = %+v, want the surrendered envelope via the dispatch queue", result)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want the dispatch queue's worker to have served the stage", fake.calls.Load())
	}

	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read workflow history: %v", err)
		}
		history.Events = append(history.Events, event)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay mode-3 workflow history (the placement-from-RunInput determinism guarantee): %v", err)
	}
}

// Declared capabilities are no longer a v1 refusal: the pod resolves them
// against the credential plane at stage start. What must hold is that the
// dispatcher receives the capability NAMES — the values are never in the
// dispatch payload.
func TestDispatchStagePassesCapabilityNamesToTheDispatcher(t *testing.T) {
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	a := &Activities{Dispatcher: fake, Surrenders: surrenderStore(t)}
	in := dispatchInput("run-1", "open-pr", 1)
	in.Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}
	in.Envelope.Capabilities = []string{"contents:write"}
	_, _ = a.DispatchStage(context.Background(), in)
	if len(fake.attempts) == 0 {
		t.Fatal("the dispatcher was never called — a declared capability must no longer be refused")
	}
	if got := fake.attempts[0].Capabilities; len(got) != 1 || got[0] != "contents:write" {
		t.Fatalf("dispatcher received Capabilities=%v, want [contents:write]", got)
	}
}

// A goobers-CLI stage is no longer refused: the pod resolves its own
// credentials (#3722), emits its own journal (#3723), and — below — receives
// the run context the CLI reads. This is the positive half of the refusal that
// used to live in TestDispatchStageRefusesV1UnsupportedRun.
func TestDispatchStageCLIStageCarriesRunContext(t *testing.T) {
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	store := surrenderStore(t)
	putSurrendered(t, store, "run-cli", "query-backlog", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	a := &Activities{Dispatcher: fake, Surrenders: store}
	input := dispatchInput("run-cli", "query-backlog", 1)
	input.Run = &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}, Workspace: apiv1.WorkspaceScratch}
	input.Envelope.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers"}
	input.Envelope.BranchNamespace = "goobernetes/"
	input.Envelope.BaseBranch = "main"

	if _, err := a.DispatchStage(context.Background(), input); err != nil {
		t.Fatalf("a goobers-CLI stage must dispatch: %v", err)
	}
	if len(fake.attempts) != 1 {
		t.Fatalf("expected one attempt, got %d", len(fake.attempts))
	}
	got := fake.attempts[0]
	if !got.CLIStage {
		t.Fatal("attempt must be marked a CLI stage, or the pod strips the run identity it needs")
	}
	for name, want := range map[string]string{
		executor.RepoProviderEnvVar:    "github",
		executor.RepoOwnerEnvVar:       "Agent-Clubhouse",
		executor.RepoNameEnvVar:        "Goobers",
		executor.BranchNamespaceEnvVar: "goobernetes/",
		executor.BaseBranchEnvVar:      "main",
	} {
		if got.RunContext[name] != want {
			t.Errorf("RunContext[%s] = %q, want %q", name, got.RunContext[name], want)
		}
	}
}

// The exemption is scoped to CLI stages. A stage running the project's own
// build must NOT carry the run's identity — in a self-hosting project that
// leaks the live run into its own test suite (#322).
func TestDispatchStageNonCLIStageCarriesNoRunContext(t *testing.T) {
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	store := surrenderStore(t)
	putSurrendered(t, store, "run-ci", "build", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	a := &Activities{Dispatcher: fake, Surrenders: store}
	input := dispatchInput("run-ci", "build", 1)
	input.Run = &apiv1.DeterministicRun{Command: []string{"make", "ci"}, Workspace: apiv1.WorkspaceScratch}
	input.Envelope.RepoRef = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers"}

	if _, err := a.DispatchStage(context.Background(), input); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got := fake.attempts[0]
	if got.CLIStage {
		t.Fatal("`make ci` is not a goobers-CLI stage")
	}
	if len(got.RunContext) != 0 {
		t.Fatalf("non-CLI stage must carry no run context, got %v", got.RunContext)
	}
}

// An AGENTIC stage pinned to a remote runner now DISPATCHES, and must carry the
// invocation the pod needs to execute it.
//
// This replaces the refusal that stood here. The refusal was correct while the
// pod had no way to invoke a goober; it now does, via a kit published to the
// blob plane and verified by digest on arrival.
//
// The two assertions below are the contract: without Agentic the dispatcher
// would not publish a kit, and without the Envelope the kit writer has nothing
// to resolve the goober from — either way the pod would start and find no
// instructions, which is the silent-wrong-result this design exists to avoid.
func TestModeThreeDispatchesAgenticStageWithItsInvocation(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "agentic-edit",
		Tasks: []apiv1.Task{
			{Name: "agentic-edit", Type: apiv1.TaskAgentic, Goal: "edit the repo", Goober: "implementer",
				Workspace: apiv1.WorkspaceRepoReadOnly},
		},
	}
	in := runInput("mode-three-agentic", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "agentic-edit", Queue: dispatcher.QueueName("web", "linux-agentic"),
		Eligible: remoteEligible(), Memory: "2Gi",
	}}
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenderStore(t)})
	env.ExecuteWorkflow(Run, in)

	if fake.calls.Load() == 0 {
		t.Fatal("an agentic stage must now reach the dispatcher")
	}
	if len(fake.attempts) == 0 {
		t.Fatal("no attempt recorded")
	}
	got := fake.attempts[0]
	// An agentic task declares its workspace on the TASK — it has no
	// DeterministicRun to carry one. Without this the pod stamps no workspace
	// mode, its checkout no-ops, and the agent runs against an empty directory
	// and truthfully reports the repo's files missing.
	if got.Workspace != string(apiv1.WorkspaceRepoReadOnly) {
		t.Fatalf("attempt workspace = %q, want the task-level %q", got.Workspace, apiv1.WorkspaceRepoReadOnly)
	}
	if !got.Agentic {
		t.Fatal("attempt is not marked agentic; the dispatcher would publish no kit and the pod would find no instructions")
	}
	if got.Envelope == nil {
		t.Fatal("attempt carries no envelope; the kit writer has nothing to resolve the goober from")
	}
	if got.Envelope.Goober == "" {
		t.Fatalf("envelope names no goober: %+v", got.Envelope)
	}
	// A repo-backed workspace is useless without the repository to check out.
	// Stamping the mode while withholding the identity is the failure this
	// guards: the pod refused with "repo workspace requested but the dispatcher
	// stamped no repository", because the stamp was gated on the stage having a
	// DeterministicRun and an agentic task has none.
	if got.RunContext[executor.RepoOwnerEnvVar] == "" || got.RunContext[executor.RepoNameEnvVar] == "" {
		t.Fatalf("agentic repo workspace carries no repository for the checkout: %v", got.RunContext)
	}
	// The repo facts must NOT make it look like a CLI stage: that flag controls
	// whether the run identity survives in the stage's own environment (#322).
	if got.CLIStage {
		t.Fatal("agentic stage marked as a goobers-CLI stage; run identity would leak into the stage environment")
	}
}

// The engine must hand what one pod committed to the next one (#3763). On the
// worker this needs no carrier: every attempt gets a worktree on the same run
// branch in the same mirror clone. A pod is disposed after surrender, so
// without this threading the second stage clones base and silently continues
// from it — MEASURED on run e1cfcfe2, where the pod arm's observe stage
// reported the PRE-COMMIT head and reported success.
func TestModeThreeThreadsWorkspaceDeltaToTheNextStage(t *testing.T) {
	const delta = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "commit",
		Tasks: []apiv1.Task{
			{Name: "commit", Type: apiv1.TaskDeterministic, Goal: "commit",
				Run: &apiv1.DeterministicRun{Command: []string{"commit.sh"}, Workspace: apiv1.WorkspaceRepo}, Next: "consume"},
			{Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume",
				Run: &apiv1.DeterministicRun{Command: []string{"consume.sh"}, Workspace: apiv1.WorkspaceRepo}},
		},
	}
	in := runInput("mode-three-delta", spec)
	in.Placements = []PinnedPlacement{
		{Stage: "commit", Queue: dispatcher.QueueName("web", "linux"), Eligible: remoteEligible(), Memory: "2Gi"},
		{Stage: "consume", Queue: dispatcher.QueueName("web", "linux"), Eligible: remoteEligible(), Memory: "2Gi"},
	}
	surrenders := surrenderStore(t)
	// The first stage surrenders a delta, exactly as a pod that committed does.
	putSurrendered(t, surrenders, in.RunID, "commit", 1, dispatcher.SurrenderedResult{
		Result:         apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		WorkspaceDelta: delta,
	})
	putSurrendered(t, surrenders, in.RunID, "consume", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)

	if len(fake.attempts) < 2 {
		t.Fatalf("expected both stages to dispatch, got %d attempt(s)", len(fake.attempts))
	}
	// The FIRST stage has nothing to continue from — a delta here would mean
	// the engine invented one.
	if got := fake.attempts[0].WorkspaceDelta; got != "" {
		t.Fatalf("first stage carried workspace delta %q; nothing precedes it", got)
	}
	// The SECOND stage must receive what the first surrendered.
	if got := fake.attempts[1].WorkspaceDelta; got != delta {
		t.Fatalf("second stage carried workspace delta %q, want %q — without it the pod clones base and silently drops the first stage's commits", got, delta)
	}
}

// A read-only or scratch stage must never be handed a delta: applying one would
// move a repo-readonly stage off the pinned base it exists to read, turning a
// research stage into a consumer of unreviewed work.
func TestModeThreeWithholdsWorkspaceDeltaFromReadOnlyStages(t *testing.T) {
	const delta = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "commit",
		Tasks: []apiv1.Task{
			{Name: "commit", Type: apiv1.TaskDeterministic, Goal: "commit",
				Run: &apiv1.DeterministicRun{Command: []string{"commit.sh"}, Workspace: apiv1.WorkspaceRepo}, Next: "read"},
			{Name: "read", Type: apiv1.TaskDeterministic, Goal: "read",
				Run: &apiv1.DeterministicRun{Command: []string{"read.sh"}, Workspace: apiv1.WorkspaceRepoReadOnly}},
		},
	}
	in := runInput("mode-three-delta-readonly", spec)
	in.Placements = []PinnedPlacement{
		{Stage: "commit", Queue: dispatcher.QueueName("web", "linux"), Eligible: remoteEligible(), Memory: "2Gi"},
		{Stage: "read", Queue: dispatcher.QueueName("web", "linux"), Eligible: remoteEligible(), Memory: "2Gi"},
	}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "commit", 1, dispatcher.SurrenderedResult{
		Result:         apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		WorkspaceDelta: delta,
	})
	putSurrendered(t, surrenders, in.RunID, "read", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)

	if len(fake.attempts) < 2 {
		t.Fatalf("expected both stages to dispatch, got %d attempt(s)", len(fake.attempts))
	}
	if got := fake.attempts[1].WorkspaceDelta; got != "" {
		t.Fatalf("a repo-readonly stage was handed workspace delta %q; it must read the pinned base", got)
	}
}

// A stage needing a repo workspace must get a credential to CLONE it without
// having to declare repository authority it does not otherwise need (#3770).
//
// MEASURED: open-pr declares provider:pr:write alone and could not run in a
// pod at all — "could not read Username for 'https://github.com'" — because the
// in-pod checkout authenticated with the stage's BUSINESS capabilities. The
// worker never had this problem: it provisions worktrees with instance
// credentials regardless of what the stage declares, so the same workflow
// worked on self and failed on a pod.
func TestModeThreeNamesACheckoutCapabilityForRepoWorkspaces(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "open-pr",
		Tasks: []apiv1.Task{
			// Exactly production open-pr's shape: provider authority, a repo
			// workspace, and no repo-shaped capability.
			{Name: "open-pr", Type: apiv1.TaskDeterministic, Goal: "open a pr",
				Capabilities:  []string{"provider:pr:write"},
				PolicyActions: []string{"open-or-update-pr"},
				Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "open-pr"}, Workspace: apiv1.WorkspaceRepo}},
		},
	}
	in := runInput("mode-three-checkout-cap", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "open-pr", Queue: dispatcher.QueueName("web", "linux"),
		Eligible: remoteEligible(), Memory: "2Gi",
	}}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "open-pr", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)

	if len(fake.attempts) == 0 {
		t.Fatalf("no attempt recorded; workflow error = %v", env.GetWorkflowError())
	}
	got := fake.attempts[0]
	if got.CheckoutCapability == "" {
		t.Fatal("no checkout capability named for a repo workspace; the pod would clone anonymously and fail on a private repository")
	}
	// It must NOT have been folded into the stage's own capabilities: those are
	// exported to the stage's environment as GOOBERS_CRED_*, so widening them
	// would hand a push token to a stage that never asked for one.
	for _, c := range got.Capabilities {
		if strings.Contains(strings.ToLower(c), "repo") {
			t.Fatalf("checkout capability leaked into the stage's declared capabilities (%v); those reach the stage environment", got.Capabilities)
		}
	}
}

// A stage that ALREADY declares repository access needs no second credential —
// the checkout uses the one it has.
func TestModeThreeSkipsCheckoutCapabilityWhenTheStageHasRepoAccess(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "push",
		Tasks: []apiv1.Task{
			{Name: "push", Type: apiv1.TaskDeterministic, Goal: "push",
				Capabilities: []string{"repo:push"},
				// A plain command: what is under test is that a declared repo
				// capability suppresses the separate checkout mint, not how a
				// goobers-CLI stage dispatches.
				Run: &apiv1.DeterministicRun{Command: []string{"build.sh"}, Workspace: apiv1.WorkspaceRepo}},
		},
	}
	in := runInput("mode-three-checkout-cap-skip", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "push", Queue: dispatcher.QueueName("web", "linux"),
		Eligible: remoteEligible(), Memory: "2Gi",
	}}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "push", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)

	if len(fake.attempts) == 0 {
		t.Fatal("no attempt recorded")
	}
	if got := fake.attempts[0].CheckoutCapability; got != "" {
		t.Fatalf("named checkout capability %q for a stage that already declares repo:push; the checkout uses the one it has", got)
	}
}

// A scratch workspace clones nothing and must never be handed a credential.
func TestModeThreeNamesNoCheckoutCapabilityForScratch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "probe",
		Tasks: []apiv1.Task{
			{Name: "probe", Type: apiv1.TaskDeterministic, Goal: "probe",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}},
		},
	}
	in := runInput("mode-three-checkout-cap-scratch", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "probe", Queue: dispatcher.QueueName("web", "linux"),
		Eligible: remoteEligible(), Memory: "1Gi",
	}}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "probe", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)

	if len(fake.attempts) == 0 {
		t.Fatal("no attempt recorded")
	}
	if got := fake.attempts[0].CheckoutCapability; got != "" {
		t.Fatalf("named checkout capability %q for a scratch workspace, which clones nothing", got)
	}
}

// TestStagePlacementAccompaniesSettledAttemptsOnly pins the contract
// StagePlacement's doc now states, because that doc is what the step-6 runner
// will code against and a comment cannot fail.
//
// Two halves, and the second is the one that was previously mis-documented:
//
//   - a SETTLED attempt (success, or ErrStageFailed with surrender confirmed)
//     carries provenance with ALL FIVE fields populated — so a caller may read
//     Pod/Image/PodStartedAt without a nil-ish branch;
//   - an attempt refused BEFORE its pod existed carries none at all, even
//     though the dispatcher's report already names the runner and the image.
//     DispatchStage discards that report. The evidence that survives is the
//     classified error's text, so the runner and the skew subject are asserted
//     there instead — that is what §11 acceptance 6 has to be journalled from
//     for a refused placement.
func TestStagePlacementAccompaniesSettledAttemptsOnly(t *testing.T) {
	settled := func(t *testing.T, dispatchErr error, phase corev1.PodPhase) *StagePlacement {
		t.Helper()
		store := surrenderStore(t)
		queuedAt := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
		fake := &fakeStageDispatcher{
			report: dispatcher.Report{
				Runner: "win-ci", Pod: "goobers-run-settled-build-1", Image: "ghcr.io/example/win:v1",
				Phase: phase, SurrenderConfirmed: true, Disposed: true,
				QueuedAt: queuedAt, PodStartedAt: queuedAt.Add(11 * time.Second),
			},
			err: dispatchErr,
		}
		putSurrendered(t, store, "run-settled", "build", 1, dispatcher.SurrenderedResult{
			Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "settled"},
		})
		a := &Activities{Dispatcher: fake, Surrenders: store}
		result, err := a.DispatchStage(context.Background(), dispatchInput("run-settled", "build", 1))
		if err != nil {
			t.Fatalf("DispatchStage on a settled attempt: %v", err)
		}
		if result.Placement == nil {
			t.Fatal("settled attempt returned no placement provenance")
		}
		return result.Placement
	}

	for _, tc := range []struct {
		name        string
		dispatchErr error
		phase       corev1.PodPhase
	}{
		{name: "success", phase: corev1.PodSucceeded},
		{name: "stage failed with surrender confirmed", phase: corev1.PodFailed,
			dispatchErr: fmt.Errorf("%w (run run-settled stage build attempt 1, pod p)", dispatcher.ErrStageFailed)},
	} {
		t.Run("settled: "+tc.name+" populates every field", func(t *testing.T) {
			got := settled(t, tc.dispatchErr, tc.phase)
			// Field by field rather than a DeepEqual against a want: the
			// claim under test is "no field is ever zero", and a struct
			// comparison would still pass if the contract later grew a
			// sixth field nobody populated.
			if got.Runner == "" || got.Pod == "" || got.Image == "" ||
				got.QueuedAt.IsZero() || got.PodStartedAt.IsZero() {
				t.Fatalf("placement = %+v; a settled attempt must populate runner, pod, image, queuedAt and podStartedAt — "+
					"StagePlacement's doc tells the driver it may read all five without a partial-block branch", got)
			}
		})
	}

	// The three refusals §11 acceptance 6 most wants journalled, each raised
	// where Dispatch raises it: before CreatePod, with a report that already
	// carries Runner (and, for skew, the image).
	for _, tc := range []struct {
		name        string
		err         error
		report      dispatcher.Report
		wantInError []string
	}{
		{
			name:        "capacity wait",
			err:         fmt.Errorf("dispatcher: capacity wait for runner %q timed out", "win-ci"),
			report:      dispatcher.Report{Runner: "win-ci", QueuedAt: time.Now().UTC()},
			wantInError: []string{"win-ci"},
		},
		{
			name:        "decision-009 skew refusal",
			err:         &dispatcher.SkewError{Image: "ghcr.io/example/win:v1", Reason: "tag is not this dispatcher's commit"},
			report:      dispatcher.Report{Runner: "win-ci", QueuedAt: time.Now().UTC()},
			wantInError: []string{"ghcr.io/example/win:v1"},
		},
		{
			name:        "agentic kit publish",
			err:         fmt.Errorf("dispatcher: publish agentic kit for run run-refused stage build attempt 1: blob plane unavailable"),
			report:      dispatcher.Report{},
			wantInError: []string{"publish agentic kit"},
		},
	} {
		t.Run("refused before the pod existed: "+tc.name, func(t *testing.T) {
			fake := &fakeStageDispatcher{report: tc.report, err: tc.err}
			a := &Activities{Dispatcher: fake, Surrenders: surrenderStore(t)}
			result, err := a.DispatchStage(context.Background(), dispatchInput("run-refused", "build", 1))
			if err == nil {
				t.Fatal("DispatchStage returned no error for a refused placement")
			}
			if result.Placement != nil {
				t.Fatalf("placement = %+v, want nil: DispatchStage discards the report on an unsettled "+
					"dispatcher error, so provenance cannot accompany a refusal", result.Placement)
			}
			// Not a gap left silent: the runner (and the skew subject) reach
			// the driver in the classified error's message, which is the
			// evidence a refused placement is journalled from.
			for _, want := range tc.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("classified error %q does not name %q; a refused placement's only provenance is this text", err, want)
				}
			}
		})
	}
}
