package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// continuity_test.go covers the workspace continuity record (continuity.go,
// #3803/#3767): the pure selector on both arms, and the threading of the
// selected digest into EVERY consumer — pod, self-placed task, agentic
// gate — plus the self arm's publish, at the seams the engine actually
// crosses (the dispatcher's attempt, the provisioner's request).

const (
	deltaA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deltaB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	deltaX = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestSelectDelta(t *testing.T) {
	record := []continuityEntry{
		{Stage: "a", Attempt: 1, Digest: deltaA},
		{Stage: "b", Attempt: 1, Digest: deltaB},
	}
	t.Run("empty record selects nothing on both arms", func(t *testing.T) {
		if got, err := selectDelta(nil, "c", nil); err != nil || got.Digest != "" {
			t.Fatalf("nil arm = %+v, %v", got, err)
		}
		if got, err := selectDelta(nil, "c", []string{"a"}); err != nil || got.Digest != "" {
			t.Fatalf("declared arm = %+v, %v", got, err)
		}
	})
	t.Run("nil repoFrom (2.0, gates) is last-writer", func(t *testing.T) {
		got, err := selectDelta(record, "c", nil)
		if err != nil || got.Stage != "b" || got.Digest != deltaB {
			t.Fatalf("selectDelta = %+v, %v; want b's entry", got, err)
		}
	})
	t.Run("declared repoFrom picks the most recent declared producer", func(t *testing.T) {
		got, err := selectDelta(record, "c", []string{"a", "b"})
		if err != nil || got.Stage != "b" {
			t.Fatalf("selectDelta = %+v, %v; want b", got, err)
		}
	})
	t.Run("own prior attempts are continuity (decision 001 rule 3)", func(t *testing.T) {
		own := append(append([]continuityEntry{}, record...), continuityEntry{Stage: "c", Attempt: 1, Digest: deltaX})
		got, err := selectDelta(own, "c", []string{"a", "b"})
		if err != nil || got.Stage != "c" || got.Digest != deltaX {
			t.Fatalf("selectDelta = %+v, %v; want c's own prior entry", got, err)
		}
	})
	t.Run("undeclared most-recent producer fails closed naming both stages", func(t *testing.T) {
		_, err := selectDelta(record, "c", []string{"a"})
		if err == nil {
			t.Fatal("selectDelta accepted a consumer building on an undeclared producer")
		}
		for _, want := range []string{`stage "c"`, `commits from "b"`, "repoFrom [a]", "WF022 runtime", deltaB} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})
}

func podTask(name, next string, extra func(*apiv1.Task)) apiv1.Task {
	t := apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: name,
		Run:  &apiv1.DeterministicRun{Command: []string{name + ".sh"}, Workspace: apiv1.WorkspaceRepo},
		Next: next,
	}
	if extra != nil {
		extra(&t)
	}
	return t
}

func remotePin(stage string) PinnedPlacement {
	return PinnedPlacement{Stage: stage, Queue: dispatcher.QueueName("web", "linux"), Eligible: remoteEligible(), Memory: "2Gi"}
}

func surrenderDelta(t *testing.T, store *dispatcher.SurrenderDir, runID, stage string, attempt int, digest string) {
	t.Helper()
	putSurrendered(t, store, runID, stage, attempt, dispatcher.SurrenderedResult{
		Result:             apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		WorkspaceDelta:     digest,
		WorkspaceDeltaBase: "1111111", WorkspaceDeltaTip: "2222222",
	})
}

func requestFor(t *testing.T, requests []WorkspaceRequest, stage string) WorkspaceRequest {
	t.Helper()
	for _, r := range requests {
		if r.Stage == stage {
			return r
		}
	}
	t.Fatalf("no workspace request for stage %q in %+v", stage, requests)
	return WorkspaceRequest{}
}

func deltaEvents(proj JournalProjection) []journal.Event {
	var out []journal.Event
	for _, op := range proj.Ops {
		if op.Kind == opAppend && op.Event != nil && op.Event.Type == journal.EventRunnerWorkspaceDelta {
			out = append(out, *op.Event)
		}
	}
	return out
}

// #3803: a self-placed stage after a pod stage must be provisioned WITH the
// pod's delta. Before the record the self arm received workspaceBranch alone
// and the worker's mirror handed it base (M-35).
func TestSelfPlacedStageReceivesPodDelta(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "commit",
		Tasks: []apiv1.Task{
			podTask("commit", "consume", nil),
			{Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	in := projectionInput("self-receives-pod-delta", spec)
	in.Placements = []PinnedPlacement{remotePin("commit")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "commit", 1, deltaA)
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	workspaces := testWorkspaces(t)
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	executeForProjection(t, in, &Activities{Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)
	if fake.calls.Load() != 1 {
		t.Fatalf("dispatcher calls = %d, want the commit stage to have run in a pod", fake.calls.Load())
	}
	got := requestFor(t, workspaces.provisioned(), "consume")
	if got.WorkspaceDelta != deltaA {
		t.Fatalf("consume was provisioned with WorkspaceDelta %q, want the pod's %s — without it the worker hands the stage base", got.WorkspaceDelta, deltaA)
	}
}

// Decision 001 (gates) ruling 4: a gate inherits its subject's repo state
// through the nil-repoFrom arm, and evaluates in the workspace the gate
// declares instead of a hard-coded writable repo.
func TestAgenticGateReceivesPodDeltaAndHonoursDeclaredWorkspace(t *testing.T) {
	build := func(workspace apiv1.WorkspaceMode) (apiv1.WorkflowSpec, RunInput) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
			Tasks: []apiv1.Task{podTask("implement", "review", nil)},
			Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "reviewer", Workspace: workspace},
				Branches: map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort, "needs-changes": "implement"},
			}},
		}
		in := runInput("gate-delta-"+string(workspace), spec)
		in.Placements = []PinnedPlacement{remotePin("implement")}
		return spec, in
	}
	reviewer := &fakeInvoker{review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		return apiv1.Verdict{Decision: "pass", Summary: "ok"}, nil
	}}

	t.Run("default workspace: writable repo, delta threaded", func(t *testing.T) {
		_, in := build("")
		surrenders := surrenderStore(t)
		surrenderDelta(t, surrenders, in.RunID, "implement", 1, deltaA)
		fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
		workspaces := testWorkspaces(t)
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterActivity(&Activities{Goober: reviewer, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders})
		env.ExecuteWorkflow(Run, in)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		got := requestFor(t, workspaces.provisioned(), "review")
		if got.Mode != apiv1.WorkspaceRepo || got.WorkspaceDelta != deltaA {
			t.Fatalf("review provisioned as %+v, want repo mode with the pod's delta %s (the reviewer would review base)", got, deltaA)
		}
	})
	t.Run("declared repo-readonly: honoured, and no delta for a pinned-base reader", func(t *testing.T) {
		_, in := build(apiv1.WorkspaceRepoReadOnly)
		surrenders := surrenderStore(t)
		surrenderDelta(t, surrenders, in.RunID, "implement", 1, deltaA)
		fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
		workspaces := testWorkspaces(t)
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterActivity(&Activities{Goober: reviewer, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders})
		env.ExecuteWorkflow(Run, in)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		got := requestFor(t, workspaces.provisioned(), "review")
		if got.Mode != apiv1.WorkspaceRepoReadOnly || got.WorkspaceDelta != "" {
			t.Fatalf("review provisioned as %+v, want the declared repo-readonly mode and no delta", got)
		}
	})
}

// The reverse direction (#3803): a self-placed stage that commits publishes,
// and the NEXT pod is handed that digest — not the earlier pod's stale one.
func TestSelfPlacedStagePublishesDeltaForNextPod(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "seed",
		Tasks: []apiv1.Task{
			podTask("seed", "commit", nil),
			{Name: "commit", Type: apiv1.TaskDeterministic, Goal: "commit", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "consume"},
			podTask("consume", "", nil),
		},
	}
	in := projectionInput("self-publishes", spec)
	in.Placements = []PinnedPlacement{remotePin("seed"), remotePin("consume")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "seed", 1, deltaA)
	putSurrendered(t, surrenders, in.RunID, "consume", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	workspaces := testWorkspaces(t)
	workspaces.publish = func(stage string) (WorkspaceDeltaPublication, error) {
		return WorkspaceDeltaPublication{Digest: deltaB, Base: "b0", Tip: "b1"}, nil
	}
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	proj := executeForProjection(t, in, &Activities{Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)
	if len(fake.attempts) != 2 {
		t.Fatalf("expected two pod attempts, got %d", len(fake.attempts))
	}
	if got := requestFor(t, workspaces.provisioned(), "commit").WorkspaceDelta; got != deltaA {
		t.Fatalf("commit was provisioned with %q, want the seed pod's %s", got, deltaA)
	}
	if got := fake.attempts[1].WorkspaceDelta; got != deltaB {
		t.Fatalf("consume pod carried workspace delta %q, want the self stage's %s — a stale digest would rewind (or miss) the worker's commit", got, deltaB)
	}
	// The record's movements are journaled with producer and SHAs.
	var published, selected int
	for _, ev := range deltaEvents(proj) {
		switch ev.Runner["action"] {
		case string(journal.WorkspaceDeltaPublished):
			published++
			if ev.Stage == "commit" && (ev.Runner["digest"] != deltaB || ev.Runner["baseSha"] != "b0" || ev.Runner["tipSha"] != "b1") {
				t.Errorf("commit's published event = %+v", ev.Runner)
			}
		case string(journal.WorkspaceDeltaSelected):
			selected++
			if ev.Stage == "consume" && ev.Runner["producer"] != "commit" {
				t.Errorf("consume's selected event names producer %v, want commit", ev.Runner["producer"])
			}
		}
	}
	if published != 2 || selected != 2 {
		t.Fatalf("journal has %d published / %d selected runner.workspace.delta events, want 2 / 2: %+v", published, selected, deltaEvents(proj))
	}
}

// A writable self stage that did not move its branch is journaled as
// unchanged, and a publish FAILURE fails the stage rather than stranding the
// commits (the pod's workspace_delta_failed rule, on the self arm).
func TestSelfPlacedPublishUnchangedAndFailure(t *testing.T) {
	spec := crSpec("commit", []apiv1.Task{{Name: "commit", Type: apiv1.TaskDeterministic, Goal: "commit", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}}, nil)
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	t.Run("unchanged", func(t *testing.T) {
		workspaces := testWorkspaces(t)
		workspaces.publish = func(string) (WorkspaceDeltaPublication, error) {
			return WorkspaceDeltaPublication{Unchanged: true}, nil
		}
		proj := executeForProjection(t, projectionInput("self-unchanged", spec), &Activities{Det: det, Workspaces: workspaces}, false)
		events := deltaEvents(proj)
		if len(events) != 1 || events[0].Runner["action"] != string(journal.WorkspaceDeltaUnchanged) || events[0].Stage != "commit" || events[0].Attempt != 1 {
			t.Fatalf("delta events = %+v, want one unchanged event for commit attempt 1", events)
		}
	})
	t.Run("publish failure fails the stage", func(t *testing.T) {
		workspaces := testWorkspaces(t)
		workspaces.publish = func(string) (WorkspaceDeltaPublication, error) {
			return WorkspaceDeltaPublication{}, errors.New("blob store unreachable")
		}
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterActivity(&Activities{Det: det, Workspaces: workspaces})
		env.ExecuteWorkflow(Run, runInput("self-publish-fails", spec))
		err := env.GetWorkflowError()
		if err == nil || !strings.Contains(err.Error(), "could not be carried to the next stage") {
			t.Fatalf("workflow error = %v, want the stranded-commits failure", err)
		}
	})
}

// #3767 positive arm on a 3.0 branch shape: A -> gate(fail) -> B(repoFrom A)
// -> C(repoFrom [A, B]). C continues from B, the most recent declared
// producer that executed.
func TestRepoFromSelectsDeclaredProducer(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "a",
		Tasks: []apiv1.Task{
			podTask("a", "route", func(t *apiv1.Task) { t.CommitsRepo = true }),
			podTask("b", "c", func(t *apiv1.Task) { t.CommitsRepo = true; t.RepoFrom = apiv1.RepoFrom{"a"} }),
			podTask("c", "", func(t *apiv1.Task) { t.RepoFrom = apiv1.RepoFrom{"a", "b"} }),
		},
		Gates: []apiv1.Gate{{
			Name: "route", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "route", "equals": "ok"}},
			Branches:  map[string]string{"pass": "c", "fail": "b"},
		}},
	}
	in := runInput("repofrom-declared", spec)
	in.DSLVersion = "3.0"
	in.Placements = []PinnedPlacement{remotePin("a"), remotePin("b"), remotePin("c")}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "a", 1, dispatcher.SurrenderedResult{
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"route": "fix"}}, WorkspaceDelta: deltaA,
	})
	surrenderDelta(t, surrenders, in.RunID, "b", 1, deltaB)
	putSurrendered(t, surrenders, in.RunID, "c", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(fake.attempts) != 3 {
		t.Fatalf("attempts = %d, want a, b, c", len(fake.attempts))
	}
	if got := fake.attempts[1].WorkspaceDelta; got != deltaA {
		t.Fatalf("b carried %q, want a's %s", got, deltaA)
	}
	if got := fake.attempts[2].WorkspaceDelta; got != deltaB {
		t.Fatalf("c carried %q, want b's %s (most recent declared producer)", got, deltaB)
	}
}

// #3767 negative arm — the discriminating one (WF022 runtime half): an
// UNCLASSIFIED committer (an sh stage on the repo workspace without
// commitsRepo) is bundled by its pod regardless, so the record's most recent
// entry is a stage the consumer's repoFrom does not declare. The run fails
// closed with the named WF022-runtime error, the consumer never dispatches,
// and the journal carries the refusal — where last-writer would have carried
// the undeclared commits silently.
func TestRepoFromRefusesUndeclaredRuntimeProducer(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "a",
		Tasks: []apiv1.Task{
			podTask("a", "x", func(t *apiv1.Task) { t.CommitsRepo = true }),
			// x is a consumer (repo workspace) and must declare a; it is NOT a
			// producer (no commitsRepo), so c's coverage is [a] alone.
			podTask("x", "c", func(t *apiv1.Task) { t.RepoFrom = apiv1.RepoFrom{"a"} }),
			podTask("c", "", func(t *apiv1.Task) { t.RepoFrom = apiv1.RepoFrom{"a"} }),
		},
	}
	in := projectionInput("repofrom-undeclared", spec)
	in.DSLVersion = "3.0"
	in.Placements = []PinnedPlacement{remotePin("a"), remotePin("x"), remotePin("c")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "a", 1, deltaA)
	surrenderDelta(t, surrenders, in.RunID, "x", 1, deltaX) // the undeclared runtime committer
	putSurrendered(t, surrenders, in.RunID, "c", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("the run completed: c built on x's undeclared commits (last-writer), where the WF022 runtime half (#3767) requires a fail-closed refusal")
	}
	for _, want := range []string{`stage "c"`, `commits from "x"`, "repoFrom [a]", "WF022 runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("workflow error %q does not name %q", err, want)
		}
	}
	if len(fake.attempts) != 2 {
		t.Fatalf("attempts = %d, want a and x only — c must never dispatch", len(fake.attempts))
	}
	val, qerr := env.QueryWorkflow(JournalQuery)
	if qerr != nil {
		t.Fatal(qerr)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatal(err)
	}
	var refused bool
	for _, op := range proj.Ops {
		if op.Kind == opAppend && op.Event != nil && op.Event.Type == journal.EventError && op.Event.Error != nil &&
			op.Event.Error.Code == RepoHandoffUndeclaredErrorCode && op.Event.Stage == "c" {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("journal carries no %s error for c: the far-side record of the refusal is missing", RepoHandoffUndeclaredErrorCode)
	}
	last := proj.Ops[len(proj.Ops)-1].Event
	if last == nil || last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseFailed) {
		t.Fatalf("journal terminal = %+v, want run.finished failed", last)
	}
}

// A 2.0 document has no repoFrom: last-writer is the only rule, and an
// unclassified committer's delta IS carried — byte-identical to the
// pre-record behaviour of TestModeThreeThreadsWorkspaceDeltaToTheNextStage.
func TestTwoPointZeroKeepsLastWriter(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "a",
		Tasks: []apiv1.Task{podTask("a", "x", nil), podTask("x", "c", nil), podTask("c", "", nil)},
	}
	in := runInput("two-point-zero", spec)
	in.Placements = []PinnedPlacement{remotePin("a"), remotePin("x"), remotePin("c")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "a", 1, deltaA)
	surrenderDelta(t, surrenders, in.RunID, "x", 1, deltaX)
	putSurrendered(t, surrenders, in.RunID, "c", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if len(fake.attempts) != 3 || fake.attempts[2].WorkspaceDelta != deltaX {
		t.Fatalf("c carried %q over %d attempts, want last-writer x's %s", fake.attempts[len(fake.attempts)-1].WorkspaceDelta, len(fake.attempts), deltaX)
	}
}

// Repass (decision 001 rule 3): a stage re-entered from a gate continues
// from its OWN prior publication even though its repoFrom does not — cannot
// — name itself.
func TestRepassUsesOwnPriorDelta(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "seed",
		Tasks: []apiv1.Task{
			podTask("seed", "implement", func(t *apiv1.Task) { t.CommitsRepo = true }),
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", RepoFrom: apiv1.RepoFrom{"seed"}, Next: "review"},
		},
		Gates: []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort, "needs-changes": "implement"},
		}},
	}
	in := runInput("repass-own-delta", spec)
	in.DSLVersion = "3.0"
	in.MaxRepasses = 2
	in.Placements = []PinnedPlacement{remotePin("seed")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "seed", 1, deltaA)
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	workspaces := testWorkspaces(t)
	implementRuns := 0
	workspaces.publish = func(stage string) (WorkspaceDeltaPublication, error) {
		if stage != "implement" {
			return WorkspaceDeltaPublication{}, nil
		}
		implementRuns++
		if implementRuns == 1 {
			return WorkspaceDeltaPublication{Digest: deltaB}, nil
		}
		return WorkspaceDeltaPublication{Digest: deltaX}, nil
	}
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			reviews++
			if reviews == 1 {
				return apiv1.Verdict{Decision: "needs-changes", Summary: "again"}, nil
			}
			return apiv1.Verdict{Decision: "pass", Summary: "ok"}, nil
		},
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Goober: inv, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var implementDeltas, reviewDeltas []string
	for _, r := range workspaces.provisioned() {
		switch r.Stage {
		case "implement":
			implementDeltas = append(implementDeltas, r.WorkspaceDelta)
		case "review":
			reviewDeltas = append(reviewDeltas, r.WorkspaceDelta)
		}
	}
	if len(implementDeltas) != 2 || implementDeltas[0] != deltaA || implementDeltas[1] != deltaB {
		t.Fatalf("implement deltas = %v, want [seed's %s, its own prior %s]", implementDeltas, deltaA, deltaB)
	}
	if len(reviewDeltas) != 2 || reviewDeltas[0] != deltaB || reviewDeltas[1] != deltaX {
		t.Fatalf("review deltas = %v, want each pass to review the implement publication before it", reviewDeltas)
	}
}

// DS5 two-sidedness (critic O5): runner.workspace.delta is emitted by the
// LIVE writer path and accepted by the history projection, so a mode-3 run
// never files a live_journal_divergence over it — and it stays out of the
// conformance view on both sides.
func TestWorkspaceDeltaJournaledOnLivePathAndProjection(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "commit",
		Tasks: []apiv1.Task{
			podTask("commit", "consume", nil),
			{Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	in := projectionInput("live-delta", spec)
	in.LiveJournal = true
	in.Placements = []PinnedPlacement{remotePin("commit")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "commit", 1, deltaA)
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	det := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	proj := executeLive(t, in, &Activities{Det: det, Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders, Journal: writer}, false)

	count := func(events []journal.Event) (n int) {
		for _, ev := range events {
			if ev.Type == journal.EventRunnerWorkspaceDelta {
				n++
				if ev.IsConformanceNormative() {
					t.Errorf("runner.workspace.delta reports conformance-normative: %+v", ev)
				}
			}
		}
		return n
	}
	live := liveEvents(t, runsDir, in.RunID)
	if got := count(live); got != 2 {
		t.Fatalf("live journal has %d runner.workspace.delta events, want 2 (published by commit, selected by consume): live path half missing", got)
	}
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun rejected a history carrying runner.workspace.delta: %v", err)
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if got := count(projected); got != 2 {
		t.Fatalf("projection has %d runner.workspace.delta events, want 2: projection half missing", got)
	}
	divergence, err := DiffLiveJournal(live, proj)
	if err != nil {
		t.Fatal(err)
	}
	if divergence != "" {
		t.Fatalf("live journal diverges from history re-projection:\n%s", divergence)
	}
}

// A history recorded BEFORE this change — two-argument InvokeGoober /
// ReviewGoober / RunDeterministic (testdata/continuity-prechange-history.json,
// recorded against a dev server at the parent commit) — must replay under
// the three/four-argument code: an in-flight run at deploy time continues
// rather than failing non-deterministic.
func TestContinuityPreChangeHistoryReplays(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "continuity-prechange-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	history := &historypb.History{}
	if err := protojson.Unmarshal(data, history); err != nil {
		t.Fatalf("decode recorded history: %v", err)
	}
	var seen []string
	for _, ev := range history.Events {
		if attrs := ev.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			seen = append(seen, attrs.ActivityType.Name+"/"+string(rune('0'+len(attrs.Input.Payloads))))
		}
	}
	if strings.Join(seen, ",") != "InvokeGoober/2,ReviewGoober/2,RunDeterministic/3" {
		t.Fatalf("fixture is not the pre-change shape: activities scheduled with payload counts %v", seen)
	}
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(Run)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatalf("pre-change history does not replay under the continuity-record code: %v", err)
	}
}

// Both spellings of one declaration — Task.Workspace and Run.Workspace —
// resolve through apiv1.Task.EffectiveWorkspace on EVERY arm, over the full
// WorkspaceMode enum. Found in review of this PR: a deterministic task
// declaring `workspace: repo` at the TASK level with a run block that omits
// one compiles clean (checks.go refuses only two DIFFERENT values), was cut a
// WRITABLE repo pod workspace by dispatch (which resolved Run-then-Task), and
// was handed NO delta by the selector (a private copy that read Run.Workspace
// alone) — the exact #3803 silent drop, still open for one legal spelling.
// Every earlier continuity test built its pod tasks with podTask(), which
// always spells the mode on Run, so the table is what makes the two
// spellings' agreement a tested fact at the real seams: the dispatcher's
// attempt on the pod arm, the provisioner's request on the self arm.
func TestWorkspaceDeclarationSpellingsAgreeOnEveryArm(t *testing.T) {
	spellings := []struct {
		name  string
		apply func(*apiv1.Task, apiv1.WorkspaceMode)
	}{
		{"run.workspace", func(t *apiv1.Task, m apiv1.WorkspaceMode) { t.Run.Workspace = m }},
		{"task.workspace", func(t *apiv1.Task, m apiv1.WorkspaceMode) { t.Workspace = m }},
	}
	modes := []apiv1.WorkspaceMode{apiv1.WorkspaceRepo, apiv1.WorkspaceScratch, apiv1.WorkspaceRepoReadOnly}
	consumer := func(spell func(*apiv1.Task, apiv1.WorkspaceMode), mode apiv1.WorkspaceMode) apiv1.Task {
		consume := apiv1.Task{Name: "consume", Type: apiv1.TaskDeterministic, Goal: "consume", Run: &apiv1.DeterministicRun{Command: []string{"consume.sh"}}}
		spell(&consume, mode)
		return consume
	}
	for _, sp := range spellings {
		for _, mode := range modes {
			name := sp.name + "/" + string(mode)
			t.Run("pod/"+name, func(t *testing.T) {
				spec := apiv1.WorkflowSpec{
					Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "seed",
					Tasks: []apiv1.Task{podTask("seed", "consume", nil), consumer(sp.apply, mode)},
				}
				in := runInput("spelling-pod-"+sp.name+"-"+string(mode), spec)
				in.Placements = []PinnedPlacement{remotePin("seed"), remotePin("consume")}
				surrenders := surrenderStore(t)
				surrenderDelta(t, surrenders, in.RunID, "seed", 1, deltaA)
				putSurrendered(t, surrenders, in.RunID, "consume", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
				fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
				var ts testsuite.WorkflowTestSuite
				env := temporaltest.NewWorkflowEnvironment(&ts)
				env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders})
				env.ExecuteWorkflow(Run, in)
				if err := env.GetWorkflowError(); err != nil {
					t.Fatalf("workflow error: %v", err)
				}
				attempts, _ := fake.recorded()
				if len(attempts) != 2 {
					t.Fatalf("attempts = %d, want seed and consume", len(attempts))
				}
				got := attempts[1]
				if got.Workspace != string(mode) {
					t.Fatalf("consume pod workspace = %q, want the declared %q", got.Workspace, mode)
				}
				wantDelta := ""
				if mode.IsWritableRepo() {
					wantDelta = deltaA
				}
				if got.WorkspaceDelta != wantDelta {
					t.Fatalf("consume pod: workspace %q, delta %q; want delta %q — a WRITABLE pod workspace handed no delta checks out base and silently drops seed's commits", got.Workspace, got.WorkspaceDelta, wantDelta)
				}
			})
			t.Run("self/"+name, func(t *testing.T) {
				spec := apiv1.WorkflowSpec{
					Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "seed",
					Tasks: []apiv1.Task{podTask("seed", "consume", nil), consumer(sp.apply, mode)},
				}
				in := projectionInput("spelling-self-"+sp.name+"-"+string(mode), spec)
				in.Placements = []PinnedPlacement{remotePin("seed")}
				surrenders := surrenderStore(t)
				surrenderDelta(t, surrenders, in.RunID, "seed", 1, deltaA)
				fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
				workspaces := testWorkspaces(t)
				var ran apiv1.DeterministicRun
				det := &fakeRunner{run: func(_ context.Context, _ apiv1.InvocationEnvelope, r apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
					ran = r
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
				}}
				proj := executeForProjection(t, in, &Activities{Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)
				got := requestFor(t, workspaces.provisioned(), "consume")
				if got.Mode != mode || ran.Workspace != mode {
					t.Fatalf("consume provisioned as %q and executed with run.workspace %q, want the declared %q on both", got.Mode, ran.Workspace, mode)
				}
				wantDelta := ""
				if writableWorkspace(mode) {
					wantDelta = deltaA
				}
				if got.WorkspaceDelta != wantDelta {
					t.Fatalf("consume provisioned with delta %q on a %q workspace, want %q", got.WorkspaceDelta, mode, wantDelta)
				}
				var selected int
				for _, ev := range deltaEvents(proj) {
					if ev.Stage == "consume" && ev.Runner["action"] == string(journal.WorkspaceDeltaSelected) {
						selected++
					}
				}
				wantSelected := 0
				if wantDelta != "" {
					wantSelected = 1
				}
				if selected != wantSelected {
					t.Fatalf("journal has %d selected events for a %q consumer, want %d", selected, mode, wantSelected)
				}
			})
		}
	}
}

// An agentic task's declared workspace (Task.Workspace — the only place an
// agentic task can express one) reaches the self-arm provisioner and gates
// its PUBLICATION, not just the selector. Before this, InvokeGoober
// hard-coded the writable repo: a `workspace: repo-readonly` research stage
// (the field's own motivating case) was denied a delta by the selector yet
// cut a WRITABLE worktree at base, and publishWorkspaceDelta was invoked
// with the hard-coded repo mode — so had the agent committed, a base-rooted
// bundle would have become the record's newest entry over the real
// producer's, and the next pod would have lost the producer's work.
func TestAgenticTaskHonoursDeclaredWorkspace(t *testing.T) {
	build := func(name string, mode apiv1.WorkspaceMode) (RunInput, *dispatcher.SurrenderDir) {
		spec := apiv1.WorkflowSpec{
			Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "seed",
			Tasks: []apiv1.Task{
				podTask("seed", "research", nil),
				{Name: "research", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "research", Workspace: mode, Next: "consume"},
				podTask("consume", "", nil),
			},
		}
		in := projectionInput(name, spec)
		in.Placements = []PinnedPlacement{remotePin("seed"), remotePin("consume")}
		surrenders := surrenderStore(t)
		surrenderDelta(t, surrenders, in.RunID, "seed", 1, deltaA)
		putSurrendered(t, surrenders, in.RunID, "consume", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
		return in, surrenders
	}
	inv := &fakeInvoker{invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}
	// The fake workspace WOULD publish if asked — the declared mode is what
	// must stop the ask.
	publishing := func(t *testing.T) *fakeWorkspaces {
		w := testWorkspaces(t)
		w.publish = func(string) (WorkspaceDeltaPublication, error) {
			return WorkspaceDeltaPublication{Digest: deltaB, Base: "b0", Tip: "b1"}, nil
		}
		return w
	}
	for _, mode := range []apiv1.WorkspaceMode{apiv1.WorkspaceRepoReadOnly, apiv1.WorkspaceScratch} {
		t.Run("self/"+string(mode), func(t *testing.T) {
			in, surrenders := build("agentic-self-"+string(mode), mode)
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
			workspaces := publishing(t)
			proj := executeForProjection(t, in, &Activities{Goober: inv, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)
			got := requestFor(t, workspaces.provisioned(), "research")
			if got.Mode != mode || got.WorkspaceDelta != "" {
				t.Fatalf("research provisioned as %+v, want the declared %q mode and no delta (InvokeGoober hard-coding the writable repo is the bug)", got, mode)
			}
			attempts, _ := fake.recorded()
			if len(attempts) != 2 || attempts[1].WorkspaceDelta != deltaA {
				t.Fatalf("consume pod carried %q over %d attempts, want seed's %s — a %q research stage must never enter the record", attempts[len(attempts)-1].WorkspaceDelta, len(attempts), deltaA, mode)
			}
			for _, ev := range deltaEvents(proj) {
				if ev.Stage == "research" {
					t.Fatalf("journal carries a runner.workspace.delta event for the %q research stage: %+v", mode, ev.Runner)
				}
			}
		})
	}
	t.Run("self/default is the writable repo with the delta", func(t *testing.T) {
		in, surrenders := build("agentic-self-default", "")
		fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
		workspaces := publishing(t)
		executeForProjection(t, in, &Activities{Goober: inv, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)
		got := requestFor(t, workspaces.provisioned(), "research")
		if got.Mode != apiv1.WorkspaceRepo || got.WorkspaceDelta != deltaA {
			t.Fatalf("research provisioned as %+v, want repo mode with seed's %s (the historical default, byte-identical)", got, deltaA)
		}
		attempts, _ := fake.recorded()
		if len(attempts) != 2 || attempts[1].WorkspaceDelta != deltaB {
			t.Fatalf("consume pod carried %q, want research's own %s", attempts[len(attempts)-1].WorkspaceDelta, deltaB)
		}
	})
	t.Run("pod/repo-readonly", func(t *testing.T) {
		in, surrenders := build("agentic-pod-readonly", apiv1.WorkspaceRepoReadOnly)
		in.Placements = append(in.Placements, remotePin("research"))
		putSurrendered(t, surrenders, in.RunID, "research", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
		fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
		executeForProjection(t, in, &Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders}, false)
		attempts, _ := fake.recorded()
		if len(attempts) != 3 {
			t.Fatalf("attempts = %d, want seed, research, consume", len(attempts))
		}
		if attempts[1].Workspace != string(apiv1.WorkspaceRepoReadOnly) || attempts[1].WorkspaceDelta != "" {
			t.Fatalf("research pod = workspace %q delta %q, want repo-readonly and no delta", attempts[1].Workspace, attempts[1].WorkspaceDelta)
		}
		if attempts[2].WorkspaceDelta != deltaA {
			t.Fatalf("consume pod carried %q, want seed's %s", attempts[2].WorkspaceDelta, deltaA)
		}
	})
}

// "unchanged" on the pod arm is the pod's REPORTED finding (dispatch-exec
// checked the branch and surrendered WorkspaceDeltaUnchanged), never inferred
// from an absent digest: a stage image that predates the field, or a pod
// whose blob endpoint was absent, surrenders success with no digest and no
// claim, and the journal records nothing about its branch rather than a
// positive fact nothing verified.
func TestPodArmJournalsUnchangedOnlyWhenThePodReportsIt(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "commit",
		Tasks: []apiv1.Task{podTask("commit", "", nil)},
	}
	for _, tc := range []struct {
		name       string
		reported   bool
		wantEvents int
	}{
		{"pod reports unchanged", true, 1},
		{"pre-field stage image reports nothing", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := projectionInput("pod-unchanged-"+strings.ReplaceAll(tc.name, " ", "-"), spec)
			in.Placements = []PinnedPlacement{remotePin("commit")}
			surrenders := surrenderStore(t)
			putSurrendered(t, surrenders, in.RunID, "commit", 1, dispatcher.SurrenderedResult{
				Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, WorkspaceDeltaUnchanged: tc.reported,
			})
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
			proj := executeForProjection(t, in, &Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders}, false)
			events := deltaEvents(proj)
			if len(events) != tc.wantEvents {
				t.Fatalf("runner.workspace.delta events = %+v, want %d", events, tc.wantEvents)
			}
			if tc.reported && (events[0].Runner["action"] != string(journal.WorkspaceDeltaUnchanged) || events[0].Stage != "commit") {
				t.Fatalf("event = %+v, want unchanged for commit", events[0])
			}
		})
	}
}

// The engine's fakeWorkspaces refuses exactly the modes
// workerhost.WorktreeWorkspaces refuses, so no engine test can pass by
// threading a mode the real provisioner would reject (the seam the
// repo-readonly gate regression hid behind). The same table runs through the
// real provisioner in workerhost's TestProvisionAcceptsEveryDeclaredWorkspaceMode.
func TestFakeWorkspacesMirrorTheProvisionersAcceptedModes(t *testing.T) {
	workspaces := testWorkspaces(t)
	for _, mode := range []apiv1.WorkspaceMode{"", apiv1.WorkspaceRepo, apiv1.WorkspaceScratch, apiv1.WorkspaceRepoReadOnly} {
		if _, err := workspaces.Provision(context.Background(), WorkspaceRequest{RunID: "r", Stage: "s", Mode: mode}); err != nil {
			t.Errorf("Provision(%q) = %v, want the enum value accepted", mode, err)
		}
	}
	if _, err := workspaces.Provision(context.Background(), WorkspaceRequest{RunID: "r", Stage: "s", Mode: "warp"}); err == nil {
		t.Fatal("Provision(\"warp\") succeeded; the real provisioner refuses an unknown mode and the fake must too")
	}
}
