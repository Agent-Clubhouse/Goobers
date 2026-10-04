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
	in := d.RetainedInput(a, eligible)
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
	return checkedDispatchResult(in, result)
}

func checkedDispatchResult(in engine.ChildDispatchInput, result engine.ChildDispatchResult) (dispatcher.Report, error) {
	unknown := dispatcher.Report{ChildCreateAttempted: true}
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

// RetainedInput returns the exact immutable worker payload for host recovery.
func (d TemporalDispatch) RetainedInput(a dispatcher.Attempt, eligible []dispatcher.RunnerSpec) engine.ChildDispatchInput {
	return engine.ChildDispatchInput{Attempt: a, Eligible: eligible, Queue: d.DispatchQueue}
}

// Reconcile stops and rejoins an existing exact attempt. It never calls
// ExecuteWorkflow, consults current placement, or obtains new launch authority.
// Missing history is uncertainty, not evidence that no pod was created.
func (d TemporalDispatch) Reconcile(ctx context.Context, in engine.ChildDispatchInput) (dispatcher.Report, error) {
	unknown := dispatcher.Report{ChildCreateAttempted: true}
	if d.Client == nil {
		return unknown, errors.New("isolated worker recovery unavailable")
	}
	if err := in.Validate(); err != nil {
		return unknown, err
	}
	bounded, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	id := engine.ChildDispatchWorkflowID(in.Attempt)
	// A completed workflow may reject the stop signal; its immutable terminal
	// result can still prove custody. The result binding is always checked.
	signalErr := d.Client.SignalWorkflow(bounded, id, "", engine.ChildDispatchStopSignal, true)
	run := d.Client.GetWorkflow(bounded, id, "")
	if run == nil {
		return unknown, errors.Join(signalErr, errors.New("isolated worker history unavailable"))
	}
	var result engine.ChildDispatchResult
	if err := run.Get(bounded, &result); err != nil {
		return unknown, errors.Join(signalErr, err)
	}
	return checkedDispatchResult(in, result)
}
