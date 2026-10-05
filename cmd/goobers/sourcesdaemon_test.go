package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

func sourceHost(t *testing.T, trigger string) *eventHostFixture {
	t.Helper()
	f := eventHostConfigured(t, func(_ *eventHostFixture, source string) string {
		return strings.Replace(source, "    - type: schedule\n      schedule: \"@every 24h\"", trigger, 1)
	})
	if err := f.setup.installOrdinaryStarts(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "source-scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	f.sched = localscheduler.New([]localscheduler.WorkflowEntry{f.entry}, log, localscheduler.WithSourceQueue(f.setup.SourceStarts))
	f.now = f.now.Add(-2 * time.Minute)
	f.service.dispatch.now = time.Now
	f.service.ordinary.Now = time.Now
	if err = f.sched.ReconcileAll(nil, f.now); err != nil {
		t.Fatal(err)
	}
	f.service.dispatch.AttachScheduler(f.sched)
	f.service.dispatch.AttachDispatchContext(t.Context())
	return f
}
func TestSourceHostScheduleQueuesBeforeActualPinnedRunner(t *testing.T) {
	f := sourceHost(t, "    - type: schedule\n      schedule: \"@every 1m\"")
	now := f.now.Add(time.Minute)
	f.sched.Tick(t.Context(), now)
	records, err := f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	record := records[0]
	if _, err = f.layout.FindRunDir(strings.TrimPrefix(record.ID, "trigger-")); err == nil {
		t.Fatal("execution preceded queue commit")
	}
	envelope, err := startintent.Parse(record.Payload)
	if err != nil || envelope.Source == nil || envelope.Target.ConfigGeneration != f.generation {
		t.Fatal(envelope, err)
	}
	writeFileContent(t, f.source, "invalid pending YAML: [")
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	completed, err := f.service.queue.Get(t.Context(), record.ID, "scheduler")
	if err != nil || completed.State != triggerqueue.Dispatched {
		t.Fatal(completed, err)
	}
	dir, err := f.layout.FindRunDir(completed.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil || id.Trigger.Kind != journal.TriggerSchedule {
		t.Fatal(id, err)
	}
	if err = startintent.VerifyIdentity(id, record); err != nil {
		t.Fatal(err)
	}
	f.sched.Tick(t.Context(), now)
	pending, err := f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}

func sourceWebhook(t *testing.T, f *eventHostFixture) http.Handler {
	t.Helper()
	gate, err := webhookhttp.NewDispatchGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	gate.Start()
	t.Cleanup(gate.Stop)
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "webhook"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	handler, err := webhookhttp.NewHandler([]byte("secret"), f.sched, log, gate, webhookhttp.WithDurableSignaler(f.sched))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
func sourceDelivery(h http.Handler, id, event, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, webhookhttp.Path, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-GitHub-Delivery", id)
	request.Header.Set("X-GitHub-Event", event)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	return response
}
func TestSourceHostWebhookReopenKeepsRecipientsAndPayloadAuthority(t *testing.T) {
	f := sourceHost(t, "    - type: webhook\n      events: [issues]")
	body := `{"action":"opened","repository":{"name":"` + f.entry.RepoRef.Name + `","owner":{"login":"` + f.entry.RepoRef.Owner + `"}}}`
	h := sourceWebhook(t, f)
	if response := sourceDelivery(h, "delivery", "issues", body); response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body)
	}
	records, err := f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	original := records[0]
	if err = f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = acceptedService(t, filepath.Join(f.layout.SchedulerDir(), "accepted-triggers.db"), f.service.dispatch)
	if err = f.setup.installOrdinaryStarts(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	log, _, err := journal.OpenInstanceLog(filepath.Join(t.TempDir(), "restarted-scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	// Reload no longer matches: retry still observes original receipt rather than dropping it.
	current := f.entry
	current.Signals = nil
	f.sched = localscheduler.New([]localscheduler.WorkflowEntry{current}, log, localscheduler.WithSourceQueue(f.setup.SourceStarts))
	f.service.dispatch.AttachScheduler(f.sched)
	h = sourceWebhook(t, f)
	if response := sourceDelivery(h, "delivery", "issues", body); response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body)
	}
	if response := sourceDelivery(h, "delivery", "issues", strings.Replace(body, "opened", "closed", 1)); response.Code != http.StatusServiceUnavailable {
		t.Fatal(response.Code, response.Body)
	}
	records, err = f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(records) != 1 || records[0].ID != original.ID {
		t.Fatal(records, err)
	}
	f.drain(t)
	refused, err := f.service.queue.Get(t.Context(), original.ID, "github-webhook")
	if err != nil || refused.State != triggerqueue.Rejected {
		t.Fatal(refused, err)
	}
	if _, err = f.layout.FindRunDir(strings.TrimPrefix(original.ID, "trigger-")); err == nil {
		t.Fatal("removed subscription started")
	}
}
func TestSourceHostWebhookStartsActualRunnerAfterAcceptance(t *testing.T) {
	f := sourceHost(t, "    - type: webhook\n      events: [issues]")
	body := `{"repository":{"name":"` + f.entry.RepoRef.Name + `","owner":{"login":"` + f.entry.RepoRef.Owner + `"}}}`
	if response := sourceDelivery(sourceWebhook(t, f), "live", "issues", body); response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body)
	}
	records, err := f.service.queue.Pending(t.Context(), 100)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	writeFileContent(t, f.source, "unsaved invalid yaml: [")
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	got, err := f.service.queue.Get(t.Context(), records[0].ID, "github-webhook")
	if err != nil || got.State != triggerqueue.Dispatched {
		t.Fatal(got, err)
	}
	dir, err := f.layout.FindRunDir(got.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil || id.Trigger.Ref != "github-webhook:issues" {
		t.Fatal(id, err)
	}
	if err = startintent.VerifyIdentity(id, records[0]); err != nil {
		t.Fatal(err)
	}
}
func TestSignalCallerKeyReplaysActualCLIWithoutSecondRun(t *testing.T) {
	root := initSignalDemo(t)
	for range 2 {
		code, out, stderr := runArgs(t, "signal", "--request-id", "same-delivery", "deploy", root)
		if code != 0 {
			t.Fatal(code, out, stderr)
		}
	}
	dirs, err := instance.NewLayout(root).RunDirs()
	if err != nil || len(dirs) != 1 {
		t.Fatal(dirs, err)
	}
	code, _, stderr := runArgs(t, "signal", "--request-id", "same-delivery", "different", root)
	if code == 0 || !strings.Contains(stderr, triggerqueue.ErrConflict.Error()) {
		t.Fatal(code, stderr)
	}
}
