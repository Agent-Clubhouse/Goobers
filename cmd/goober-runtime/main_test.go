package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gooberruntime"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestConfigFromEnvDefaultsAndAliases(t *testing.T) {
	t.Setenv("GOOBERS_TEMPORAL_HOSTPORT", "temporal:7233")
	t.Setenv("GOOBERS_TEMPORAL_NAMESPACE", "goobers")
	t.Setenv("GOOBERS_TASK_QUEUE", "runtime-queue")
	t.Setenv("GOOBERS_WORKSPACE_ROOT", "/work")
	t.Setenv("GOOBERS_COPILOT_HARNESS_COMMAND", "copilot-goober --json")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.temporalHostPort != "temporal:7233" {
		t.Errorf("temporalHostPort = %q", cfg.temporalHostPort)
	}
	if cfg.temporalNamespace != "goobers" {
		t.Errorf("temporalNamespace = %q", cfg.temporalNamespace)
	}
	if cfg.taskQueue != "runtime-queue" {
		t.Errorf("taskQueue = %q", cfg.taskQueue)
	}
	if cfg.workspaceRoot != "/work" {
		t.Errorf("workspaceRoot = %q", cfg.workspaceRoot)
	}
	wantCommand := []string{"copilot-goober", "--json"}
	if !reflect.DeepEqual(cfg.harnessCommand, wantCommand) {
		t.Errorf("harnessCommand = %#v, want %#v", cfg.harnessCommand, wantCommand)
	}
}

func TestRuntimeEngineConfigSupportsTemporalCompatibilityAliases(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want instance.EngineConfig
	}{
		{
			name: "goobers aliases",
			env: map[string]string{
				"GOOBERS_TEMPORAL_ADDRESS":    "temporal:7233",
				"GOOBERS_TEMPORAL_TASK_QUEUE": "runtime",
			},
			want: instance.EngineConfig{HostPort: "temporal:7233", Namespace: instance.DefaultTemporalNamespace, TaskQueue: "runtime"},
		},
		{
			name: "temporal aliases",
			env: map[string]string{
				"TEMPORAL_ADDRESS":    "temporal:7233",
				"TEMPORAL_NAMESPACE":  "production",
				"TEMPORAL_TASK_QUEUE": "runtime",
			},
			want: instance.EngineConfig{HostPort: "temporal:7233", Namespace: "production", TaskQueue: "runtime"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			got, err := runtimeEngineConfig("")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("runtimeEngineConfig = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigFromEnvUsesTaskQueueDefault(t *testing.T) {
	t.Setenv("GOOBERS_COPILOT_HARNESS_COMMAND", "copilot-goober")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.taskQueue != instance.DefaultEngineTaskQueue {
		t.Errorf("taskQueue = %q, want %q", cfg.taskQueue, instance.DefaultEngineTaskQueue)
	}
	if cfg.temporalHostPort != "127.0.0.1:7233" {
		t.Errorf("temporalHostPort = %q, want default hostport", cfg.temporalHostPort)
	}
}

func TestConfigFromEnvLoadsInstanceEngine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GOOBERS_INSTANCE_ROOT", root)
	t.Setenv("GOOBERS_COPILOT_HARNESS_COMMAND", "copilot-goober")
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte(`
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
engine:
  hostPort: temporal:7233
  namespace: goobers
  taskQueue: runtime
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.temporalHostPort != "temporal:7233" || cfg.temporalNamespace != "goobers" || cfg.taskQueue != "runtime" {
		t.Fatalf("engine config = %+v", cfg)
	}
}

func TestRunRequiresHarnessCommand(t *testing.T) {
	called := false
	restore := replaceWorkerRunner(func(config, invoke.Goober, journal.Scrubber) (runtimeWorker, error) {
		called = true
		return &fakeRuntimeWorker{}, nil
	})
	defer restore()

	err := run(context.Background(), discardLogger())
	if err == nil {
		t.Fatal("expected missing harness command error")
	}
	if called {
		t.Fatal("worker runner must not be constructed when config is invalid")
	}
}

func TestRunStartsWorkerAndStopsOnContextCancellation(t *testing.T) {
	t.Setenv("GOOBERS_COPILOT_HARNESS_COMMAND", "copilot-goober --json")
	t.Setenv("GOOBERS_TASK_QUEUE", "runtime-queue")
	ctx, cancel := context.WithCancel(context.Background())
	fw := &fakeRuntimeWorker{onStart: cancel}
	var gotConfig config
	var gotGoober invoke.Goober
	var gotScrubber journal.Scrubber
	restore := replaceWorkerRunner(func(cfg config, goober invoke.Goober, scrubber journal.Scrubber) (runtimeWorker, error) {
		gotConfig = cfg
		gotGoober = goober
		gotScrubber = scrubber
		return fw, nil
	})
	defer restore()

	if err := run(ctx, discardLogger()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotConfig.taskQueue != "runtime-queue" {
		t.Errorf("taskQueue = %q, want runtime-queue", gotConfig.taskQueue)
	}
	if _, ok := gotGoober.(*gooberruntime.Runtime); !ok {
		t.Fatalf("goober = %T, want *gooberruntime.Runtime", gotGoober)
	}
	if gotScrubber == nil {
		t.Fatal("worker runner did not receive the runtime scrubber")
	}
	if fw.starts != 1 {
		t.Errorf("starts = %d, want 1", fw.starts)
	}
	if fw.stops != 1 {
		t.Errorf("stops = %d, want 1", fw.stops)
	}
	if fw.closes != 0 {
		t.Errorf("closes = %d, want 0 before Stop cleanup path", fw.closes)
	}
}

func TestRunClosesWorkerWhenStartFails(t *testing.T) {
	t.Setenv("GOOBERS_COPILOT_HARNESS_COMMAND", "copilot-goober")
	startErr := errors.New("worker unavailable")
	fw := &fakeRuntimeWorker{startErr: startErr}
	restore := replaceWorkerRunner(func(config, invoke.Goober, journal.Scrubber) (runtimeWorker, error) {
		return fw, nil
	})
	defer restore()

	err := run(context.Background(), discardLogger())
	if !errors.Is(err, startErr) {
		t.Fatalf("run error = %v, want %v", err, startErr)
	}
	if fw.closes != 1 {
		t.Errorf("closes = %d, want 1", fw.closes)
	}
	if fw.stops != 0 {
		t.Errorf("stops = %d, want 0 when Start fails", fw.stops)
	}
}

func TestGooberRuntimePreparerRegistersADOCredential(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "ado-token")
	t.Setenv("GOOBERS_ADO_ORG", "ado-org")
	t.Setenv("GOOBERS_ADO_PROJECT", "ado-project")

	registry, scrubber := journal.DefaultScrubber()
	observer := telemetry.NewStageRateLimitObserver("")
	preparer := gooberRuntimePreparer(config{}, registry, observer)
	resolver, ok := preparer.Providers.(gooberruntime.EnvProviderResolver)
	if !ok {
		t.Fatalf("provider resolver = %T, want EnvProviderResolver", preparer.Providers)
	}
	if _, err := resolver.RepoProvider(apiv1.ProviderADO, apiv1.RepoRef{}); err != nil {
		t.Fatalf("RepoProvider(ADO): %v", err)
	}
	if resolver.SecretRegistrar != registry {
		t.Fatal("provider resolver is not wired to the runtime output registry")
	}
	if resolver.RateLimitObserver != observer {
		t.Fatal("provider resolver is not wired to runtime rate-limit telemetry")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("goobers:ado-token"))
	if got := string(scrubber.Scrub([]byte(encoded))); got != journal.Redacted {
		t.Fatalf("encoded ADO credential was not registered: %q", got)
	}
}

func TestRegisterEngineWiresGooberActivities(t *testing.T) {
	const secret = "registered-exact-secret"
	fw := &fakeTemporalWorker{}
	goober := resultGoober{summary: "result contains " + secret}
	scheduleService := &fakeWorkflowService{}
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(secret))

	registerEngine(fw, fakeTemporalClient{service: scheduleService}, goober, scrubber)

	if len(fw.workflows) != 4 {
		t.Fatalf("registered workflows = %d, want 4", len(fw.workflows))
	}
	if len(fw.activities) != 1 {
		t.Fatalf("registered activities = %d, want 1", len(fw.activities))
	}
	activities, ok := fw.activities[0].(*engine.Activities)
	if !ok {
		t.Fatalf("activity registration = %T, want *engine.Activities", fw.activities[0])
	}
	if activities.Goober != goober {
		t.Fatal("registered engine activities did not receive the runtime goober seam")
	}
	if activities.ScheduleService != scheduleService {
		t.Fatal("registered engine activities did not receive the Temporal schedule service")
	}
	activities.Workspaces = runtimeTestWorkspaces{path: t.TempDir()}
	result, err := activities.InvokeGoober(context.Background(), apiv1.InvocationEnvelope{
		RunID: "run-1", TaskID: "run-1:implement",
	}, "")
	if err != nil {
		t.Fatalf("InvokeGoober: %v", err)
	}
	if strings.Contains(result.Summary, secret) || !strings.Contains(result.Summary, journal.Redacted) {
		t.Fatalf("engine activity result was not exact-value scrubbed: %q", result.Summary)
	}
}

func replaceWorkerRunner(fn func(config, invoke.Goober, journal.Scrubber) (runtimeWorker, error)) func() {
	prev := newWorkerRunner
	newWorkerRunner = fn
	return func() { newWorkerRunner = prev }
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeRuntimeWorker struct {
	startErr error
	onStart  func()
	starts   int
	stops    int
	closes   int
}

func (f *fakeRuntimeWorker) Start() error {
	f.starts++
	if f.onStart != nil {
		f.onStart()
	}
	return f.startErr
}

func (f *fakeRuntimeWorker) Stop() {
	f.stops++
}

func (f *fakeRuntimeWorker) Close() {
	f.closes++
}

type resultGoober struct {
	summary string
}

func (g resultGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: g.summary}, nil
}

func (resultGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, nil
}

type runtimeTestWorkspaces struct {
	path string
}

func (w runtimeTestWorkspaces) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	return runtimeTestWorkspace(w), nil
}

type runtimeTestWorkspace struct {
	path string
}

func (w runtimeTestWorkspace) Path() string               { return w.path }
func (runtimeTestWorkspace) Remove(context.Context) error { return nil }

type fakeTemporalWorker struct {
	workflows  []interface{}
	activities []interface{}
}

type fakeTemporalClient struct {
	client.Client
	service workflowservice.WorkflowServiceClient
}

func (f fakeTemporalClient) WorkflowService() workflowservice.WorkflowServiceClient {
	return f.service
}

type fakeWorkflowService struct {
	workflowservice.WorkflowServiceClient
}

var _ worker.Worker = (*fakeTemporalWorker)(nil)

func (f *fakeTemporalWorker) RegisterWorkflow(w interface{}) {
	f.workflows = append(f.workflows, w)
}

func (f *fakeTemporalWorker) RegisterWorkflowWithOptions(w interface{}, _ workflow.RegisterOptions) {
	f.workflows = append(f.workflows, w)
}

func (f *fakeTemporalWorker) RegisterDynamicWorkflow(w interface{}, _ workflow.DynamicRegisterOptions) {
	f.workflows = append(f.workflows, w)
}

func (f *fakeTemporalWorker) RegisterActivity(a interface{}) {
	f.activities = append(f.activities, a)
}

func (f *fakeTemporalWorker) RegisterActivityWithOptions(a interface{}, _ activity.RegisterOptions) {
	f.activities = append(f.activities, a)
}

func (f *fakeTemporalWorker) RegisterDynamicActivity(a interface{}, _ activity.DynamicRegisterOptions) {
	f.activities = append(f.activities, a)
}

func (f *fakeTemporalWorker) RegisterNexusService(*nexus.Service) {}

func (f *fakeTemporalWorker) Start() error { return nil }

func (f *fakeTemporalWorker) Run(<-chan interface{}) error { return nil }

func (f *fakeTemporalWorker) Stop() {}
