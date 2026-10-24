//go:build cloudintegration

package control

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type countingStartClient struct {
	client.Client
	starts atomic.Int32
}

func (c *countingStartClient) ExecuteWorkflow(context.Context, client.StartWorkflowOptions, interface{}, ...interface{}) (client.WorkflowRun, error) {
	c.starts.Add(1)
	return nil, errors.New("refused: this test never starts a workflow")
}

// The L1 tick with an Admission set admits through it and never reaches the
// legacy start-fence drain: an Accepted row stays untouched by the Temporal
// client. The same fixture without an Admission does reach it (the premise).
func TestAdmissionReplacesTheLegacyDrainOnTheL1Tick(t *testing.T) {
	for _, withAdmission := range []bool{true, false} {
		t.Run(map[bool]string{true: "admission", false: "legacy"}[withAdmission], func(t *testing.T) {
			pool := controlTestPool(t)
			manager, err := lease.New(lease.Config{Pool: pool, InstanceID: "admission-test", Name: lease.ControllerName, Holder: "only"})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := manager.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Release(context.Background(), handle) })
			store, err := acceptance.New(pool, "admission-test")
			if err != nil {
				t.Fatal(err)
			}
			store.BindFence(manager, handle)
			if _, _, err := store.Accept(t.Context(), "request/1", "operator", []byte(`{}`), time.Now()); err != nil {
				t.Fatal(err)
			}
			temporal := &countingStartClient{}
			var calls []string
			admission := &recordingAdmission{calls: &calls}
			runtime := admissionRuntime(&calls, admission, nil)
			runtime.Pool, runtime.Lease, runtime.InstanceID, runtime.Temporal = pool, manager, "admission-test", temporal
			runtime.Configuration = testConfiguration{}
			runtime.Resolve = func(_ context.Context, r triggerqueue.Record) (engine.RunInput, error) {
				return engine.RunInput{AcceptanceID: r.ID, Gaggle: "test", WorkflowName: "remote", SelfExecutionDenied: true}, nil
			}
			if !withAdmission {
				runtime.Admission = nil
			}
			services, err := runtime.Build(t.Context(), handle)
			if err != nil {
				t.Fatal(err)
			}
			if err := services.L1(t.Context()); err != nil {
				t.Fatal(err)
			}
			ticks := 0
			for _, call := range calls {
				if call == "admission.tick" {
					ticks++
				}
			}
			switch {
			case withAdmission && (ticks != 1 || temporal.starts.Load() != 0):
				t.Fatalf("admission ticks = %d, legacy starts = %d; want 1 and 0", ticks, temporal.starts.Load())
			case !withAdmission && (ticks != 0 || temporal.starts.Load() == 0):
				t.Fatalf("premise: legacy drain ticks = %d, starts = %d; want 0 and >0", ticks, temporal.starts.Load())
			}
		})
	}
}
