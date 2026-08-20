package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/scheduler"
	"github.com/goobers/goobers/internal/telemetry"
	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeTrigger drives superviseTrigger through a scripted sequence of Watch
// outcomes.
type fakeTrigger struct {
	mu       sync.Mutex
	calls    int
	behavior func(call int) error
}

func (f *fakeTrigger) Name() string { return "fake" }

func (f *fakeTrigger) Watch(_ context.Context, _ chan<- scheduler.Event) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	fn := f.behavior
	f.mu.Unlock()
	return fn(call)
}

func (f *fakeTrigger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSuperviseTriggerRetriesThenStops: a transient Watch error must not kill
// polling — it retries (logging) until the context is cancelled.
func TestSuperviseTriggerRetriesThenStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTrigger{behavior: func(call int) error {
		if call >= 3 {
			cancel() // simulate shutdown after a few failed attempts
		}
		return errors.New("transient backlog API error")
	}}

	done := make(chan struct{})
	go func() {
		superviseTrigger(ctx, quietLogger(), ft, make(chan scheduler.Event), time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseTrigger did not stop on context cancel")
	}
	if ft.count() < 3 {
		t.Errorf("expected the trigger to be retried after errors, got %d calls", ft.count())
	}
}

// TestSuperviseTriggerStopsOnCleanReturn: a clean Watch return (source closed)
// ends supervision immediately without retrying.
func TestSuperviseTriggerStopsOnCleanReturn(t *testing.T) {
	ft := &fakeTrigger{behavior: func(int) error { return nil }}
	superviseTrigger(context.Background(), quietLogger(), ft, make(chan scheduler.Event), time.Millisecond)
	if ft.count() != 1 {
		t.Errorf("expected exactly one Watch call on clean return, got %d", ft.count())
	}
}

// TestSuperviseTriggerStopsWhenCancelledDuringBackoff: cancelling during the
// backoff wait ends the loop promptly.
func TestSuperviseTriggerStopsWhenCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ft := &fakeTrigger{behavior: func(int) error { return errors.New("fail") }}
	go func() {
		time.Sleep(5 * time.Millisecond) // Intentional delayed cancellation exercises the scheduler's error loop.
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		superviseTrigger(ctx, quietLogger(), ft, make(chan scheduler.Event), time.Hour) // long backoff
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseTrigger did not honor cancellation during backoff")
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("GOOBERS_CONFIG_DIR", "")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.pollInterval != 30*time.Second {
		t.Errorf("pollInterval default = %v", cfg.pollInterval)
	}
	if cfg.triggerBackoff != 5*time.Second {
		t.Errorf("triggerBackoff default = %v", cfg.triggerBackoff)
	}
	if cfg.taskQueue == "" {
		t.Error("taskQueue should default to the engine queue")
	}
}

func TestConfigFromEnvLoadsInstanceEngine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOOBERS_INSTANCE_ROOT", root)
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte(`
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
engine:
  hostPort: temporal:7233
  namespace: goobers
  taskQueue: scheduler
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.temporalHostPort != "temporal:7233" || cfg.temporalNamespace != "goobers" || cfg.taskQueue != "scheduler" {
		t.Fatalf("engine config = %+v", cfg)
	}
}

func TestSchedulerADOCredentialSource(t *testing.T) {
	if source, err := schedulerADOCredentialSource(apiv1.ProviderGitHub, config{adoAuthKind: "azure-cli"}); err != nil || source != nil {
		t.Fatalf("GitHub source = %T, %v", source, err)
	}
	source, err := schedulerADOCredentialSource(apiv1.ProviderADO, config{adoAuthKind: "azure-cli", adoTenant: "tenant"})
	if err != nil || source == nil {
		t.Fatalf("ADO Azure CLI source = %T, %v", source, err)
	}
	if _, err := schedulerADOCredentialSource(apiv1.ProviderADO, config{adoAuthKind: "unknown"}); err == nil {
		t.Fatal("unknown ADO auth kind did not fail")
	}
	if _, err := schedulerADOCredentialSource(apiv1.ProviderADO, config{}); err == nil {
		t.Fatal("missing ADO auth did not fail")
	}
}

func TestBacklogFieldPredicateForWorkflowCombinesGaggleAndTrigger(t *testing.T) {
	gaggle := apiv1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: "engineering"},
		Spec: apiv1.GaggleSpec{Backlog: apiv1.BacklogRef{
			FieldPredicate: `fields["state"] == "open"`,
		}},
	}
	workflows := []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "engineering",
			Triggers: []apiv1.Trigger{{
				Type:           apiv1.TriggerBacklogItem,
				FieldPredicate: `fields["number"] >= 10`,
			}},
		},
	}}
	predicate, err := backlogFieldPredicateForWorkflow(gaggle, workflows, "implementation")
	if err != nil {
		t.Fatalf("backlogFieldPredicateForWorkflow: %v", err)
	}
	for _, tt := range []struct {
		fields fieldpredicate.Fields
		want   bool
	}{
		{fields: fieldpredicate.Fields{"state": "open", "number": int64(10)}, want: true},
		{fields: fieldpredicate.Fields{"state": "closed", "number": int64(10)}},
		{fields: fieldpredicate.Fields{"state": "open", "number": int64(9)}},
	} {
		got, err := predicate.Matches(tt.fields)
		if err != nil {
			t.Fatalf("Matches(%v): %v", tt.fields, err)
		}
		if got != tt.want {
			t.Fatalf("Matches(%v) = %v, want %v", tt.fields, got, tt.want)
		}
	}
}

func TestSchedulerADORegistryScrubsTelemetryExporter(t *testing.T) {
	const token = "ado-token-value"
	registry, scrubber := journal.DefaultScrubber()
	if _, _, err := bootstrap.BacklogProviderFor(apiv1.BacklogRef{
		Provider: apiv1.ProviderADO,
		Project:  "organization/project",
	}, token, nil, registry, nil); err != nil {
		t.Fatalf("BacklogProviderFor: %v", err)
	}

	exporter := telemetrytest.NewMemoryExporter()
	cfg := schedulerTelemetryConfig(config{}, scrubber)
	cfg.SpanExporter = exporter
	cfg.Batch = false
	client, err := telemetry.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	encoded := base64.StdEncoding.EncodeToString([]byte("goobers:" + token))
	_, span, err := client.StartSchedulerSpan(context.Background(), telemetry.SchedulerAttributes{
		Gaggle: "gaggle", WorkflowID: "workflow", Action: encoded,
	})
	if err != nil {
		t.Fatalf("StartSchedulerSpan: %v", err)
	}
	span.End()

	spans := exporter.Spans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	found := false
	for _, attr := range spans[0].Attributes() {
		if attr.Key == telemetry.AttrStage {
			found = true
			if attr.Value.AsString() != journal.Redacted {
				t.Fatalf("scheduler stage = %q, want redacted", attr.Value.AsString())
			}
		}
	}
	if !found {
		t.Fatal("scheduler stage attribute was not exported")
	}
}
