package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

// The daemon publishes authenticated attempts; only the existing worker owns
// Kubernetes access. An absent worker plane never falls back to local execution.
func (s *daemonCredentialService) installParentPodFactories(client childpod.TemporalClient, surrenders dispatcher.SurrenderPlane) {
	if client == nil || s.config == nil || s.config.API.PodTokenKeyFile == "" || surrenders == nil {
		return
	}
	s.parentRecovery = func(ctx context.Context, id journal.RunIdentity) error {
		return (&parentStagePod{service: s, identity: id, client: client, surrenders: surrenders}).reconcile(ctx)
	}
	s.parentExecutors = func(rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Goober, error) {
		jr, id, err := parentJournal(rec)
		if err != nil {
			return nil, err
		}
		return &parentStagePod{service: s, journal: jr, identity: id, client: client, surrenders: surrenders}, nil
	}
}

type parentStagePod struct {
	service    *daemonCredentialService
	journal    *journal.Run
	identity   journal.RunIdentity
	client     childpod.TemporalClient
	surrenders dispatcher.SurrenderPlane
}

func (p *parentStagePod) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, fmt.Errorf("%w: contained parent gates are not yet supported", childworkflow.ErrAuthorityUnavailable)
}

func (p *parentStagePod) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	// Preparation is synchronous and holds the runner's exclusive workspace.
	// The executor separately tracks the physical pod through custody import.
	var outputCustodyErr error
	if ack := invoke.RegisterWorkspaceWriter(ctx); ack != nil {
		defer func() { ack(outputCustodyErr) }()
	}
	request, pin, snapshot, release, reader, err := p.prepare(ctx, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer release()
	ctx, err = credentials.WithChildCeiling(ctx, request.Ceiling)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	blobs := &parentInvocationBlobs{ParentBlobs: childpod.ParentBlobs{RunDir: p.journal.Dir(), Identity: p.identity}, recorder: p.journal}
	if err = copyContainedPodContext(ctx, reader, p.identity.RunID, blobs, env.ContextPointers); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	request.Attempt.KitDigest, err = writeParentKit(ctx, p.service, snapshot, request.Attempt, request.Ceiling, blobs, p.journal)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	transport := childpod.TemporalDispatch{Client: p.client, WorkflowQueue: p.service.config.EffectiveEngineConfig().TaskQueue, DispatchQueue: pin.Queue, Admit: p.admission(env)}
	executor := childpod.Executor{Dispatcher: transport, Surrenders: p.surrenders, Blobs: blobs, Recorder: p.journal, KeepAttempt: blobs.KeepAttempt}
	executionCtx, podProof := invoke.WithWorkspaceQuiescence(ctx)
	out, report, callErr := executor.Execute(executionCtx, request)
	outputCustodyErr = podProof.Verify()
	if report.Runner != "" {
		placement := journal.Placement{Runner: report.Runner, Node: report.Node, OS: report.OS, Build: report.Build, Worker: report.Worker, Image: report.Image, Pod: report.Pod, QueuedAt: &report.QueuedAt, PodStartedAt: &report.PodStartedAt}
		callErr = errors.Join(callErr, p.journal.Append(journal.PlacementEvent(request.Attempt.Stage, request.Attempt.Number, request.Attempt.Class, placement)))
	}
	if report.WorkspaceWritersStopped && report.SurrenderConfirmed {
		owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		scoped := childpod.ParentAttemptBlobs{Store: blobs.ParentBlobs, ContractDigest: blobs.contractDigest}
		outputCustodyErr = errors.Join(outputCustodyErr, adoptContainedPodOutputs(owned, p.journal, scoped, &out))
		callErr = errors.Join(callErr, outputCustodyErr)
	}
	if blobs.contractDigest != "" && outputCustodyErr == nil {
		outputCustodyErr = blobs.record(parentPodWriterJoined)
		callErr = errors.Join(callErr, outputCustodyErr)
	}
	if out.ObservedUsageReported {
		invoke.ReportAgentUsage(ctx, out.ObservedUsage)
	}
	return out.Result, callErr
}

func (p *parentStagePod) admission(env apiv1.InvocationEnvelope) func(context.Context) (func(), error) {
	return func(ctx context.Context) (func(), error) {
		if p.service.children == nil {
			return nil, childworkflow.ErrAuthorityUnavailable
		}
		lease, err := p.service.children.AcquirePreparedStage(ctx, env)
		if err != nil {
			return nil, err
		}
		if err = lease.Verify(ctx); err != nil {
			lease.Release()
			return nil, err
		}
		return lease.Release, nil
	}
}

type parentInvocationBlobs struct {
	childpod.ParentBlobs
	contractDigest string
	recorder       *journal.Run
	contract       childpod.Contract
	retainedRef    journal.Ref
	retained       childpod.RetainedAttempt
}

func (b *parentInvocationBlobs) BindContract(ctx context.Context, digest string) error {
	if err := b.ParentBlobs.BindContract(ctx, digest); err != nil {
		return err
	}
	data, err := b.Get(ctx, digest)
	if err != nil {
		return err
	}
	b.contract, err = childpod.DecodeContract(data, digest)
	if err != nil {
		return err
	}
	b.contractDigest = digest
	if b.retainedRef.Digest == "" || b.retained.Input.Attempt.ChildExecutionDigest != digest {
		return errors.New("contained parent retained dispatch receipt missing")
	}
	return b.record(parentPodWriterStarted)
}

func (p *parentStagePod) prepare(ctx context.Context, env apiv1.InvocationEnvelope) (childpod.Request, dispatcher.PinnedPlacement, *workerConfigSnapshot, func(), *journal.Reader, error) {
	var request childpod.Request
	var pin dispatcher.PinnedPlacement
	if env.ChildWorkflowOrigin == nil || env.RunID != p.identity.RunID || env.Gaggle != p.identity.Gaggle || env.WorkflowID != p.identity.Workflow || env.ConfigGeneration != p.identity.ConfigGeneration || env.GooberDigest != p.identity.GooberDigest || env.InstanceID != p.identity.InstanceID {
		return request, pin, nil, nil, nil, errors.New("parent invocation differs from retained identity")
	}
	reader, err := journal.OpenReadOnly(p.journal.Dir())
	if err != nil {
		return request, pin, nil, nil, nil, err
	}
	if err = verifyParentPodCustody(reader); err != nil {
		return request, pin, nil, nil, nil, err
	}
	_, started, err := childworkflow.VerifyActiveStage(ctx, reader, p.identity.RunID, *env.ChildWorkflowOrigin)
	if err != nil {
		return request, pin, nil, nil, nil, err
	}
	if started.Seq == 0 || started.Seq > 1<<31-1 || started.Time.IsZero() || started.Attempt != int(env.Attempt) || env.TaskID != p.identity.RunID+":"+started.Stage {
		return request, pin, nil, nil, nil, errors.New("parent invocation has no exact physical attempt")
	}
	snapshot, release, err := p.snapshot(ctx)
	if err != nil {
		return request, pin, nil, nil, nil, err
	}
	request, pin, err = p.prepareSource(ctx, env, reader, started, snapshot)
	if err != nil {
		release()
		return request, pin, nil, nil, nil, err
	}
	return request, pin, snapshot, release, reader, nil
}

func (p *parentStagePod) prepareSource(ctx context.Context, env apiv1.InvocationEnvelope, reader *journal.Reader, started journal.Event, snapshot *workerConfigSnapshot) (childpod.Request, dispatcher.PinnedPlacement, error) {
	var request childpod.Request
	var pin dispatcher.PinnedPlacement
	machine, err := runner.PinnedWorkflowMachine(reader, p.identity)
	if err != nil {
		return request, pin, err
	}
	pins, err := containedParentPlacements(p.service.config, snapshot.set, machine)
	if err != nil {
		return request, pin, err
	}
	task, ok := machine.Task(started.Stage)
	if !ok || task.Goober != env.Goober || !slices.Equal(task.Capabilities, env.Capabilities) {
		return request, pin, errors.New("parent task differs from retained source")
	}
	for _, candidate := range pins {
		if candidate.Stage == started.Stage {
			pin = candidate
			break
		}
	}
	if pin.Stage == "" {
		return request, pin, errors.New("parent placement unavailable")
	}
	policy, err := childSnapshotPolicy(env.Workspace, p.service.layout.Root, p.service.config)
	if err != nil {
		return request, pin, err
	}
	fork, err := recovery.CaptureChildSnapshot(ctx, env.Workspace, childRepoKey(env.RepoRef), p.identity.RunID, started.Time, started.Time.AddDate(0, 0, 30), policy)
	if err != nil {
		return request, pin, err
	}
	ceiling := credentials.NewChildCeiling(false, task.Capabilities, task.Capabilities)
	workerEnv := env
	workerEnv.Workspace = ""
	a := dispatcher.Attempt{InstanceID: p.identity.InstanceID, RunID: p.identity.RunID, Gaggle: p.identity.Gaggle, Workflow: p.identity.Workflow, Stage: started.Stage, Number: int(env.Attempt), PodAttempt: int(started.Seq), Class: started.AttemptClass, CPU: pin.CPU, Memory: pin.Memory, Disk: pin.Disk, Restrictions: pin.Restrictions, RunsOnCapabilities: pin.Capabilities, LedgerTouching: pin.LedgerTouching, Timeout: time.Duration(env.Limits.MaxDurationSeconds) * time.Second, Capabilities: env.Capabilities, Inputs: childPodInputs(env.Inputs), Workspace: string(task.EffectiveWorkspace()), ArtifactPublication: env.ArtifactPublication, Agentic: true, Envelope: &workerEnv, WorkflowParent: true}
	request = childpod.Request{ParentOrigin: env.ChildWorkflowOrigin, Identity: p.identity, Attempt: a, Eligible: pin.Eligible, Ceiling: ceiling, StartedAt: started.Time, Workspace: &childpod.WorkspaceInput{Path: env.Workspace, Fork: fork}}
	return request, pin, nil
}
