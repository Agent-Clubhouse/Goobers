package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestConfiguredQueueControlsPreventCancelledAndExpiredExecution(t *testing.T) {
	f := ordinaryHost(t)
	p := httpapi.Principal{Issuer: "https://identity", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	access, err := interactiveaccess.New([]apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: p.Issuer, Subject: p.Subject}}}, Actions: []apiv1.InteractiveAction{"queue.cancel"}}}}}, nil, interactiveaccess.Dependencies{Registrar: journal.NewRegistryScrubber()})
	if err != nil {
		t.Fatal(err)
	}
	f.setup.InteractiveAccess = access
	u := &upSession{}
	u.l, u.setup, u.durableTriggers = f.layout, f.setup, f.service
	u.configureStartQueue()
	opts := append([]httpapi.HandlerOption{httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p})}, u.apiHandlerOpts...)
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cancel", "expire"} {
		accepted := submitOrdinaryHTTP(t, ordinaryHTTP(t, f), mode, `{"workflow":"default-implement","gaggle":"example"}`)
		record, err := f.service.queue.Get(t.Context(), accepted.AcceptanceID, "alice")
		if err != nil {
			t.Fatal(err)
		}
		control, err := f.service.startControls.Ensure(t.Context(), record)
		if err != nil || control.Scope.Generation != f.generation {
			t.Fatal(control, err)
		}
		if mode == "cancel" {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/gaggles/example/start-queue/"+record.ID+"/cancel", strings.NewReader(`{"requestId":"cancel","reason":"Plan changed"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var view apicontract.StartQueueItem
			if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil || response.Code != 202 || view.Disposition != "cancelled" || view.RunID != "" {
				t.Fatal(view, response.Code, response.Body, err)
			}
		} else {
			f.service.startControls.Now = func() time.Time { return control.Scope.Deadline }
		}
		f.drain(t)
		f.sched.Wait()
		f.wg.Wait()
		got, err := f.service.queue.StartControl(t.Context(), "example", record.ID)
		if err != nil || got.Record.State != triggerqueue.Rejected || got.Disposition == "" {
			t.Fatal(got, err)
		}
		if _, err = f.layout.FindRunDir(control.Scope.ReservedRunID); err == nil {
			t.Fatal("terminal unattempted receipt ran")
		}
	}
}
func TestQueueCancellationTerminalEvidenceRemainsTruthful(t *testing.T) {
	now := time.Now().UTC()
	control := triggerqueue.StartControl{CancelRequestedAt: now}
	for _, tc := range []struct {
		phase journal.RunPhase
		at    time.Time
		want  startcontrol.CancellationState
	}{{journal.PhaseAborted, now.Add(-time.Second), startcontrol.CancellationAlreadyTerminal}, {journal.PhaseAborted, now, startcontrol.CancellationConfirmed}, {journal.PhaseCompleted, now.Add(time.Second), startcontrol.CancellationAlreadyTerminal}, {journal.PhaseFailed, now.Add(time.Second), startcontrol.CancellationAlreadyTerminal}, {journal.PhaseEscalated, now, startcontrol.CancellationRequested}} {
		events := []journal.Event{{Seq: 1, Type: journal.EventRunStarted, Time: now.Add(-time.Minute)}, {Seq: 2, Type: journal.EventRunFinished, Status: string(tc.phase), Time: tc.at}}
		got, err := controlledTerminalEvents(events, control)
		if err != nil || got.State != tc.want {
			t.Fatal(tc, got, err)
		}
	}
}
