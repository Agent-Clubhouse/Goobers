//go:build cloudintegration

package acceptance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
)

// pgStartClient controls Temporal responses; custody and takeover use real PG16.
type pgStartClient struct {
	client.Client
	mu           sync.Mutex
	memo         *commonpb.Memo
	input        engine.RunInput
	executions   int
	calls        int
	loseResponse bool
	before       func()
}
type pgStartRun struct {
	client.WorkflowRun
	id string
}

func (r pgStartRun) GetID() string { return r.id }
func (c *pgStartClient) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	if c.before != nil {
		before := c.before
		c.before = nil
		before()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.memo != nil {
		return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("exists", "request", "execution")
	}
	c.memo = &commonpb.Memo{Fields: make(map[string]*commonpb.Payload)}
	for key, value := range options.Memo {
		payload, err := converter.GetDefaultDataConverter().ToPayload(value)
		if err != nil {
			return nil, err
		}
		c.memo.Fields[key] = payload
	}
	c.input = args[0].(engine.RunInput)
	c.executions++
	if c.loseResponse {
		c.loseResponse = false
		return nil, errors.New("start response lost")
	}
	return pgStartRun{id: options.ID}, nil
}
func (c *pgStartClient) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Memo: c.memo, Type: &commonpb.WorkflowType{Name: "Run"}}}, nil
}

func acceptedStartInput(t *testing.T, store *Store) engine.RunInput {
	t.Helper()
	record, _, err := store.Accept(t.Context(), "manual/start", "operator", []byte(`{"workflow":"flow"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return engine.RunInput{AcceptanceID: record.ID, Gaggle: "web", WorkflowName: "flow", Version: 1,
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober", ConfigGeneration: "sha256:generation"}
}

func testRunPins(acceptanceID, runID string) RunPins {
	return RunPins{RunID: runID, AcceptanceID: acceptanceID, Gaggle: "web", Workflow: "flow", WorkflowVersion: 1,
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober", ConfigGeneration: "sha256:generation", InputDigest: "sha256:input"}
}

func TestPGStartRecordsTheStartedRunsPinsOnce(t *testing.T) {
	store, pool := testStore(t, 1)
	temporal := &pgStartClient{}
	starter := startfence.New(temporal, "cloud", NewStartAdmission(store), testGenerationAdmission{})
	in := acceptedStartInput(t, store)
	result, err := starter.Start(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	var generation, workflowDigest, inputDigest string
	var epoch int64
	if err := pool.QueryRow(t.Context(), `SELECT config_generation, workflow_digest, input_digest, owner_epoch FROM control.run_pin
		WHERE instance_id=$1 AND run_id=$2`, store.instanceID, result.RunID).Scan(&generation, &workflowDigest, &inputDigest, &epoch); err != nil {
		t.Fatalf("start recorded no pins: %v", err)
	}
	if generation != in.ConfigGeneration || workflowDigest != in.WorkflowDigest || epoch != store.handle.Epoch || inputDigest == "" {
		t.Fatalf("recorded pins %s/%s/%s@%d do not match the started input", generation, workflowDigest, inputDigest, epoch)
	}
	identity, err := (&startCustody{store: store}).ReadStart(t.Context(), in.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	pins := testRunPins(identity.AcceptanceID, identity.RunID)
	var recorded RunPins
	if err := pool.QueryRow(t.Context(), `SELECT input_digest FROM control.run_pin WHERE run_id=$1`, identity.RunID).Scan(&recorded.InputDigest); err != nil {
		t.Fatal(err)
	}
	pins.InputDigest = recorded.InputDigest
	pins.BranchNamespace = in.BranchNamespace
	// Repeating the confirmation of the same start is a no-op ...
	if err := store.ConfirmStart(t.Context(), identity.AcceptanceID, identity.RunID, pins); err != nil {
		t.Fatalf("repeated confirmation of the same pins: %v", err)
	}
	// ... but no confirmation can rewrite a recorded run's pins.
	moved := pins
	moved.ConfigGeneration = "sha256:reloaded-generation"
	if err := store.ConfirmStart(t.Context(), identity.AcceptanceID, identity.RunID, moved); !errors.Is(err, ErrRunPinsDiffer) {
		t.Fatalf("confirmation with different pins = %v, want ErrRunPinsDiffer", err)
	}
	if err := store.ConfirmStart(t.Context(), identity.AcceptanceID, "other-run", pins); err == nil {
		t.Fatal("confirmed pins for a different run")
	}
}

func TestPGStartUsesDurableIdentityAndRejectsInventedPair(t *testing.T) {
	store, _ := testStore(t, 1)
	temporal := &pgStartClient{}
	starter := startfence.New(temporal, "cloud", NewStartAdmission(store), testGenerationAdmission{})
	invented := engine.RunInput{AcceptanceID: "trigger-invented", RunID: "invented", OwnerEpoch: store.handle.Epoch, StarterID: store.handle.Holder}
	if _, err := starter.Start(t.Context(), invented); err == nil {
		t.Fatal("unpersisted matching strings reached Temporal")
	}
	if temporal.calls != 0 {
		t.Fatal("missing row caused a Temporal start")
	}
	in := acceptedStartInput(t, store)
	temporal.before = func() {
		provisional, err := store.Get(t.Context(), in.AcceptanceID, "operator")
		if err != nil || provisional.State != "dispatching" || provisional.RunID == "" {
			t.Fatalf("Temporal reached before durable provisional reservation: %+v %v", provisional, err)
		}
	}
	result, err := starter.Start(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(t.Context(), in.AcceptanceID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "dispatched" {
		t.Fatalf("owned start not confirmed: %+v", record)
	}
	if result.RunID != record.RunID || temporal.input.RunID != record.RunID || temporal.input.OwnerEpoch != store.handle.Epoch || temporal.input.StarterID != store.handle.Holder || temporal.input.InstanceID != store.instanceID {
		t.Fatalf("start not sourced from durable custody: result=%+v input=%+v row=%+v", result, temporal.input, record)
	}
}

func TestPGLostStartResponseIsMine(t *testing.T) {
	store, _ := testStore(t, 1)
	temporal := &pgStartClient{loseResponse: true}
	starter := startfence.New(temporal, "cloud", NewStartAdmission(store), testGenerationAdmission{})
	in := acceptedStartInput(t, store)
	if _, err := starter.Start(t.Context(), in); err == nil {
		t.Fatal("lost response unexpectedly succeeded")
	}
	provisional, readErr := store.Get(t.Context(), in.AcceptanceID, "operator")
	if readErr != nil || provisional.State != "dispatching" {
		t.Fatalf("lost response lost provisional custody: %+v %v", provisional, readErr)
	}
	result, err := starter.Start(t.Context(), in)
	if err != nil || !result.AlreadyRunning || result.NotMine || result.Adopt || temporal.executions != 1 {
		t.Fatalf("lost response = %+v, %v, executions=%d", result, err, temporal.executions)
	}
}

func TestPGReconcileAdoptsPredecessorStart(t *testing.T) {
	old, pool := testStore(t, 1)
	temporal := &pgStartClient{loseResponse: true}
	in := acceptedStartInput(t, old)
	if _, err := startfence.New(temporal, "cloud", NewStartAdmission(old), testGenerationAdmission{}).Start(t.Context(), in); err == nil {
		t.Fatal("lost response unexpectedly succeeded")
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	result, err := startfence.New(temporal, "cloud", NewStartAdmission(current), testGenerationAdmission{}).Start(t.Context(), in)
	if err != nil || !result.Adopt || result.NotMine || temporal.executions != 1 {
		t.Fatalf("adopt = %+v, %v", result, err)
	}
	var epoch int64
	var state string
	if err := pool.QueryRow(t.Context(), "SELECT owner_epoch,state FROM control.acceptance WHERE instance_id=$1 AND id=$2", current.instanceID, in.AcceptanceID).Scan(&epoch, &state); err != nil {
		t.Fatal(err)
	}
	if epoch != current.handle.Epoch || state != "dispatched" {
		t.Fatalf("adoption not fenced into current owner: epoch=%d state=%s", epoch, state)
	}
}

func TestPGZombieStartPreservesNotMineAndWinnerCustody(t *testing.T) {
	old, pool := testStore(t, 1)
	temporal := &pgStartClient{}
	in := acceptedStartInput(t, old)
	temporal.before = func() {
		// Lose the lease after the old read fence but before its Temporal RPC.
		if err := old.manager.Release(t.Context(), old.handle); err != nil {
			t.Fatal(err)
		}
		current := successorStore(t, pool)
		result, err := startfence.New(temporal, "cloud", NewStartAdmission(current), testGenerationAdmission{}).Start(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if result.NotMine {
			t.Fatal("current owner refused")
		}
	}
	result, err := startfence.New(temporal, "cloud", NewStartAdmission(old), testGenerationAdmission{}).Start(t.Context(), in)
	if err == nil || !result.NotMine {
		t.Fatalf("stale NotMine classification lost: %+v %v", result, err)
	}
	var epoch int64
	var state string
	if err := pool.QueryRow(t.Context(), "SELECT owner_epoch,state FROM control.acceptance WHERE id=$1", in.AcceptanceID).Scan(&epoch, &state); err != nil {
		t.Fatal(err)
	}
	if state != "dispatched" || epoch <= old.handle.Epoch || temporal.executions != 1 {
		t.Fatalf("zombie changed winner custody: state=%s epoch=%d executions=%d", state, epoch, temporal.executions)
	}

}

func TestPGMissingMemoRefusesAdoption(t *testing.T) {
	store, _ := testStore(t, 1)
	in := acceptedStartInput(t, store)
	temporal := &pgStartClient{memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{}}}
	result, err := startfence.New(temporal, "cloud", NewStartAdmission(store), testGenerationAdmission{}).Start(t.Context(), in)
	if err != nil || !result.NotMine || result.Adopt {
		t.Fatalf("missing memo = %+v, %v", result, err)
	}
	var state string
	if err := store.pool.QueryRow(t.Context(), "SELECT state FROM control.acceptance WHERE instance_id=$1 AND id=$2", store.instanceID, in.AcceptanceID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == "dispatched" {
		t.Fatal("unverifiable execution adopted")
	}
}

func TestPGSuccessorLostStartResponseIsMine(t *testing.T) {
	old, pool := testStore(t, 1)
	in := acceptedStartInput(t, old)
	if err := old.BeginDispatch(t.Context(), in.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	temporal := &pgStartClient{loseResponse: true}
	starter := startfence.New(temporal, "cloud", NewStartAdmission(current), testGenerationAdmission{})
	if _, err := starter.Start(t.Context(), in); err == nil {
		t.Fatal("lost response unexpectedly succeeded")
	}
	result, err := starter.Start(t.Context(), in)
	if err != nil || !result.AlreadyRunning || result.NotMine || result.Adopt || temporal.executions != 1 {
		t.Fatalf("successor retry = %+v, %v", result, err)
	}
}

func TestPGStartConfirmationRejectsStaleAndCrossInstanceLease(t *testing.T) {
	old, pool := testStore(t, 1)
	in := acceptedStartInput(t, old)
	identity, err := (&startCustody{store: old}).ReadStart(t.Context(), in.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	if err := (&startCustody{store: old}).ConfirmStart(t.Context(), identity, false, testRunPins(identity.AcceptanceID, identity.RunID)); err == nil {
		t.Fatal("stale confirmation committed")
	}
	if err := (&startCustody{store: old}).CheckStartLease(t.Context()); err == nil {
		t.Fatal("stale lease was not detected")
	}
	other, err := New(pool, "other-instance")
	if err != nil {
		t.Fatal(err)
	}
	other.BindFence(current.manager, current.handle)
	if err := (&startCustody{store: other}).CheckStartLease(t.Context()); err == nil {
		t.Fatal("cross-instance start authority accepted")
	}
}

func TestPGStartReadDoesNotAdoptAndDoubleAdoptRefuses(t *testing.T) {
	old, pool := testStore(t, 1)
	in := acceptedStartInput(t, old)
	if err := old.BeginDispatch(t.Context(), in.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	previous, err := old.ReadStartAuthority(t.Context(), in.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool)
	observed, err := current.ReadStartAuthority(t.Context(), in.AcceptanceID)
	if err != nil || observed != previous {
		t.Fatalf("read adopted or hid predecessor: %+v, %v", observed, err)
	}
	if err := current.AdoptStart(t.Context(), observed.AcceptanceID, observed.RunID, observed.OwnerEpoch, testRunPins(observed.AcceptanceID, observed.RunID)); err != nil {
		t.Fatal(err)
	}
	if err := current.AdoptStart(t.Context(), observed.AcceptanceID, observed.RunID, observed.OwnerEpoch, testRunPins(observed.AcceptanceID, observed.RunID)); err == nil {
		t.Fatal("double adoption did not enforce predecessor CAS")
	}
	if err := old.ConfirmStart(t.Context(), observed.AcceptanceID, observed.RunID, testRunPins(observed.AcceptanceID, observed.RunID)); err == nil {
		t.Fatal("stale owner confirmed after adoption")
	}
	if err := current.ConfirmStart(t.Context(), observed.AcceptanceID, observed.RunID, testRunPins(observed.AcceptanceID, observed.RunID)); err != nil {
		t.Fatal(err)
	}
}

// Config eligibility is independently qualified by the controller composition.
type testGenerationAdmission struct{}

func (testGenerationAdmission) Admit(string, string, string) error { return nil }

// A parent that is still running long after its start finished keeps its pins:
// acceptance pruning does not cascade to them, and the retention sweep marks
// and removes them only once the run's execution is closed.
func TestPGRunPinsOutliveTheirAcceptanceWhileTheRunIsLive(t *testing.T) {
	store, pool := testStore(t, 1)
	temporal := &pgStartClient{}
	starter := startfence.New(temporal, "cloud", NewStartAdmission(store), testGenerationAdmission{})
	in := acceptedStartInput(t, store)
	result, err := starter.Start(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	// The start finished eight days ago; the run is still going.
	if _, err := pool.Exec(t.Context(), `UPDATE control.acceptance SET accepted_at=accepted_at-interval '8 days', finished_at=finished_at-interval '8 days' WHERE instance_id=$1 AND id=$2`,
		store.instanceID, in.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE control.run_pin SET created_at=created_at-interval '8 days' WHERE instance_id=$1 AND run_id=$2`, store.instanceID, result.RunID); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	var acceptances int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.acceptance WHERE instance_id=$1 AND id=$2`, store.instanceID, in.AcceptanceID).Scan(&acceptances); err != nil || acceptances != 0 {
		t.Fatalf("aged acceptance not pruned: %d (%v)", acceptances, err)
	}
	pinState := func() (present bool, terminal bool) {
		t.Helper()
		var terminalAt *time.Time
		err := pool.QueryRow(t.Context(), `SELECT terminal_at FROM control.run_pin WHERE instance_id=$1 AND run_id=$2`, store.instanceID, result.RunID).Scan(&terminalAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true, terminalAt != nil
	}
	if present, _ := pinState(); !present {
		t.Fatal("pruning the acceptance removed the live run's pins")
	}
	started := in
	started.RunID = result.RunID
	want, err := RunPinsFor(started)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.RunPinsOf(t.Context(), result.RunID)
	if err != nil || got.InputDigest == "" {
		t.Fatalf("RunPinsOf = %+v, %v", got, err)
	}
	// The start fence digests the input it actually started (the starter fills
	// in more than this test input); everything else is the input as pinned.
	want.InputDigest = got.InputDigest
	if got != want {
		t.Fatalf("RunPinsOf = %+v; want %+v", got, want)
	}
	if _, err := store.RunPinsOf(t.Context(), "never-started"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RunPinsOf(unknown run) = %v, want sql.ErrNoRows", err)
	}

	if err := store.manager.Release(t.Context(), store.handle); err != nil {
		t.Fatal(err)
	}
	controller := successorStore(t, childRolePool(t, pool, "goobers_controller"))
	running := func(context.Context, string) (bool, error) { return false, nil }
	if sweep, err := controller.SweepRunPins(t.Context(), running, "", time.Minute); err != nil || sweep.Marked != 0 || sweep.Deleted != 0 {
		t.Fatalf("sweep of a running parent = %+v, %v", sweep, err)
	}
	if present, terminal := pinState(); !present || terminal {
		t.Fatalf("running parent's pins: present=%v terminal=%v, want kept and live", present, terminal)
	}

	closed := func(context.Context, string) (bool, error) { return true, nil }
	if sweep, err := controller.SweepRunPins(t.Context(), closed, "", time.Minute); err != nil || sweep.Marked != 1 || sweep.Deleted != 0 {
		t.Fatalf("sweep of the closed run = %+v, %v", sweep, err)
	}
	if present, terminal := pinState(); !present || !terminal {
		t.Fatalf("closed run's pins: present=%v terminal=%v, want kept and terminal", present, terminal)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE control.run_pin SET terminal_at=terminal_at-interval '8 days' WHERE instance_id=$1 AND run_id=$2`, store.instanceID, result.RunID); err != nil {
		t.Fatal(err)
	}
	if sweep, err := controller.SweepRunPins(t.Context(), closed, "", time.Minute); err != nil || sweep.Deleted != 1 {
		t.Fatalf("sweep after retention deleted %d (%v), want 1", sweep.Deleted, err)
	}
	if present, _ := pinState(); present {
		t.Fatal("pins outlived their retention after the run closed")
	}
}

func TestPGRunPinSweepRotatesPastLiveRunsAndIsolatesUnconfirmed(t *testing.T) {
	store, pool := testStore(t, 1)
	runs := []string{"aa-unconfirmed", "zz-closed"}
	for i := range runPinSweepBatch {
		runs = append(runs, fmt.Sprintf("live-%03d", i))
	}
	for _, runID := range runs {
		if _, err := pool.Exec(t.Context(), `
			INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
				goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch, created_at)
			VALUES ($1, $2, $2, 'g', 'w', 1, 'wd', 'gd', 'cg', '{}', 'goobers', 'id', 1, clock_timestamp()-interval '8 days')`,
			store.instanceID, runID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.manager.Release(t.Context(), store.handle); err != nil {
		t.Fatal(err)
	}
	controller := successorStore(t, childRolePool(t, pool, "goobers_controller"))
	outage := errors.New("temporal cannot vouch for this run")
	status := func(_ context.Context, runID string) (bool, error) {
		switch runID {
		case "aa-unconfirmed":
			return false, outage
		case "zz-closed", "live-010":
			return true, nil
		}
		return false, nil
	}

	// A full batch of candidates: the unconfirmed run is reported, not failed
	// on, and the closed run inside the batch is still marked.
	first, err := controller.SweepRunPins(t.Context(), status, "", time.Minute)
	if err != nil || first.Marked != 1 || first.Next == "" || len(first.Unconfirmed) != 1 || !errors.Is(first.Unconfirmed["aa-unconfirmed"], outage) || first.Halted != nil {
		t.Fatalf("first pass = %+v, %v; want 1 marked, a cursor, and only the unconfirmed run reported", first, err)
	}
	// The cursor carries the sweep past the long-running parents to the closed
	// run behind them, and the lap then wraps.
	second, err := controller.SweepRunPins(t.Context(), status, first.Next, time.Minute)
	if err != nil || second.Marked != 1 || second.Next != "" {
		t.Fatalf("second pass = %+v, %v; want 1 marked and the lap complete", second, err)
	}

	// An engine outage halts the pass at once: one call, nothing marked, and
	// the cursor stays put so the next pass retries from the same place.
	calls := 0
	unavailable := func(context.Context, string) (bool, error) {
		calls++
		return false, fmt.Errorf("%w: connection refused", ErrRunStatusUnavailable)
	}
	halted, err := controller.SweepRunPins(t.Context(), unavailable, first.Next, time.Minute)
	if err != nil || calls != 1 || halted.Marked != 0 || halted.Next != first.Next || !errors.Is(halted.Halted, ErrRunStatusUnavailable) {
		t.Fatalf("halted pass = %+v, %v after %d calls; want one call, the cursor kept and the halt reported", halted, err, calls)
	}
	rows, err := pool.Query(t.Context(), `SELECT run_id FROM control.run_pin WHERE instance_id=$1 AND terminal_at IS NOT NULL ORDER BY run_id`, store.instanceID)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(terminal) != "[live-010 zz-closed]" {
		t.Fatalf("terminal pins = %v, want only the closed runs", terminal)
	}
}
