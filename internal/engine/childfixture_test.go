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
}

func newChildFixture() *childFixture {
	return &childFixture{now: time.Now, bindings: map[string]ChildRunBinding{}, manifests: map[string][]byte{}, handoffs: 1}
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
	f.envelopes = append(f.envelopes, env)
	f.calls = append(f.calls, "dispatch")
	if len(f.envelopes) > f.handoffs {
		return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
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
	return DispatchStageResult{ChildWait: &request}, nil
}

func (f *childFixture) Det(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun, _ string, _ string) (DispatchStageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "child-stage")
	return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
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
	if f.changeLoad != nil {
		f.changeLoad(&manifest)
	}
	data, _ := json.Marshal(manifest)
	b := childBindingFor(in, data, f.now().UTC())
	f.bindings[in.Request.RequestID] = b
	f.manifests[b.ManifestDigest] = data
	return b, nil
}

func (f *childFixture) Load(_ context.Context, b ChildRunBinding) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "load")
	data, ok := f.manifests[b.ManifestDigest]
	if !ok {
		return nil, errors.New("manifest custody missing")
	}
	if f.corrupt {
		data = append(append([]byte(nil), data...), ' ')
	}
	return data, nil
}

func (f *childFixture) Cancel(_ context.Context, b ChildRunBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "cancel")
	f.cancelled = append(f.cancelled, b.Request.RequestID)
	return nil
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
	r.RegisterActivity(a.EmitJournal)
	r.RegisterActivity(a.AdmitEngineChild)
	r.RegisterActivity(a.LoadEngineChild)
	r.RegisterActivity(a.CancelEngineChild)
	r.RegisterActivity(a.EvaluateAutomated)
	r.RegisterActivityWithOptions(f.Invoke, activity.RegisterOptions{Name: ActInvokeGoober})
	r.RegisterActivityWithOptions(f.Det, activity.RegisterOptions{Name: ActRunDeterministic})
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
