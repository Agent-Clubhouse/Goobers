package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactivesession"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sandbox"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type conversationProcess struct {
	mu       sync.Mutex
	requests []harness.ProcessRequest
	unknown  bool
}

func (p *conversationProcess) Run(ctx context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	unknown := p.unknown
	p.mu.Unlock()
	ack := invoke.RegisterWorkspaceWriter(ctx)
	if ack == nil {
		return harness.ProcessResult{}, errors.New("session writer tracker missing")
	}
	if unknown {
		ack(errors.New("unknown fixture writer"))
	} else {
		ack(nil)
	}
	err := harness.WriteCompletion(req.Dir, harness.DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "We can scope the next feature. sk-ant-api03-session-canary"})
	return harness.ProcessResult{ExitCode: 0, Transcript: []byte("session fixture transcript")}, err
}

type conversationFixture struct {
	up      *upSession
	process *conversationProcess
	handler http.Handler
}

func conversationSetup(t *testing.T) *conversationFixture {
	t.Helper()
	if _, err := sandbox.New(); err != nil {
		if errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, sandbox.ErrUnsupported) {
			t.Skipf("native sandbox unavailable: %v", err)
		}
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"GH_CONFIG_DIR", "AZURE_CONFIG_DIR", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "KUBECONFIG", "DOCKER_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "SSH_AUTH_SOCK"} {
		t.Setenv(key, "")
	}
	setup, pin := interactiveExecutionFixture(t)
	setup.SessionGeneration = pin.parent.ConfigGeneration
	setup.RunnerRegistry = newDaemonRunnerRegistry()
	setup.RunnerRegistry.Replace(setup.Runners)
	setup.Definitions.Gaggles[0].Spec.InteractiveAccess.Actions = append(setup.Definitions.Gaggles[0].Spec.InteractiveAccess.Actions, "session.create", "session.message")
	t.Setenv("HUMAN_MODEL", "sk-ant-api03-session-canary")
	t.Setenv("HUMAN_REPO", "human-repo-must-not-leak")
	t.Setenv("HUMAN_BACKLOG", "human-backlog-must-not-leak")
	t.Setenv("GH_TOKEN", "automation-must-not-leak")
	audit, _, err := journal.OpenInstanceLog(pin.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	setup.InstanceLog = audit
	u := &upSession{}
	u.l = pin.layout
	u.setup = setup
	u.ctx = t.Context()
	u.triggerPlane = &daemonTriggerService{}
	u.triggerPlane.AttachDispatchContext(t.Context())
	u.triggerPlane.AttachScheduler(localscheduler.New(nil, audit))
	u.durableTriggers, err = newDurableTriggerService(filepath.Join(pin.layout.SchedulerDir(), "accepted-triggers.db"), u.triggerPlane)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.durableTriggers.sessions.Wait(); _ = u.durableTriggers.queue.Close() })
	if err = u.configureInteractiveAccess(); err != nil {
		t.Fatal(err)
	}
	if err = u.configureInteractiveSessions(); err != nil {
		t.Fatal(err)
	}
	process := &conversationProcess{}
	setup.SessionRuntime.process = process
	principal := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &principal}), httpapi.WithInterventionContext(t.Context()))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &conversationFixture{up: u, process: process, handler: handler}
}
func (f *conversationFixture) post(t *testing.T, path, key string, body any) sessioning.Acceptance {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code < 200 || response.Code >= 300 {
		t.Fatalf("status=%d %s", response.Code, response.Body.String())
	}
	var accepted sessioning.Acceptance
	if err = json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	return accepted
}
func (f *conversationFixture) createAndSend(t *testing.T) (sessioning.Acceptance, sessioning.Acceptance) {
	t.Helper()
	created := f.post(t, "/api/v1/gaggles/example/sessions", "create", map[string]string{"title": "Scope v0.6", "goober": "coder"})
	sent := f.post(t, "/api/v1/gaggles/example/sessions/"+created.Session.ID+"/messages", "one", map[string]string{"text": "Help scope human interactions"})
	return created, sent
}

func TestSessionPortalAcceptanceRunsPinnedModelOnlyGoober(t *testing.T) {
	f := conversationSetup(t)
	created, sent := f.createAndSend(t)
	if sent.Message == nil || sent.Message.RunID != "" {
		t.Fatal("reserved run exposed", sent)
	}
	// The configured Goober declares repo:push. The dedicated human turn must
	// retain its instructions/model while granting only the model capability.
	record, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.up.durableTriggers.drainOne(t.Context(), record.Record); err != nil {
		t.Fatal(err)
	}
	f.up.durableTriggers.sessions.Wait()
	f.up.wg.Wait()
	turn, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil || turn.State != "settled" || turn.Outcome != "success" {
		t.Fatalf("turn=%+v error=%v", turn, err)
	}
	if len(f.process.requests) != 1 {
		t.Fatal("session did not execute one model turn", len(f.process.requests))
	}
	req := f.process.requests[0]
	env := strings.Join(req.Env, "\n")
	for _, forbidden := range []string{"human-repo-must-not-leak", "human-backlog-must-not-leak", "automation-must-not-leak"} {
		if strings.Contains(env, forbidden) {
			t.Fatal("wrong credential plane", forbidden)
		}
	}
	if !strings.Contains(env, "sk-ant-api03-session-canary") {
		t.Fatal("model credential absent")
	}
	page, err := f.up.durableTriggers.queue.SessionMessages(t.Context(), "example", created.Session.ID, 0, 100)
	if err != nil || len(page.Items) != 2 || page.Items[1].ActorKind != "agent" || strings.Contains(page.Items[1].Text, "sk-ant-api03-session-canary") {
		t.Fatal(page, err)
	}
	if page.Items[0].Actor.Issuer != "issuer" || page.Items[0].Actor.Subject != "human" || page.Items[0].RunID == "" {
		t.Fatal(page.Items[0])
	}
	dir := filepath.Join(f.up.l.ForGaggle("example").RunsDir(), page.Items[0].RunID)
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil || id.Session == nil || id.Child != nil || id.ContinuedFromRunID != "" {
		t.Fatal(id, err)
	}
	events, err := rd.EventsBounded(8<<20, 8192)
	if err != nil {
		t.Fatal(err)
	}
	response, joined, err := journal.SessionWriterEvidence(events, id)
	if err != nil || !joined || response == nil {
		t.Fatal(response, joined, err)
	}
	if err = acknowledgeTriggerBeforePrune(t.Context(), f.up.durableTriggers.queue, retention.Result{RunID: id.RunID, RunDir: dir}, time.Now()); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("session evidence not retained", err)
	}
	pins, err := retainedExecutionGenerationPins(t.Context(), f.up.l)
	if err != nil || !pins[created.Session.ConfigGeneration] {
		t.Fatal(pins, err)
	}
	if _, err = f.up.setup.RunnerRegistry.executionGeneration(t.Context(), id); err == nil {
		t.Fatal("session fell through automation recovery")
	}
	assertNativeSessionQueueCancellation(t, f, turn)
}

func assertNativeSessionQueueCancellation(t *testing.T, f *conversationFixture, turn triggerqueue.SessionTurn) {
	t.Helper()
	queue := f.up.durableTriggers.queue
	meta, err := startcontrol.Describe(t.Context(), queue, turn.Record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = queue.PinStartControl(t.Context(), turn.Record.ID, meta.Scope); err != nil {
		t.Fatal(err)
	}
	control, _, err := queue.RequestStartCancellation(t.Context(), "example", turn.Record.ID, triggerqueue.StartCancellation{RequestID: "stop", Actor: "human", Reason: "Stop this turn", Authority: []byte(`{"issuer":"issuer","subject":"human"}`)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	permissions := f.up.durableTriggers.sessions.Permissions
	gaggle := f.up.setup.Definitions.Gaggles[0].DeepCopy()
	gaggle.Spec.InteractiveAccess.Actions = append(gaggle.Spec.InteractiveAccess.Actions, "queue.cancel")
	if err = permissions.Apply([]apiv1.Gaggle{*gaggle}, nil); err != nil {
		t.Fatal(err)
	}
	principal := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	err = permissions.WithQueueCancellation(t.Context(), principal, "example", func(ctx context.Context) error {
		observed, handled, observeErr := f.up.durableTriggers.cancelTypedQueueStart(ctx, control)
		if !handled || observed.State != startcontrol.CancellationAlreadyTerminal {
			t.Fatalf("native terminal observation = %+v, handled=%v, error=%v", observed, handled, observeErr)
		}
		return observeErr
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSessionNativeUnknownWriterCannotSettleOrRetry(t *testing.T) {
	f := conversationSetup(t)
	f.process.unknown = true
	_, sent := f.createAndSend(t)
	turn, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.up.durableTriggers.drainOne(t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	f.up.durableTriggers.sessions.Wait()
	f.up.wg.Wait()
	current, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil || current.State != "running" || current.Record.RunID == "" {
		t.Fatal(current, err)
	}
	if len(f.process.requests) != 1 {
		t.Fatal("uncertain process retried", len(f.process.requests))
	}
	if err = f.up.durableTriggers.sessions.Reconcile(t.Context(), current.Record, false); err != nil {
		t.Fatal(err)
	}
	current, err = f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil || current.State != "running" {
		t.Fatal("terminal event closed uncertain writer", err)
	}
}

func TestSessionPublicationBarrierFailureSettlesWithoutModelEffects(t *testing.T) {
	f := conversationSetup(t)
	_, sent := f.createAndSend(t)
	original := f.up.durableTriggers.sessions.Runtime.Observe
	calls := 0
	f.up.durableTriggers.sessions.Runtime.Observe = func(ctx context.Context, turn triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (interactivesession.Observation, error) {
		calls++
		if calls == 2 {
			return interactivesession.Observation{}, errors.New("fixture publication observation failed")
		}
		return original(ctx, turn, inputs)
	}
	turn, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.up.durableTriggers.drainOne(t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	f.up.durableTriggers.sessions.Wait()
	f.up.wg.Wait()
	actual, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), sent.AcceptanceID)
	if err != nil || actual.State != "settled" || actual.Outcome != "failed" || actual.Record.RunID == "" {
		t.Fatal(actual, err)
	}
	if len(f.process.requests) != 0 {
		t.Fatal("effects passed failed journal publication barrier")
	}
}
