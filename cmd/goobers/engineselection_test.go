package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// selectionDefinition compiles one workflow with the named gates into the
// wfpkg.Definition shape selectEngineForEntry reads.
func selectionDefinition(t *testing.T, gates ...apiv1.Gate) workflow.Definition {
	t.Helper()
	return workflow.Definition{
		Name:    "implementation",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "web",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement",
				Type: apiv1.TaskDeterministic,
				Goal: "implement",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			}},
			Gates: gates,
		},
	}
}

// TestSelectEngineForEntryRefusesSelfPinnedLaneWithoutRefusingItGlobally is
// the corrected predicate's central claim, and the one the naive version got
// wrong in a way that would have been a four-lane production outage.
//
// EVERY 2.0-DSL lane self-pins: pre-3.0 StagePlacements emits task-only
// requiredCapabilities rows and no gate rows, so SelectRunner lands them on
// the daemon's own host. A predicate that treated a self pin as a REFUSAL —
// rather than as "this lane stays on the local runner" — would stop
// dispatching those lanes entirely. The assertion here is therefore two-sided:
// the lane does not go to the engine, AND the selection carries a fallback
// (not an error) naming the stage and what it would need.
func TestSelectEngineForEntryRefusesSelfPinnedLaneWithoutRefusingItGlobally(t *testing.T) {
	sel := selectEngineForEntry(selectionDefinition(t), []engine.PinnedPlacement{
		{Stage: "implement", Self: true},
	})
	if sel.UseEngine {
		t.Fatal("a self-pinned stage cannot execute on a Temporal worker; the lane must stay on the local runner")
	}
	if got := sel.SelfPinnedStages; len(got) != 1 || got[0] != "implement" {
		t.Errorf("SelfPinnedStages = %v, want [implement]; the annotation must name the stage an operator has to re-declare", got)
	}
	if sel.FallbackReason == "" {
		t.Fatal("a declined lane must carry an operator-facing reason, or the fallback is invisible")
	}
	// Naming the plane the stage would need is what makes the annotation
	// actionable rather than merely descriptive.
	if !strings.Contains(sel.FallbackReason, "remote-runner plane") {
		t.Errorf("FallbackReason = %q, want it to name the plane client a self-pinned stage needs", sel.FallbackReason)
	}
}

// TestSelectEngineForEntryRequiresEveryAgenticGatePinned covers the second
// disqualifier. engine.evaluateGate takes its remote arm only when
// remotePlacementFor finds a row; an agentic gate with no pin therefore
// evaluates on the WORKER's self arm, which has none of the daemon's
// credentials. The lane would dispatch and then be unable to make progress.
func TestSelectEngineForEntryRequiresEveryAgenticGatePinned(t *testing.T) {
	def := selectionDefinition(t, apiv1.Gate{Name: "review", Evaluator: apiv1.EvaluatorAgentic})
	sel := selectEngineForEntry(def, []engine.PinnedPlacement{
		{Stage: "implement", Queue: "remote-1"},
	})
	if sel.UseEngine {
		t.Fatal("an agentic gate with no placement row would evaluate on the worker's self arm; the lane must stay on the runner")
	}
	if got := sel.UnpinnedGates; len(got) != 1 || got[0] != "review" {
		t.Errorf("UnpinnedGates = %v, want [review]", got)
	}
	if !strings.Contains(sel.FallbackReason, "gate credential plane") {
		t.Errorf("FallbackReason = %q, want it to name the plane an unpinned agentic gate needs", sel.FallbackReason)
	}
}

// TestSelectEngineForEntryIgnoresNonAgenticGates: an automated gate
// evaluates inside the workflow itself, so it needs no placement row.
// Reporting it as "unpinned" would send an operator hunting for a runsOn
// declaration that would not help — and would keep a fully-remote lane off
// the engine for no reason.
func TestSelectEngineForEntryIgnoresNonAgenticGates(t *testing.T) {
	def := selectionDefinition(t,
		apiv1.Gate{Name: "automated-check", Evaluator: apiv1.EvaluatorAutomated},
	)
	sel := selectEngineForEntry(def, []engine.PinnedPlacement{
		{Stage: "implement", Queue: "remote-1"},
	})
	if !sel.UseEngine {
		t.Fatalf("a lane whose only unpinned gate is automated must reach the engine; declined with %q", sel.FallbackReason)
	}
	if len(sel.UnpinnedGates) != 0 {
		t.Errorf("UnpinnedGates = %v, want none: an automated gate needs no placement row", sel.UnpinnedGates)
	}
}

// TestSelectEngineForEntryDoesNotReportAHumanGateAsUnpinned: a human gate
// disqualifies the lane, but for a reason a runsOn declaration cannot fix.
// Naming it as an unpinned gate would send an operator editing placement.
func TestSelectEngineForEntryDoesNotReportAHumanGateAsUnpinned(t *testing.T) {
	def := selectionDefinition(t, apiv1.Gate{Name: "human-signoff", Evaluator: apiv1.EvaluatorHuman})
	sel := selectEngineForEntry(def, []engine.PinnedPlacement{
		{Stage: "implement", Queue: "remote-1"},
	})
	if sel.UseEngine {
		t.Fatal("the engine walk refuses a human gate before it writes any journal; the lane must stay on the runner")
	}
	if len(sel.UnpinnedGates) != 0 {
		t.Errorf("UnpinnedGates = %v, want none: a human gate is refused outright, not unpinned", sel.UnpinnedGates)
	}
	if sel.Refusal == nil {
		t.Error("the walk-level refusal must be carried")
	}
}

// TestSelectEngineForEntryRefusesEmptyPinSet is the zero-declaration /
// local-mode instance (architecture §11 item 1's invariance). No pins means
// there is no remote runner inventory to dispatch INTO, and an empty set must
// never be read as "nothing disqualified, therefore engine".
func TestSelectEngineForEntryRefusesEmptyPinSet(t *testing.T) {
	for _, placements := range [][]engine.PinnedPlacement{nil, {}} {
		sel := selectEngineForEntry(selectionDefinition(t), placements)
		if sel.UseEngine {
			t.Fatalf("placements %v: an empty pin set is a local-mode instance, not a fully-remote one", placements)
		}
		if sel.FallbackReason == "" {
			t.Error("an empty pin set must still explain itself")
		}
	}
}

// TestSelectEngineForEntryUsesEngineWhenFullyPinned is the positive arm: the
// predicate must actually say yes for the shape D1 exists to enable, or every
// other test here is satisfied by a function that always declines.
func TestSelectEngineForEntryUsesEngineWhenFullyPinned(t *testing.T) {
	def := selectionDefinition(t, apiv1.Gate{Name: "review", Evaluator: apiv1.EvaluatorAgentic})
	sel := selectEngineForEntry(def, []engine.PinnedPlacement{
		{Stage: "implement", Queue: "remote-1"},
		{Stage: "review", Queue: "remote-2"},
	})
	if !sel.UseEngine {
		t.Fatalf("a lane with every stage and every agentic gate pinned to a non-self runner must reach the engine; declined with %q", sel.FallbackReason)
	}
	if sel.FallbackReason != "" {
		t.Errorf("FallbackReason = %q, want empty for an accepted lane", sel.FallbackReason)
	}
}

// TestEngineSelectionsAblationWithoutEngineConfig is the ABLATION: with the
// engine switched off at the instance level, every lane must stay on the
// runner and say why — and, critically, the reason must be the engine
// configuration rather than placement facts that are not the cause. An
// operator told "self-pinned stages" on an instance with no engine would go
// rewrite runsOn declarations that could not possibly help.
func TestEngineSelectionsAblationWithoutEngineConfig(t *testing.T) {
	_, set, _ := runControlsFixture()
	machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{
		{Gaggle: "web", Workflow: "implementation"}: {},
	}
	selections, err := engineSelections(&instance.Config{}, set, machines)
	if err != nil {
		t.Fatalf("engineSelections: %v", err)
	}
	sel := selections[localscheduler.WorkflowIdentity{Gaggle: "web", Workflow: "implementation"}]
	if sel.UseEngine {
		t.Fatal("no engine configuration means nowhere to dispatch; the lane must stay on the runner")
	}
	if !strings.Contains(sel.FallbackReason, "no engine configuration") {
		t.Errorf("FallbackReason = %q, want it to name the missing engine configuration", sel.FallbackReason)
	}
}

// recordingStarter is a localscheduler.Starter that records what it was asked
// to start, so the fallback wrapper can be proved to delegate UNMODIFIED.
type recordingStarter struct {
	calls  []localscheduler.StartRequest
	result localscheduler.StartResult
	err    error
}

func (s *recordingStarter) Start(_ context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.calls = append(s.calls, req)
	return s.result, s.err
}

// TestRunnerFallbackStarterDelegatesUnmodifiedAndAnnotates is the per-lane
// rollback property, asserted directly.
//
// Reverting a lane's runsOn declarations must restore its PREVIOUS behaviour
// exactly — not approximately — so the fallback wrapper is only allowed to add
// an instance-log event. If it altered the StartRequest, or swallowed the
// wrapped starter's result, rollback would not be rollback.
func TestRunnerFallbackStarterDelegatesUnmodifiedAndAnnotates(t *testing.T) {
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	next := &recordingStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted, NoWork: true}}
	fallback := &runnerFallbackStarter{
		next:     next,
		log:      log,
		workflow: "implementation",
		selection: engineSelection{
			FallbackReason:   "self-pinned stages [implement] need a non-self runner",
			SelfPinnedStages: []string{"implement"},
			UnpinnedGates:    []string{"review"},
		},
	}

	req := localscheduler.StartRequest{RunID: "run-1", Gaggle: "web", Item: &apiv1.BacklogItem{ID: "42"}}
	got, err := fallback.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("fallback Start: %v", err)
	}
	if len(next.calls) != 1 {
		t.Fatalf("wrapped starter called %d times, want exactly 1", len(next.calls))
	}
	if next.calls[0].RunID != req.RunID || next.calls[0].Gaggle != req.Gaggle || next.calls[0].Item != req.Item {
		t.Errorf("wrapped starter got %+v, want the request unmodified (%+v)", next.calls[0], req)
	}
	if got != next.result {
		t.Errorf("fallback returned %+v, want the wrapped starter's result %+v", got, next.result)
	}

	events := instanceLogEventsForRun(t, dir, "run-1")
	var annotation *journal.Event
	for i := range events {
		if events[i].Type == journal.EventRunnerAnnotation && events[i].Runner["kind"] == engineStarterSelectionKind {
			annotation = &events[i]
			break
		}
	}
	if annotation == nil {
		t.Fatalf("no %s annotation in %d instance-log events; an operator reading THIS run has no way to see why it stayed on the runner",
			engineStarterSelectionKind, len(events))
	}
	if annotation.Runner["starter"] != "runner" {
		t.Errorf("annotation starter = %v, want \"runner\"", annotation.Runner["starter"])
	}
	if annotation.Runner["reason"] != fallback.selection.FallbackReason {
		t.Errorf("annotation reason = %v, want %q", annotation.Runner["reason"], fallback.selection.FallbackReason)
	}
	for _, key := range []string{"selfPinnedStages", "unpinnedGates"} {
		if _, ok := annotation.Runner[key]; !ok {
			t.Errorf("annotation is missing %s; the disqualifying sets must be data, not only prose", key)
		}
	}
}

// TestUnwrapStarterReachesTheRealStarter: the wrapper must stay transparent to
// callers that introspect a scheduler entry's Starter, or every such caller
// silently changes its answer the day a lane falls back.
func TestUnwrapStarterReachesTheRealStarter(t *testing.T) {
	inner := &recordingStarter{}
	wrapped := &runnerFallbackStarter{next: &runnerFallbackStarter{next: inner}}
	if got := unwrapStarter(wrapped); got != localscheduler.Starter(inner) {
		t.Errorf("unwrapStarter = %T, want the innermost *recordingStarter", got)
	}
	if got := unwrapStarter(inner); got != localscheduler.Starter(inner) {
		t.Errorf("unwrapStarter of an unwrapped starter = %T, want it unchanged", got)
	}
}

// instanceLogEventsForRun reads the instance log and returns the events
// carrying the named run id.
func instanceLogEventsForRun(t *testing.T, dir, runID string) []journal.Event {
	t.Helper()
	all, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatalf("read instance log: %v", err)
	}
	var out []journal.Event
	for _, ev := range all {
		if ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out
}

// unwrapStarter peels the selection wrappers off a scheduler entry's Starter,
// yielding the starter that actually dispatches. A lane the engine predicate
// declined carries a runnerFallbackStarter around its trackedStarter, and a
// caller asking "what does the runner path pin for this lane?" must not get a
// different answer depending on that.
func unwrapStarter(starter localscheduler.Starter) localscheduler.Starter {
	for {
		unwrapper, ok := starter.(interface {
			Unwrap() localscheduler.Starter
		})
		if !ok {
			return starter
		}
		next := unwrapper.Unwrap()
		if next == nil {
			return starter
		}
		starter = next
	}
}

// TestSelectEngineForEntryDeclinesDefinitionsTheEngineWalkRefuses is the
// hazard that the placement predicate alone cannot see.
//
// Human gates and the R9 unsupported-feature set (spec.parallels,
// task.experiment, task.limits.maxTokens/maxCostUSD, task.outbox) contribute
// NO placement rows — placeableStages emits rows only for tasks and
// runsOn-declaring agentic gates. So a fully remote-pinned lane declaring any
// of them passes every placement check, is dispatched to the engine, and is
// then refused: R9 at Registry.StartInputVersion, a human gate inside run()
// itself before the journal is even created. Permanently, on every tick, on a
// lane that ran perfectly well on the local runner the day before — and the
// goobers gaggle's quality-sprint.yaml declares parallels today.
func TestSelectEngineForEntryDeclinesDefinitionsTheEngineWalkRefuses(t *testing.T) {
	pinned := []engine.PinnedPlacement{{Stage: "implement", Queue: "remote-1"}}

	for _, tc := range []struct {
		name   string
		mutate func(def workflow.Definition) workflow.Definition
		want   string
	}{
		{
			"human gate",
			func(def workflow.Definition) workflow.Definition {
				def.Spec.Gates = []apiv1.Gate{{Name: "approve", Evaluator: apiv1.EvaluatorHuman}}
				return def
			},
			"human gates",
		},
		{
			"spec.parallels",
			func(def workflow.Definition) workflow.Definition {
				def.Spec.Parallels = []apiv1.Parallel{{Name: "fan", FailurePolicy: apiv1.BranchFailFast}}
				return def
			},
			"spec.parallels",
		},
		{
			"task.outbox",
			func(def workflow.Definition) workflow.Definition {
				def.Spec.Tasks[0].Outbox = []string{"out.txt"}
				return def
			},
			"task.outbox",
		},
		{
			"task.limits",
			func(def workflow.Definition) workflow.Definition {
				def.Spec.Tasks[0].Limits = &apiv1.Limits{MaxTokens: 1000}
				return def
			},
			"maxTokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := selectEngineForEntry(tc.mutate(selectionDefinition(t)), pinned)
			if sel.UseEngine {
				t.Fatalf("the engine walk refuses this definition; dispatching it would fail the lane on every tick")
			}
			if sel.Refusal == nil {
				t.Fatal("the refusal must be carried, so the annotation can classify it")
			}
			if !strings.Contains(sel.FallbackReason, tc.want) {
				t.Errorf("FallbackReason = %q, want it to name %q", sel.FallbackReason, tc.want)
			}
			// A refused lane is NOT a placement problem, so it must not be
			// reported as one — that sends an operator editing runsOn.
			if len(sel.SelfPinnedStages) > 0 || len(sel.UnpinnedGates) > 0 {
				t.Errorf("selection = %+v, want no placement complaints for a walk-level refusal", sel)
			}
		})
	}
}

// TestSelectEngineForEntryReportsPlacementBeforeRefusal: when a lane is
// disqualified on BOTH grounds, the placement reason is the one an operator
// can act on with a runsOn edit, so it wins.
func TestSelectEngineForEntryReportsPlacementBeforeRefusal(t *testing.T) {
	def := selectionDefinition(t)
	def.Spec.Parallels = []apiv1.Parallel{{Name: "fan", FailurePolicy: apiv1.BranchFailFast}}
	sel := selectEngineForEntry(def, []engine.PinnedPlacement{{Stage: "implement", Self: true}})
	if sel.UseEngine {
		t.Fatal("a self-pinned, parallels-declaring lane must stay on the runner")
	}
	if len(sel.SelfPinnedStages) != 1 {
		t.Fatalf("selection = %+v, want the self-pin reported", sel)
	}
	if !strings.Contains(sel.FallbackReason, "self-pinned") {
		t.Errorf("FallbackReason = %q, want the actionable placement reason", sel.FallbackReason)
	}
}
