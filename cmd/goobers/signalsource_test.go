package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

type sourceDaemonFixture struct {
	service   *durableTriggerService
	setup     *schedulerSetup
	scheduler *localscheduler.Scheduler
	workers   *sync.WaitGroup
	address   string
	close     func()
}

func openSourceDaemonFixture(t *testing.T, layout instance.Layout) sourceDaemonFixture {
	t.Helper()
	wg := &sync.WaitGroup{}
	setup, err := buildSchedulerSetup(t.Context(), layout, wg)
	if err != nil {
		t.Fatal(err)
	}
	closeSetup := sync.OnceFunc(func() {
		if err := setup.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(closeSetup)
	service, err := installOneShotSignalQueue(layout, setup)
	if err != nil {
		t.Fatal(err)
	}
	closeQueue := sync.OnceFunc(func() { _ = service.queue.Close() })
	t.Cleanup(closeQueue)
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, setup.SchedulerOptions()...)
	service.dispatch.AttachScheduler(sched)
	service.dispatch.AttachDispatchContext(t.Context())
	gate, err := webhookhttp.NewDispatchGate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Start() {
		t.Fatal("webhook gate did not start")
	}
	t.Cleanup(gate.Stop)
	handler, err := webhookhttp.NewHandler([]byte("source-secret"), sched, setup.InstanceLog, gate, webhookhttp.WithDurableSignaler(sched))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	var once sync.Once
	close := func() {
		once.Do(func() {
			server.Close()
			gate.Stop()
			sched.Wait()
			wg.Wait()
			closeQueue()
			closeSetup()
		})
	}
	t.Cleanup(close)
	return sourceDaemonFixture{service, setup, sched, wg, strings.TrimPrefix(server.URL, "http://"), close}
}

func TestSourceWebhookReopenRunsCapturedDefinitionOnce(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	workflow := strings.Replace(deterministicWorkflowYAML, "    - type: schedule\n      schedule: \"@every 24h\"\n", "    - type: webhook\n      events: [issues]\n", 1)
	if workflow == deterministicWorkflowYAML {
		t.Fatal("missing schedule fixture")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workflow = strings.Replace(workflow, `command: ["true"]`, fmt.Sprintf("command: [%q, \"-test.run=^TestSourceStageResult$\"]\n        env:\n          SOURCE_STAGE_RESULT: pass", self), 1)
	writeFileContent(t, workflowPath, workflow)
	first := openSourceDaemonFixture(t, layout)
	body := []byte(`{"action":"opened"}`)
	if status := postWebhook(t, first.address, "source-secret", "issues", "durable-delivery", body); status != http.StatusAccepted {
		t.Fatal(status)
	}
	pending, err := first.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	accepted := pending[0]
	envelope, err := startintent.Parse(accepted.Payload)
	if err != nil || envelope.Source == nil || !envelope.Source.Webhook {
		t.Fatal(envelope, err)
	}
	first.close() // The acknowledging host disappears before any dispatch attempt.
	writeFileContent(t, workflowPath, strings.Replace(workflow, "SOURCE_STAGE_RESULT: pass", "SOURCE_STAGE_RESULT: fail", 1))
	restarted := openSourceDaemonFixture(t, layout)
	if restarted.setup.ExecutionGeneration == envelope.Target.ConfigGeneration {
		t.Fatal("replacement did not change generation")
	}
	if status := postWebhook(t, restarted.address, "source-secret", "issues", "durable-delivery", body); status != http.StatusAccepted {
		t.Fatal(status)
	}
	pending, err = restarted.service.queue.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != accepted.ID {
		t.Fatal(pending, err)
	}
	if status := postWebhook(t, restarted.address, "source-secret", "issues", "durable-delivery", []byte(`{"action":"closed"}`)); status != http.StatusServiceUnavailable {
		t.Fatalf("changed delivery accepted: %d", status)
	}
	if err := restarted.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted.scheduler.Wait()
	restarted.workers.Wait()
	if err := restarted.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	record, err := restarted.service.queue.Get(t.Context(), accepted.ID, "github-webhook")
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
		t.Fatalf("captured successful stage did not complete: %v, %v", phase, err)
	}
	if status := postWebhook(t, restarted.address, "source-secret", "issues", "durable-delivery", body); status != http.StatusAccepted {
		t.Fatal(status)
	}
	if err := restarted.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("replayed delivery created %d starts", starts)
	}
}

// Re-exec the Go test binary for an OS-independent stage outcome.
func TestSourceStageResult(t *testing.T) {
	if os.Getenv("SOURCE_STAGE_RESULT") == "fail" {
		t.Fatal("replacement definition must not execute")
	}
}
