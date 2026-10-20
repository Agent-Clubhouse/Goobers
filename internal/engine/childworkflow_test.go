package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

// ecwFixture is a fault-injectable custody state machine, not an HA backend.
// Its single permit demonstrates ordering across actual parent/child Run walks.
type ecwFixture struct {
	parentIntegrity apiv1.Integrity
	usage           *AttemptUsage
	forgeOrigin     bool
	now             func() time.Time
	mu              sync.Mutex
	bindings        map[string]ChildRunBinding
	manifests       map[string][]byte
	terminal        map[string]childresult.TerminalReturn
	parentSlots     int
	childSlots      int
	calls           []string
	envelopes       []apiv1.InvocationEnvelope
	waits           int
	corrupt         bool
	parkAckLoss     bool
	completeAckLoss bool
	blockChild      bool
	childStarted    chan struct{}
	childStopped    chan struct{}
	cancelObserved  bool
	waitDuration    time.Duration
	failChild       bool
}

func newECWFixture() *ecwFixture {
	return &ecwFixture{now: time.Now, bindings: map[string]ChildRunBinding{}, manifests: map[string][]byte{}, terminal: map[string]childresult.TerminalReturn{}, parentSlots: 1, waits: 1, waitDuration: time.Hour, childStarted: make(chan struct{}, 1), childStopped: make(chan struct{}, 1)}
}
func ecwInput() RunInput {
	t := apiv1.Task{Name: "parent", Type: apiv1.TaskAgentic, Goober: "coder", Workspace: apiv1.WorkspaceScratch, Next: wf.TerminalComplete, ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}
	in := runInput("ecw-parent", fixtureSpec("parent", []apiv1.Task{t}, nil))
	in.DSLVersion = "3.1"
	in.LiveJournal = true
	return in
}
func (f *ecwFixture) Invoke(_ context.Context, env apiv1.InvocationEnvelope, _ string, _ string, _ apiv1.WorkspaceMode, _ string) (DispatchStageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parentSlots != 1 || f.childSlots != 0 {
		return DispatchStageResult{}, errors.New("parent dispatched without exclusive capacity")
	}
	f.envelopes = append(f.envelopes, env)
	f.calls = append(f.calls, "parent")
	if len(f.envelopes) > f.waits {
		return DispatchStageResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: f.parentIntegrity}}, nil
	}
	if env.ChildWorkflowOrigin == nil {
		return DispatchStageResult{}, errors.New("missing committed origin")
	}
	childID := fmt.Sprintf("%s-child-%d", env.RunID, len(f.envelopes))
	request := journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: env.RunID, RequestID: journal.Digest([]byte(childID)), Action: "wait", ChildRunID: childID, AcceptanceID: "trigger-" + childID, InvocationKey: childID, SourceDigest: journal.Digest([]byte("proposal")), Origin: *env.ChildWorkflowOrigin}
	if f.forgeOrigin {
		request.Origin.AttemptID = journal.StageAttemptID("other", 0, "parent", 1)
	}
	return DispatchStageResult{ChildWait: &request, Usage: f.usage}, nil
}
func (f *ecwFixture) Admit(_ context.Context, in ChildAdmissionInput) (ChildRunBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.bindings[in.Request.RequestID]; ok {
		return old, nil
	}
	f.calls = append(f.calls, "admit")
	child := runInput("generated", fixtureSpec("work", []apiv1.Task{detTask("work", "check")}, []apiv1.Gate{statusGate("check", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})}))
	child.RunID = in.Request.ChildRunID
	child.LiveJournal = true
	child.DSLVersion = "3.1"
	child.GooberDigest = journal.Digest([]byte("kit"))
	child.ConfigGeneration = journal.Digest([]byte("config"))
	child.WorkflowDigest, _ = wf.ComputeDigest(wf.Definition{Name: child.WorkflowName, Version: child.Version, DSLVersion: child.DSLVersion, Spec: child.Spec})
	data, _ := json.Marshal(FrozenChildManifest{Version: 1, SourceDigest: in.Request.SourceDigest, Input: child})
	accepted := f.now().UTC()
	b := ChildRunBinding{Request: in.Request, ParentWorkflow: in.ParentWorkflow, ParentWorkflowID: in.ParentWorkflowID, ParentTemporalRunID: in.ParentTemporalRunID, ManifestDigest: journal.Digest(data), Deadline: accepted.Add(f.waitDuration), Child: triggerqueue.ChildRecord{Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: in.Request.Gaggle, ParentRunID: in.Request.ParentRunID}, StageOccurrence: in.Request.Origin.StageOccurrence, InvocationKey: in.Request.InvocationKey}, AcceptanceID: in.Request.AcceptanceID, RunID: in.Request.ChildRunID, ProposalDigest: in.Request.SourceDigest, AcceptedAt: accepted}}
	f.bindings[in.Request.RequestID] = b
	f.manifests[b.ManifestDigest] = data
	return b, nil
}
func (f *ecwFixture) Park(_ context.Context, b ChildRunBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "park")
	f.parentSlots = 0
	if f.parkAckLoss {
		f.parkAckLoss = false
		return errors.New("park committed; acknowledgement lost")
	}
	return nil
}
func (f *ecwFixture) Load(_ context.Context, b ChildRunBinding) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.parentSlots != 0 {
		return nil, errors.New("waiting parent retained permit")
	}
	data := append([]byte(nil), f.manifests[b.ManifestDigest]...)
	if f.corrupt {
		data = append(data, ' ')
	}
	return data, nil
}
func (f *ecwFixture) Det(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun, _ string, _ string) (DispatchStageResult, error) {
	f.mu.Lock()
	if f.parentSlots != 0 || f.childSlots != 0 {
		f.mu.Unlock()
		return DispatchStageResult{}, errors.New("capacity exceeded")
	}
	f.childSlots = 1
	f.calls = append(f.calls, "child")
	block := f.blockChild
	fail := f.failChild
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.childSlots = 0
		f.calls = append(f.calls, "child-stopped")
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
func (f *ecwFixture) Finalize(_ context.Context, in ChildFinalizeInput) (childresult.TerminalReturn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.childSlots != 0 {
		return childresult.TerminalReturn{}, errors.New("child writers still active")
	}
	f.calls = append(f.calls, "finalize")
	b := in.Binding
	r := childresult.TerminalReturn{Version: 1, Identity: b.Child.Identity, AcceptanceID: b.Child.AcceptanceID, RunID: b.Child.RunID, SourceDigest: b.Child.ProposalDigest, Input: childresult.TerminalResultInput{State: in.State, FinishedAt: in.FinishedAt, Summary: "bounded child summary"}, ResultRef: journal.Digest([]byte("result"))}
	f.terminal[b.Request.RequestID] = r
	return r, nil
}
func (f *ecwFixture) Complete(_ context.Context, in ChildCompletionInput) (apiv1.ContextPointer, error) {
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
func (f *ecwFixture) Cancel(ctx context.Context, _ ChildRunBinding) error {
	// In production this is a durable termination fence. Never claim settlement
	// merely because Temporal accepted CancelWorkflow.
	for {
		f.mu.Lock()
		f.cancelObserved = true
		active := f.childSlots != 0
		if !active {
			f.calls = append(f.calls, "cancel-settled")
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

type ecwRegistrar interface {
	RegisterActivity(interface{})
	RegisterActivityWithOptions(interface{}, activity.RegisterOptions)
	RegisterWorkflowWithOptions(interface{}, workflow.RegisterOptions)
}

func registerECW(r ecwRegistrar, a *Activities, f *ecwFixture) {
	r.RegisterWorkflowWithOptions(runWithChildWorkflows, workflow.RegisterOptions{Name: "Run"})
	r.RegisterActivity(a.EmitJournal)
	r.RegisterActivity(a.AdmitEngineChild)
	r.RegisterActivity(a.ParkEngineChild)
	r.RegisterActivity(a.LoadEngineChild)
	r.RegisterActivity(a.FinalizeEngineChild)
	r.RegisterActivity(a.CompleteEngineChild)
	r.RegisterActivity(a.CancelEngineChild)
	r.RegisterActivity(a.EvaluateAutomated)
	r.RegisterActivityWithOptions(f.Invoke, activity.RegisterOptions{Name: ActInvokeGoober})
	r.RegisterActivityWithOptions(f.Det, activity.RegisterOptions{Name: ActRunDeterministic})
}
func ecwEnvironment(t *testing.T, f *ecwFixture) (*testsuite.TestWorkflowEnvironment, string) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	f.now = env.Now
	env.SetWorkerOptions(temporalworker.Options{MaxHeartbeatThrottleInterval: 10 * time.Millisecond, DefaultHeartbeatThrottleInterval: 10 * time.Millisecond})
	writer, dir := newLiveWriter(t)
	registerECW(env, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
	return env, dir
}

func TestEngineChildWaitSerialGraphsAndCapacity(t *testing.T) {
	f := newECWFixture()
	f.waits = 2
	f.parkAckLoss = true
	f.completeAckLoss = true
	env, dir := ecwEnvironment(t, f)
	env.ExecuteWorkflow("Run", ecwInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("%+v", result)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bindings) != 2 || len(f.envelopes) != 3 || f.parentSlots != 1 || f.childSlots != 0 {
		t.Fatalf("bad custody %+v", f.calls)
	}
	a, b := f.envelopes[0], f.envelopes[1]
	if a.ChildWorkflowOrigin.StageOccurrence != b.ChildWorkflowOrigin.StageOccurrence || a.ChildWorkflowOrigin.AttemptID == b.ChildWorkflowOrigin.AttemptID || b.Attempt != 2 || len(b.ContextPointers) != 1 {
		t.Fatalf("lost continuation: %+v %+v", a, b)
	}
	if len(f.envelopes[2].ContextPointers) != 2 {
		t.Fatal("lost earlier child result reference")
	}
	for _, binding := range f.bindings {
		reader, err := journal.OpenRead(filepath.Join(dir, binding.Child.RunID))
		if err != nil {
			t.Fatal(err)
		}
		id, err := reader.Identity()
		if err != nil {
			t.Fatal(err)
		}
		if id.Child == nil || id.Child.EnvelopeDigest != binding.ManifestDigest {
			t.Fatalf("missing frozen lineage: %+v", id)
		}
	}
}

func TestEngineChildDefaultAndLegacyRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacy     bool
		preview    *bool
		production bool
	}{{"production", false, boolPointer(true), true}, {"unmarked", true, boolPointer(true), false}, {"nil-preview", false, nil, false}, {"preview-off", false, boolPointer(false), false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newECWFixture()
			env, _ := ecwEnvironment(t, f)
			in := ecwInput()
			in.PreviewFeaturesEnabled = tc.preview
			if tc.legacy {
				env.OnGetVersion(childWorkflowChangeID, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			if tc.production {
				env.RegisterWorkflowWithOptions(Run, workflow.RegisterOptions{Name: "ProductionRun"})
				env.ExecuteWorkflow("ProductionRun", in)
			} else {
				env.ExecuteWorkflow("Run", in)
			}
			if env.GetWorkflowError() == nil {
				t.Fatal("child refusal lifted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("refused input dispatched %v", f.calls)
			}
		})
	}
}
func TestEngineChildDigestFailureNeverResumesParent(t *testing.T) {
	f := newECWFixture()
	f.corrupt = true
	env, _ := ecwEnvironment(t, f)
	env.ExecuteWorkflow("Run", ecwInput())
	if env.GetWorkflowError() == nil {
		t.Fatal("corrupt manifest accepted")
	}
	if len(f.envelopes) != 1 || !f.cancelObserved || f.parentSlots != 0 {
		t.Fatalf("unsafe resume: %v", f.calls)
	}
}
func TestEngineChildFailedResultReachesParent(t *testing.T) {
	f := newECWFixture()
	f.failChild = true
	env, _ := ecwEnvironment(t, f)
	env.ExecuteWorkflow("Run", ecwInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.terminal {
		if r.Input.State != triggerqueue.ChildFailed {
			t.Fatalf("failure not returned: %+v", r)
		}
	}
	if len(f.envelopes) != 2 {
		t.Fatal("parent did not resume on bounded failure result")
	}
}

func TestEngineChildCancellationAndDurableWaitDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			f := newECWFixture()
			f.blockChild = true
			env, _ := ecwEnvironment(t, f)
			// Model the dispatch adapter stopping its writer on Temporal cancellation.
			// Real heartbeat delivery and parent-close behavior are covered against
			// the dev server; the test suite does not implement that server transport.
			stopped := make(chan struct{})
			var stopOnce sync.Once
			env.SetOnActivityCanceledListener(func(info *activity.Info) {
				if info.ActivityType.Name == ActRunDeterministic {
					stopOnce.Do(func() { close(stopped) })
				}
			})
			env.OnActivity(ActRunDeterministic, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(func(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun, _ string, _ string) (DispatchStageResult, error) {
				f.mu.Lock()
				f.childSlots = 1
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
				case <-ctx.Done():
				case <-stopped:
				}
				return DispatchStageResult{}, temporal.NewCanceledError("writer stopped")
			})
			if deadline {
				f.waitDuration = time.Second
			} else {
				env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Second)
			}
			env.ExecuteWorkflow("Run", ecwInput())
			if env.GetWorkflowError() == nil {
				t.Fatal("cancel/timeout succeeded")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.cancelObserved || f.parentSlots != 0 || len(f.envelopes) != 1 {
				t.Fatalf("unsafe cancellation %v", f.calls)
			}
		})
	}
}

func TestEngineChildManifestValidation(t *testing.T) {
	f := newECWFixture()
	origin := journal.StageAttemptID("parent", 0, "work", 3)
	request := journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: "parent", RequestID: journal.Digest([]byte("req")), SourceDigest: journal.Digest([]byte("source")), Action: "wait", ChildRunID: "child", AcceptanceID: "trigger-child", InvocationKey: "one", Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: origin, AttemptID: origin}}
	binding, err := f.Admit(context.Background(), ChildAdmissionInput{Request: request, ParentWorkflow: "parent", ParentWorkflowID: "parent", ParentTemporalRunID: "epoch", Stage: "work"})
	if err != nil {
		t.Fatal(err)
	}
	f.parentSlots = 0
	a := Activities{EngineChildren: f}
	for _, tc := range []string{"source", "definition", "recursive", "preview", "kit"} {
		t.Run(tc, func(t *testing.T) {
			var manifest FrozenChildManifest
			_ = json.Unmarshal(f.manifests[binding.ManifestDigest], &manifest)
			switch tc {
			case "source":
				manifest.SourceDigest = journal.Digest([]byte("other"))
			case "definition":
				manifest.Input.Spec.Tasks[0].Next = wf.TargetAbort
			case "recursive":
				manifest.Input.Spec.Tasks[0].ChildWorkflows = &apiv1.ChildWorkflowPolicy{}
			case "preview":
				manifest.Input.PreviewFeaturesEnabled = nil
			case "kit":
				manifest.Input.GooberDigest = "mutable-name"
			}
			data, _ := json.Marshal(manifest)
			changed := binding
			changed.ManifestDigest = journal.Digest(data)
			f.manifests[changed.ManifestDigest] = data
			if _, err := a.LoadEngineChild(context.Background(), changed); err == nil {
				t.Fatal("invalid manifest admitted")
			}
		})
	}
}

func TestEngineChildForgedOriginAndCountBound(t *testing.T) {
	for _, forged := range []bool{true, false} {
		t.Run(fmt.Sprint(forged), func(t *testing.T) {
			f := newECWFixture()
			f.forgeOrigin = forged
			f.waits = 2
			env, _ := ecwEnvironment(t, f)
			in := ecwInput()
			in.Spec.Tasks[0].ChildWorkflows.MaxChildren = 1
			env.ExecuteWorkflow("Run", in)
			if env.GetWorkflowError() == nil {
				t.Fatal("invalid child admitted")
			}
			want := 1
			if forged {
				want = 0
			}
			if len(f.bindings) != want {
				t.Fatalf("accepted %d children, want %d", len(f.bindings), want)
			}
		})
	}
}
func TestEngineChildCumulativeUsageAcrossWaits(t *testing.T) {
	f := newECWFixture()
	f.waits = 2
	f.usage = &AttemptUsage{Reported: true, Metrics: map[string]float64{telemetry.AttrGenAIUsageInputTokens: 5, telemetry.AttrGenAIUsageOutputTokens: 1}}
	env, _ := ecwEnvironment(t, f)
	in := ecwInput()
	in.Spec.Tasks[0].Limits = &apiv1.Limits{MaxTokens: 10}
	env.ExecuteWorkflow("Run", in)
	if env.GetWorkflowError() == nil {
		t.Fatal("child wait reset cumulative token budget")
	}
	if len(f.terminal) != 1 || !f.cancelObserved {
		t.Fatalf("exhausted budget started second child: %v", f.calls)
	}
}
