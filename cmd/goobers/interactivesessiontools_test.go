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
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/interactivesession"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

type sessionSourceReader struct {
	source sessionops.SourceContext
	reads  *int
}

func (r sessionSourceReader) Get(ctx context.Context, binding string, q workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
	if binding != "issues" {
		return workbench.BacklogItem{}, errors.New("source is not bound")
	}
	credential, err := r.source.Lease.Credential(ctx, "backlog.read", interactiveaccess.Target{Kind: "backlog"})
	if err != nil || credential.Value != "human-backlog-must-not-leak" {
		return workbench.BacklogItem{}, errors.New("selected human backlog credential unavailable")
	}
	project := r.source.RetainedGaggle.Spec.Project
	client := providers.NewGitHubProvider(credential.Value, providers.WithHTTPClient(&http.Client{Transport: sessionSourceTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != "api.github.com" || request.URL.Path != "/repos/"+project.Owner+"/"+project.Name+"/issues/"+q.ID || request.Header.Get("Authorization") != "Bearer "+credential.Value {
			return nil, errors.New("provider read escaped source/credential binding")
		}
		*r.reads++
		raw, _ := json.Marshal(map[string]any{"id": 1001, "number": 42, "title": "Scope this feature", "body": "private source human-backlog-must-not-leak", "state": "open", "html_url": "https://github.com/" + project.Owner + "/" + project.Name + "/issues/42"})
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}))
	source := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: binding, Kind: "backlog"}, Backlog: r.source.RetainedGaggle.Spec.Backlog, BacklogIdentity: apiv1.InteractiveRepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: project.Owner, Name: project.Name}}
	reader, err := workbenchprovider.NewBacklogReader(workbench.Scope{GaggleID: r.source.Identity.Gaggle, Bindings: map[string]bool{binding: true}}, source, client)
	if err != nil {
		return workbench.BacklogItem{}, err
	}
	return reader.Get(ctx, q)
}
func (r sessionSourceReader) Page(context.Context, string, workbench.BacklogPageRequest) (workbench.BacklogPage, error) {
	return workbench.BacklogPage{}, errors.New("fixture not needed")
}

type sessionSourceTransport func(*http.Request) (*http.Response, error)

func (f sessionSourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sourceConversationProcess struct {
	base    *conversationProcess
	handler http.Handler
	access  *mcpio.SessionOperationAccess
	run     string
}

func (p *sourceConversationProcess) Run(ctx context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	cfg, err := mcpio.LoadConfig(filepath.Join(req.Dir, ".goobers", "mcp-io", mcpio.ConfigFileName))
	if err != nil {
		return harness.ProcessResult{}, err
	}
	if cfg.SessionOperations == nil {
		return harness.ProcessResult{}, errors.New("real native MCP configuration omitted source tools")
	}
	p.access, p.run = cfg.SessionOperations, cfg.RunID
	raw, _ := json.Marshal(sessioning.BacklogReadRequest{SourceBindingID: "issues", BacklogItemRequest: workbench.BacklogItemRequest{ID: "42"}})
	r := httptest.NewRequest(http.MethodPost, strings.ReplaceAll(sessioning.OperationPath, "{run}", cfg.RunID)+"/get_backlog_item", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+p.access.BearerToken)
	response := httptest.NewRecorder()
	p.handler.ServeHTTP(response, r)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Scope this feature") || strings.Contains(response.Body.String(), "human-backlog-must-not-leak") {
		return harness.ProcessResult{}, errors.New("scoped source read failed")
	}
	return p.base.Run(ctx, req)
}
func TestSessionNativeToolsUseLiveHumanLeaseAndRevokeOnReturn(t *testing.T) {
	f := conversationSetup(t)
	reads := 0
	f.up.setup.SessionBacklogReader = func(_ context.Context, source sessionops.SourceContext) (sessionops.BacklogReader, error) {
		return sessionSourceReader{source: source, reads: &reads}, nil
	}
	f.up.credentialPlane = &daemonCredentialService{grants: &stageGrantIssuer{endpoint: "https://daemon.invalid"}}
	if err := f.up.configureSessionOperations(f.up.setup.SessionRuntime); err != nil {
		t.Fatal(err)
	}
	opts := append(f.up.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	process := &sourceConversationProcess{base: f.process, handler: handler}
	f.handler = handler
	f.up.setup.SessionRuntime.process = process
	runErrors := make(chan error, 1)
	originalBuild := f.up.durableTriggers.sessions.Runtime.Build
	f.up.durableTriggers.sessions.Runtime.Build = func(ctx context.Context, turn triggerqueue.SessionTurn, inputs sessioning.ExecutionInputs) (interactivesession.PreparedTurn, error) {
		p, err := originalBuild(ctx, turn, inputs)
		if err != nil {
			return p, err
		}
		run := p.Run
		p.Run = func(ctx context.Context, lease *interactiveaccess.ExecutionLease, published func() error) error {
			err := run(ctx, lease, published)
			runErrors <- err
			return err
		}
		return p, nil
	}
	_, accepted := f.createAndSend(t)
	turn, err := f.up.durableTriggers.queue.SessionTurn(t.Context(), accepted.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.up.durableTriggers.drainOne(t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	f.up.durableTriggers.sessions.Wait()
	f.up.wg.Wait()
	turn, err = f.up.durableTriggers.queue.SessionTurn(t.Context(), accepted.AcceptanceID)
	if err != nil || turn.Outcome != "success" || reads != 1 {
		rd, _ := journal.OpenReadOnly(filepath.Join(f.up.l.ForGaggle("example").RunsDir(), turn.Record.RunID))
		if rd != nil {
			events, _ := rd.EventsBounded(8<<20, 8192)
			for _, event := range events {
				if event.Reason != "" {
					t.Log(event.Type, event.Reason)
				}
			}
		}
		t.Fatalf("outcome=%s reads=%d err=%v runtime=%v", turn.Outcome, reads, err, <-runErrors)
	}
	if _, err = f.up.setup.SessionRuntime.operations.AuthenticateSessionOperation(process.access.BearerToken); err == nil {
		t.Fatal("grant survives native turn return")
	}
	for _, req := range f.process.requests {
		if strings.Contains(strings.Join(req.Env, "\n"), "human-backlog-must-not-leak") {
			t.Fatal("raw provider credential reached model process")
		}
	}
	rd, err := journal.OpenReadOnly(filepath.Join(f.up.l.ForGaggle("example").RunsDir(), process.run))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.EventsBounded(8<<20, 8192)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Runner["kind"] == "session.operation.finished" {
			count++
			if e.Runner["humanIssuer"] != "issuer" || e.Runner["humanSubject"] != "human" || e.Runner["turnId"] != turn.ID || len(e.Artifacts) != 1 {
				t.Fatal("source read lost attributed evidence")
			}
		}
	}
	if count != 1 {
		t.Fatal("source command not durably audited")
	}
}
