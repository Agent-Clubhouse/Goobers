package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/sandbox"
	"github.com/goobers/goobers/internal/workflow"
)

type humanAcceptanceProcess struct {
	mu             sync.Mutex
	requests       []harness.ProcessRequest
	interruptedLog []byte
}

func (p *humanAcceptanceProcess) Run(ctx context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.requests) == 1 {
		runtime, err := interactiveRuntime(ctx)
		if err != nil {
			return harness.ProcessResult{}, err
		}
		p.interruptedLog, err = os.ReadFile(filepath.Join(runtime.execution.layout.RunsDir(), runtime.execution.source.RunID, "events.jsonl"))
		if err != nil {
			return harness.ProcessResult{}, err
		}
		// An execution error spends Task.Retry. A model-reported failure is a
		// normal result and follows workflow transitions instead.
		return harness.ProcessResult{ExitCode: 1}, errors.New("fixture model process failed")
	}
	err := harness.WriteCompletion(req.Dir, harness.DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "model fixture"})
	return harness.ProcessResult{ExitCode: 0, Transcript: []byte("model fixture sk-ant-api03-human-model-canary human-repository-canary human-backlog-canary")}, err
}

type humanAcceptanceFixture struct {
	setup         *schedulerSetup
	pinned        pinnedChildFixture
	handler       http.Handler
	wg            sync.WaitGroup
	process       *humanAcceptanceProcess
	durable       *durableTriggerService
	interventions *intervention.Service
	scheduler     *localscheduler.Scheduler
	log           bytes.Buffer
}

func newHumanAcceptanceFixture(t *testing.T) *humanAcceptanceFixture {
	t.Helper()
	if _, err := sandbox.New(); err != nil {
		if errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, sandbox.ErrUnsupported) {
			t.Skipf("native sandbox policy unavailable: %v", err)
		}
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Controlled host stores keep sandbox policy validation deterministic. The
	// real policy still denies these stores; the model process alone is fake.
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	for _, name := range []string{"GH_CONFIG_DIR", "AZURE_CONFIG_DIR", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "KUBECONFIG", "DOCKER_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "SSH_AUTH_SOCK"} {
		t.Setenv(name, "")
	}
	if err := os.Mkdir(filepath.Join(hostHome, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostHome, ".codex", "auth.json"), []byte("ambient-login-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	setup, pinned := interactiveExecutionFixture(t, func(root string) {
		path := filepath.Join(root, "config/gaggles/example/workflows/default-implement.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.Replace(string(raw), "capabilities: [agent:model]", "capabilities: [agent:model, repo:read, github:issues:read]\n      retry: {maxAttempts: 2}", 1))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(root, "config/gaggles/example/goobers/coder/goober.yaml")
		raw, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.Replace(string(raw), "    - repo:push", "    - repo:read\n    - github:issues:read", 1))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	})
	f := &humanAcceptanceFixture{setup: setup, pinned: pinned, process: &humanAcceptanceProcess{}}
	t.Setenv("HUMAN_MODEL", "sk-ant-api03-human-model-canary")
	t.Setenv("HUMAN_REPO", "human-repository-canary")
	t.Setenv("HUMAN_BACKLOG", "human-backlog-canary")
	t.Setenv("GH_TOKEN", "automation-canary")
	setup.SharedRegistry.Register([]byte("saved-secret-canary"))
	instanceLog, _, err := journal.OpenInstanceLog(pinned.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })
	setup.InstanceLog = instanceLog
	execution, err := setup.loadInteractiveRestartExecution(t.Context(), pinned.parent)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.WorkflowIdentity{Gaggle: pinned.parent.Gaggle, Workflow: pinned.parent.Workflow}
	setup.Interventions = newInterventionDefinitionRegistry(interventionDefinitionSet{runners: setup.Runners, machines: map[localscheduler.WorkflowIdentity]*workflow.Machine{key: execution.machine}, gooberDigests: map[localscheduler.WorkflowIdentity]string{key: pinned.parent.GooberDigest}, repoRefs: map[localscheduler.WorkflowIdentity]apiv1.RepoRef{key: execution.gaggle.Spec.Project}})
	setup.RunnerRegistry = newDaemonRunnerRegistry()
	setup.RunnerRegistry.Replace(setup.Runners)
	setup.RunnerRegistry.setGenerationResolver(func(_ context.Context, id journal.RunIdentity) (executionGenerationRuntime, error) {
		if id.RunID != pinned.parent.RunID {
			return executionGenerationRuntime{}, errors.New("automation resolver received a human epoch")
		}
		return executionGenerationRuntime{runner: setup.Runners[id.Gaggle], machine: execution.machine, gooberDigest: id.GooberDigest, repoRef: execution.gaggle.Spec.Project}, nil
	})
	setup.InteractiveRestartExecution = func(ctx context.Context, plan runner.StageRestartPlan) (intervention.Execution, error) {
		id := plan.Source
		id.RunID = plan.Continuation.RunID
		return setup.buildInteractiveRestartWithProcess(ctx, id, f.process)
	}
	setup.InteractiveRestartRecovery = func(ctx context.Context, id journal.RunIdentity) (intervention.Execution, error) {
		return setup.buildInteractiveRestartWithProcess(ctx, id, f.process)
	}
	runDir := filepath.Join(pinned.layout.ForGaggle("example").RunsDir(), pinned.parent.RunID)
	run, _, err := journal.Recover(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: string(apiv1.ResultFailure)},
		{Type: journal.EventStageStarted, Stage: "plan", Attempt: 2},
		{Type: journal.EventStageFinished, Stage: "plan", Attempt: 2, Status: string(apiv1.ResultFailure)},
		{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: "human-source", CommitSHA: strings.Repeat("a", 40)}},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	// Real provider construction and delivered token, with only HTTP transport
	// replaced. Unexpected providers, credentials or writes fail the fixture.
	oldTransport := http.DefaultTransport
	http.DefaultTransport = appDeliveryTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Host != "api.github.com" || req.Header.Get("Authorization") != "Bearer human-repository-canary" {
			return nil, fmt.Errorf("unexpected provider request: %s %s", req.Method, req.URL)
		}
		body := "[]"
		if strings.HasSuffix(req.URL.Path, "/git/ref/heads/human-source") {
			body = `{"ref":"refs/heads/human-source","object":{"sha":"` + strings.Repeat("a", 40) + `"}}`
		} else if !strings.HasSuffix(req.URL.Path, "/activity") {
			return nil, fmt.Errorf("unexpected provider endpoint: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	session := &upSession{}
	session.l, session.setup = pinned.layout, setup
	if err := session.configureInteractiveAccess(); err != nil {
		t.Fatal(err)
	}
	session.durableTriggers = acceptedService(t, filepath.Join(pinned.layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	f.durable = session.durableTriggers
	session.interventions = newRunInterventionService(pinned.layout, setup, &f.wg, log.New(&f.log, "", 0))
	f.interventions = session.interventions
	f.scheduler = localscheduler.New([]localscheduler.WorkflowEntry{{Workflow: key.Workflow, Gaggle: key.Gaggle, Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}, instanceLog)
	session.interventions.AttachScheduler(f.scheduler)
	if err := session.configureInteractiveRuns(newDaemonRunJournalService(pinned.layout, instanceLog)); err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append(session.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}), httpapi.WithInterventionContext(context.Background()))
	f.handler, err = httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.wg.Wait)
	return f
}

func (f *humanAcceptanceFixture) command(t *testing.T, key string, command apicontract.InteractiveRunCommand) apicontract.InteractiveRunCommandResult {
	t.Helper()
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+f.pinned.parent.RunID+"/interactive-commands", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK && response.Code != http.StatusAccepted {
		t.Fatalf("%s: %d %s", key, response.Code, response.Body)
	}
	var result apicontract.InteractiveRunCommandResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("command not retained: %+v", result)
	}
	return result
}

func TestInteractiveRestartHTTPExecutesPinnedHumanEpoch(t *testing.T) {
	f := newHumanAcceptanceFixture(t)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+f.pinned.parent.RunID+"/interactive", nil))
	var view apicontract.InteractiveRunView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	for _, action := range view.Actions {
		if action.Kind == "restart" && action.Available {
			sequence = action.SubjectSequence
		}
	}
	if sequence == 0 {
		t.Fatalf("no restart: %d %+v", response.Code, view)
	}
	saved := f.command(t, "selected-guidance", apicontract.InteractiveRunCommand{Kind: "guidance", Stage: "plan", ExpectedSubjectSequence: sequence, Guidance: "Apply reviewed instruction saved-secret-canary"})
	f.command(t, "unselected-guidance", apicontract.InteractiveRunCommand{Kind: "guidance", Stage: "plan", ExpectedSubjectSequence: sequence, Guidance: "Do not select this instruction"})
	sourceEvents := filepath.Join(f.pinned.layout.ForGaggle("example").RunsDir(), f.pinned.parent.RunID, "events.jsonl")
	before, err := os.ReadFile(sourceEvents)
	if err != nil {
		t.Fatal(err)
	}
	command := apicontract.InteractiveRunCommand{Kind: "restart", Stage: "plan", ExpectedSubjectSequence: sequence, GuidanceIDs: []string{saved.Guidance.Request.RequestID}, Rationale: "Use reviewed context"}
	accepted := f.command(t, "restart-epoch", command)
	if accepted.Status != "pending" {
		t.Fatalf("restart bypassed queue: %+v", accepted)
	}
	if err := f.durable.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	if accepted.ContinuationRunID == "" {
		t.Fatal("no epoch")
	}
	dir := filepath.Join(filepath.Dir(sourceEvents), "..", accepted.ContinuationRunID)
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseCompleted {
		t.Fatalf("epoch phase=%s err=%v logs=%s", phase, err, &f.log)
	}
	if after, err := os.ReadFile(sourceEvents); err != nil || !bytes.Equal(before, after) {
		t.Fatal("restart modified source journal")
	}
	f.process.mu.Lock()
	requests := append([]harness.ProcessRequest(nil), f.process.requests...)
	f.process.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("fresh retry allowance: %d invocations", len(requests))
	}
	for _, req := range requests {
		prompt := strings.Join(req.Command, "\n")
		if !strings.Contains(prompt, "Apply reviewed instruction") || strings.Contains(prompt, "Do not select this instruction") || strings.Contains(prompt, "saved-secret-canary") {
			t.Fatal("selected scrubbed guidance was not delivered")
		}
		env := "\n" + strings.Join(req.Env, "\n") + "\n"
		for key, want := range map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-human-model-canary", executor.CredentialEnvVar("repo:read"): "human-repository-canary", executor.CredentialEnvVar("github:issues:read"): "human-backlog-canary"} {
			if !strings.Contains(env, "\n"+key+"="+want+"\n") {
				t.Fatalf("missing selected identity for %s", key)
			}
		}
		if strings.Contains(env, "automation-canary") {
			t.Fatal("ambient automation token leaked")
		}
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var attempts []int
	isolated := false
	for _, event := range events {
		if event.Type == journal.EventRunnerIsolationPosture && event.Runner["posture"] == "enforced" {
			isolated = true
		}
		if event.Type == journal.EventStageStarted && event.Stage == "plan" {
			attempts = append(attempts, event.Attempt)
		}
	}
	if fmt.Sprint(attempts) != "[3 4]" {
		t.Fatalf("attempt history reset: %v", attempts)
	}
	if !isolated {
		t.Fatal("model execution did not construct enforced sandbox policy")
	}
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{"sk-ant-api03-human-model-canary", "human-repository-canary", "human-backlog-canary", "saved-secret-canary"} {
			if bytes.Contains(raw, []byte(secret)) {
				return fmt.Errorf("credential persisted in %s", filepath.Base(path))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	duplicate := f.command(t, "restart-epoch", command)
	f.wg.Wait()
	if duplicate.ContinuationRunID != accepted.ContinuationRunID {
		t.Fatal("duplicate created a second epoch")
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if id.ContinuedFromRunID != f.pinned.parent.RunID || id.SourceTerminalSeq != sequence || id.ConfigGeneration != f.pinned.parent.ConfigGeneration {
		t.Fatal("epoch lost source lineage or config pin")
	}
	// Recreate the durable boundary before the first model process completed.
	// Only the test journal is rewound; this represents a crash snapshot, not a
	// production journal mutation or an extra human restart/allowance.
	f.process.mu.Lock()
	interruptedLog := append([]byte(nil), f.process.interruptedLog...)
	f.process.mu.Unlock()
	if len(interruptedLog) == 0 {
		t.Fatal("no interrupted execution snapshot")
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), interruptedLog, 0600); err != nil {
		t.Fatal(err)
	}
	registry := newDaemonRunnerRegistry()
	registry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
		return executionGenerationRuntime{}, errors.New("human recovery used automation")
	})
	registry.setInteractiveGenerationResolver(interactiveGenerationResolver(f.pinned.layout, f.setup, f.setup.InteractiveRestartRecovery))
	recovered, err := registry.executionGeneration(t.Context(), id)
	if err != nil || recovered.runner == nil {
		t.Fatalf("human recovery failed: %v", err)
	}
	if _, err = recovered.runner.Start(t.Context(), runner.StartInput{RunID: "automation"}); err == nil {
		t.Fatal("recovery selected an automation driver")
	}
	result, err := recovered.runner.Resume(t.Context(), runner.ResumeInput{RunID: id.RunID, Machine: recovered.machine, GooberDigest: recovered.gooberDigest, RepoRef: recovered.repoRef, RecoveryReason: "daemon-restart"})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("crash recovery: %+v %v", result, err)
	}
	f.process.mu.Lock()
	count := len(f.process.requests)
	last := f.process.requests[len(f.process.requests)-1]
	f.process.mu.Unlock()
	if count != 3 {
		t.Fatalf("recovery dispatched %d model turns", count)
	}
	if !strings.Contains(strings.Join(last.Command, "\n"), "Apply reviewed instruction") || !strings.Contains(strings.Join(last.Env, "\n"), "human-repository-canary") {
		t.Fatal("crash recovery lost selected guidance or human identity")
	}
	if after, err := os.ReadFile(sourceEvents); err != nil || !bytes.Equal(before, after) {
		t.Fatal("recovery modified source journal")
	}
	changed := f.setup.Definitions.Gaggles[0].DeepCopy()
	changed.Spec.InteractiveAccess.Humans.Operators = nil
	if err := f.setup.InteractiveAccess.Apply([]apiv1.Gaggle{*changed}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.executionGeneration(t.Context(), id); err == nil {
		t.Fatal("revoked human recovered execution")
	}
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+f.pinned.parent.RunID+"/interactive-commands", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "revoked-command")
	response = httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked HTTP command: %d %s", response.Code, response.Body)
	}
}
