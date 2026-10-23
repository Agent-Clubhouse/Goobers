//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
	return engine.RunInput{AcceptanceID: record.ID, Gaggle: "web", WorkflowName: "flow", Version: 1}
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
	if err := (&startCustody{store: old}).ConfirmStart(t.Context(), identity, false); err == nil {
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
	if err := current.AdoptStart(t.Context(), observed.AcceptanceID, observed.RunID, observed.OwnerEpoch); err != nil {
		t.Fatal(err)
	}
	if err := current.AdoptStart(t.Context(), observed.AcceptanceID, observed.RunID, observed.OwnerEpoch); err == nil {
		t.Fatal("double adoption did not enforce predecessor CAS")
	}
	if err := old.ConfirmStart(t.Context(), observed.AcceptanceID, observed.RunID); err == nil {
		t.Fatal("stale owner confirmed after adoption")
	}
	if err := current.ConfirmStart(t.Context(), observed.AcceptanceID, observed.RunID); err != nil {
		t.Fatal(err)
	}
}

// Config eligibility is independently qualified by the controller composition.
type testGenerationAdmission struct{}

func (testGenerationAdmission) Admit(string, string, string) error { return nil }
