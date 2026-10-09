package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
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
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
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
	executeErr error
	lostReply  bool
}

func (c *factoryWorkerClient) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	c.starts++
	in := args[0].(engine.ChildDispatchInput)
	out, err := c.execute(ctx, in)
	c.executeErr = err
	out.BindingDigest = in.BindingDigest()
	c.result, c.workflowID = out, options.ID
	if err != nil {
		return nil, err
	}
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
	testProductionChildFactory(t, childFactoryTestOptions{lost: true})
}

func TestProductionChildFactoryUsesRetainedKitAndScopedSurrender(t *testing.T) {
	testProductionChildFactory(t, childFactoryTestOptions{})
}
func TestProductionChildFactoryDrivesActualGeneratedRunner(t *testing.T) {
	testProductionChildFactory(t, childFactoryTestOptions{resume: true})
}
func TestProductionChildFactoryParksActualRunnerAfterLostReply(t *testing.T) {
	testProductionChildFactory(t, childFactoryTestOptions{resume: true, lost: true})
}

type childFactoryTestOptions struct{ resume, lost, queued, handoff bool }

func TestProductionChildFactoryDrainsAcceptedQueueThroughWorkerAndResult(t *testing.T) {
	testProductionChildFactory(t, childFactoryTestOptions{queued: true})
}

func TestProductionChildFactoryQueueRecoversLostWorkerBeforeContinuing(t *testing.T) {
	testProductionChildFactory(t, childFactoryTestOptions{queued: true, lost: true})
}

func testProductionChildFactory(t *testing.T, options childFactoryTestOptions) {
	t.Helper()
	f := newChildKitFixtureConfigured(t, childKitFixtureOptions{isolated: true, queued: options.queued})
	s := f.writer.service

	launcher := &queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}
	plane, err := dispatcher.NewSurrenderDir(filepath.Join(t.TempDir(), "surrender"))
	if err != nil {
		t.Fatal(err)
	}
	s.childCredentials = launcher.credentialCeiling
	s.Replace(credentialPlaneDefinitionsFromSet(f.parent.applied))
	s.log, _, err = journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.log.Close() })
	s.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, map[string]credentials.ResolveFunc{
			"agent:model": func(context.Context) (string, error) { return "test-model-credential", nil },
		}, nil)
		return resolver, []credentials.Grant{{Capability: "agent:model", Ref: "agent:model"}}, err
	}
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	var workerToken string
	journals := childFactoryJournals(t, s)
	worker := &factoryWorkerClient{lostReply: options.lost}
	worker.execute = func(ctx context.Context, in engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		a := in.Attempt
		var err error
		workerToken, err = key.MintChildPod(a.RunID, a.ChildExecutionDigest, time.Hour)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		remoteBlobs := &dispatcher.BlobClient{BaseURL: server.URL, Token: workerToken, RetryDeadline: time.Second}
		if options.resume || options.queued {
			if !journals.IsOpen(a.RunID) {
				return engine.ChildDispatchResult{}, errors.New("child driver did not lend its journal before dispatch")
			}
			_, err := (&livejournal.HTTPEmitter{BaseURL: server.URL, Token: workerToken, RetryDeadline: time.Second}).Emit(ctx, livejournal.EmitRequest{RunID: a.RunID, Gaggle: a.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: fmt.Sprintf("child-worker-heartbeat-%d", a.PodAttempt), Time: time.Now(), Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: a.Stage, Attempt: a.Number}}}})
			if err != nil {
				return engine.ChildDispatchResult{}, fmt.Errorf("remote observation could not use the driver-owned journal: %w", err)
			}
		}
		if a.Stage != "check" || a.PodAttempt < 2 || a.Envelope == nil || a.Envelope.Workspace != "" {
			return engine.ChildDispatchResult{}, fmt.Errorf("incorrect physical child request: %+v", a)
		}
		raw, err := remoteBlobs.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		contract, err := childpod.DecodeContract(raw, a.ChildExecutionDigest)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		if contract.Ceiling.AllowPublication {
			return engine.ChildDispatchResult{}, errors.New("host publication grant escaped to pod")
		}
		for _, key := range contract.Ceiling.AllowedKeys {
			if key != "agent:model" {
				return engine.ChildDispatchResult{}, fmt.Errorf("provider key escaped contract: %+v", key)
			}
		}
		if contract.Identity.Child == nil || *contract.Identity.Child != *f.writer.identity.Child || contract.Stage != "check" || contract.KitDigest != a.KitDigest {
			return engine.ChildDispatchResult{}, fmt.Errorf("custody changed: %+v", contract)
		}
		owned, stop, err := remoteChildExecutionFence(ctx, server.URL, workerToken, contract)
		if err != nil {
			return engine.ChildDispatchResult{}, fmt.Errorf("startup execution observer refused worker: %w", err)
		}
		defer stop()
		credentialClient := &dispatcher.CredentialResolveClient{BaseURL: server.URL, Token: workerToken, RetryDeadline: time.Second}
		resolved, err := credentialClient.ResolveStage(owned, a.RunID, a.Stage, []string{"agent:model"})
		if err != nil {
			return engine.ChildDispatchResult{}, fmt.Errorf("startup credential owner refused worker: %w", err)
		}
		if len(resolved.Credentials) != 1 || resolved.Credentials[0].Value != "test-model-credential" {
			return engine.ChildDispatchResult{}, fmt.Errorf("startup credential owner returned unexpected credentials: %+v", resolved)
		}
		raw, err = remoteBlobs.Get(owned, a.KitDigest)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		var kit agentickit.Kit
		if err = json.Unmarshal(raw, &kit); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		if kit.Goobers["coder"].Harness != apiv1.HarnessClaudeCode || kit.Envelope.Workspace != "" {
			return engine.ChildDispatchResult{}, fmt.Errorf("mutable host kit leaked: %+v", kit)
		}
		output := []byte("verified result")
		ref, err := journal.ArtifactRef(output)
		if err != nil {
			return engine.ChildDispatchResult{}, err
		}
		scoped := remoteBlobs
		if err = scoped.Put(ctx, ref.Digest, output); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		carrier, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: a.ChildExecutionDigest})
		digest := journal.Digest(carrier)
		if err = scoped.Put(ctx, digest, carrier); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		surrendered := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: digest, ObservedUsageReported: true, ObservedUsage: map[string]float64{"tokens.input": 17}, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Metrics: map[string]float64{"tokens.input": 999}, Artifacts: []apiv1.ArtifactPointer{{Path: ref.Path, Digest: ref.Digest, Size: ref.Size}}}}
		data, _ := json.Marshal(surrendered)
		if err = (&dispatcher.SurrenderPutClient{BaseURL: server.URL, Token: workerToken, RetryDeadline: time.Second}).Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			return engine.ChildDispatchResult{}, err
		}
		return engine.ChildDispatchResult{Report: dispatcher.Report{Runner: "isolated", ChildCreateAttempted: true, ChildPodUID: fmt.Sprintf("exact-worker-%d", a.PodAttempt), WorkspaceWritersStopped: true, SurrenderConfirmed: true}}, nil
	}
	previousKey := s.config.API.PodTokenKeyFile
	s.config.API.PodTokenKeyFile = "configured-host-key"
	observe := childObservationFunc(func(context.Context, httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
		return httpapi.ClaimListResponse{ClaimVisibility: "local", ObservedAt: time.Now()}, nil
	})
	opts := s.installChildPodPlane(worker, plane, journals, observe, nil)
	opts = append(opts, httpapi.WithAuthenticator(auth.WithChildWorkflowGrants(s.grants.key)), httpapi.WithChildWorkflowService(s.children.HTTPService()))
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s.config.API.PodTokenKeyFile = previousKey
	if s.childExecutors == nil {
		t.Fatal("production factories not installed")
	}
	if options.queued {
		var finish func()
		if options.handoff {
			finish = prepareQueuedParentReturn(t, f, server.URL)
		}
		testQueuedChildFactory(t, f, worker)
		if finish != nil {
			finish()
		}
		return
	}
	start, release, err := launcher.admittedChildIdentity(t.Context(), f.writer.identity)
	if err != nil {
		t.Fatal(err)
	}
	release()
	runtime := preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{gooberDigest: f.writer.identity.GooberDigest}}
	factories, err := s.childExecutors(t.Context(), start, runtime)
	if err != nil {
		t.Fatal(err)
	}
	recorder := f.writer.recorder.(*journal.Run)
	if options.resume {
		if err = recorder.Close(); err != nil {
			t.Fatal(err)
		}
		isolated, err := f.driver.ForChildExecution(f.writer.identity, factories)
		if err != nil {
			t.Fatal(err)
		}
		result, err := isolated.Resume(t.Context(), runner.ResumeInput{RunID: f.writer.identity.RunID, Machine: start.Proposal.Machine, GooberDigest: f.writer.identity.GooberDigest})
		if worker.lostReply {
			if !errors.Is(err, invoke.ErrChildCustodyPending) || result.Phase != journal.PhaseRunning || worker.starts != 1 {
				t.Fatal("uncertain child retried or terminalized", result, err, worker.starts)
			}
			rd, err := journal.OpenReadOnly(recorder.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == journal.EventRunFinished || event.Type == journal.EventStageFinished {
					t.Fatal("uncertain child invented completion", event)
				}
			}
			if journals.IsOpen(f.writer.identity.RunID) {
				t.Fatal("parked driver did not release journal loan")
			}
			// The original physical attempt can still return its observations
			// after the driver has parked. Recovery must use that attempt.
			_, err = (&livejournal.HTTPEmitter{BaseURL: server.URL, Token: workerToken, RetryDeadline: time.Second}).Emit(t.Context(), livejournal.EmitRequest{RunID: f.writer.identity.RunID, Gaggle: f.writer.identity.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "after-park", Time: time.Now(), Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: "check", Attempt: 1}}}})
			if err != nil {
				t.Fatal("pending worker lost final journal custody", err)
			}
			journals.CloseIdle(0)
			if err := s.reconcileChildPodCustody(t.Context(), rd); err != nil {
				t.Fatal("original worker recovery failed", err)
			}
			if worker.starts != 1 || worker.gets != 1 {
				t.Fatal("recovery launched a replacement", worker.starts, worker.gets)
			}
			return
		}
		if err != nil || result.Phase != journal.PhaseCompleted || worker.starts != 1 {
			t.Fatal("actual generated runner failed", result, err, worker.starts)
		}
		if journals.IsOpen(f.writer.identity.RunID) {
			t.Fatal("completed child retained a borrowed journal handle")
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
	s.installChildPodFactories(worker, plane, childFactoryJournals(t, s))
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

func childFactoryJournals(t *testing.T, s *daemonCredentialService) *livejournal.Writer {
	t.Helper()
	journals, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return s.layout.ForGaggle(gaggle).RunsDir(), true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(journals.Close)
	return journals
}
