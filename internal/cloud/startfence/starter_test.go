package startfence

import (
	"context"
	"errors"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
)

// fakeRun is a minimal client.WorkflowRun for tests.
type fakeRun struct {
	id    string
	runID string
}

func (f fakeRun) GetID() string                          { return f.id }
func (f fakeRun) GetRunID() string                       { return f.runID }
func (f fakeRun) GetFirstExecutionRunID() string         { return f.runID }
func (f fakeRun) Get(context.Context, interface{}) error { return nil }
func (f fakeRun) GetWithOptions(context.Context, interface{}, client.WorkflowRunGetOptions) error {
	return nil
}

// fakeStarter is a fake workflowStarter capturing the last options.
type fakeStarter struct {
	run     client.WorkflowRun
	err     error
	gotOpts client.StartWorkflowOptions
	desc    *workflowservice.DescribeWorkflowExecutionResponse
	descErr error
}

func (f *fakeStarter) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return f.desc, f.descErr
}

func (f *fakeStarter) ExecuteWorkflow(_ context.Context, opts client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.gotOpts = opts
	return f.run, f.err
}

func sampleInput() engine.RunInput {
	return engine.RunInput{
		RunID:        "web/flow/item-1",
		Gaggle:       "web",
		WorkflowName: "flow",
		Version:      1,
	}
}

func TestTemporalStarterStartsRun(t *testing.T) {
	fs := &fakeStarter{run: fakeRun{id: "web/flow/item-1", runID: "exec-1"}}
	s := &TemporalStarter{generation: testGenerationAdmission{}, admission: testStartAdmission{}, client: fs, taskQueue: "goobers"}

	res, err := s.Start(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.AlreadyRunning {
		t.Error("expected a fresh start, got AlreadyRunning")
	}
	if res.RunID != "web/flow/item-1" {
		t.Errorf("RunID = %q, want web/flow/item-1", res.RunID)
	}
	// The starter must pin the workflow id and ask Temporal to error on duplicate.
	if fs.gotOpts.ID != "web/flow/item-1" {
		t.Errorf("opts.ID = %q, want the deterministic RunID", fs.gotOpts.ID)
	}
	if !fs.gotOpts.WorkflowExecutionErrorWhenAlreadyStarted {
		t.Error("expected WorkflowExecutionErrorWhenAlreadyStarted = true")
	}
	if fs.gotOpts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
		t.Errorf("reuse policy = %v, want REJECT_DUPLICATE", fs.gotOpts.WorkflowIDReusePolicy)
	}
	if fs.gotOpts.TaskQueue != "goobers" {
		t.Errorf("task queue = %q, want goobers", fs.gotOpts.TaskQueue)
	}
	if got := fs.gotOpts.Memo[RunGaggleMemoKey]; got != "web" {
		t.Errorf("gaggle memo = %q, want web", got)
	}
	if got := fs.gotOpts.Memo[RunWorkflowMemoKey]; got != "flow" {
		t.Errorf("workflow memo = %q, want flow", got)
	}
	if got := fs.gotOpts.Memo[RunWorkflowVersionMemoKey]; got != "1" {
		t.Errorf("workflow version memo = %q, want \"1\"", got)
	}
	if _, ok := fs.gotOpts.Memo[RunBacklogItemMemoKey]; ok {
		t.Errorf("backlog item memo present for a run with no Item: %v", fs.gotOpts.Memo[RunBacklogItemMemoKey])
	}
	if _, ok := fs.gotOpts.Memo[StartOwnerMemoKey]; !ok {
		t.Fatal("local start omitted the start-owner memo")
	}
}

// TestTemporalStarterMemoCarriesBacklogItem covers #2911's other arm: a run
// driven by a backlog item carries an identifiable item memo, so an operator
// reading the Temporal execution does not need to decode engine.RunInput's input
// payload to find it.
func TestTemporalStarterMemoCarriesBacklogItem(t *testing.T) {
	fs := &fakeStarter{run: fakeRun{id: "web/flow/item-1", runID: "exec-1"}}
	s := &TemporalStarter{generation: testGenerationAdmission{}, admission: testStartAdmission{}, client: fs, taskQueue: "goobers"}

	in := sampleInput()
	in.Item = &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, URL: "https://example/issues/42"}

	if _, err := s.Start(context.Background(), in); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, ok := fs.gotOpts.Memo[RunBacklogItemMemoKey]
	if !ok {
		t.Fatalf("no backlog item memo for a run with Item set")
	}
	item, ok := got.(BacklogItemMemo)
	if !ok {
		t.Fatalf("backlog item memo = %#v (%T), want BacklogItemMemo", got, got)
	}
	want := BacklogItemMemo{ID: "42", Provider: string(apiv1.ProviderGitHub), URL: "https://example/issues/42"}
	if item != want {
		t.Errorf("backlog item memo = %+v, want %+v", item, want)
	}
}

func TestTemporalStarterPropagatesOtherErrors(t *testing.T) {
	fs := &fakeStarter{err: errors.New("temporal unavailable")}
	s := &TemporalStarter{generation: testGenerationAdmission{}, admission: testStartAdmission{}, client: fs, taskQueue: "goobers"}
	if _, err := s.Start(context.Background(), sampleInput()); err == nil {
		t.Error("expected a non-already-started error to propagate")
	}
}

func TestTemporalStarterRequiresRunID(t *testing.T) {
	s := &TemporalStarter{generation: testGenerationAdmission{}, admission: testStartAdmission{}, client: &fakeStarter{}, taskQueue: "goobers"}
	in := sampleInput()
	in.RunID = ""
	if _, err := s.Start(context.Background(), in); err == nil {
		t.Error("expected an error when RunID is empty")
	}
}

func TestLostStartResponseIsMine(t *testing.T) {
	input := cloudStartInput(7)
	starter := existingStart(t, StartOwnerMemo{
		AcceptanceID: input.AcceptanceID, OwnerEpoch: input.OwnerEpoch, StarterID: input.StarterID,
	})
	result, err := starter.Start(t.Context(), input)
	if err != nil || !result.AlreadyRunning || result.Adopt || result.NotMine {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestZombieStartIsNotMine(t *testing.T) {
	input := cloudStartInput(7)
	starter := existingStart(t, StartOwnerMemo{
		AcceptanceID: input.AcceptanceID, OwnerEpoch: 8, StarterID: "newer",
	})
	result, err := starter.Start(t.Context(), input)
	if err != nil || !result.NotMine || result.Adopt {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestReconcileAdoptsPredecessorStart(t *testing.T) {
	input := cloudStartInput(7)
	starter := existingStart(t, StartOwnerMemo{
		AcceptanceID: input.AcceptanceID, OwnerEpoch: 6, StarterID: "predecessor",
	})
	result, err := starter.Start(t.Context(), input)
	if err != nil || !result.Adopt || result.NotMine {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestCloudRejectsUnassignedRunID(t *testing.T) {
	fake := &fakeStarter{}
	starter := &TemporalStarter{generation: testGenerationAdmission{}, client: fake, taskQueue: "cloud"}
	input := cloudStartInput(1)
	input.AcceptanceID = "trigger-another-run"
	if _, err := starter.Start(t.Context(), input); err == nil {
		t.Fatal("cloud start accepted a run ID not assigned by its acceptance")
	}
	if fake.gotOpts.ID != "" {
		t.Fatal("rejected cloud start reached Temporal")
	}
}

func cloudStartInput(epoch int64) engine.RunInput {
	input := sampleInput()
	input.RunID = "367a0f0c2c9c47b4d4946044615a1c2f"
	input.AcceptanceID = "trigger-" + input.RunID
	input.OwnerEpoch = epoch
	input.StarterID = "controller-a"
	input.SelfExecutionDenied = true
	return input
}

func existingStart(t *testing.T, owner StartOwnerMemo) *TemporalStarter {
	t.Helper()
	return existingStartWithInput(t, cloudStartInput(7), owner)
}

func existingStartWithInput(t *testing.T, input engine.RunInput, owner StartOwnerMemo) *TemporalStarter {
	t.Helper()
	identity, err := startOwner(input)
	if err != nil {
		t.Fatal(err)
	}
	owner.InputDigest, owner.InstanceID = identity.InputDigest, identity.InstanceID
	payload, err := converter.GetDefaultDataConverter().ToPayload(owner)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeStarter{
		err: serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "request", "execution"),
		desc: &workflowservice.DescribeWorkflowExecutionResponse{
			WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
				Type: &commonpb.WorkflowType{Name: "Run"},
				Memo: &commonpb.Memo{Fields: map[string]*commonpb.Payload{StartOwnerMemoKey: payload}},
			},
		},
	}
	return &TemporalStarter{generation: testGenerationAdmission{}, client: fake, taskQueue: "cloud", admission: testStartAdmission{}}
}

// Unit-test custody seam; PostgreSQL tests exercise the real admission adapter.
type testStartAdmission struct{}

func (testStartAdmission) AdmitStart(_ context.Context, in engine.RunInput) (engine.RunInput, error) {
	return in, nil
}
func (testStartAdmission) ConfirmStart(context.Context, engine.RunInput, engine.StartResult) error {
	return nil
}

func TestDescribeStartedDecodesStatusAndStartOwner(t *testing.T) {
	memo, err := converter.GetDefaultDataConverter().ToPayload(StartOwnerMemo{InstanceID: "instance", InputDigest: "sha256:input"})
	if err != nil {
		t.Fatal(err)
	}
	describe := func(memo *commonpb.Memo, workflowType string) *fakeStarter {
		return &fakeStarter{desc: &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, Type: &commonpb.WorkflowType{Name: workflowType}, Memo: memo,
		}}}
	}
	started, err := DescribeStarted(t.Context(), describe(&commonpb.Memo{Fields: map[string]*commonpb.Payload{StartOwnerMemoKey: memo}}, "Run"), "run")
	if err != nil || !started.Engine || started.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING || started.Owner == nil || started.Owner.InputDigest != "sha256:input" {
		t.Fatalf("DescribeStarted = %+v, %v", started, err)
	}
	if started, err := DescribeStarted(t.Context(), describe(nil, "Other"), "run"); err != nil || started.Engine || started.Owner != nil {
		t.Fatalf("foreign execution without a memo = %+v, %v", started, err)
	}
	corrupt := &commonpb.Memo{Fields: map[string]*commonpb.Payload{StartOwnerMemoKey: {Data: []byte("not json")}}}
	if _, err := DescribeStarted(t.Context(), describe(corrupt, "Run"), "run"); err == nil {
		t.Fatal("an undecodable start-owner memo was read as no owner")
	}
	if _, err := DescribeStarted(t.Context(), &fakeStarter{desc: &workflowservice.DescribeWorkflowExecutionResponse{}}, "run"); err == nil {
		t.Fatal("an incomplete description was accepted")
	}
	outage := serviceerror.NewUnavailable("down")
	if _, err := DescribeStarted(t.Context(), &fakeStarter{descErr: outage}, "run"); !errors.Is(err, outage) {
		t.Fatalf("describe error = %v, want it returned", err)
	}
}
