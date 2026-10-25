package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

// childFixture is a fault-injectable admission state machine, not an HA
// backend. It records every effect so tests can assert ordering and counts.
type childFixture struct {
	now         func() time.Time
	mu          sync.Mutex
	bindings    map[string]ChildRunBinding
	manifests   map[string][]byte
	calls       []string
	envelopes   []apiv1.InvocationEnvelope
	admissions  []ChildAdmissionInput
	cancelled   []string
	forgeOrigin bool
	// handoffs is how many leading dispatches request a child.
	handoffs   int
	admitErr   error
	corrupt    bool
	changeLoad func(*FrozenChildManifest)
	// Custody state: one shared permit and fault injection for execution.
	parentSlots     int
	childSlots      int
	terminal        map[string]childresult.TerminalReturn
	parks           []ChildParkInput
	parkAckLoss     bool
	completeAckLoss bool
	failChild       bool
	blockChild      bool
	childStarted    chan struct{}
	childStopped    chan struct{}
	waitDuration    time.Duration
	usage           *AttemptUsage
	parentIntegrity apiv1.Integrity
}

func newChildFixture() *childFixture {
	return &childFixture{now: time.Now, bindings: map[string]ChildRunBinding{}, manifests: map[string][]byte{}, handoffs: 1,
		parentSlots: 1, terminal: map[string]childresult.TerminalReturn{}, waitDuration: time.Hour,
		childStarted: make(chan struct{}, 1), childStopped: make(chan struct{}, 1)}
}

func childParentInput() RunInput {
	t := apiv1.Task{Name: "parent", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "parent", Workspace: apiv1.WorkspaceScratch, Next: wf.TerminalComplete,
		ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}
	in := runInput("child-parent", fixtureSpec("parent", []apiv1.Task{t}, nil))
	in.DSLVersion = "3.1"
	in.LiveJournal = true
	return in
}

func childSourceBytes(request journal.ChildHandoffRequest) []byte {
	return []byte("proposal for " + request.ChildRunID)
}

func (f *childFixture) Invoke(_ context.Context, env apiv1.InvocationEnvelope, _ string, _ string, _ apiv1.WorkspaceMode, _ string) (DispatchStageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parentSlots != 1 || f.childSlots != 0 {
		return DispatchStageResult{}, errors.New("parent dispatched without exclusive capacity")
	}
	f.envelopes = append(f.envelopes, env)
	f.calls = append(f.calls, "dispatch")
	if len(f.envelopes) > f.handoffs {
		return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: f.parentIntegrity}}, nil
	}
	origin := apiv1.ChildWorkflowOrigin{}
	if env.ChildWorkflowOrigin != nil {
		origin = *env.ChildWorkflowOrigin
	}
	childID := fmt.Sprintf("%s-child-%d", env.RunID, len(f.envelopes))
	request := journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: env.RunID, RequestID: journal.Digest([]byte(childID)), Action: "wait", ChildRunID: childID,
		AcceptanceID: "trigger-" + childID, InvocationKey: childID, Origin: origin}
	request.SourceDigest = journal.Digest(childSourceBytes(request))
	if f.forgeOrigin {
		request.Origin.AttemptID = journal.StageAttemptID("other", 0, "parent", 1)
	}
	return DispatchStageResult{ChildWait: &request, Usage: f.usage}, nil
}

// Det is the generated child's only stage. It requires the permit the parked
// parent released, which is what proves parking precedes child execution.
func (f *childFixture) Det(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun, _ string, _ string) (DispatchStageResult, error) {
	f.mu.Lock()
	if f.parentSlots != 0 || f.childSlots != 0 {
		f.mu.Unlock()
		return DispatchStageResult{}, errors.New("child ran without the released permit")
	}
	f.childSlots = 1
	f.calls = append(f.calls, "child-stage")
	block, fail := f.blockChild, f.failChild
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.childSlots = 0
		f.mu.Unlock()
		select {
		case f.childStopped <- struct{}{}:
		default:
		}
	}()
	select {
	case f.childStarted <- struct{}{}:
	default:
	}
	if block {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return DispatchStageResult{}, ctx.Err()
			case <-ticker.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}
	if fail {
		return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{Code: "child-failed", Message: "child failed"}}}, nil
	}
	return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
}

func (f *childFixture) Park(_ context.Context, in ChildParkInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "park")
	f.parks = append(f.parks, in)
	f.parentSlots = 0
	if f.parkAckLoss {
		f.parkAckLoss = false
		return errors.New("park committed; acknowledgement lost")
	}
	return nil
}

func (f *childFixture) Finalize(_ context.Context, in ChildFinalizeInput) (childresult.TerminalReturn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.childSlots != 0 {
		return childresult.TerminalReturn{}, errors.New("child writers still active")
	}
	f.calls = append(f.calls, "finalize")
	b := in.Binding
	r := childresult.TerminalReturn{Version: 1, Identity: b.Child.Identity, AcceptanceID: b.Child.AcceptanceID, RunID: b.Child.RunID, SourceDigest: b.Child.ProposalDigest,
		Input: childresult.TerminalResultInput{State: in.State, FinishedAt: in.FinishedAt, Summary: "bounded child summary"}, ResultRef: journal.Digest([]byte("result"))}
	f.terminal[b.Request.RequestID] = r
	return r, nil
}

func (f *childFixture) Complete(_ context.Context, in ChildCompletionInput) (apiv1.ContextPointer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.childSlots != 0 || f.terminal[in.Binding.Request.RequestID].ResultRef != in.Result.ResultRef {
		return apiv1.ContextPointer{}, errors.New("result custody incomplete")
	}
	f.calls = append(f.calls, "complete")
	f.parentSlots = 1
	if f.completeAckLoss {
		f.completeAckLoss = false
		return apiv1.ContextPointer{}, errors.New("resume committed; acknowledgement lost")
	}
	return apiv1.ContextPointer{Name: "child-completion", Artifact: &apiv1.ArtifactPointer{Path: "stages/parent/child-completion.json", Digest: journal.Digest([]byte("continuation")), Size: 512}}, nil
}

// childManifestFor freezes a serial task/gate graph for an admitted request.
func childManifestFor(request journal.ChildHandoffRequest) FrozenChildManifest {
	child := runInput("generated", fixtureSpec("work", []apiv1.Task{detTask("work", "check")}, []apiv1.Gate{statusGate("check", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})}))
	child.RunID = request.ChildRunID
	child.LiveJournal = true
	child.DSLVersion = "3.1"
	child.GooberDigest = journal.Digest([]byte("kit"))
	child.ConfigGeneration = journal.Digest([]byte("config"))
	child.WorkflowDigest, _ = wf.ComputeDigest(wf.Definition{Name: child.WorkflowName, Version: child.Version, DSLVersion: child.DSLVersion, Spec: child.Spec})
	source := childSourceBytes(request)
	return FrozenChildManifest{Version: 1, Source: source, SourceDigest: journal.Digest(source), CanonicalDigest: journal.Digest([]byte("canonical")), Input: child}
}

func childBindingFor(in ChildAdmissionInput, manifest []byte, accepted time.Time) ChildRunBinding {
	r := in.Request
	return ChildRunBinding{Request: r, ParentWorkflow: in.ParentWorkflow, ParentWorkflowID: in.ParentWorkflowID, ParentTemporalRunID: in.ParentTemporalRunID,
		ManifestDigest: journal.Digest(manifest), Deadline: accepted.Add(time.Hour),
		Child: triggerqueue.ChildRecord{Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: r.Gaggle, ParentRunID: r.ParentRunID}, StageOccurrence: r.Origin.StageOccurrence, InvocationKey: r.InvocationKey},
			AcceptanceID: r.AcceptanceID, RunID: r.ChildRunID, ProposalDigest: r.SourceDigest, AcceptedAt: accepted}}
}

func (f *childFixture) Admit(_ context.Context, in ChildAdmissionInput) (ChildRunBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admissions = append(f.admissions, in)
	if f.admitErr != nil {
		return ChildRunBinding{}, f.admitErr
	}
	if old, ok := f.bindings[in.Request.RequestID]; ok {
		return old, nil
	}
	f.calls = append(f.calls, "admit")
	manifest := childManifestFor(in.Request)
	manifest.Input.ChildWorkflowsEnabled = in.ChildWorkflowsEnabled
	if f.changeLoad != nil {
		f.changeLoad(&manifest)
	}
	data, _ := json.Marshal(manifest)
	b := childBindingFor(in, data, f.now().UTC())
	b.Deadline = b.Child.AcceptedAt.Add(f.waitDuration)
	f.bindings[in.Request.RequestID] = b
	f.manifests[b.ManifestDigest] = data
	return b, nil
}

func (f *childFixture) Load(_ context.Context, b ChildRunBinding) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "load")
	if f.parentSlots != 0 {
		return nil, errors.New("waiting parent retained its permit")
	}
	data, ok := f.manifests[b.ManifestDigest]
	if !ok {
		return nil, errors.New("manifest custody missing")
	}
	if f.corrupt {
		data = append(append([]byte(nil), data...), ' ')
	}
	return data, nil
}

// Cancel is a durable termination fence in production. It never claims
// settlement merely because Temporal accepted a cancellation.
func (f *childFixture) Cancel(ctx context.Context, b ChildRunBinding) error {
	for {
		f.mu.Lock()
		if f.childSlots == 0 {
			f.calls = append(f.calls, "cancel")
			f.cancelled = append(f.cancelled, b.Request.RequestID)
			f.mu.Unlock()
			return nil
		}
		f.mu.Unlock()
		select {
		case <-f.childStopped:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type childRegistrar interface {
	RegisterWorkflowWithOptions(any, workflow.RegisterOptions)
	RegisterActivity(any)
	RegisterActivityWithOptions(any, activity.RegisterOptions)
}

// registerChildProtocol registers the test-only protocol entry under Run's
// name with the activities this slice may schedule.
func registerChildProtocol(r childRegistrar, a *Activities, f *childFixture) {
	r.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
	registerChildActivities(r, a, f)
}

func registerChildActivities(r childRegistrar, a *Activities, f *childFixture) {
	r.RegisterActivity(a.EmitJournal)
	r.RegisterActivity(a.AdmitEngineChild)
	r.RegisterActivity(a.LoadEngineChild)
	r.RegisterActivity(a.CancelEngineChild)
	r.RegisterActivity(a.ParkEngineChild)
	r.RegisterActivity(a.FinalizeEngineChild)
	r.RegisterActivity(a.CompleteEngineChild)
	r.RegisterActivity(a.EvaluateAutomated)
	r.RegisterActivity(a.EvaluateAutomatedCancelSafe)
	r.RegisterActivityWithOptions(f.Invoke, activity.RegisterOptions{Name: ActInvokeGoober})
	r.RegisterActivityWithOptions(f.Invoke, activity.RegisterOptions{
		Name: actInvokeGooberCancelSafe,
	})
	r.RegisterActivityWithOptions(f.Det, activity.RegisterOptions{Name: ActRunDeterministic})
	r.RegisterActivityWithOptions(f.Det, activity.RegisterOptions{
		Name: actRunDeterministicCancelSafe,
	})
}

func childEnvironment(t *testing.T, f *childFixture) (*testsuite.TestWorkflowEnvironment, string) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	f.now = env.Now
	env.SetWorkerOptions(temporalworker.Options{DeadlockDetectionTimeout: temporaltest.DeadlockDetectionTimeout})
	writer, dir := newLiveWriter(t)
	registerChildProtocol(env, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
	return env, dir
}
