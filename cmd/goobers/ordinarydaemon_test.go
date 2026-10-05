package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func ordinaryHost(t *testing.T) *eventHostFixture {
	t.Helper()
	f := eventHost(t)
	if err := f.setup.installOrdinaryStarts(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	f.service.dispatch.AttachDispatchContext(t.Context())
	return f
}

func ordinaryHTTP(t *testing.T, f *eventHostFixture) http.Handler {
	t.Helper()
	p := httpapi.Principal{Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}), httpapi.WithTriggerService(f.service))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func submitOrdinaryHTTP(t *testing.T, handler http.Handler, key, body string) httpapi.TriggerResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, apicontract.TriggerIngestPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted && response.Code != http.StatusOK {
		t.Fatalf("acceptance=%d %s", response.Code, response.Body)
	}
	var result httpapi.TriggerResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestOrdinaryHTTPPinsAppliedArchiveBeforeStart(t *testing.T) {
	f := ordinaryHost(t)
	handler := ordinaryHTTP(t, f)
	body := `{"workflow":"default-implement","gaggle":"example"}`
	accepted := submitOrdinaryHTTP(t, handler, "pinned", body)
	if accepted.State != "accepted" || accepted.RunID != "" {
		t.Fatal(accepted)
	}
	record, err := f.service.queue.Get(t.Context(), accepted.AcceptanceID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	e, err := startintent.Parse(record.Payload)
	if err != nil || e.Target.ConfigGeneration != f.generation {
		t.Fatal(e, err)
	}
	if _, err = f.layout.FindRunDir(strings.TrimPrefix(record.ID, "trigger-")); err == nil {
		t.Fatal("execution preceded durable admission")
	}
	writeFileContent(t, f.source, "invalid pending configuration: [")
	// A newly applied selection cannot rewrite accepted pins on exact replay.
	f.setup.OrdinaryCatalog.mu.Lock()
	f.setup.OrdinaryCatalog.generation = "new-generation"
	f.setup.OrdinaryCatalog.mu.Unlock()
	retry := submitOrdinaryHTTP(t, handler, "pinned", body)
	if !retry.Duplicate || retry.AcceptanceID != accepted.AcceptanceID {
		t.Fatal(retry)
	}
	pins, err := retainedExecutionGenerationPins(t.Context(), f.layout)
	if err != nil || !pins[f.generation] {
		t.Fatal(pins, err)
	}
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	record, err = f.service.queue.Get(t.Context(), record.ID, "alice")
	if err != nil || record.State != triggerqueue.Dispatched {
		t.Fatal(record, err)
	}
	dir, err := f.layout.FindRunDir(record.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err = startintent.VerifyIdentity(id, record); err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil || journal.PhaseFromEvents(events) != journal.PhaseCompleted {
		t.Fatal(events, err)
	}
}

func TestOrdinaryFileTransferRecoversLostAckAndStartsOnce(t *testing.T) {
	f := ordinaryHost(t)
	id, err := writeTriggerRequestContext(t.Context(), f.layout.SchedulerDir(), "example", "default-implement")
	if err != nil {
		t.Fatal(err)
	}
	sweep := func(options triggerSweepOptions) {
		t.Helper()
		if err := sweepPendingTriggersWithAdmission(t.Context(), f.layout.SchedulerDir(), nil, f.sched, func() time.Time { return f.now }, options, f.service.delegatedAdmission()); err != nil {
			t.Fatal(err)
		}
	}
	sweep(triggerSweepOptions{})
	record, err := f.service.queue.ByKey(t.Context(), "delegated:"+id)
	if err != nil || record.State != triggerqueue.Accepted {
		t.Fatal(record, err)
	}
	if withdrawn, err := withdrawTriggerRequest(f.layout.SchedulerDir(), id); err != nil || withdrawn {
		t.Fatalf("accepted start falsely withdrawn=%v %v", withdrawn, err)
	}
	dir := filepath.Join(f.layout.SchedulerDir(), pendingTriggersDir)
	if err = os.Remove(filepath.Join(dir, id+ackSuffix)); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(dir, id+requestSuffix), filepath.Join(dir, id+activeSuffix)); err != nil {
		t.Fatal(err)
	}
	if err = f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = acceptedService(t, filepath.Join(f.layout.SchedulerDir(), "accepted-triggers.db"), f.service.dispatch)
	if err = f.setup.installOrdinaryStarts(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	sweep(triggerSweepOptions{recoverActiveRequests: true})
	replayed, err := f.service.queue.ByKey(t.Context(), "delegated:"+id)
	if err != nil || replayed.ID != record.ID {
		t.Fatal(replayed, err)
	}
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	sweep(triggerSweepOptions{})
	response, ok := readTriggerResponseFile(filepath.Join(dir, id+responseSuffix))
	if !ok || response.RunID != strings.TrimPrefix(record.ID, "trigger-") {
		t.Fatal(response, ok)
	}
	// A repeated original source file observes the same durable outcome.
	if err = writeDelegateJSON(filepath.Join(dir, id+requestSuffix), triggerRequest{Workflow: "default-implement", Gaggle: "example", CreatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	sweep(triggerSweepOptions{})
	response, ok = readTriggerResponseFile(filepath.Join(dir, id+responseSuffix))
	if !ok || response.RunID != strings.TrimPrefix(record.ID, "trigger-") {
		t.Fatal(response, ok)
	}
}

func TestOrdinaryCurrentDisableRefusesAcceptedPinnedStart(t *testing.T) {
	f := ordinaryHost(t)
	r, err := f.service.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: f.entry.Workflow, Gaggle: f.entry.Gaggle, RequestID: "revoked", Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	entry := f.entry
	entry.DisabledReason = "workflow disabled"
	if err = f.sched.Reload([]localscheduler.WorkflowEntry{entry}, nil, f.now, "before", "after"); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	status, err := f.service.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: r.AcceptanceID, Actor: "alice"})
	if err != nil || status.State != "rejected" {
		t.Fatal(status, err)
	}
	if _, err = f.layout.FindRunDir(strings.TrimPrefix(r.AcceptanceID, "trigger-")); err == nil {
		t.Fatal("disabled workflow executed")
	}
}

func TestOrdinaryCatalogFailedPublicationPreservesTarget(t *testing.T) {
	f := ordinaryHost(t)
	reloader := &configReloader{setup: f.setup}
	failed := errors.New("scheduler publication failed")
	if err := reloader.publishOrdinaryDefinitions(&schedulerDefinitions{EventCatalog: eventPublicationSnapshot{generation: "unapplied"}}, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	target, release, err := f.setup.OrdinaryCatalog.capture(context.Background(), startintent.Request{Workflow: f.entry.Workflow, Gaggle: f.entry.Gaggle})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if target.ConfigGeneration != f.generation {
		t.Fatal(target)
	}
}

func TestOrdinaryForceStillWaitsForCapacityAndPriorityKeepsProvenance(t *testing.T) {
	for _, priority := range []bool{false, true} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			f := ordinaryHost(t)
			release, ok, reason := f.sched.ReserveContinuation("11111111111111111111111111111111", f.entry.Gaggle, f.entry.Workflow)
			if !ok {
				t.Fatal(reason)
			}
			request := httpapi.TriggerRequest{Workflow: f.entry.Workflow, Gaggle: f.entry.Gaggle, RequestID: "waiting", Actor: "alice", Force: !priority}
			if priority {
				request.SourceRun = "source-run"
			}
			receipt, err := f.service.Trigger(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			f.drain(t)
			record, err := f.service.queue.Get(t.Context(), receipt.AcceptanceID, "alice")
			if err != nil || record.State != triggerqueue.Accepted || record.Reason != localscheduler.ReasonMaxParallel {
				t.Fatal(record, err)
			}
			release()
			f.drain(t)
			f.sched.Wait()
			f.wg.Wait()
			// Crash after execution/journal publication, before its dispatch receipt.
			record, err = f.service.queue.Get(t.Context(), receipt.AcceptanceID, "alice")
			if err != nil || record.State != triggerqueue.Dispatching {
				t.Fatal(record, err)
			}
			if err = f.service.queue.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = acceptedService(t, filepath.Join(f.layout.SchedulerDir(), "accepted-triggers.db"), f.service.dispatch)
			if err = f.setup.installOrdinaryStarts(f.layout, f.service); err != nil {
				t.Fatal(err)
			}
			f.drain(t)
			record, err = f.service.queue.Get(t.Context(), receipt.AcceptanceID, "alice")
			if err != nil || record.State != triggerqueue.Dispatched {
				t.Fatal(record, err)
			}
			observed, err := f.service.ordinary.Observe(t.Context(), record)
			if err != nil || !observed {
				t.Fatal(observed, err)
			}
			duplicate, err := f.service.Trigger(t.Context(), request)
			if err != nil || !duplicate.Duplicate || duplicate.RunID != record.RunID {
				t.Fatal(duplicate, err)
			}
		})
	}
}

func TestOrdinaryFileCannotBypassQueueWithLegacyDispatchMarker(t *testing.T) {
	f := ordinaryHost(t)
	id, err := writeTriggerRequestPayload(f.layout.SchedulerDir(), triggerRequest{Workflow: f.entry.Workflow, Gaggle: f.entry.Gaggle, CreatedAt: f.now, DispatchRunID: "11111111111111111111111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	if err = sweepPendingTriggersWithAdmission(t.Context(), f.layout.SchedulerDir(), nil, f.sched, func() time.Time { return f.now }, triggerSweepOptions{}, f.service.delegatedAdmission()); err != nil {
		t.Fatal(err)
	}
	response, ok := readTriggerResponseFile(filepath.Join(f.layout.SchedulerDir(), pendingTriggersDir, id+responseSuffix))
	if !ok || response.Error == "" || response.RunID != "" {
		t.Fatal(response, ok)
	}
	if _, err = f.layout.FindRunDir("11111111111111111111111111111111"); err == nil {
		t.Fatal("forged legacy marker bypassed queue")
	}
}
