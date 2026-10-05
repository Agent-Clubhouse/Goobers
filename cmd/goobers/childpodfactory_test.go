package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type factoryWorkerRun struct {
	client.WorkflowRun
	result engine.ChildDispatchResult
	err    error
}

func (r factoryWorkerRun) Get(_ context.Context, out any) error {
	*out.(*engine.ChildDispatchResult) = r.result
	return r.err
}

type factoryWorkerClient struct {
	client.Client
	execute    func(context.Context, engine.ChildDispatchInput) (engine.ChildDispatchResult, error)
	starts     int
	gets       int
	result     engine.ChildDispatchResult
	workflowID string
	getErr     error
	lostReply  bool
}

func (c *factoryWorkerClient) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	c.starts++
	in := args[0].(engine.ChildDispatchInput)
	out, err := c.execute(ctx, in)
	out.BindingDigest = in.BindingDigest()
	c.result, c.workflowID = out, options.ID
	if c.lostReply {
		err = errors.New("lost worker acceptance reply")
	}
	return factoryWorkerRun{result: out}, err
}

func (c *factoryWorkerClient) GetWorkflow(_ context.Context, id, _ string) client.WorkflowRun {
	c.gets++
	if id != c.workflowID {
		return factoryWorkerRun{err: errors.New("wrong recovery identity")}
	}
	return factoryWorkerRun{result: c.result, err: c.getErr}
}
func (c *factoryWorkerClient) SignalWorkflow(_ context.Context, id, _, signal string, _ interface{}) error {
	if id != c.workflowID || signal != engine.ChildDispatchStopSignal {
		return errors.New("wrong recovery stop identity")
	}
	return nil
}
func TestProductionChildFactoryRecoversLostWorkerReply(t *testing.T) {
	testProductionChildFactory(t, false, true)
}

func TestProductionChildFactoryUsesRetainedKitAndScopedSurrender(t *testing.T) {
	testProductionChildFactory(t, false)
}
func TestProductionChildFactoryDrivesActualGeneratedRunner(t *testing.T) {
	testProductionChildFactory(t, true)
}
func TestProductionChildFactoryWithPublicationStillDispatchesModelOnly(t *testing.T) {
	testProductionChildFactory(t, false, false, true)
}
func testProductionChildFactory(t *testing.T, resume bool, lost ...bool) {
	t.Helper()
	var f childKitFixture
	if len(lost) > 1 && lost[1] {
		parent := strings.Replace(childValidationParent, "capabilities: [agent:model]", "capabilities: [agent:model, repo:push]", 1)
		f = newChildKitFixtureConfigured(t, childKitFixtureOptions{isolated: true, parent: parent})
	} else {
		f = newChildKitFixture(t, true)
	}
	s := f.writer.service

	launcher := &queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}
	start, release, err := launcher.admittedChildIdentity(t.Context(), f.writer.identity)
	if err != nil {
		t.Fatal(err)
	}
	release()
	plane, err := dispatcher.NewSurrenderDir(filepath.Join(t.TempDir(), "surrender"))
	if err != nil {
		t.Fatal(err)
	}
	blobs := childpod.ScopedBlobs{Queue: s.childQueue, Identity: f.child.Identity}
	worker := &factoryWorkerClient{lostReply: len(lost) > 0 && lost[0]}
	worker.execute = func(ctx context.Context, in engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		a := in.Attempt
		if a.Stage != "check" || a.PodAttempt < 2 || a.Envelope == nil || a.Envelope.Workspace != "" {
			t.Fatal("incorrect physical child request", a)
		}
		raw, err := blobs.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		contract, err := childpod.DecodeContract(raw, a.ChildExecutionDigest)
		if err != nil {
			t.Fatal(err)
		}
		if contract.Ceiling.AllowPublication {
			t.Fatal("host publication grant escaped to pod")
		}
		for _, key := range contract.Ceiling.AllowedKeys {
			if key != "agent:model" {
				t.Fatal("provider key escaped contract", key)
			}
		}
		if contract.Identity.Child == nil || *contract.Identity.Child != start.Lineage || contract.Stage != "check" || contract.KitDigest != a.KitDigest {
			t.Fatal("custody changed", contract)
		}
		raw, err = blobs.Get(ctx, a.KitDigest)
		if err != nil {
			t.Fatal(err)
		}
		var kit agentickit.Kit
		if err = json.Unmarshal(raw, &kit); err != nil {
			t.Fatal(err)
		}
		if kit.Goobers["coder"].Harness != apiv1.HarnessClaudeCode || kit.Envelope.Workspace != "" {
			t.Fatal("mutable host kit leaked", kit)
		}
		output := []byte("verified result")
		ref, err := journal.ArtifactRef(output)
		if err != nil {
			t.Fatal(err)
		}
		scoped := childpod.ChildAttemptBlobs{Store: blobs, ContractDigest: a.ChildExecutionDigest}
		if err = scoped.Put(ctx, ref.Digest, output); err != nil {
			t.Fatal(err)
		}
		carrier, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: a.ChildExecutionDigest})
		digest := journal.Digest(carrier)
		if err = scoped.Put(ctx, digest, carrier); err != nil {
			t.Fatal(err)
		}
		surrendered := dispatcher.SurrenderedResult{ChildWorkspaceDigest: digest, ObservedUsageReported: true, ObservedUsage: map[string]float64{"tokens.input": 17}, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Metrics: map[string]float64{"tokens.input": 999}, Artifacts: []apiv1.ArtifactPointer{{Path: ref.Path, Digest: ref.Digest, Size: ref.Size}}}}
		data, _ := json.Marshal(surrendered)
		if err = plane.Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			t.Fatal(err)
		}
		return engine.ChildDispatchResult{Report: dispatcher.Report{Runner: "isolated", ChildCreateAttempted: true, ChildPodUID: "exact-worker-uid", WorkspaceWritersStopped: true, SurrenderConfirmed: true}}, nil
	}
	previousKey := s.config.API.PodTokenKeyFile
	s.config.API.PodTokenKeyFile = "configured-host-key"
	s.installChildPodFactories(worker, plane)
	s.config.API.PodTokenKeyFile = previousKey
	if s.childExecutors == nil {
		t.Fatal("production factories not installed")
	}
	runtime := preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{gooberDigest: f.writer.identity.GooberDigest}}
	factories, err := s.childExecutors(t.Context(), start, runtime)
	if err != nil {
		t.Fatal(err)
	}
	recorder := f.writer.recorder.(*journal.Run)
	if resume {
		if err = recorder.Close(); err != nil {
			t.Fatal(err)
		}
		isolated, err := f.driver.ForChildExecution(f.writer.identity, factories)
		if err != nil {
			t.Fatal(err)
		}
		result, err := isolated.Resume(t.Context(), runner.ResumeInput{RunID: f.writer.identity.RunID, Machine: start.Proposal.Machine, GooberDigest: f.writer.identity.GooberDigest})
		if err != nil || result.Phase != journal.PhaseCompleted || worker.starts != 1 {
			t.Fatal("actual generated runner failed", result, err, worker.starts)
		}
		return
	}

	if err = recorder.Append(journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	executor, err := factories.NewAgentic("coder", recorder, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := credentials.WithChildCeiling(t.Context(), start.Proposal.CredentialCeiling())
	if err != nil {
		t.Fatal(err)
	}
	ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
	var usage map[string]float64
	ctx = invoke.WithAgentUsageReporter(ctx, func(m map[string]float64) { usage = m })
	out, err := executor.Invoke(ctx, *f.attempt.Envelope)
	if worker.lostReply {
		if err == nil || proof.Verify() == nil {
			t.Fatal("unknown worker lost pending proof", err)
		}
		testChildFactoryLostReplyRecovery(t, s, launcher, start, recorder, worker)
		return
	}
	if err != nil || out.Status != apiv1.ResultSuccess || proof.Verify() != nil {
		t.Fatal(out, err, proof.Verify())
	}
	if usage["tokens.input"] != 17 || worker.starts != 1 {
		t.Fatal("untrusted result metrics used", usage, worker.starts)
	}
	reader, err := journal.OpenReadOnly(recorder.Dir())
	if err != nil {
		t.Fatal(err)
	}
	p := out.Artifacts[0]
	data, err := reader.ArtifactBytes(journal.Ref{Path: p.Path, Digest: p.Digest, Size: p.Size})
	if err != nil || string(data) != "verified result" {
		t.Fatal(string(data), err)
	}
	// A live family fence must refuse another physical start, even though all
	// source, placement and kit bytes remain in accepted custody.
	if err = s.childQueue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "human", time.Now()); err != nil {
		t.Fatal(err)
	}
	refused, proof := invoke.WithWorkspaceQuiescence(ctx)
	if _, err = executor.Invoke(refused, *f.attempt.Envelope); err == nil || worker.starts != 1 || proof.Verify() != nil {
		t.Fatal("revoked authority dispatched or invented an active writer", err, worker.starts, proof.Verify())
	}
}

func TestChildFactoryUnsupportedPlacementHasNoHostFallback(t *testing.T) {
	f := newChildKitFixture(t)
	s := f.writer.service

	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := &factoryWorkerClient{execute: func(context.Context, engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		return engine.ChildDispatchResult{}, errors.New("unexpected dispatch")
	}}
	previousKey := s.config.API.PodTokenKeyFile
	s.config.API.PodTokenKeyFile = "configured-host-key"
	s.installChildPodFactories(worker, plane)
	s.config.API.PodTokenKeyFile = previousKey
	start, release, err := (&queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}).admittedChildIdentity(t.Context(), f.writer.identity)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err = s.childExecutors(t.Context(), start, preparedChildRuntime{}); err == nil || worker.starts != 0 {
		t.Fatal("self silently executed", err)
	}
}

func testChildFactoryLostReplyRecovery(t *testing.T, s *daemonCredentialService, launcher *queuedChildLauncher, start childExecutionStart, recorder *journal.Run, worker *factoryWorkerClient) {
	t.Helper()
	if err := recorder.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(recorder.Dir())
	if err != nil {
		t.Fatal(err)
	}
	launcher.runners = newDaemonRunnerRegistry()
	launcher.result = launcher.captureTerminal
	launcher.reconcile = s.reconcileChildPodCustody
	// Missing Temporal history is never interpreted as an absent pod or success.
	worker.getErr = errors.New("workflow history temporarily unavailable")
	if _, err = launcher.Result(t.Context(), start.childExecutionRef); err == nil {
		t.Fatal("unknown history settled custody")
	}
	pending, _, err := s.pendingChildPodScopes(t.Context(), reader)
	if err != nil || len(pending) != 1 {
		t.Fatal("lost pending physical attempt", pending, err)
	}
	worker.getErr = nil
	result, err := launcher.Result(t.Context(), start.childExecutionRef)
	if err != nil || result.State != triggerqueue.ChildFailed || result.ResultRef == "" {
		t.Fatal(result, err)
	}
	pending, _, err = s.pendingChildPodScopes(t.Context(), reader)
	if err != nil || len(pending) != 0 {
		t.Fatal("custody remained unjoined", pending, err)
	}
	if _, err = launcher.Result(t.Context(), start.childExecutionRef); err != nil {
		t.Fatal(err)
	}
	if worker.starts != 1 || worker.gets != 2 {
		t.Fatal("recovery launched or replayed a joined worker", worker.starts, worker.gets)
	}
	ref, _ := journal.ArtifactRef([]byte("verified result"))
	data, err := reader.ArtifactBytes(ref)
	if err != nil || string(data) != "verified result" {
		t.Fatal("late output not adopted", string(data), err)
	}
}
