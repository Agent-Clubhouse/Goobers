package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	wf "github.com/goobers/goobers/internal/workflow"
)

type scriptedCall struct {
	usage  map[string]float64
	result apiv1.ResultEnvelope
	err    error
}

type scriptedExec struct {
	mu          sync.Mutex
	script      map[string][]scriptedCall
	calls       map[string]int
	verdicts    map[string][]apiv1.Verdict
	reviewCalls map[string]int
	addenda     map[string]string
	held        map[string]<-chan struct{}
}

const scriptedHoldBound = 30 * time.Second

func newScriptedExec(script map[string][]scriptedCall) *scriptedExec {
	return &scriptedExec{script: script}
}

func (s *scriptedExec) holdFor(stage string, release <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held = map[string]<-chan struct{}{}
	}
	s.held[stage] = release
}

func (s *scriptedExec) waitForRelease(stage string) {
	s.mu.Lock()
	release, ok := s.held[stage]
	if ok {
		delete(s.held, stage)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-release:
	case <-time.After(scriptedHoldBound):
	}
}

func (s *scriptedExec) next(ctx context.Context, taskID string) (apiv1.ResultEnvelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stage := stageOfTaskID(taskID)
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	n := s.calls[stage]
	s.calls[stage] = n + 1
	script := s.script[stage]
	if len(script) == 0 {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	if n >= len(script) {
		n = len(script) - 1
	}
	call := script[n]
	if call.usage != nil {
		invoke.ReportAgentUsage(ctx, call.usage)
	}
	return call.result, call.err
}

func stageOfTaskID(taskID string) string {
	return taskID[strings.Index(taskID, ":")+1:]
}

func (s *scriptedExec) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.waitForRelease(stageOfTaskID(env.TaskID))
	return s.next(ctx, env.TaskID)
}

func (s *scriptedExec) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	s.recordAddendum(env)
	s.waitForRelease(stageOfTaskID(env.TaskID))
	return s.next(ctx, env.TaskID)
}

func (s *scriptedExec) recordAddendum(env apiv1.InvocationEnvelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addenda == nil {
		s.addenda = map[string]string{}
	}
	s.addenda[stageOfTaskID(env.TaskID)] = env.InstructionAddendum
}

func (s *scriptedExec) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gate := stageOfTaskID(env.TaskID)
	script := s.verdicts[gate]
	if len(script) == 0 {
		return apiv1.Verdict{}, fmt.Errorf("scriptedExec: gate %q dispatched a reviewer without a scripted verdict", gate)
	}
	if s.reviewCalls == nil {
		s.reviewCalls = map[string]int{}
	}
	n := s.reviewCalls[gate]
	s.reviewCalls[gate] = n + 1
	if n >= len(script) {
		n = len(script) - 1
	}
	return script[n], nil
}

func detTask(name, next string) apiv1.Task {
	return apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: name,
		Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		Next: next,
	}
}

func agenticTask(name, next string, capabilities ...string) apiv1.Task {
	return apiv1.Task{
		Name: name, Type: apiv1.TaskAgentic, Goober: "coder", Goal: name,
		Capabilities: capabilities, Next: next,
	}
}

func statusGate(name string, branches map[string]string) apiv1.Gate {
	return apiv1.Gate{
		Name: name, Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "status-equals"},
		Branches:  branches,
	}
}

func fixtureSpec(start string, tasks []apiv1.Task, gates []apiv1.Gate) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start: start, Tasks: tasks, Gates: gates,
	}
}

func podTask(name, next string, extra func(*apiv1.Task)) apiv1.Task {
	task := apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: name,
		Run:  &apiv1.DeterministicRun{Command: []string{name + ".sh"}, Workspace: apiv1.WorkspaceRepo},
		Next: next,
	}
	if extra != nil {
		extra(&task)
	}
	return task
}

func remotePin(stage string) PinnedPlacement {
	return PinnedPlacement{Stage: stage, Queue: dispatcher.QueueName("web", "linux"), Eligible: remoteEligible(), Memory: "2Gi"}
}

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
	in := runInput(name, placedGateSpec())
	in.DSLVersion = "3.0"
	in.GateGooberCapabilities = map[string][]string{"reviewer": {"agent:model"}}
	return in
}

func reviewSurrender(verdict apiv1.Verdict) dispatcher.SurrenderedResult {
	return dispatcher.SurrenderedResult{
		Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "reviewer verdict surrendered"},
		Verdict: &verdict,
	}
}

func refusingReviewer(t *testing.T) *fakeInvoker {
	t.Helper()
	return &fakeInvoker{
		invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
		},
		review: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			t.Errorf("ActReviewGoober ran for remotely pinned gate %s", env.TaskID)
			return apiv1.Verdict{Decision: apiv1.VerdictFail}, nil
		},
	}
}
