package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type ordinaryTestAuthenticator struct{}

func (ordinaryTestAuthenticator) Authenticate(*http.Request) (*httpapi.Principal, error) {
	return &httpapi.Principal{Subject: "ordinary-operator", Roles: []httpapi.Role{httpapi.RoleOperate}}, nil
}

func TestOrdinaryHTTPKeepsAcceptedDefinitionAfterSourceEdit(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(t.Context(), layout, &wg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := setup.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, setup.SchedulerOptions()...)
	dispatch := newDaemonTriggerService()
	dispatch.AttachScheduler(sched)
	dispatch.AttachDispatchContext(t.Context())
	service, _, cancels, err := newDaemonCoordinationServices(layout, dispatch, setup.RunnerRegistry, setup.InstanceLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.queue.Close(); _ = cancels.receipts.Close() })
	if err := setup.installOrdinaryStarts(layout, service); err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(ordinaryTestAuthenticator{}), httpapi.WithTriggerService(service))
	if err != nil {
		t.Fatal(err)
	}
	submit := func() httpapi.TriggerResponse {
		request := httptest.NewRequest(http.MethodPost, apicontract.TriggerIngestPath, strings.NewReader(`{"workflow":"default-implement","gaggle":"example"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "captured-before-edit")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted && response.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
		}
		var result httpapi.TriggerResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	accepted := submit()
	if accepted.State != "accepted" || accepted.RunID != "" {
		t.Fatal(accepted)
	}
	record, err := service.queue.Get(t.Context(), accepted.AcceptanceID, "ordinary-operator")
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := startintent.Parse(record.Payload)
	if err != nil || envelope.Target.ConfigGeneration != setup.ExecutionGeneration {
		t.Fatal(envelope, err)
	}
	writeFileContent(t, filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml"), "invalid pending configuration: [")
	repeated := submit()
	if !repeated.Duplicate || repeated.AcceptanceID != accepted.AcceptanceID {
		t.Fatal(repeated)
	}
	// Apply a valid replacement that would fail if the queue resolved today's
	// definition instead of the accepted snapshot. Use the production reloader.
	writeFileContent(t, filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml"), strings.Replace(deterministicWorkflowYAML, `command: ["true"]`, `command: ["false"]`, 1))
	openPRs := newOpenPRLoop(t.Context(), setup.OpenPRRefresher)
	t.Cleanup(openPRs.Stop)
	reloader := &configReloader{layout: layout, setup: setup, scheduler: sched, openPRs: openPRs, reads: &readservice.Local{}, wg: &wg, appliedDigest: setup.ConfigDigest, observedDigest: setup.ConfigDigest, digests: newConfigDigestPublisher(setup.ConfigDigest)}
	applied, _, _, rejected, err := reloader.pollOnce(time.Now())
	if err != nil || !applied || rejected != "" {
		t.Fatal("replacement was not applied", applied, rejected, err)
	}
	if setup.OrdinaryCatalog.generation == envelope.Target.ConfigGeneration {
		t.Fatal("reload did not change applied generation")
	}
	if afterReload := submit(); !afterReload.Duplicate || afterReload.AcceptanceID != accepted.AcceptanceID {
		t.Fatal("reload changed existing acceptance", afterReload)
	}
	// A fresh boot retainer has no daemon queue attached and no journal yet.
	fresh, err := newExecutionGenerationRetainer(layout)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := fresh.DurablePins(t.Context())
	if err != nil || !pins[envelope.Target.ConfigGeneration] {
		t.Fatal(pins, err)
	}
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	sched.Wait()
	wg.Wait()
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	record, err = service.queue.Get(t.Context(), record.ID, "ordinary-operator")
	if err != nil || record.State != triggerqueue.Dispatched {
		t.Fatal(record, err)
	}
	dir, err := layout.FindRunDir(record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := startintent.VerifyIdentity(identity, record); err != nil {
		t.Fatal(err)
	}
	phase, err := reader.Phase()
	if err != nil || phase != journal.PhaseCompleted {
		t.Fatal(phase, err)
	}
}
