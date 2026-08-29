package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// gateplacement_test.go is the engine half of decision 001 (rulings 7–8): an
// agentic gate whose pinned placement resolved to a non-self runner evaluates
// through ActDispatchStage in review mode, and the verdict the pod surrenders
// lands in the same gate.evaluated / verdict artifact / "<gate>.verdict"
// pointer the in-process arm produces — while the unpinned and self-pinned
// arms keep calling ActReviewGoober exactly as before.

// placedGateSpec is implement (agentic, self) → review (agentic gate) → land
// (deterministic, self): the gate sits between two self stages so both the
// verdict pointer handed downstream and the absence of a worker-side attempt
// for the gate are observable.
func placedGateSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goal: "implement", Goober: "implementer", Next: "review"},
			{Name: "land", Type: apiv1.TaskDeterministic, Goal: "land",
				Run: &apiv1.DeterministicRun{Command: []string{"land.sh"}, Workspace: apiv1.WorkspaceScratch}},
		},
		Gates: []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic,
			Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
			RunsOn:   &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi"},
			Branches: map[string]string{"pass": "land", "fail": wf.TargetAbort, "needs-changes": "implement"},
		}},
	}
}

func remoteGatePin() PinnedPlacement {
	return PinnedPlacement{
		Stage: "review", Queue: dispatcher.QueueName("web", "linux-agentic"),
		Eligible: remoteEligible(), CPU: "1000m", Memory: "2Gi",
	}
}

func placedGateInput(name string) RunInput {
	in := projectionInput(name, placedGateSpec())
	// gates[].runsOn is a 3.0 surface; the frozen 2.0 interpreter refuses it.
	in.DSLVersion = "3.0"
	in.GateGooberCapabilities = map[string][]string{"reviewer": {"agent:model"}}
	return in
}

// reviewSurrender is what a review pod surrenders (dispatchexec.go): a bare
// success status and the verdict.
func reviewSurrender(verdict apiv1.Verdict) dispatcher.SurrenderedResult {
	return dispatcher.SurrenderedResult{
		Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "reviewer verdict surrendered"},
		Verdict: &verdict,
	}
}

// refusingReviewer is the self arm's reviewer seam for a run whose gate must
// NEVER reach it: a pinned non-self gate that still called ActReviewGoober
// would evaluate in the worker's process, outside its declared isolation.
func refusingReviewer(t *testing.T) *fakeInvoker {
	t.Helper()
	return &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
		},
		review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			t.Errorf("ActReviewGoober ran for %s: a gate pinned to a non-self runner must evaluate in a pod, never in-process", env.TaskID)
			return apiv1.Verdict{Decision: apiv1.VerdictFail}, nil
		},
	}
}

func gateEvaluatedEvents(proj JournalProjection) []journal.Event {
	var out []journal.Event
	for _, op := range proj.Ops {
		if op.Kind == opAppend && op.Event != nil && op.Event.Type == journal.EventGateEvaluated {
			out = append(out, *op.Event)
		}
	}
	return out
}

func artifactOp(proj JournalProjection, name string) *JournalArtifactOp {
	for _, op := range proj.Ops {
		if op.Kind == opArtifact && op.Artifact != nil && op.Artifact.Name == name {
			return op.Artifact
		}
	}
	return nil
}

// The ruling itself: a pinned non-self agentic gate reaches the dispatcher as
// a REVIEW attempt carrying the reviewer's invocation, the pod's surrendered
// verdict is what gate.evaluated records, its bytes are the verdict artifact,
// and the next stage receives the "<gate>.verdict" pointer — with no
// worker-side workspace and no ActReviewGoober for the gate.
func TestPlacedAgenticGateEvaluatesThroughDispatchActivity(t *testing.T) {
	in := placedGateInput("placed-gate")
	in.Placements = []PinnedPlacement{remoteGatePin()}
	verdict := apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "the change is right", Rationale: "read the diff"}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "review", 1, reviewSurrender(verdict))
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "linux-agentic", Pod: "goobers-review-1", Image: "ghcr.io/example/goobers-ci:v1",
		Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true,
		QueuedAt: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC), PodStartedAt: time.Date(2026, 8, 29, 12, 0, 5, 0, time.UTC),
	}}
	var landPointers []apiv1.ContextPointer
	det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		landPointers = append([]apiv1.ContextPointer(nil), env.ContextPointers...)
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "landed"}, nil
	}}
	workspaces := testWorkspaces(t)

	proj := executeForProjection(t, in, &Activities{
		Goober: refusingReviewer(t), Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders,
	}, false)

	attempts, eligible := fake.recorded()
	if len(attempts) != 1 {
		t.Fatalf("dispatcher attempts = %+v, want exactly the gate's review attempt (the tasks are self-placed)", attempts)
	}
	got := attempts[0]
	if got.Stage != "review" || got.Number != 1 {
		t.Fatalf("attempt = %+v, want stage review, pod attempt 1", got)
	}
	if !got.Review || !got.Agentic {
		t.Fatalf("attempt Review=%t Agentic=%t, want both: the kit writer stamps mode: review from Review, and only an agentic attempt carries a kit", got.Review, got.Agentic)
	}
	if got.Envelope == nil || got.Envelope.Goober != "reviewer" || got.Envelope.TaskID != in.RunID+":review" {
		t.Fatalf("attempt envelope = %+v, want the gate's reviewer invocation", got.Envelope)
	}
	if !containsString(got.Envelope.Capabilities, "agent:model") {
		t.Fatalf("attempt capabilities = %v, want the reviewer's PINNED agent:model (the pod mints it from the gate map)", got.Envelope.Capabilities)
	}
	if got.Workspace != string(apiv1.WorkspaceRepo) {
		t.Fatalf("attempt workspace = %q, want the reviewer's default writable repo (ReviewGoober's own reading of an unset AgenticGate.Workspace)", got.Workspace)
	}
	if got.CheckoutCapability != string(capability.RepoPush) {
		t.Fatalf("attempt checkout capability = %q, want %s: the reviewer declares agent:model only, so the pod's checkout mints its own", got.CheckoutCapability, capability.RepoPush)
	}
	if got.CPU != "1000m" || got.Memory != "2Gi" {
		t.Fatalf("attempt quantities = cpu %q memory %q, want the gate's pinned runsOn", got.CPU, got.Memory)
	}
	if len(eligible) != 1 || len(eligible[0]) != 1 || eligible[0][0].Name != remoteEligible()[0].Name {
		t.Fatalf("eligible = %+v, want the pinned eligible set", eligible)
	}
	for _, req := range workspaces.provisioned() {
		if req.Stage == "review" {
			t.Fatalf("a worker-side workspace was provisioned for the placed gate (%+v); the reviewer must run in its pod only", req)
		}
	}

	evaluated := gateEvaluatedEvents(proj)
	if len(evaluated) != 1 || evaluated[0].Gate != "review" || evaluated[0].Verdict != "pass" || evaluated[0].Target != "land" {
		t.Fatalf("gate.evaluated = %+v, want one pass for review routing to land", evaluated)
	}
	wantBytes, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	artifact := artifactOp(proj, evaluated[0].Name)
	if artifact == nil {
		t.Fatalf("no verdict artifact op named %q in the projection", evaluated[0].Name)
	}
	if string(artifact.Data) != string(wantBytes) {
		t.Fatalf("verdict artifact bytes = %s, want the surrendered verdict %s", artifact.Data, wantBytes)
	}
	ref, err := journal.ArtifactRef(wantBytes)
	if err != nil {
		t.Fatal(err)
	}
	var verdictPointer *apiv1.ContextPointer
	for i := range landPointers {
		if landPointers[i].Name == "review.verdict" {
			verdictPointer = &landPointers[i]
		}
	}
	if verdictPointer == nil || verdictPointer.Artifact == nil || verdictPointer.Artifact.Digest != ref.Digest {
		t.Fatalf("land received pointers %+v, want review.verdict naming the surrendered verdict's digest %s (#412 on the pod path)", landPointers, ref.Digest)
	}
	if proj.Identity.RunID != in.RunID {
		t.Fatalf("projection identity = %+v", proj.Identity)
	}
}

// A placed gate is a repo consumer like any other: it is handed the delta the
// continuity selector chose for it (decision 001 ruling 4, the nil-repoFrom
// arm) and evaluates in the workspace it declares; a read-only reviewer is
// handed none, exactly as DispatchStage withholds it from a read-only task.
func TestPlacedGateReceivesSelectedDeltaAndDeclaredWorkspace(t *testing.T) {
	build := func(name string, workspace apiv1.WorkspaceMode) RunInput {
		spec := placedGateSpec()
		spec.Tasks[0] = podTask("implement", "review", nil)
		spec.Gates[0].Agentic.Workspace = workspace
		in := projectionInput(name, spec)
		in.DSLVersion = "3.0"
		in.GateGooberCapabilities = map[string][]string{"reviewer": {"agent:model"}}
		in.Placements = []PinnedPlacement{remotePin("implement"), remoteGatePin()}
		return in
	}
	for _, tc := range []struct {
		name      string
		workspace apiv1.WorkspaceMode
		wantMode  string
		wantDelta string
	}{
		{name: "default writable repo carries the pod's delta", workspace: "", wantMode: string(apiv1.WorkspaceRepo), wantDelta: deltaA},
		{name: "declared repo-readonly is honoured and handed no delta", workspace: apiv1.WorkspaceRepoReadOnly, wantMode: string(apiv1.WorkspaceRepoReadOnly), wantDelta: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := build("placed-gate-delta-"+string(tc.workspace), tc.workspace)
			surrenders := surrenderStore(t)
			surrenderDelta(t, surrenders, in.RunID, "implement", 1, deltaA)
			putSurrendered(t, surrenders, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass}))
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
			det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			executeForProjection(t, in, &Activities{Goober: refusingReviewer(t), Det: det, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders}, false)
			attempts, _ := fake.recorded()
			var review *dispatcher.Attempt
			for i := range attempts {
				if attempts[i].Stage == "review" {
					review = &attempts[i]
				}
			}
			if review == nil {
				t.Fatalf("attempts = %+v, want the gate's review attempt", attempts)
			}
			if review.Workspace != tc.wantMode || review.WorkspaceDelta != tc.wantDelta {
				t.Fatalf("review attempt workspace = %q delta = %q, want %q / %q", review.Workspace, review.WorkspaceDelta, tc.wantMode, tc.wantDelta)
			}
		})
	}
}

// Ruling 8 (as amended by #3845): an unpinned gate and a self-pinned gate take
// the in-process arm with identical arguments — the same reviewer envelope,
// the same worker-side workspace request — and the dispatcher never sees
// them. A pre-change history replays under this code too
// (TestContinuityPreChangeHistoryReplays).
func TestUnpinnedAndSelfPinnedGatesKeepTheReviewGooberArm(t *testing.T) {
	type observed struct {
		Env     apiv1.InvocationEnvelope
		Request WorkspaceRequest
	}
	run := func(t *testing.T, placements []PinnedPlacement) observed {
		t.Helper()
		in := placedGateInput("self-gate")
		in.Placements = placements
		var seen apiv1.InvocationEnvelope
		reviewer := &fakeInvoker{
			invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			},
			review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
				seen = env
				return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
			},
		}
		det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}}
		fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
		workspaces := testWorkspaces(t)
		executeForProjection(t, in, &Activities{Goober: reviewer, Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenderStore(t)}, false)
		if fake.calls.Load() != 0 {
			t.Fatalf("dispatcher called %d times for a gate with placements %+v; the in-process arm must not dispatch", fake.calls.Load(), placements)
		}
		if seen.TaskID == "" {
			t.Fatal("ActReviewGoober never ran")
		}
		// The workspace path is stamped by the provisioner per attempt (a
		// fresh temp dir each run) and is the one field that legitimately
		// differs between two executions of the same arm.
		if seen.Workspace == "" {
			t.Fatal("the reviewer's envelope carries no provisioned workspace")
		}
		seen.Workspace = ""
		return observed{Env: seen, Request: requestFor(t, workspaces.provisioned(), "review")}
	}
	unpinned := run(t, nil)
	selfPinned := run(t, []PinnedPlacement{{Stage: "review", Self: true}})
	want, _ := json.Marshal(unpinned)
	got, _ := json.Marshal(selfPinned)
	if string(want) != string(got) {
		t.Fatalf("self-pinned gate diverged from the unpinned gate:\nunpinned:    %s\nself-pinned: %s", want, got)
	}
	if unpinned.Request.Mode != apiv1.WorkspaceRepo || unpinned.Env.Goober != "reviewer" {
		t.Fatalf("in-process arm provisioned %+v for %q, want the historical writable repo worktree for the reviewer", unpinned.Request, unpinned.Env.Goober)
	}
}

// The activity re-validates what the pod surrendered (#3838's shape): a
// review that comes back without a routable verdict fails closed, whatever
// the pod's own harness validation claimed.
func TestDispatchStageRefusesReviewWithoutDecision(t *testing.T) {
	cases := []struct {
		name        string
		review      bool
		surrendered dispatcher.SurrenderedResult
		want        string
	}{
		{
			name: "empty decision", review: true,
			surrendered: reviewSurrender(apiv1.Verdict{Summary: "forgot to decide"}),
			want:        "empty decision",
		},
		{
			name: "decision outside the verdict schema", review: true,
			surrendered: reviewSurrender(apiv1.Verdict{Decision: "maybe"}),
			want:        "verdict schema",
		},
		{
			name: "no verdict at all", review: true,
			surrendered: dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}},
			want:        "surrendered no verdict",
		},
		{
			name: "a delta beside the verdict", review: true,
			surrendered: dispatcher.SurrenderedResult{
				Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, Verdict: &apiv1.Verdict{Decision: apiv1.VerdictPass},
				WorkspaceDelta: deltaA,
			},
			want: "a reviewer never publishes",
		},
		{
			name: "a task attempt surrendering a verdict", review: false,
			surrendered: reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass}),
			want:        "only a review attempt surrenders one",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := surrenderStore(t)
			putSurrendered(t, store, "run-v", "review", 1, tc.surrendered)
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
			a := &Activities{Dispatcher: fake, Surrenders: store}
			input := dispatchInput("run-v", "review", 1)
			input.Review = tc.review
			result, err := a.DispatchStage(context.Background(), input)
			if err == nil {
				t.Fatalf("DispatchStage = %+v, want a fail-closed refusal (%s)", result, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) || appErr.Type() != FailureTypeStage {
				t.Fatalf("error %v must be a policy-classed application error (a substituted document is not a retryable fault)", err)
			}
		})
	}
}

// A valid review surrender is projected with its verdict, and the verdict is
// what the walk routes on.
func TestDispatchStageProjectsAValidReviewVerdict(t *testing.T) {
	store := surrenderStore(t)
	verdict := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "one more pass", Findings: []apiv1.Finding{{Severity: "error", Message: "missing test"}}}
	putSurrendered(t, store, "run-ok", "review", 2, reviewSurrender(verdict))
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Pod: "p", Image: "i", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, QueuedAt: time.Now(), PodStartedAt: time.Now()}}
	a := &Activities{Dispatcher: fake, Surrenders: store}
	input := dispatchInput("run-ok", "review", 2)
	input.Review = true
	result, err := a.DispatchStage(context.Background(), input)
	if err != nil {
		t.Fatalf("DispatchStage: %v", err)
	}
	if result.Verdict == nil || result.Verdict.Decision != apiv1.VerdictNeedsChanges || len(result.Verdict.Findings) != 1 {
		t.Fatalf("result verdict = %+v, want the surrendered verdict projected verbatim", result.Verdict)
	}
	if result.Status != apiv1.ResultSuccess || result.Placement == nil || result.Placement.Runner != "win-ci" {
		t.Fatalf("result = %+v, want the bare success status and the pod's placement provenance", result)
	}
	attempts, _ := fake.recorded()
	if len(attempts) != 1 || !attempts[0].Review || attempts[0].Number != 2 {
		t.Fatalf("attempt = %+v, want Review=true numbered 2", attempts)
	}
}

// Review=true with a DeterministicRun is a shape the workflow never builds; the
// activity refuses it before any pod exists.
func TestDispatchStageRefusesReviewWithRun(t *testing.T) {
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	a := &Activities{Dispatcher: fake, Surrenders: surrenderStore(t)}
	input := dispatchInput("run-r", "review", 1)
	input.Review = true
	input.Run = &apiv1.DeterministicRun{Command: []string{"review.sh"}, Workspace: apiv1.WorkspaceScratch}
	_, err := a.DispatchStage(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "marked review but carries a DeterministicRun") {
		t.Fatalf("error = %v, want the review-with-command refusal", err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("the dispatcher was reached: a refused shape must never create a pod")
	}
}

// A review pod that failed before its harness ran surrenders a failure the
// engine classifies by the pod's own Retryable marking: a substrate fault
// retries on a fresh pod under the gate's evaluator retry bound, a harness
// failure fails the run — the same split ReviewGoober's own errors take.
func TestDispatchStageReviewFailureClassifiesByThePodsRetryableMarking(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retryable bool
		want      string
	}{
		{name: "substrate fault retries as infra", retryable: true, want: FailureTypeInfrastructure},
		{name: "harness failure is policy", retryable: false, want: FailureTypeStage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := surrenderStore(t)
			putSurrendered(t, store, "run-f", "review", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "workspace_provision_failed", Message: "clone refused", Retryable: tc.retryable},
			}})
			a := &Activities{Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "win-ci", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}, Surrenders: store}
			input := dispatchInput("run-f", "review", 1)
			input.Review = true
			_, err := a.DispatchStage(context.Background(), input)
			var appErr *temporal.ApplicationError
			if !errors.As(err, &appErr) {
				t.Fatalf("error = %v, want a typed application error", err)
			}
			if appErr.Type() != tc.want || !strings.Contains(err.Error(), "workspace_provision_failed") {
				t.Fatalf("failure type = %q (%v), want %q naming the pod's own code", appErr.Type(), err, tc.want)
			}
		})
	}
}

// Every pod attempt of a gate is a fresh surrender key and a fresh pod: the
// counter climbs across an infra retry AND across a repass, and never resets
// on pass the way the repass count does.
func TestGatePodAttemptIsUniquePerGateAcrossTheRun(t *testing.T) {
	dispatches := map[string]int{}
	if got := []int{gatePodAttempt(dispatches, "review"), gatePodAttempt(dispatches, "review"), gatePodAttempt(dispatches, "audit"), gatePodAttempt(dispatches, "review")}; got[0] != 1 || got[1] != 2 || got[2] != 1 || got[3] != 3 {
		t.Fatalf("attempts = %v, want review 1,2,3 and audit 1", got)
	}
}

// A placed gate's walk is a pure function of pinned data: a run recorded
// against a real server — the gate on its pinned queue, the tasks on the
// workflow queue — replays byte for byte, which is the determinism guarantee
// the dispatch arm inherits from runTask's.
func TestPlacedGateQueueRoutingAndHistoryReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})

	const workflowQueue = "placed-gate-routing"
	in := placedGateInput("placed-gate-routing")
	in.Placements = []PinnedPlacement{remoteGatePin()}
	store := surrenderStore(t)
	putSurrendered(t, store, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "routed"}))
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Pod: "pod-r", Phase: corev1.PodSucceeded, SurrenderConfirmed: true, Disposed: true}}
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "landed"}, nil
	}}

	temporalClient := server.Client()
	// The workflow queue's worker has NO dispatcher: a gate mis-routed onto it
	// would fail "not configured" instead of completing the run.
	workflowWorker := temporalworker.New(temporalClient, workflowQueue, temporalworker.Options{})
	RegisterWith(workflowWorker, &Activities{Goober: refusingReviewer(t), Det: det, Workspaces: testWorkspaces(t)})
	if err := workflowWorker.Start(); err != nil {
		t.Fatalf("start workflow-queue worker: %v", err)
	}
	t.Cleanup(workflowWorker.Stop)
	dispatchWorker := temporalworker.New(temporalClient, remoteGatePin().Queue, temporalworker.Options{})
	RegisterWith(dispatchWorker, &Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: store})
	if err := dispatchWorker.Start(); err != nil {
		t.Fatalf("start dispatch-queue worker: %v", err)
	}
	t.Cleanup(dispatchWorker.Stop)

	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.RunID, TaskQueue: workflowQueue}, Run, in)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.Status != StatusCompleted || result.Outputs["land"].Summary != "landed" {
		t.Fatalf("result = %+v, want completion through the pass branch", result)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want the dispatch queue's worker to have served the gate", fake.calls.Load())
	}

	iter := temporalClient.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	var scheduled []string
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read workflow history: %v", err)
		}
		history.Events = append(history.Events, event)
		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			scheduled = append(scheduled, attrs.ActivityType.Name+"@"+attrs.TaskQueue.Name)
		}
	}
	if want := "InvokeGoober@" + workflowQueue + ",DispatchStage@" + remoteGatePin().Queue + ",RunDeterministic@" + workflowQueue; strings.Join(scheduled, ",") != want {
		t.Fatalf("scheduled activities = %v, want %s (no ReviewGoober anywhere)", scheduled, want)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("replay placed-gate workflow history: %v", err)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// One attempt is one pod (D1) and the surrender plane's Put is idempotent per
// (run, stage, attempt), so a REPASSED gate — evaluated again after
// needs-changes sent the run back to implement — must dispatch under a fresh
// pod attempt or it would read its own earlier verdict back. The counter is
// per gate for the whole run, never the gate's non-pass count (which resets
// on pass), and each evaluation reviews the delta the implement pass before
// it published. The implement stage stays on the self arm here so the
// subject's publications are scripted per pass (TestRepassUsesOwnPriorDelta's
// shape): a pod TASK re-entered by a repass is re-dispatched as attempt 1 by
// dispatchWithRetry today, which is the task path's own numbering to settle,
// not this gate's.
func TestRepassedPlacedGateDispatchesUnderAFreshAttempt(t *testing.T) {
	in := placedGateInput("placed-gate-repass")
	in.Placements = []PinnedPlacement{remoteGatePin()}
	in.MaxRepasses = 2
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "again"}))
	putSurrendered(t, surrenders, in.RunID, "review", 2, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "now fine"}))
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "landed"}, nil
	}}
	workspaces := testWorkspaces(t)
	implementRuns := 0
	workspaces.publish = func(stage string) (WorkspaceDeltaPublication, error) {
		if stage != "implement" {
			return WorkspaceDeltaPublication{}, nil
		}
		implementRuns++
		if implementRuns == 1 {
			return WorkspaceDeltaPublication{Digest: deltaA}, nil
		}
		return WorkspaceDeltaPublication{Digest: deltaB}, nil
	}
	proj := executeForProjection(t, in, &Activities{Goober: refusingReviewer(t), Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)

	evaluated := gateEvaluatedEvents(proj)
	if len(evaluated) != 2 || evaluated[0].Verdict != "needs-changes" || evaluated[1].Verdict != "pass" {
		t.Fatalf("gate.evaluated = %+v, want needs-changes then pass", evaluated)
	}
	if implementRuns != 2 {
		t.Fatalf("implement published %d time(s), want 2 (the first pass and the repass)", implementRuns)
	}
	attempts, _ := fake.recorded()
	var gateNumbers []int
	var gateDeltas []string
	for _, a := range attempts {
		if a.Stage != "review" {
			t.Fatalf("dispatcher attempt %+v for a self-placed stage", a)
		}
		gateNumbers = append(gateNumbers, a.Number)
		gateDeltas = append(gateDeltas, a.WorkspaceDelta)
	}
	if fmt.Sprint(gateNumbers) != "[1 2]" {
		t.Fatalf("gate pod attempts = %v, want [1 2]: a repeated number would surrender against the earlier verdict", gateNumbers)
	}
	if fmt.Sprint(gateDeltas) != fmt.Sprint([]string{deltaA, deltaB}) {
		t.Fatalf("gate deltas = %v, want [%s %s]: each evaluation reviews the implement pass before it", gateDeltas, deltaA, deltaB)
	}
}

// The walk-level half of the fail-closed rule: a review pod that surrenders a
// verdict with no decision fails the RUN — the refusal is stage-classed, so it
// is not retried on the gate's evaluator budget (a redispatch would reproduce
// it) and it never falls through to the in-process reviewer. The rule is
// asserted at BOTH boundaries: the activity refuses the surrender, and the
// workflow re-reads the one field it routes on, so an activity host that
// predates the check (a skewed worker handing back an empty decision) cannot
// route the run on nothing either.
func TestPlacedGateRefusesAnEmptyVerdictFromTheActivity(t *testing.T) {
	t.Run("the activity refuses the surrendered verdict", func(t *testing.T) {
		in := placedGateInput("placed-gate-empty")
		in.Placements = []PinnedPlacement{remoteGatePin()}
		// A retry bound the gate WOULD spend on an infrastructure-classed
		// failure: the assertion below is that the refusal does not touch it.
		in.Spec.Gates[0].Agentic.Retry = &apiv1.RetryPolicy{MaxAttempts: 3}
		surrenders := surrenderStore(t)
		putSurrendered(t, surrenders, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Summary: "no decision"}))
		fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterActivity(&Activities{Goober: refusingReviewer(t), Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
		env.ExecuteWorkflow(Run, in)
		err := env.GetWorkflowError()
		if err == nil {
			t.Fatal("the run completed on a verdict with no decision")
		}
		if !strings.Contains(err.Error(), "empty decision") || !strings.Contains(err.Error(), "fail closed") {
			t.Fatalf("workflow error = %v, want the fail-closed empty-decision refusal", err)
		}
		if got := fake.calls.Load(); got != 1 {
			t.Fatalf("dispatcher calls = %d, want exactly one gate attempt: a stage-classed refusal is never retried on the evaluator budget", got)
		}
	})
	t.Run("the workflow refuses an empty decision from a skewed activity host", func(t *testing.T) {
		in := placedGateInput("placed-gate-empty-skew")
		in.Placements = []PinnedPlacement{remoteGatePin()}
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterActivity(&skewedDispatchHost{real: &Activities{Goober: refusingReviewer(t), Workspaces: testWorkspaces(t)}})
		env.ExecuteWorkflow(Run, in)
		err := env.GetWorkflowError()
		if err == nil {
			t.Fatal("the run routed on a verdict with no decision handed back by the activity")
		}
		if !strings.Contains(err.Error(), "empty decision") || !strings.Contains(err.Error(), "fail closed") {
			t.Fatalf("workflow error = %v, want the workflow-side fail-closed empty-decision refusal", err)
		}
	})
}

// skewedDispatchHost is an activity host that predates the surrendered-verdict
// check: its DispatchStage projects a review surrender without validating it —
// a success envelope beside a verdict with no decision, which no current host
// returns. InvokeGoober delegates to the real activity so the walk reaches the
// gate. Registered by method name, exactly as the worker registers Activities.
type skewedDispatchHost struct{ real *Activities }

func (h *skewedDispatchHost) InvokeGoober(ctx context.Context, env apiv1.InvocationEnvelope, workspaceBranch, workspaceDelta string, workspace apiv1.WorkspaceMode) (stageActivityResult, error) {
	return h.real.InvokeGoober(ctx, env, workspaceBranch, workspaceDelta, workspace)
}

func (h *skewedDispatchHost) DispatchStage(context.Context, DispatchStageInput) (stageActivityResult, error) {
	return stageActivityResult{
		ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		Verdict:        &apiv1.Verdict{Summary: "decision stripped by a skewed host"},
	}, nil
}
