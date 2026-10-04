package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// installChildPodFactories composes the existing authenticated worker transport.
// No Kubernetes client, credential, RBAC grant or ambient config read is added.
func (s *daemonCredentialService) installChildPodFactories(client childpod.TemporalClient, surrenders dispatcher.SurrenderPlane) {
	if client == nil || s.config == nil || s.config.API.PodTokenKeyFile == "" || surrenders == nil {
		return
	}
	s.childExecutors = func(_ context.Context, start childExecutionStart, runtime preparedChildRuntime) (runner.ChildExecutionFactories, error) {
		if err := admitChildPodPlan(start); err != nil {
			return runner.ChildExecutionFactories{}, err
		}
		factory := childPodFactory{service: s, start: start, runtime: runtime, client: client, surrenders: surrenders}
		return runner.ChildExecutionFactories{
			NewDeterministic: func(rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Deterministic, error) {
				return factory.executor(rec, "")
			},
			NewAgentic: func(goober string, rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Goober, error) {
				return factory.executor(rec, goober)
			},
		}, nil
	}
}

func admitChildPodPlan(start childExecutionStart) error {
	if start.Proposal == nil || start.Proposal.CredentialCeiling().AllowPublication || len(start.Proposal.Placements) == 0 {
		return &childStartDeferred{Reason: "child requires explicit isolated pod placement without publication delegation"}
	}
	if err := admitChildPodStages(start); err != nil {
		return err
	}
	for _, pin := range start.Proposal.Placements {
		if pin.Self || pin.Queue == "" || len(pin.Eligible) == 0 {
			return &childStartDeferred{Reason: "child self placement is not a contained execution backend"}
		}
		for _, spec := range pin.Eligible {
			if spec.HostKind != instance.RunnerHostImage || spec.OS != "linux" {
				return &childStartDeferred{Reason: "child requires Linux image runner placement"}
			}
		}
	}
	return nil
}

type childPodFactory struct {
	service    *daemonCredentialService
	start      childExecutionStart
	runtime    preparedChildRuntime
	client     childpod.TemporalClient
	surrenders dispatcher.SurrenderPlane
}

type childStagePod struct {
	childPodFactory
	journal  *journal.Run
	identity journal.RunIdentity
	goober   string
}

func (f childPodFactory) executor(rec runner.ArtifactRecorder, goober string) (*childStagePod, error) {
	jr, ok := rec.(*journal.Run)
	if !ok {
		return nil, errors.New("child pod execution requires its owned journal writer")
	}
	r, err := journal.OpenReadOnly(jr.Dir())
	if err != nil {
		return nil, err
	}
	id, err := r.Identity()
	if err != nil {
		return nil, err
	}
	if id.RunID != f.start.Child.RunID || id.Gaggle != f.start.Envelope.Gaggle || !reflect.DeepEqual(id.Child, &f.start.Lineage) || id.WorkflowDigest != f.start.Envelope.WorkflowDigest || id.ConfigGeneration != f.start.Envelope.ConfigGeneration || id.GooberDigest != f.runtime.gooberDigest {
		return nil, errors.New("child pod factory journal differs from accepted source")
	}
	return &childStagePod{childPodFactory: f, journal: jr, identity: id, goober: goober}, nil
}

func (p *childStagePod) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	out, err := p.execute(ctx, env, &run, false)
	return out.Result, err
}
func (p *childStagePod) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	out, err := p.execute(ctx, env, nil, false)
	return out.Result, err
}
func (p *childStagePod) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	out, err := p.execute(ctx, env, nil, true)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if out.Verdict == nil {
		return apiv1.Verdict{}, errors.New("child reviewer surrendered no verdict")
	}
	return *out.Verdict, nil
}

func (p *childStagePod) execute(ctx context.Context, env apiv1.InvocationEnvelope, run *apiv1.DeterministicRun, review bool) (dispatcher.SurrenderedResult, error) {
	// Synchronous preparation owns no detached process; the executor registers
	// its separate physical pod writer before transport acceptance.
	if ack := invoke.RegisterWorkspaceWriter(ctx); ack != nil {
		defer ack(nil)
	}
	// Ordinary local runner envelopes omit the Goober; this factory was
	// selected by the trusted compiled stage and repeats that binding below.
	if env.Goober == "" {
		env.Goober = p.goober
	}
	request, pin, reader, err := p.prepare(ctx, env, run, review)
	if err != nil {
		return dispatcher.SurrenderedResult{}, err
	}
	blobs := childpod.ScopedBlobs{Queue: p.service.childQueue, Identity: p.start.Child.Identity}
	if err = copyContainedPodContext(ctx, reader, p.identity.RunID, blobs, env.ContextPointers); err != nil {
		return dispatcher.SurrenderedResult{}, err
	}
	if request.Attempt.Agentic {
		request.Attempt.KitDigest, err = (childKitWriter{service: p.service, identity: p.identity, blobs: blobs, recorder: p.journal}).WriteKit(ctx, request.Attempt)
		if err != nil {
			return dispatcher.SurrenderedResult{}, err
		}
	}
	transport := childpod.TemporalDispatch{Client: p.client, WorkflowQueue: p.service.config.EffectiveEngineConfig().TaskQueue, DispatchQueue: pin.Queue, Admit: p.admit}
	executor := childpod.Executor{Dispatcher: transport, Surrenders: p.surrenders, Blobs: blobs, Recorder: p.journal}
	out, report, callErr := executor.Execute(ctx, request)
	if report.Runner != "" {
		placement := journal.Placement{Runner: report.Runner, Node: report.Node, OS: report.OS, Build: report.Build, Worker: report.Worker, Image: report.Image, Pod: report.Pod, QueuedAt: &report.QueuedAt, PodStartedAt: &report.PodStartedAt}
		callErr = errors.Join(callErr, p.journal.Append(journal.PlacementEvent(request.Attempt.Stage, request.Attempt.Number, request.Attempt.Class, placement)))
	}
	if report.WorkspaceWritersStopped && report.SurrenderConfirmed {
		owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		callErr = errors.Join(callErr, adoptContainedPodOutputs(owned, p.journal, blobs, &out))
	}
	if out.ObservedUsageReported {
		invoke.ReportAgentUsage(ctx, out.ObservedUsage)
	}
	return out, callErr
}

func (p *childStagePod) admit(ctx context.Context) (func(), error) {
	launcher := &queuedChildLauncher{layout: p.service.layout, queue: p.service.childQueue, authority: p.service.children}
	start, release, err := launcher.admittedChildIdentity(ctx, p.identity)
	if err != nil {
		return nil, err
	}
	if !sameChildCeiling(start.Proposal.CredentialCeiling(), p.start.Proposal.CredentialCeiling()) {
		release()
		return nil, errors.New("child delegation changed before worker admission")
	}
	return release, nil
}

func (p *childStagePod) prepare(ctx context.Context, env apiv1.InvocationEnvelope, run *apiv1.DeterministicRun, review bool) (childpod.Request, dispatcher.PinnedPlacement, *journal.Reader, error) {
	var empty childpod.Request
	var noPin dispatcher.PinnedPlacement
	stage, ok := strings.CutPrefix(env.TaskID, p.identity.RunID+":")
	if !ok || env.InstanceID != p.identity.InstanceID || env.RunID != p.identity.RunID || env.Gaggle != p.identity.Gaggle || env.WorkflowID != p.identity.Workflow || env.ConfigGeneration != p.identity.ConfigGeneration || env.GooberDigest != p.identity.GooberDigest || env.Goober != p.goober || env.ChildWorkflowOrigin != nil {
		return empty, noPin, nil, errors.New("child invocation differs from retained identity")
	}
	reader, err := journal.OpenReadOnly(p.journal.Dir())
	if err != nil {
		return empty, noPin, nil, err
	}
	started, err := childPodStarted(reader, stage, int(env.Attempt), review)
	if err != nil {
		return empty, noPin, nil, err
	}
	pin, workspace, err := p.stagePlan(stage, run, review)
	if err != nil {
		return empty, noPin, nil, err
	}
	ceiling, ok := credentials.ChildCeilingFromContext(ctx)
	if !ok || !sameChildCeiling(ceiling, p.start.Proposal.CredentialCeiling()) {
		return empty, noPin, nil, errors.New("child invocation credential ceiling differs")
	}
	a := dispatcher.Attempt{InstanceID: p.identity.InstanceID, RunID: p.identity.RunID, Gaggle: p.identity.Gaggle, Workflow: p.identity.Workflow, Stage: stage, Number: int(env.Attempt), PodAttempt: int(started.Seq), Class: started.AttemptClass, CPU: pin.CPU, Memory: pin.Memory, Disk: pin.Disk, Restrictions: pin.Restrictions, RunsOnCapabilities: pin.Capabilities, LedgerTouching: pin.LedgerTouching, Timeout: time.Duration(env.Limits.MaxDurationSeconds) * time.Second, Capabilities: env.Capabilities, Inputs: childPodInputs(env.Inputs), Workspace: string(workspace), ArtifactPublication: env.ArtifactPublication, Agentic: run == nil, Review: review}
	if run != nil {
		a.Command, a.Script, a.Env = run.Command, run.Script, run.Env
	} else {
		remote := env
		remote.Workspace = ""
		a.Envelope = &remote
	}
	request := childpod.Request{Identity: p.identity, Attempt: a, Eligible: pin.Eligible, Ceiling: ceiling, StartedAt: started.Time}
	request.Workspace, err = p.workspace(ctx, reader, env, workspace)
	return request, pin, reader, err
}

func (p *childStagePod) stagePlan(stage string, run *apiv1.DeterministicRun, review bool) (dispatcher.PinnedPlacement, apiv1.WorkspaceMode, error) {
	var pin dispatcher.PinnedPlacement
	for _, candidate := range p.start.Proposal.Placements {
		if candidate.Stage == stage {
			pin = candidate
			break
		}
	}
	if pin.Stage == "" || pin.Self {
		return pin, "", errors.New("child stage has no pinned isolated placement")
	}
	if review {
		gate, ok := p.start.Proposal.Machine.Gate(stage)
		if !ok || gate.Agentic == nil || gate.Agentic.Goober != p.goober {
			return pin, "", errors.New("child reviewer differs from source")
		}
		return pin, gate.EffectiveWorkspace(), nil
	}
	task, ok := p.start.Proposal.Machine.Task(stage)
	if !ok || task.Goober != p.goober || !reflect.DeepEqual(task.Run, run) {
		return pin, "", errors.New("child task differs from retained source")
	}
	return pin, task.EffectiveWorkspace(), nil
}

func childPodStarted(reader *journal.Reader, stage string, attempt int, review bool) (journal.Event, error) {
	events, err := reader.Events()
	if err != nil {
		return journal.Event{}, err
	}
	startKind, endKind := journal.EventStageStarted, journal.EventStageFinished
	if review {
		startKind, endKind = journal.EventReviewerStarted, journal.EventReviewerFinished
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type == journal.EventRunFinished {
			return journal.Event{}, errors.New("child run already settled")
		}
		if e.Stage != stage || e.Attempt != attempt {
			continue
		}
		if e.Type == endKind {
			break
		}
		if e.Type == startKind && e.Branch == 0 && e.Seq > 0 && e.Seq <= 1<<31-1 && !e.Time.IsZero() {
			return e, nil
		}
	}
	return journal.Event{}, fmt.Errorf("child stage %q has no exact active journal attempt", stage)
}

func childPodInputs(inputs map[string]interface{}) map[string]string {
	out := make(map[string]string, len(inputs))
	for key, value := range inputs {
		if rendered, ok := containedInputString(value); ok {
			out[key] = rendered
		}
	}
	return out
}

func containedInputString(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case bool:
		return strconv.FormatBool(value), true
	case int:
		return strconv.Itoa(value), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", false
		}
		return strconv.FormatFloat(value, 'f', -1, 64), true
	default:
		return "", false
	}
}

func admitChildPodStages(start childExecutionStart) error {
	placed := map[string]bool{}
	for _, pin := range start.Proposal.Placements {
		placed[pin.Stage] = true
	}
	for _, task := range start.Proposal.Workflow.Spec.Tasks {
		if !placed[task.Name] {
			return &childStartDeferred{Reason: "child task has no explicit contained placement"}
		}
		if task.Run != nil && (task.Run.Network != "" || task.Run.SyncBase || task.Run.InjectRunContext) {
			return &childStartDeferred{Reason: "child deterministic execution requests unsupported network, base synchronization or run context"}
		}
	}
	for _, gate := range start.Proposal.Workflow.Spec.Gates {
		if gate.Agentic != nil && !placed[gate.Name] {
			return &childStartDeferred{Reason: "child reviewer has no explicit contained placement"}
		}
	}
	return nil
}
