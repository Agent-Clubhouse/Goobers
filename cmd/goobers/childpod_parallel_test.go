package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	"github.com/goobers/goobers/internal/workflow"
)

type parallelChildClient struct {
	client.Client
	execute func(context.Context, engine.ChildDispatchInput) (engine.ChildDispatchResult, error)
	mu      sync.Mutex
	results map[string]engine.ChildDispatchResult
	lost    bool
	gets    int
}

func (c *parallelChildClient) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	in := args[0].(engine.ChildDispatchInput)
	out, err := c.execute(ctx, in)
	if err != nil {
		return nil, err
	}
	out.BindingDigest = in.BindingDigest()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.results == nil {
		c.results = map[string]engine.ChildDispatchResult{}
	}
	c.results[options.ID] = out
	if c.lost && in.Attempt.Stage != "collate" {
		return nil, errors.New("lost durable worker acceptance reply")
	}
	return factoryWorkerRun{result: out}, nil
}

func (c *parallelChildClient) GetWorkflow(_ context.Context, id, _ string) client.WorkflowRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	out, ok := c.results[id]
	if !ok {
		return factoryWorkerRun{err: errors.New("missing exact worker")}
	}
	return factoryWorkerRun{result: out}
}
func (c *parallelChildClient) SignalWorkflow(_ context.Context, id, _, signal string, _ any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.results[id]; !ok || signal != engine.ChildDispatchStopSignal {
		return errors.New("wrong physical worker stop")
	}
	return nil
}

func parallelGeneratedSource(t *testing.T) string {
	t.Helper()
	var doc apiv1.Workflow
	if err := yaml.Unmarshal([]byte(childValidationProposal), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Spec.Start = "fan"
	doc.Spec.Tasks = nil
	for _, name := range []string{"inspect-a", "inspect-b", "collate"} {
		task := apiv1.Task{Name: name, Type: apiv1.TaskAgentic, Goober: "coder", Goal: "inspect and collate", Workspace: apiv1.WorkspaceScratch, Capabilities: []string{"agent:model"}}
		// Reuse the exact placement shape accepted by the existing child fixture.
		data := []byte("runsOn: {os: linux, capabilities: [isolated-child]}\n")
		if err := yaml.Unmarshal(data, &task); err != nil {
			t.Fatal(err)
		}
		if name != "collate" {
			task.Next = workflow.TargetJoin
		}
		doc.Spec.Tasks = append(doc.Spec.Tasks, task)
	}
	doc.Spec.Parallels = []apiv1.Parallel{{Name: "fan", Join: "collate", MaxConcurrentBranches: 2, FailurePolicy: apiv1.BranchAllOrNothing, OnFailure: workflow.TargetAbort, Branches: []apiv1.Branch{{Name: "a", Start: "inspect-a"}, {Name: "b", Start: "inspect-b"}}}}
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProductionChildParallelFactoryScopesWorkersAndObservations(t *testing.T) {
	testProductionChildParallel(t, false)
}
func TestProductionChildParallelRecoveryJoinsEachOriginalWorker(t *testing.T) {
	testProductionChildParallel(t, true)
}
func testProductionChildParallel(t *testing.T, lost bool) {
	t.Helper()
	f := newChildKitFixtureConfigured(t, childKitFixtureOptions{isolated: true, source: parallelGeneratedSource(t)})
	s := f.writer.service
	launcher := &queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}
	start, release, err := launcher.admittedChildIdentity(t.Context(), f.writer.identity)
	if err != nil {
		t.Fatal(err)
	}
	release()
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
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
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, map[string]credentials.ResolveFunc{"agent:model": func(context.Context) (string, error) { return "parallel-model", nil }}, nil)
		return resolver, []credentials.Grant{{Capability: "agent:model", Ref: "agent:model"}}, err
	}
	key, err := podauth.NewSignedKey([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	journals := childFactoryJournals(t, s)
	// These requests exercise scope and durable custody, not response latency.
	// Allow the in-process plane to make progress under the full race/coverage suite.
	const planeDeadline = 10 * time.Second
	const siblingDeadline = 30 * time.Second
	var server *httptest.Server
	var mu sync.Mutex
	arrived := make(chan struct{})
	seen := map[string]int{}
	returned := make(chan struct{})
	finished := 0
	worker := &parallelChildClient{lost: lost, execute: func(ctx context.Context, in engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		out := engine.ChildDispatchResult{}
		a := in.Attempt
		token, err := key.MintChildPod(a.RunID, a.ChildExecutionDigest, time.Hour)
		if err != nil {
			return out, err
		}
		blobs := &dispatcher.BlobClient{BaseURL: server.URL, Token: token, RetryDeadline: planeDeadline}
		data, err := blobs.Get(ctx, a.ChildExecutionDigest)
		if err != nil {
			return out, err
		}
		contract, err := childpod.DecodeContract(data, a.ChildExecutionDigest)
		if err != nil {
			return out, err
		}
		want := map[string]int{"inspect-a": 1, "inspect-b": 2, "collate": 0}[a.Stage]
		if contract.ChildBranch != want {
			return out, fmt.Errorf("wrong child branch %d for %s", contract.ChildBranch, a.Stage)
		}
		mu.Lock()
		seen[a.Stage]++
		if seen["inspect-a"] == 1 && seen["inspect-b"] == 1 && a.Stage != "collate" {
			close(arrived)
		}
		mu.Unlock()
		if a.Stage != "collate" {
			select {
			case <-arrived:
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(siblingDeadline):
				return out, errors.New("parallel worker sibling never dispatched")
			}
		}
		resolved, err := (&dispatcher.CredentialResolveClient{BaseURL: server.URL, Token: token, RetryDeadline: planeDeadline}).ResolveStage(ctx, a.RunID, a.Stage, []string{"agent:model"})
		if err != nil || len(resolved.Credentials) != 1 {
			return out, fmt.Errorf("parallel model credential: %w", err)
		}
		if a.Stage == "inspect-a" {
			_, err := (&dispatcher.CredentialResolveClient{BaseURL: server.URL, Token: token, RetryDeadline: planeDeadline}).ResolveStage(ctx, a.RunID, "inspect-b", []string{"agent:model"})
			if err == nil {
				return out, errors.New("parallel worker acquired sibling stage credentials")
			}
		}
		emitter := &livejournal.HTTPEmitter{BaseURL: server.URL, Token: token, RetryDeadline: planeDeadline}
		_, forgedErr := emitter.Emit(ctx, livejournal.EmitRequest{RunID: a.RunID, Gaggle: a.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "forged-branch", Time: time.Now(), Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: a.Stage, Attempt: a.Number, Branch: want + 1}}}})
		if forgedErr == nil {
			return out, errors.New("worker selected an observation branch")
		}
		_, err = emitter.Emit(ctx, livejournal.EmitRequest{RunID: a.RunID, Gaggle: a.Gaggle, Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "parallel-heartbeat", Time: time.Now(), Event: &journal.Event{Type: journal.EventStageHeartbeat, Stage: a.Stage, Attempt: a.Number}}}})
		if err != nil {
			return out, err
		}
		carrier, _ := json.Marshal(childpod.Output{Version: 1, ContractDigest: a.ChildExecutionDigest})
		digest := journal.Digest(carrier)
		if err = blobs.Put(ctx, digest, carrier); err != nil {
			return out, err
		}
		surrendered := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: digest, ObservedUsageReported: true, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"inspection": a.Stage}}}
		data, _ = json.Marshal(surrendered)
		if err = (&dispatcher.SurrenderPutClient{BaseURL: server.URL, Token: token, RetryDeadline: planeDeadline}).Put(ctx, a.RunID, a.Stage, a.PodAttempt, data); err != nil {
			return out, err
		}
		if a.Stage != "collate" {
			mu.Lock()
			finished++
			if finished == 2 {
				close(returned)
			}
			mu.Unlock()
			select {
			case <-returned:
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(siblingDeadline):
				return out, errors.New("sibling never surrendered")
			}
		}
		out.Report = dispatcher.Report{Runner: "isolated", ChildCreateAttempted: true, ChildPodUID: "physical-" + a.Stage, WorkspaceWritersStopped: true, SurrenderConfirmed: true}
		return out, nil
	}}
	previousKey := s.config.API.PodTokenKeyFile
	s.config.API.PodTokenKeyFile = "configured-host-key"
	observe := childObservationFunc(func(context.Context, httpapi.ClaimListRequest) (httpapi.ClaimListResponse, error) {
		return httpapi.ClaimListResponse{ClaimVisibility: "local", ObservedAt: time.Now()}, nil
	})
	opts := s.installChildPodPlane(worker, plane, journals, observe, nil)
	opts = append(opts, httpapi.WithAuthenticator(auth))
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s.config.API.PodTokenKeyFile = previousKey
	factories, err := s.childExecutors(t.Context(), start, preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{gooberDigest: f.writer.identity.GooberDigest}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := f.writer.recorder.(*journal.Run)
	if err = recorder.Close(); err != nil {
		t.Fatal(err)
	}
	driver, err := f.driver.ForChildExecution(f.writer.identity, factories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := driver.Resume(t.Context(), runner.ResumeInput{RunID: f.writer.identity.RunID, Machine: start.Proposal.Machine, GooberDigest: f.writer.identity.GooberDigest})
	if lost {
		if !errors.Is(err, invoke.ErrChildCustodyPending) || result.Phase != journal.PhaseRunning {
			t.Fatal("uncertain parallel worker was terminalized", result, err)
		}
		rd, readErr := journal.OpenReadOnly(recorder.Dir())
		if readErr != nil {
			t.Fatal(readErr)
		}
		pending, _, readErr := s.pendingChildPodScopes(t.Context(), rd)
		if readErr != nil || len(pending) != 2 {
			t.Fatal("lost parallel custody", pending, readErr)
		}
		journals.CloseIdle(0)
		if err := s.reconcileChildPodCustody(t.Context(), rd); !errors.Is(err, invoke.ErrChildCustodyPending) {
			t.Fatal("first worker settled entire family", err)
		}
		pending, _, readErr = s.pendingChildPodScopes(t.Context(), rd)
		if readErr != nil || len(pending) != 1 {
			t.Fatal("first recovery lost sibling", pending, readErr)
		}
		if err := s.reconcileChildPodCustody(t.Context(), rd); err != nil {
			t.Fatal("second original worker recovery", err)
		}
		worker.mu.Lock()
		if worker.gets != 2 {
			t.Fatal("recovery did not rejoin each exact worker", worker.gets)
		}
		worker.lost = false
		worker.mu.Unlock()
		result, err = driver.Resume(t.Context(), runner.ResumeInput{RunID: f.writer.identity.RunID, Machine: start.Proposal.Machine, GooberDigest: f.writer.identity.GooberDigest})
	}
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatal(result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	wantStarts := 1
	if lost {
		wantStarts = 2
	}
	if len(seen) != 3 || seen["inspect-a"] != wantStarts || seen["inspect-b"] != wantStarts || seen["collate"] != 1 {
		t.Fatal("dispatches", seen)
	}
	reader, err := journal.OpenReadOnly(recorder.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]bool{}
	for _, event := range events {
		if event.Type == journal.EventStageHeartbeat {
			if want, ok := map[string]int{"inspect-a": 1, "inspect-b": 2, "collate": 0}[event.Stage]; ok {
				if event.Branch != want {
					t.Fatal("observation attributed to wrong branch", event)
				}
				observed[event.Stage] = true
			}
		}
	}
	if len(observed) != 3 {
		t.Fatal("missing worker observations", observed)
	}
	pending, _, err := s.pendingChildPodScopes(t.Context(), reader)
	if err != nil || len(pending) != 0 {
		t.Fatal("child completed before every physical worker joined", pending, err)
	}
}
