package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func TestWorkbenchHostSharesAuthorizedReaderWithNativeSessions(t *testing.T) {
	setup, pin := interactiveExecutionFixture(t)
	g := setup.Definitions.Gaggles[0].DeepCopy()
	g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "issues", Kind: "backlog"}}}
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "session.message")
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HUMAN_BACKLOG", "human-read-canary")
	t.Setenv("GH_TOKEN", "automation-must-not-be-used")
	reads := 0
	factory := workbenchservice.ProviderFactory{SchedulerDirectory: pin.layout.SchedulerDir(), Registrar: setup.SharedRegistry, Client: &http.Client{Transport: sessionSourceTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/parent") {
			return &http.Response{Request: r, StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`))}, nil
		}
		if strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") {
			return &http.Response{Request: r, StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[]`))}, nil
		}
		if r.Header.Get("Authorization") != "Bearer human-read-canary" || r.URL.Host != "api.github.com" || r.URL.Path != "/repos/"+g.Spec.Backlog.Project+"/issues/42" {
			t.Fatalf("wrong provider target or identity: %s", r.URL)
		}
		reads++
		status, body := 200, `{"id":1001,"number":42,"title":"Scope this feature","state":"open","updated_at":"2026-10-04T12:00:00Z","html_url":"https://github.com/`+g.Spec.Backlog.Project+`/issues/42"}`
		if reads > 1 {
			if r.Header.Get("If-None-Match") != `"first"` {
				t.Fatal("session read skipped shared conditional revalidation")
			}
			status, body = 304, ""
		}
		return &http.Response{Request: r, StatusCode: status, Header: http.Header{"Etag": []string{`"first"`}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	u := &upSession{}
	u.setup, u.l = setup, pin.layout
	u.credentialPlane = &daemonCredentialService{grants: &stageGrantIssuer{endpoint: "https://daemon.invalid"}}
	u.installWorkbenchReads(&workbenchservice.Service{Permissions: setup.InteractiveAccess, Backlog: factory.Backlog})
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	capabilities, err := setup.InteractiveAccess.InteractiveCapabilities(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	available := false
	for _, action := range capabilities.Actions {
		if action.Action == "backlog.read" {
			available = action.Available
		}
	}
	if !available || setup.SessionBacklogReader == nil {
		t.Fatal("installed provider readers are unavailable")
	}
	options := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/gaggles/example/workbench/sources/issues/items/42?expectedSourceId=1001", nil))
	var item workbench.BacklogItem
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &item) != nil || item.Ref.SourceID != "1001" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("browser source read failed: %d %s", response.Code, response.Body.String())
	}
	lease, err := setup.InteractiveAccess.BeginSessionExecution(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	source := sessionops.SourceContext{Identity: pin.parent, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}, Lease: lease, RetainedGaggle: *g}
	reader, err := setup.SessionBacklogReader(t.Context(), source)
	if err != nil || reader == nil {
		t.Fatalf("native reader missing: %v", err)
	}
	got, err := reader.Get(t.Context(), "issues", workbench.BacklogItemRequest{ID: "42", ExpectedSourceID: "1001"})
	if err != nil || got.Ref != item.Ref || reads != 2 {
		t.Fatalf("native source differs: %+v reads=%d err=%v", got, reads, err)
	}
	source.Actor.Subject = "other"
	if _, err = setup.SessionBacklogReader(t.Context(), source); err == nil {
		t.Fatal("reader accepted another session actor")
	}
	lease.Close()
	if _, err = reader.Get(t.Context(), "issues", workbench.BacklogItemRequest{ID: "42"}); err == nil || reads != 2 {
		t.Fatal("closed lease reached provider")
	}
}

func TestWorkbenchSessionFactoryKeepsUnconfiguredSessionsModelOnly(t *testing.T) {
	setup, pin := interactiveExecutionFixture(t)
	g := setup.Definitions.Gaggles[0].DeepCopy()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "session.message")
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	lease, err := setup.InteractiveAccess.BeginSessionExecution(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	reader, err := workbenchSessionReader(nil)(t.Context(), sessionops.SourceContext{Identity: pin.parent, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}, Lease: lease, RetainedGaggle: *g})
	if err != nil || reader != nil {
		t.Fatalf("model-only session failed: %v", err)
	}
}
