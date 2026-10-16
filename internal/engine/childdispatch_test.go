package engine

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type childWireDispatcher struct {
	calls   int
	attempt dispatcher.Attempt
}

func (d *childWireDispatcher) Dispatch(_ context.Context, a dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	d.calls++
	d.attempt = a
	return dispatcher.Report{Runner: "linux", ChildCreateAttempted: true, ChildPodUID: "exact-uid", WorkspaceWritersStopped: true, SurrenderConfirmed: true, Disposed: true, DisposeErr: errors.New("safe local error")}, dispatcher.ErrStageFailed
}

func TestChildDispatchWorkflowPreservesProofAndBindsPayload(t *testing.T) {
	in := ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: "run", Gaggle: "g", Stage: "test", Number: 1, PodAttempt: 4, ChildExecutionDigest: journal.Digest([]byte("contract"))}, Eligible: []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage, Host: "image:v1"}}, Queue: "dispatch-linux"}
	d := &childWireDispatcher{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: ChildDispatchWorkflowID(in.Attempt)})
	env.RegisterWorkflow(ChildDispatchOne)
	env.RegisterActivity((&Activities{Dispatcher: d}).DispatchChildPod)
	env.ExecuteWorkflow(ChildDispatchOne, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out ChildDispatchResult
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if d.calls != 1 || d.attempt.OwningWorkflowID != ChildDispatchWorkflowID(in.Attempt) || out.BindingDigest != in.BindingDigest() || out.Report.ChildPodUID != "exact-uid" || !out.Report.WorkspaceWritersStopped || out.Report.DisposeErr != nil || !out.DisposalFailed || !errors.Is(out.DispatchError(), dispatcher.ErrStageFailed) {
		t.Fatalf("wire custody changed: %+v", out)
	}
}

func TestChildDispatchRejectsUnsupportedOrCredentialBearingTransport(t *testing.T) {
	in := ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: "run", Gaggle: "g", Stage: "s", Number: 1, PodAttempt: 1, ChildExecutionDigest: journal.Digest(nil)}, Eligible: []dispatcher.RunnerSpec{{OS: "linux", HostKind: instance.RunnerHostImage}}, Queue: "q"}
	for _, alter := range []func(*ChildDispatchInput){
		func(i *ChildDispatchInput) { i.Attempt.PodToken = "secret" },
		func(i *ChildDispatchInput) { i.Attempt.PlaneTokens.Claims = "secret" },
		func(i *ChildDispatchInput) { i.Attempt.CheckoutCapability = "repo:read" },
		func(i *ChildDispatchInput) {
			i.Eligible = []dispatcher.RunnerSpec{{OS: "linux", HostKind: instance.RunnerHostSelf}}
		},
	} {
		copy := in
		alter(&copy)
		if copy.Validate() == nil {
			t.Fatal("unsupported request admitted")
		}
	}
}
