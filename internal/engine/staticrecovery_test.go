package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// This composes the scheduler with the real dispatch reattachment loop. The
// injected boundary is the dispatch activity's recorded handoff result, not a
// replacement branch scheduler. Physical pod adoption is separately qualified.
func TestConformanceStaticBranchHandoffKeepsAttemptAndJoinBarrier(t *testing.T) {
	in := staticSchedulerInput{Run: branchBodyInput("branch-handoff", false, false).Run}
	in.Run.Placements = []PinnedPlacement{remotePin("alpha")}
	writer, dir := newLiveWriter(t)
	journalSink := &siblingSettlementEmitter{JournalEmitter: writer, settled: make(chan struct{})}
	handoff := make(chan struct{})
	var recovered, joined atomic.Bool
	acts := &Activities{Journal: journalSink, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		stage := stageOfTaskID(env.TaskID)
		if stage == "beta" {
			select {
			case <-handoff:
			case <-ctx.Done():
				return apiv1.ResultEnvelope{}, ctx.Err()
			}
		}
		if stage == "join" {
			if !recovered.Load() {
				return apiv1.ResultEnvelope{}, fmt.Errorf("join dispatched before branch reattachment settled")
			}
			joined.Store(true)
		}
		return recoveryResult(stage), nil
	}}}
	custody := dispatcher.PodCustody{Namespace: "gaggle-web", Pod: "alpha", UID: "alpha-uid", Attempt: 1, Custody: true}
	var inputs []DispatchStageInput
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(acts)
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(ctx context.Context, input DispatchStageInput) (DispatchStageResult, error) {
		inputs = append(inputs, input)
		if len(inputs) == 1 {
			close(handoff)
			return DispatchStageResult{}, dispatchWorkerHandoff(custody)
		}
		select {
		case <-journalSink.settled:
		case <-ctx.Done():
			return DispatchStageResult{}, ctx.Err()
		}
		if joined.Load() {
			return DispatchStageResult{}, fmt.Errorf("join ran while branch was held for recovery")
		}
		recovered.Store(true)
		return DispatchStageResult{ResultEnvelope: recoveryResult("alpha")}, nil
	})
	env.ExecuteWorkflow(staticScheduler, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "" || out.Result.Status != StatusCompleted || !joined.Load() {
		t.Fatalf("handoff result: %+v", out)
	}
	if len(inputs) != 2 || inputs[0].Envelope.Attempt != 1 || inputs[1].Envelope.Attempt != 1 || inputs[0].Class != "" || inputs[1].Class != "" || inputs[1].Custody == nil || *inputs[1].Custody != custody {
		t.Fatalf("handoff changed attempt or custody: %+v", inputs)
	}
	engine := liveEvents(t, dir, in.Run.RunID)
	assertRecoveryBarrier(t, engine)
	local := runLocalFixture(t, conformanceFixture{spec: in.Run.Spec, script: map[string][]scriptedCall{
		"seed": {succeed(recoveryResult("seed").Outputs)}, "alpha": {succeed(recoveryResult("alpha").Outputs)},
		"beta": {succeed(recoveryResult("beta").Outputs)}, "join": {succeed(recoveryResult("join").Outputs)},
	}}, "local-branch-handoff")
	for branch := 1; branch <= 2; branch++ {
		want, got := branchNormativeEvents(local, branch), branchNormativeEvents(engine, branch)
		if len(want) < 4 || !slices.Equal(want, got) {
			t.Fatalf("branch %d journal differs across handoff:\nlocal=%+v\nengine=%+v", branch, want, got)
		}
	}
}

func recoveryResult(stage string) apiv1.ResultEnvelope {
	value := stage
	if stage == "seed" {
		value = "frozen-root"
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": value}}
}

type siblingSettlementEmitter struct {
	JournalEmitter
	settled chan struct{}
	once    sync.Once
}

func (e *siblingSettlementEmitter) Emit(ctx context.Context, in livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	out, err := e.JournalEmitter.Emit(ctx, in)
	if err == nil {
		for _, op := range in.Ops {
			if op.Event != nil && op.Event.Type == journal.EventBranchFinished && op.Event.Branch == 2 {
				e.once.Do(func() { close(e.settled) })
			}
		}
	}
	return out, err
}

func branchNormativeEvents(events []journal.Event, branch int) []journal.NormativeEvent {
	var selected []journal.Event
	for _, event := range events {
		if event.Branch == branch {
			selected = append(selected, event)
		}
	}
	return journal.ConformanceView(selected)
}

func assertRecoveryBarrier(t *testing.T, events []journal.Event) {
	t.Helper()
	starts, finishes := 0, 0
	var alphaSettled, betaSettled, parallelFinished, joinStarted uint64
	for _, event := range events {
		switch {
		case event.Type == journal.EventStageStarted && event.Stage == "alpha":
			starts++
		case event.Type == journal.EventStageFinished && event.Stage == "alpha":
			finishes++
		case event.Type == journal.EventBranchFinished && event.Branch == 1:
			alphaSettled = event.Seq
		case event.Type == journal.EventBranchFinished && event.Branch == 2:
			betaSettled = event.Seq
		case event.Type == journal.EventParallelFinished:
			parallelFinished = event.Seq
		case event.Type == journal.EventStageStarted && event.Stage == "join":
			joinStarted = event.Seq
		}
	}
	if starts != 1 || finishes != 1 || betaSettled == 0 || alphaSettled <= betaSettled || parallelFinished <= alphaSettled || joinStarted <= parallelFinished {
		t.Fatalf("recovery barrier/attempt custody: starts=%d finishes=%d beta=%d alpha=%d parallel=%d join=%d", starts, finishes, betaSettled, alphaSettled, parallelFinished, joinStarted)
	}
}
