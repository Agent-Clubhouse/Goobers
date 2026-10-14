//go:build integration

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type childHandoffFixture struct {
	t          *testing.T
	mu         sync.Mutex
	envs       []apiv1.InvocationEnvelope
	accepted   chan apiv1.InvocationEnvelope
	waiting    chan struct{}
	complete   chan struct{}
	yielded    bool
	suspended  bool
	reacquired bool
}

func (f *childHandoffFixture) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	f.mu.Lock()
	f.envs = append(f.envs, env)
	call := len(f.envs)
	f.mu.Unlock()
	ack := invoke.RegisterWorkspaceWriter(ctx)
	if ack != nil {
		defer ack(nil)
	}
	if call == 1 {
		if err := os.WriteFile(filepath.Join(env.Workspace, "pending.txt"), []byte("preserve parent edits"), 0600); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		f.accepted <- env
		<-ctx.Done()
		return apiv1.ResultEnvelope{}, ctx.Err()
	}
	data, err := os.ReadFile(filepath.Join(env.Workspace, "pending.txt"))
	if err != nil || string(data) != "preserve parent edits" {
		return apiv1.ResultEnvelope{}, errors.New("continuation reset parent's edits")
	}
	if len(env.ContextPointers) == 0 || env.ContextPointers[len(env.ContextPointers)-1].Name != "child-completion" {
		return apiv1.ResultEnvelope{}, errors.New("continuation lost child receipt")
	}
	f.mu.Lock()
	reacquired := f.reacquired
	f.mu.Unlock()
	if !reacquired {
		return apiv1.ResultEnvelope{}, errors.New("continuation launched before capacity reacquisition")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (*childHandoffFixture) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	panic("unexpected review")
}
func (f *childHandoffFixture) Await(ctx context.Context, _ apiv1.InvocationEnvelope) (ChildHandoffRequest, error) {
	select {
	case env := <-f.accepted:
		return ChildHandoffRequest{Gaggle: "web", ParentRunID: "origin-run", RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *env.ChildWorkflowOrigin}, nil
	case <-ctx.Done():
		return ChildHandoffRequest{}, ctx.Err()
	}
}
func (f *childHandoffFixture) Yield(_ context.Context, _ ChildHandoffRequest, custody ChildWorkspaceCustody) error {
	if _, err := os.Stat(filepath.Join(custody.Path, "pending.txt")); err != nil {
		return err
	}
	f.mu.Lock()
	f.yielded = true
	f.mu.Unlock()
	return nil
}
func (f *childHandoffFixture) Wait(ctx context.Context, _ ChildHandoffRequest) (ChildHandoffCompletion, error) {
	close(f.waiting)
	select {
	case <-f.complete:
		return ChildHandoffCompletion{State: "completed", ResultRef: "verified-child-result", Summary: "child completed"}, nil
	case <-ctx.Done():
		return ChildHandoffCompletion{}, ctx.Err()
	}
}
func (f *childHandoffFixture) SuspendChildParent(context.Context, string) (ChildParentSuspension, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.yielded {
		return nil, errors.New("suspended before custody")
	}
	f.suspended = true
	return f, nil
}
func (f *childHandoffFixture) Resume(context.Context) error {
	f.mu.Lock()
	f.reacquired = true
	f.mu.Unlock()
	return nil
}

func TestIntegrationChildWaitRetainsParentWorkspaceAndRetryAllowance(t *testing.T) {
	testdep.Require(t, "git")
	r, run, frame, f, fork := prepareChildWaitRuntime(t)
	done := make(chan error, 1)
	go func() { _, _, err := r.runTask(t.Context(), frame, 0, 1, "", "", nil, false, nil); done <- err }()
	select {
	case <-f.waiting:
	case err := <-done:
		t.Fatalf("ended before waiting: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("did not park")
	}
	reader, _ := journal.OpenRead(run.Dir())
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if !ParkedOnChild(events) {
		t.Fatal("active wait has no durable marker")
	}
	// A crash before the wait marker must recover the same unconsumed slot.
	var accountingFound bool
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == childAttemptAccountingKind {
			value, err := readChildAttemptAccounting(event, *f.envs[0].ChildWorkflowOrigin, accountingFound)
			if err != nil || value.PolicyAttempts != 0 || value.InfrastructureFailures != 0 {
				t.Fatal("pre-dispatch recovery spent the child wait allowance", value, err)
			}
			accountingFound = true
		}
	}
	if !accountingFound {
		t.Fatal("missing pre-dispatch recovery accounting")
	}
	if _, stalled, err := inspectStalledCandidate(run.Dir(), frame.in.RunID, time.Now().Add(24*time.Hour), time.Second, nil); err != nil || stalled {
		t.Fatalf("child wait treated as stalled: %v %v", stalled, err)
	}
	if _, err := fork.config.Worktrees.FinalizeRun(t.Context(), frame.in.RunID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fork.config.Worktrees.Reap(t.Context(), worktree.ReapOptions{StaleAfter: time.Nanosecond, IsRunAbandoned: func(string, string) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	close(f.complete)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not continue")
	}
	f.mu.Lock()
	envs := append([]apiv1.InvocationEnvelope(nil), f.envs...)
	f.mu.Unlock()
	if len(envs) != 2 || envs[0].ChildWorkflowOrigin.StageOccurrence != envs[1].ChildWorkflowOrigin.StageOccurrence || envs[0].ChildWorkflowOrigin.AttemptID == envs[1].ChildWorkflowOrigin.AttemptID {
		t.Fatalf("continuation identity: %+v", envs)
	}
	events, _ = reader.Events()
	if ParkedOnChild(events) {
		t.Fatal("completed stage stayed parked")
	}
	for _, event := range events {
		if event.Type == journal.EventError || event.Type == journal.EventStageFinished && event.Status == string(apiv1.ResultFailure) {
			t.Fatalf("yield consumed failure allowance: %+v", event)
		}
	}
	fork.assertParentUnchanged(t)
}

func prepareChildWaitRuntime(t *testing.T) (*Runner, *journal.Run, taskFrame, *childHandoffFixture, childWorkspaceFixture) {
	t.Helper()
	testdep.Require(t, "git")
	fork := prepareChildWorkspaceFixture(t, false)
	r, run, frame := childOriginRuntime(t, &childOriginGoober{})
	f := &childHandoffFixture{t: t, accepted: make(chan apiv1.InvocationEnvelope, 1), waiting: make(chan struct{}), complete: make(chan struct{})}
	r.cfg.Worktrees = fork.config.Worktrees
	r.cfg.RepoCloneURL = fork.config.RepoCloneURL
	r.cfg.ChildHandoff = f
	r.cfg.ChildParentCapacity = f
	r.cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return f, nil }
	frame.branchRecorded = new(bool)
	frame.reboundRecorded = new(string)
	frame.ex = childOriginExecutors(r.cfg, run)
	frame.in.RepoRef = fork.input.RepoRef
	frame.t.Workspace = apiv1.WorkspaceRepo
	frame.t.Retry.MaxAttempts = 1
	return r, run, frame, f, fork
}

func TestIntegrationChildWaitRecoversBeforeAndAfterContinuedMarker(t *testing.T) {
	testdep.Require(t, "git")
	for _, continued := range []bool{false, true} {
		name := "parked"
		if continued {
			name = "continued-before-dispatch"
		}
		t.Run(name, func(t *testing.T) {
			r, run, frame, f, fork := prepareChildWaitRuntime(t)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, _, err := r.runTask(ctx, frame, 0, 1, "", "keep this human guidance", nil, false, nil)
				done <- err
			}()
			select {
			case <-f.waiting:
			case <-time.After(10 * time.Second):
				t.Fatal("did not park")
			}
			cancel()
			if err := <-done; !errors.Is(err, errChildWaitDrain) {
				t.Fatalf("shutdown lost wait: %v", err)
			}
			reader, _ := journal.OpenRead(run.Dir())
			events, _ := reader.Events()
			record, event, err := pendingChildWait(events)
			if err != nil || record == nil {
				t.Fatalf("missing wait: %v", err)
			}
			if continued {
				pointer, err := recordChildCompletion(&frame, event.Attempt, event.AttemptClass, *record, ChildHandoffCompletion{State: "completed", ResultRef: "verified-result"})
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: frame.t.Name, Attempt: event.Attempt, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": record.Request.RequestID, "context": pointer}}); err != nil {
					t.Fatal(err)
				}
				f.reacquired = true
			}
			dir := run.Dir()
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, _, err := journal.Recover(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = recovered.Close() }()
			reader, _ = journal.OpenRead(dir)
			events, _ = reader.Events()
			resumed, ok := recoverChildTaskContext(events, frame.t.Name)
			if !ok || resumed.childWaitErr != nil {
				t.Fatalf("recovery context: %+v", resumed)
			}
			f.waiting = make(chan struct{})
			frame.jr = recovered
			frame.ex = childOriginExecutors(r.cfg, recovered)
			frame.childWaitResume = resumed.childWait
			frame.childWaitAttempt = resumed.attempt
			frame.childWaitClass = resumed.class
			frame.childWaitCompletion = resumed.childWaitCompletion
			accounting := &resumeRetryAccounting{policyAttempts: resumed.childWait.PolicyAttempts, infrastructureFailures: resumed.childWait.InfrastructureFailures, replacementConsumesPolicy: true}
			go func() {
				_, _, err := r.runTask(t.Context(), frame, 0, int32(resumed.attempt)+1, resumed.class, "", nil, false, accounting)
				done <- err
			}()
			if !continued {
				select {
				case <-f.waiting:
				case err := <-done:
					t.Fatalf("resume ended: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("resume did not wait")
				}
				close(f.complete)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("resume did not finish")
			}
			f.mu.Lock()
			envs := append([]apiv1.InvocationEnvelope(nil), f.envs...)
			f.mu.Unlock()
			if len(envs) != 2 || envs[1].InstructionAddendum != "keep this human guidance" || envs[0].ChildWorkflowOrigin.StageOccurrence != envs[1].ChildWorkflowOrigin.StageOccurrence {
				t.Fatalf("lost resumed context: %+v", envs)
			}
			fork.assertParentUnchanged(t)
		})
	}
}
