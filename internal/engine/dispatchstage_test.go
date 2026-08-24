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

func dispatchInput(runID, stage string, attempt int) dispatchStageInput {
	return dispatchStageInput{
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

// #3699: the pod actually needs to know what to run. dispatchStageInput
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
	for name, mutate := range map[string]func(*dispatchStageInput){
		"repo workspace": func(in *dispatchStageInput) {
			in.Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}
		},
		"unset workspace defaults to repo": func(in *dispatchStageInput) {
			in.Run = &apiv1.DeterministicRun{Command: []string{"true"}}
		},
		"declared capabilities": func(in *dispatchStageInput) {
			in.Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}
			in.Envelope.Capabilities = []string{"repo:push"}
		},
		"goobers CLI builtin": func(in *dispatchStageInput) {
			in.Run = &apiv1.DeterministicRun{Command: []string{"goobers", "open-pr"}, Workspace: apiv1.WorkspaceScratch}
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

// #3699 v1 scope, at the workflow level: a pinned stage whose Run the in-pod
// executor cannot yet honor (here, no explicit workspace: scratch — the
// default is repo) is refused BEFORE the dispatcher is ever consulted, so no
// pod is created for it — dispatchRemoteTask's guard, not DispatchStage's
// defensive re-check, is what fires here.
func TestModeThreeRefusesUnsupportedRunBeforeDispatch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "build",
		Tasks: []apiv1.Task{
			{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build",
				Run: &apiv1.DeterministicRun{Command: []string{"build.cmd"}}},
		},
	}
	in := runInput("mode-three-unsupported", spec)
	in.Placements = []PinnedPlacement{{
		Stage: "build", Queue: dispatcher.QueueName("web", "win-ci"),
		Eligible: remoteEligible(), Memory: "4Gi",
	}}
	fake := &fakeStageDispatcher{}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenderStore(t)})
	env.ExecuteWorkflow(Run, in)
	// dispatchRemoteTask's guard returns a plain error, exactly like the
	// existing empty-command guard it sits beside — this is a workflow
	// execution error, not a graceful ResultFailure branch.
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), "does not yet provision a pod-side repo checkout") {
		t.Fatalf("workflow error = %v, want the unsupported-workspace refusal", err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("the dispatcher was consulted for a stage the v1 guard should have refused first — a pod may have been created")
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
