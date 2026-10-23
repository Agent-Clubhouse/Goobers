//go:build cloudintegration

package control

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type describingTemporal struct {
	client.Client
	describe    func(runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	describeCtx func(ctx context.Context, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

func (d describingTemporal) DescribeWorkflowExecution(ctx context.Context, workflowID, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if d.describeCtx != nil {
		return d.describeCtx(ctx, workflowID)
	}
	return d.describe(workflowID)
}

func insertAgedRunPins(t *testing.T, pool *pgxpool.Pool, instance string, runIDs ...string) {
	t.Helper()
	for _, runID := range runIDs {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
				goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch, created_at)
			VALUES ($1, $2, $2, 'g', 'w', 1, 'wd', 'gd', 'cg', '{}', 'goobers', 'id', 1, clock_timestamp()-interval '8 days')`,
			instance, runID); err != nil {
			t.Fatal(err)
		}
	}
}

func runPinTestServices(t *testing.T, pool *pgxpool.Pool, instance string, temporal client.Client, report func(error)) EpochServices {
	t.Helper()
	manager, err := lease.New(lease.Config{Pool: pool, InstanceID: instance, Name: lease.ControllerName, Holder: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Configuration: testConfiguration{}, Pool: pool, InstanceID: instance, Lease: manager, Temporal: temporal, TaskQueue: "q",
		Report: report,
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
	return services
}

// L2 has no deadline of its own and also renews claims and reaps, so a
// Temporal that hangs must cost the run-pin sweep its budget once, not one
// timeout per candidate, and be reported once, not on every pass.
func TestL2RunPinSweepIsBoundedWhenTemporalHangs(t *testing.T) {
	pool := controlTestPool(t)
	const instance = "run-pins-hang"
	insertAgedRunPins(t, pool, instance, "a", "b", "c")
	previous := runPinSweepBudget
	runPinSweepBudget = 200 * time.Millisecond
	t.Cleanup(func() { runPinSweepBudget = previous })
	var calls atomic.Int32
	temporal := describingTemporal{describeCtx: func(ctx context.Context, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	var reported []error
	services := runPinTestServices(t, pool, instance, temporal, func(err error) { reported = append(reported, err) })
	for pass := range 2 {
		started := time.Now()
		if err := services.L2(t.Context()); err != nil {
			t.Fatalf("pass %d: L2 returned %v", pass, err)
		}
		if elapsed := time.Since(started); elapsed > 20*runPinSweepBudget {
			t.Fatalf("pass %d: L2 took %v against a %v sweep budget", pass, elapsed, runPinSweepBudget)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("hanging Temporal was called %d times over two passes, want one per pass", calls.Load())
	}
	if len(reported) != 1 || !errors.Is(reported[0], acceptance.ErrRunStatusUnavailable) {
		t.Fatalf("reported %v, want the outage once", reported)
	}
	var terminal int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.run_pin WHERE instance_id=$1 AND terminal_at IS NOT NULL`, instance).Scan(&terminal); err != nil || terminal != 0 {
		t.Fatalf("%d pins marked terminal during an outage (%v)", terminal, err)
	}
}

// An L2 error ends every host loop and drops the epoch, so a run Temporal
// cannot vouch for must be reported, never returned, and must not keep the
// closed run beside it from being swept.
func TestL2RunPinSweepReportsUnconfirmedRunsAndStillSweepsClosedOnes(t *testing.T) {
	pool := controlTestPool(t)
	const instance = "run-pins"
	insertAgedRunPins(t, pool, instance, "blip", "done")
	blip := errors.New("temporal: workflow history is corrupt")
	temporal := describingTemporal{describe: func(runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
		if runID == "blip" {
			return nil, blip
		}
		return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_COMPLETED}}, nil
	}}
	var reported []error
	services := runPinTestServices(t, pool, instance, temporal, func(err error) { reported = append(reported, err) })
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
