package childpod

import (
	"context"
	"errors"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type childTransportRun struct {
	client.WorkflowRun
	result engine.ChildDispatchResult
	get    func(context.Context) error
}

func (r *childTransportRun) Get(ctx context.Context, out any) error {
	if r.get != nil {
		if err := r.get(ctx); err != nil {
			return err
		}
	}
	*out.(*engine.ChildDispatchResult) = r.result
	return nil
}

type childTransportClient struct {
	client.Client
	run     *childTransportRun
	starts  int
	signals int
	options client.StartWorkflowOptions
	err     error
}

func (c *childTransportClient) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	c.starts++
	c.options = options
	if c.err != nil {
		return nil, c.err
	}
	if c.starts > 1 {
		return nil, &serviceerror.WorkflowExecutionAlreadyStarted{}
	}
	c.run.result.BindingDigest = args[0].(engine.ChildDispatchInput).BindingDigest()
	return c.run, nil
}
func (c *childTransportClient) GetWorkflow(context.Context, string, string) client.WorkflowRun {
	return c.run
}
func (c *childTransportClient) SignalWorkflow(ctx context.Context, _ string, _ string, name string, _ interface{}) error {
	if ctx.Err() != nil || name != engine.ChildDispatchStopSignal {
		return errors.New("wrong cleanup signal")
	}
	c.signals++
	return nil
}

func TestTemporalChildDispatchRejoinsAndWaitsForCleanup(t *testing.T) {
	a := dispatcher.Attempt{RunID: "run", Gaggle: "g", Stage: "s", Number: 1, PodAttempt: 8, ChildExecutionDigest: journal.Digest(nil)}
	eligible := []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage}}
	run := &childTransportRun{result: engine.ChildDispatchResult{Report: dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "uid", WorkspaceWritersStopped: true, SurrenderConfirmed: true}}}
	c := &childTransportClient{run: run}
	held := false
	d := TemporalDispatch{Client: c, WorkflowQueue: "main", DispatchQueue: "linux", Admit: func(context.Context) (func(), error) { held = true; return func() { held = false }, nil }}
	run.get = func(context.Context) error {
		if held {
			t.Fatal("authority lease held through pod execution")
		}
		return nil
	}
	for range 2 {
		report, err := d.Dispatch(t.Context(), a, eligible)
		if err != nil || report.ChildPodUID != "uid" {
			t.Fatal(report, err)
		}
	}
	if c.options.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE || c.options.ID != engine.ChildDispatchWorkflowID(a) || len(c.options.Memo) != 0 {
		t.Fatal("unsafe start options", c.options)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reads := 0
	run.get = func(ctx context.Context) error {
		reads++
		if reads == 1 {
			return context.Canceled
		}
		if ctx.Err() != nil {
			t.Fatal("cancelled cleanup wait")
		}
		return nil
	}
	report, err := d.Dispatch(ctx, a, eligible)
	if err != nil || !report.WorkspaceWritersStopped || c.signals != 1 || reads != 2 {
		t.Fatal("lost cleanup proof", report, err)
	}
	run.result.BindingDigest = "foreign"
	if report, err = d.Dispatch(t.Context(), a, eligible); err == nil || !report.ChildCreateAttempted || report.WorkspaceWritersStopped {
		t.Fatal("reused foreign custody accepted", report, err)
	}
	c.err = errors.New("unknown network outcome")
	if report, err = d.Dispatch(t.Context(), a, eligible); err == nil || !report.ChildCreateAttempted || report.WorkspaceWritersStopped {
		t.Fatal("ambiguous start claimed stopped", report, err)
	}
}

func TestTemporalChildDispatchAuthorityRefusalProvesNoSubmission(t *testing.T) {
	c := &childTransportClient{}
	d := TemporalDispatch{Client: c, WorkflowQueue: "main", DispatchQueue: "linux", Admit: func(context.Context) (func(), error) { return nil, errors.New("revoked") }}
	a := dispatcher.Attempt{RunID: "run", Gaggle: "g", Stage: "s", Number: 1, PodAttempt: 1, ChildExecutionDigest: journal.Digest(nil)}
	report, err := d.Dispatch(t.Context(), a, []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage}})
	if err == nil || report.ChildCreateAttempted || c.starts != 0 {
		t.Fatal(report, err, c.starts)
	}
}
