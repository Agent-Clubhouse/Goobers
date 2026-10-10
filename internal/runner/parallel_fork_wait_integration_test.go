//go:build integration

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// A one-slot compiled graph must run the queued sibling while its first branch
// waits durably. This fixture models host-accepted child completion; the queue,
// worker transport and result-disposition owners have separate qualification.
func TestIntegrationCompiledParallelChildWait(t *testing.T) {
	testdep.Require(t, "git")
	waiting := &compiledChildWait{accepted: make(chan apiv1.InvocationEnvelope, 1), yielded: make(chan struct{}), complete: make(chan struct{})}
	verifyCompiledForkJoin(t, 1, waiting)
}

type compiledChildWait struct {
	mu                 sync.Mutex
	accepted           chan apiv1.InvocationEnvelope
	yielded            chan struct{}
	complete           chan struct{}
	attempts           []apiv1.InvocationEnvelope
	suspended, resumed bool
}

type compiledWaitingAgent struct {
	base     invoke.Goober
	waiting  *compiledChildWait
	recorder ArtifactRecorder
}

func (a *compiledWaitingAgent) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	f := a.waiting
	if env.TaskID == env.RunID+":a" {
		f.mu.Lock()
		f.attempts = append(f.attempts, env)
		first := len(f.attempts) == 1
		f.mu.Unlock()
		if first {
			return a.request(ctx, env)
		}
		if err := f.checkContinuation(env); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
	}
	if env.TaskID == env.RunID+":b" {
		select {
		case <-f.yielded:
		default:
			return apiv1.ResultEnvelope{}, errors.New("sibling ran before first branch yielded custody")
		}
		result, err := a.base.Invoke(ctx, env)
		close(f.complete)
		return result, err
	}
	return a.base.Invoke(ctx, env)
}

func (a *compiledWaitingAgent) request(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	if err := os.WriteFile(filepath.Join(env.Workspace, "pending.txt"), []byte("retained branch edits"), 0600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	a.waiting.accepted <- env
	<-ctx.Done()
	if err := recordCompiledParentReturn(a.recorder, env); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{}, ctx.Err()
}

func (a *compiledWaitingAgent) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return a.base.Review(ctx, env)
}

func (f *compiledChildWait) Await(ctx context.Context, env apiv1.InvocationEnvelope) (ChildHandoffRequest, error) {
	if env.TaskID != env.RunID+":a" || env.Attempt != 1 {
		<-ctx.Done()
		return ChildHandoffRequest{}, ctx.Err()
	}
	select {
	case original := <-f.accepted:
		return ChildHandoffRequest{Gaggle: env.Gaggle, ParentRunID: env.RunID, RequestID: journal.Digest([]byte("compiled-wait")), Action: "wait", ChildRunID: "compiled-child", AcceptanceID: "trigger-compiled-child", InvocationKey: "child", SourceDigest: journal.Digest([]byte("child-source")), Origin: *original.ChildWorkflowOrigin}, nil
	case <-ctx.Done():
		return ChildHandoffRequest{}, ctx.Err()
	}
}

func (f *compiledChildWait) Yield(_ context.Context, _ ChildHandoffRequest, custody ChildWorkspaceCustody) error {
	data, err := os.ReadFile(filepath.Join(custody.Path, "pending.txt"))
	if err != nil {
		return err
	}
	if string(data) != "retained branch edits" {
		return errors.New("yield lost branch edits")
	}
	close(f.yielded)
	return nil
}

func (f *compiledChildWait) Wait(ctx context.Context, _ ChildHandoffRequest) (ChildHandoffCompletion, error) {
	select {
	case <-f.complete:
		return ChildHandoffCompletion{State: "completed", ResultRef: "verified-compiled-child", Summary: "child complete"}, nil
	case <-ctx.Done():
		return ChildHandoffCompletion{}, ctx.Err()
	}
}

func (f *compiledChildWait) SuspendChildParent(context.Context, string) (ChildParentSuspension, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspended = true
	return f, nil
}
func (f *compiledChildWait) Resume(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.suspended {
		return errors.New("resumed without parent suspension")
	}
	f.resumed = true
	return nil
}

func (f *compiledChildWait) checkContinuation(env apiv1.InvocationEnvelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	first := f.attempts[0]
	if env.Workspace != first.Workspace || env.ChildWorkflowOrigin.StageOccurrence != first.ChildWorkflowOrigin.StageOccurrence || env.ChildWorkflowOrigin.AttemptID == first.ChildWorkflowOrigin.AttemptID {
		return errors.New("continuation replaced workspace or invocation occurrence")
	}
	if !f.resumed {
		return errors.New("continuation ran before parent permit reacquisition")
	}
	data, err := os.ReadFile(filepath.Join(env.Workspace, "pending.txt"))
	if err != nil {
		return err
	}
	if string(data) != "retained branch edits" {
		return errors.New("continuation lost branch edits")
	}
	for _, pointer := range env.ContextPointers {
		if pointer.Name == "child-completion" {
			return nil
		}
	}
	return errors.New("continuation lost child completion")
}

func (f *compiledChildWait) verify(t *testing.T, run *journal.Run) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.attempts) != 2 || !f.suspended || !f.resumed {
		t.Fatalf("attempts=%d suspended=%v resumed=%v", len(f.attempts), f.suspended, f.resumed)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var waited, siblingFinished, continued uint64
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) {
			t.Fatal("child wait charged a stage failure", event)
		}
		if event.Branch == 1 && event.Runner["kind"] == ChildWaitKind {
			waited = event.Seq
		}
		if event.Branch == 2 && event.Type == journal.EventBranchFinished {
			siblingFinished = event.Seq
		}
		if event.Branch == 1 && event.Runner["kind"] == ChildContinuedKind {
			continued = event.Seq
		}
	}
	if waited == 0 || siblingFinished <= waited || continued <= siblingFinished {
		t.Fatalf("wait=%d sibling=%d continuation=%d", waited, siblingFinished, continued)
	}
	if ParkedOnChild(events) {
		t.Fatal("finished parallel remained parked")
	}
}
