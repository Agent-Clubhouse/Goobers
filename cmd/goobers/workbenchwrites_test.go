package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchservice"
)

func TestWorkbenchHostInstallsSingleAttemptEditorAndRetention(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "confirmed"
		if lost {
			name = "lost-response"
		}
		t.Run(name, func(t *testing.T) {
			setup, pin := interactiveExecutionFixture(t)
			g := setup.Definitions.Gaggles[0].DeepCopy()
			g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "issues", Kind: "backlog", Writes: &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title"}}}}}
			g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "backlog.edit", "session.message")
			if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HUMAN_BACKLOG", "human-write-canary")
			t.Setenv("GH_TOKEN", "automation-must-not-be-used")
			calls, patches := 0, 0
			title, revision := "Before", "2026-10-04T12:00:00Z"
			factory := workbenchservice.ProviderFactory{SchedulerDirectory: pin.layout.SchedulerDir(), Registrar: setup.SharedRegistry, Client: &http.Client{Transport: sessionSourceTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer human-write-canary" || r.URL.Host != "api.github.com" || r.URL.Path != "/repos/"+g.Spec.Backlog.Project+"/issues/42" {
					t.Fatalf("wrong source or credential: %s", r.URL)
				}
				calls++
				status := 200
				if r.Method == http.MethodPatch {
					patches++
					var input struct {
						Title string `json:"title"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Fatal(err)
					}
					title, revision = input.Title, "2026-10-04T12:00:01Z"
					if lost {
						status = 503
					}
				} else if r.Method != http.MethodGet {
					t.Fatal("unexpected method", r.Method)
				}
				body, _ := json.Marshal(map[string]any{"id": 1001, "number": 42, "title": title, "state": "open", "updated_at": revision, "html_url": "https://github.com/" + g.Spec.Backlog.Project + "/issues/42", "labels": []string{}, "assignees": []string{}})
				return &http.Response{Request: r, StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}}
			dispatch := newDaemonTriggerService()
			now := time.Now().UTC()
			dispatch.now = func() time.Time { return now }
			queue := acceptedService(t, filepath.Join(pin.layout.SchedulerDir(), "accepted-triggers.db"), dispatch)
			u := &upSession{}
			u.setup, u.l, u.durableTriggers = setup, pin.layout, queue
			u.credentialPlane = &daemonCredentialService{grants: &stageGrantIssuer{endpoint: "https://daemon.invalid"}}
			u.installWorkbenchReads(&workbenchservice.Service{Permissions: setup.InteractiveAccess, Backlog: factory.Backlog})
			p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
			capabilities, err := setup.InteractiveAccess.InteractiveCapabilities(t.Context(), p, g.Name)
			if err != nil {
				t.Fatal(err)
			}
			available := false
			for _, action := range capabilities.Actions {
				if action.Action == "backlog.edit" {
					available = action.Available
				}
			}
			if !available {
				t.Fatal("installed writer not advertised")
			}
			if setup.SessionBacklogWriter == nil {
				t.Fatal("session writer was not installed")
			}
			lease, err := setup.InteractiveAccess.BeginSessionExecution(t.Context(), p, g.Name)
			if err != nil {
				t.Fatal(err)
			}
			source := sessionops.SourceContext{Identity: pin.parent, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}, Lease: lease, RetainedGaggle: *g}
			writer, err := setup.SessionBacklogWriter(t.Context(), source)
			if err != nil || writer == nil {
				t.Fatal("host writer did not bind", err)
			}
			permitted, err := writer.Capabilities(t.Context(), "issues")
			if err != nil || len(permitted.Fields) != 1 || permitted.Fields[0] != "title" {
				t.Fatal("session write fields differ", permitted, err)
			}
			source.Actor.Subject = "different-human"
			if _, err = setup.SessionBacklogWriter(t.Context(), source); err == nil {
				t.Fatal("session writer borrowed another actor")
			}
			lease.Close()

			options := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
			handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/gaggles/example/workbench/sources/issues"
			patch := func() workbench.BacklogEditCommand {
				request := httptest.NewRequest(http.MethodPatch, path+"/items/42", strings.NewReader(`{"sourceId":"1001","expectedRevision":"2026-10-04T12:00:00Z","field":"title","value":"After"}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "same-human-command")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				var command workbench.BacklogEditCommand
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &command) != nil {
					t.Fatalf("edit: %d %s", response.Code, response.Body)
				}
				return command
			}
			first := patch()
			want := "confirmed"
			if lost {
				want = "unknown"
			}
			if first.State != want || first.Actor.Subject != "human" || patches != 1 || title != "After" {
				t.Fatalf("wrong effect evidence: %+v patches=%d", first, patches)
			}
			before := calls
			t.Setenv("HUMAN_BACKLOG", "")
			repeated := patch()
			if repeated.ID != first.ID || !repeated.Duplicate || calls != before {
				t.Fatal("replay repeated provider work", repeated)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+"/commands/"+first.ID, nil))
			if response.Code != 200 || calls != before {
				t.Fatal("receipt minted credential or touched provider")
			}
			scope := triggerqueue.WorkbenchCommandScope{Gaggle: g.Name, SourceBindingID: "issues", Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}}
			now = now.Add(triggerqueue.WorkbenchCommandRetention + time.Minute)
			if err = queue.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			retained, err := queue.queue.WorkbenchCommand(t.Context(), scope, first.ID)
			if lost {
				if err != nil || retained.State != "unknown" {
					t.Fatal("uncertain custody expired", err)
				}
			} else if !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
				t.Fatal("settled receipt was not pruned", err)
			}
			changed := g.DeepCopy()
			changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"backlog.read"}
			if err = setup.InteractiveAccess.Apply([]apiv1.Gaggle{*changed}, nil); err != nil {
				t.Fatal(err)
			}
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+"/commands/"+first.ID, nil))
			if response.Code != 403 || calls != before {
				t.Fatal("revoked receipt authority reached source", response.Code)
			}
		})
	}
}
