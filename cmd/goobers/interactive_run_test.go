package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestInteractiveRunDaemonAssemblyWritesSharedGuidanceAndApproves(t *testing.T) {
	var wg sync.WaitGroup
	fixture := newInterventionWiringFixture(t, interventionTestMachine(t, apiv1.EvaluatorHuman), "human-run", []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}, {Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}, {Type: journal.EventGateStarted, Gate: "review"}, {Type: journal.EventGatePaused, Gate: "review"},
	}, interventionDeterministic{}, func(cfg *intervention.Config) { cfg.WaitGroup = &wg })
	t.Cleanup(wg.Wait)
	session := &upSession{}
	session.interventions = fixture.service
	session.setup = &schedulerSetup{Config: &instance.Config{}, SharedRegistry: journal.NewRegistryScrubber(), Definitions: &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "alice"}}}, Actions: []apiv1.InteractiveAction{"run.intervene"}}}}}}}
	session.setup.SharedRegistry.Register([]byte("daemon-human-secret"))
	if err := session.configureInteractiveAccess(); err != nil {
		t.Fatal(err)
	}
	if err := session.configureInteractiveRuns(newDaemonRunJournalService(fixture.layout, fixture.instanceLog)); err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append([]httpapi.HandlerOption(nil), session.apiHandlerOpts...)
	opts = append(opts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}), httpapi.WithInterventionContext(context.Background()))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/runs/human-run/interactive", nil))
	if response.Code != 200 {
		t.Fatalf("inspect=%d %s", response.Code, response.Body)
	}
	var view apicontract.InteractiveRunView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var seq uint64
	for _, action := range view.Actions {
		if action.Kind == "approve" {
			seq = action.SubjectSequence
		}
	}
	if seq == 0 {
		t.Fatalf("no current approval: %+v", view)
	}
	for _, input := range []apicontract.InteractiveRunCommand{{Kind: "guidance", Stage: "review", ExpectedSubjectSequence: seq, Guidance: "Inspect daemon-human-secret first."}, {Kind: "approve", Stage: "review", ExpectedSubjectSequence: seq, Decision: "pass"}} {
		raw, _ := json.Marshal(input)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/human-run/interactive-commands", strings.NewReader(string(raw)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", input.Kind)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("%s=%d %s", input.Kind, response.Code, response.Body)
		}
		var result apicontract.InteractiveRunCommandResult
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Accepted {
			t.Fatalf("not durable: %+v", result)
		}
	}
	wg.Wait()
	reader, _ := journal.OpenRead(fixture.runDir)
	events, _ := reader.Events()
	records := journal.ReplayOperatorMessages(events)
	if len(records) != 1 || records[0].Request.DeliveryMode != "shared-guidance" || records[0].Outcome != nil || records[0].Request.PrincipalRef != "https://identity.example:alice" {
		t.Fatalf("notes=%+v", records)
	}
	raw, err := os.ReadFile(filepath.Join(fixture.runDir, "events.jsonl"))
	if err != nil || strings.Contains(string(raw), "daemon-human-secret") {
		t.Fatal("guidance credential was persisted")
	}
	phase, _ := reader.Phase()
	if phase != journal.PhaseCompleted {
		t.Fatalf("approval did not execute continuation: %s", phase)
	}
}
