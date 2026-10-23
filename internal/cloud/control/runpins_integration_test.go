//go:build cloudintegration

package control

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type describingTemporal struct {
	client.Client
	describe func(runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

func (d describingTemporal) DescribeWorkflowExecution(_ context.Context, workflowID, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return d.describe(workflowID)
}

// An L2 error ends every host loop and drops the epoch, so a run Temporal
// cannot vouch for must be reported, never returned, and must not keep the
// closed run beside it from being swept.
func TestL2RunPinSweepReportsUnconfirmedRunsAndStillSweepsClosedOnes(t *testing.T) {
	pool := controlTestPool(t)
	const instance = "run-pins"
	for _, runID := range []string{"blip", "done"} {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
				goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch, created_at)
			VALUES ($1, $2, $2, 'g', 'w', 1, 'wd', 'gd', 'cg', '{}', 'goobers', 'id', 1, clock_timestamp()-interval '8 days')`,
			instance, runID); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: instance, Name: lease.ControllerName, Holder: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	blip := errors.New("temporal: deadline exceeded")
	temporal := describingTemporal{describe: func(runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
		if runID == "blip" {
			return nil, blip
		}
		return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_COMPLETED}}, nil
	}}
	var reported []error
	runtime := Runtime{Configuration: testConfiguration{}, Pool: pool, InstanceID: instance, Lease: manager, Temporal: temporal, TaskQueue: "q",
		Report: func(err error) { reported = append(reported, err) },
		Resolve: func(context.Context, triggerqueue.Record) (engine.RunInput, error) {
			return engine.RunInput{}, errors.New("no starts in this test")
		},
		Services: func(context.Context, *lease.Handle) (EpochServices, error) {
			s := testServices()
			s.RestoreObservations = func(context.Context, []Observation) error { return nil }
			s.CaptureObservations = func(context.Context) ([]Observation, error) { return nil, nil }
			return s, nil
		}}
	services, err := runtime.Build(t.Context(), handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := services.L2(t.Context()); err != nil {
		t.Fatalf("L2 returned %v; a Temporal error must not end the epoch", err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], blip) {
		t.Fatalf("reported %v, want only the unconfirmed run", reported)
	}
	terminal := map[string]bool{}
	rows, err := pool.Query(t.Context(), `SELECT run_id, terminal_at IS NOT NULL FROM control.run_pin WHERE instance_id=$1`, instance)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var done bool
		if err := rows.Scan(&runID, &done); err != nil {
			t.Fatal(err)
		}
		terminal[runID] = done
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(terminal) != 2 || !terminal["done"] || terminal["blip"] {
		t.Fatalf("pins terminal = %v, want the closed run marked and the unconfirmed run kept live", terminal)
	}
}
