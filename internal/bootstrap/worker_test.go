package bootstrap

import (
	"context"
	"io"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestNewStarterDefaultsTaskQueue(t *testing.T) {
	// A nil client is fine for construction (the starter only dials on Start).
	if NewStarter(nil, "") == nil {
		t.Fatal("NewStarter returned nil")
	}
	if NewStarter(nil, "custom-queue") == nil {
		t.Fatal("NewStarter with explicit queue returned nil")
	}
	if DefaultTaskQueue == "" {
		t.Fatal("DefaultTaskQueue must be set")
	}
}

func TestRegisterEngineWiresPublicScheduleReconciler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error",
		Stdout:   io.Discard,
		Stderr:   io.Discard,
		ExtraArgs: []string{
			"--dynamic-config-value", "history.enableCHASMSchedulerCreation=true",
		},
	})
	if err != nil {
		t.Fatalf("start Temporal dev server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop Temporal dev server: %v", err)
		}
	})

	const (
		namespace = "default"
		taskQueue = "goobers-bootstrap-schedule"
	)
	temporalClient := server.Client()
	temporalWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	RegisterEngine(temporalWorker, temporalClient, EngineDeps{})
	if err := temporalWorker.Start(); err != nil {
		t.Fatalf("start Temporal worker: %v", err)
	}
	t.Cleanup(temporalWorker.Stop)

	reconciler, err := engine.NewScheduleReconciler(temporalClient, namespace, taskQueue, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := engine.ScheduleSnapshot{
		InstanceID:       "bootstrap-test",
		ConfigSHA:        "abc123",
		ConfigGeneration: 1,
		Runs: []engine.RunInput{{
			Gaggle:       "web",
			WorkflowName: "implement",
			Spec: apiv1.WorkflowSpec{
				Triggers: []apiv1.Trigger{{
					Type:     apiv1.TriggerSchedule,
					Schedule: "0 0 1 1 *",
				}},
			},
		}},
	}
	if err := reconciler.Reconcile(ctx, snapshot); err != nil {
		t.Fatalf("reconcile schedule through registered worker: %v", err)
	}
	id := engine.ScheduleID(snapshot.InstanceID, "web", "implement", 0)
	description, err := temporalClient.ScheduleClient().GetHandle(ctx, id).Describe(ctx)
	if err != nil {
		t.Fatalf("describe reconciled schedule: %v", err)
	}
	action, ok := description.Schedule.Action.(*client.ScheduleWorkflowAction)
	if !ok || action.ID != id {
		t.Fatalf("reconciled schedule action = %#v", description.Schedule.Action)
	}
}
