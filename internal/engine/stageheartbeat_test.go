package engine

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// #8230: replacing the registered CancelSafe activity with a scripted activity
// bypasses its heartbeat loop. Keep the production registration and substitute
// only the invoker underneath it, then inspect an actual new run's history.
func TestNewHistorySchedulesHeartbeatingCancelSafeStage(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	c := hitlReplayServer(t, ctx)
	const queue = "stage-heartbeat-contract"
	w := worker.New(c, queue, worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{&stageCancellationProbe{t: t}},
	})
	RegisterWith(w, &Activities{
		Goober: newScriptedExec(nil), Workspaces: testWorkspaces(t),
	})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	in := runInput(queue, fixtureSpec("work", []apiv1.Task{agenticTask("work", "")}, nil))
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: in.RunID, TaskQueue: queue,
	}, Run, in)
	if err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("run status = %q, want %q", result.Status, StatusCompleted)
	}
	history := hitlFetchHistory(t, ctx, c, run.GetID(), run.GetRunID())
	stages := 0
	for _, event := range history.Events {
		scheduled := event.GetActivityTaskScheduledEventAttributes()
		if scheduled == nil {
			continue
		}
		switch scheduled.GetActivityType().GetName() {
		case ActInvokeGoober:
			t.Fatal("new history scheduled the ordinary, non-heartbeating stage activity")
		case actInvokeGooberCancelSafe:
			stages++
			if got := scheduled.GetHeartbeatTimeout().AsDuration(); got != 10*time.Second {
				t.Errorf("stage heartbeat timeout = %s, want 10s", got)
			}
		}
	}
	if stages != 1 {
		t.Fatalf("scheduled cancel-safe stages = %d, want 1", stages)
	}
}

// WaitForCancellation is SDK-local and is not serialized in the scheduled
// event. Observe the production options without replacing or modifying them.
type stageCancellationProbe struct {
	interceptor.WorkerInterceptorBase
	t *testing.T
}

func (p *stageCancellationProbe) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	return &stageCancellationInbound{
		WorkflowInboundInterceptorBase: interceptor.WorkflowInboundInterceptorBase{Next: next},
		t:                              p.t,
	}
}

type stageCancellationInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	t *testing.T
}

func (p *stageCancellationInbound) Init(next interceptor.WorkflowOutboundInterceptor) error {
	return p.Next.Init(&stageCancellationOutbound{
		WorkflowOutboundInterceptorBase: interceptor.WorkflowOutboundInterceptorBase{Next: next},
		t:                               p.t,
	})
}

type stageCancellationOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	t *testing.T
}

func (p *stageCancellationOutbound) ExecuteActivity(ctx workflow.Context, name string, args ...any) workflow.Future {
	if name == actInvokeGooberCancelSafe && !workflow.GetActivityOptions(ctx).WaitForCancellation {
		p.t.Error("cancel-safe stage must wait for activity cancellation")
	}
	return p.Next.ExecuteActivity(ctx, name, args...)
}
