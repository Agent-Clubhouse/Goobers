package childpod

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
)

// TemporalClient is the existing authenticated daemon/worker transport. No
// Kubernetes credential or client is introduced into the daemon.
type TemporalClient interface {
	ExecuteWorkflow(context.Context, client.StartWorkflowOptions, interface{}, ...interface{}) (client.WorkflowRun, error)
	GetWorkflow(context.Context, string, string) client.WorkflowRun
	SignalWorkflow(context.Context, string, string, string, interface{}) error
}

// TemporalDispatch rejoins a durable attempt instead of creating another pod.
type TemporalDispatch struct {
	Client        TemporalClient
	WorkflowQueue string
	DispatchQueue string
	// Admit holds current applied authority only across durable transport
	// acceptance. The long-running pod does not block configuration reload.
	Admit func(context.Context) (func(), error)
}

// Dispatch submits or rejoins one worker-owned pod attempt and retains cleanup proof.
func (d TemporalDispatch) Dispatch(ctx context.Context, a dispatcher.Attempt, eligible []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	in := engine.ChildDispatchInput{Attempt: a, Eligible: eligible, Queue: d.DispatchQueue}
	if d.Client == nil || d.WorkflowQueue == "" {
		return dispatcher.Report{}, errors.New("isolated child worker transport unavailable")
	}
	if err := in.Validate(); err != nil {
		return dispatcher.Report{}, err
	}
	id := engine.ChildDispatchWorkflowID(a)
	run, attempted, err := d.submit(ctx, id, in)
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &already) {
		run, err = d.Client.GetWorkflow(context.WithoutCancel(ctx), id, ""), nil
	}
	// An unknown transport result may already have created a pod. Never report
	// an empty no-create proof, including when the initial RPC was cancelled.
	unknown := dispatcher.Report{ChildCreateAttempted: true}
	if !attempted {
		return dispatcher.Report{}, err
	}
	if err != nil {
		return unknown, err
	}
	var result engine.ChildDispatchResult
	err = run.Get(ctx, &result)
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
		defer cancel()
		if signalErr := d.Client.SignalWorkflow(cleanup, id, "", engine.ChildDispatchStopSignal, true); signalErr != nil {
			return unknown, signalErr
		}
		err = run.Get(cleanup, &result)
	}
	if err != nil {
		return unknown, err
	}
	if result.BindingDigest != in.BindingDigest() {
		return unknown, errors.New("isolated child worker response differs from submitted custody")
	}
	if result.DisposalFailed {
		result.Report.DisposeErr = errors.New("isolated child pod disposal unconfirmed")
	}
	return result.Report, result.DispatchError()
}

func (d TemporalDispatch) submit(ctx context.Context, id string, in engine.ChildDispatchInput) (client.WorkflowRun, bool, error) {
	if d.Admit != nil {
		release, err := d.Admit(ctx)
		if err != nil {
			return nil, false, err
		}
		defer release()
	}
	run, err := d.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: d.WorkflowQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, engine.ChildDispatchOne, in)
	return run, true, err
}
