package startfence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/engine"
)

// RunGaggleMemoKey identifies engine runs in Temporal visibility without
// requiring a namespace-specific search attribute registration.
const RunGaggleMemoKey = "goobers.run.gaggle.v1"

// StartOwnerMemoKey binds a Temporal execution to its accepted owner epoch.
const StartOwnerMemoKey = "goobers.start-owner"

// RunWorkflowMemoKey, RunWorkflowVersionMemoKey, and RunBacklogItemMemoKey
// extend RunGaggleMemoKey with the rest of a run's Goobers identity (#2911):
// an operator starting from a Temporal workflow execution — the Web UI, tctl,
// or a query — can identify the gaggle, workflow, version, and driving
// backlog item without decoding RunInput from the execution's input payload.
// Additive: existing readers of RunGaggleMemoKey (completed_runs.go's
// reconciler) are unaffected, and every key here uses the same versioned,
// namespace-registration-free Memo mechanism.
const (
	RunWorkflowMemoKey        = "goobers.run.workflow.v1"
	RunWorkflowVersionMemoKey = "goobers.run.workflow_version.v1"
	RunBacklogItemMemoKey     = "goobers.run.backlog_item.v1"
)

// BacklogItemMemo is RunBacklogItemMemoKey's value shape: enough to identify
// and follow the item driving a run without decoding the full BacklogItem
// (title/body/labels) into Temporal visibility.
type BacklogItemMemo struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	URL      string `json:"url,omitempty"`
}

// StartOwnerMemo is the start-fence identity persisted in Temporal.
type StartOwnerMemo struct {
	AcceptanceID string `json:"acceptanceId,omitempty"`
	OwnerEpoch   int64  `json:"ownerEpoch,omitempty"`
	StarterID    string `json:"starterId,omitempty"`
	InstanceID   string `json:"instanceId,omitempty"`
	InputDigest  string `json:"inputDigest,omitempty"`
}

// workflowStarter is the slice of the Temporal client the TemporalStarter needs.
// client.Client satisfies it; tests provide a fake.
type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

type workflowDescriber interface {
	DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// TemporalStarter starts engine runs on a Temporal task queue. It sets the run's
// WorkflowID to RunInput.RunID and asks Temporal to reject reuse whether the
// first run is open or closed, mapping that rejection to AlreadyRunning.
type TemporalStarter struct {
	client    workflowStarter
	taskQueue string
	admission engine.StartAdmission
}

// Start launches the engine workflow for in, idempotently on in.RunID.
func (s *TemporalStarter) Start(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
	return s.start(ctx, in)
}

func (s *TemporalStarter) start(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
	var err error
	in, err = s.admit(ctx, in)
	if err != nil {
		return engine.StartResult{}, err
	}
	if in.RunID == "" {
		return engine.StartResult{}, errors.New("engine: RunInput.RunID is required to start a run")
	}
	memo, err := runMemo(in)
	if err != nil {
		return engine.StartResult{}, err
	}
	opts := client.StartWorkflowOptions{
		ID:                                       in.RunID,
		TaskQueue:                                s.taskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		Memo:                                     memo,
	}
	run, err := s.client.ExecuteWorkflow(ctx, opts, engine.Run, in)
	if err != nil {
		if isAlreadyStarted(err) {
			result, classifyErr := s.classifyExisting(ctx, in)
			if classifyErr != nil {
				return engine.StartResult{}, classifyErr
			}
			return s.confirm(ctx, in, result)
		}
		return engine.StartResult{}, err
	}
	return s.confirm(ctx, in, engine.StartResult{RunID: run.GetID()})
}

func (s *TemporalStarter) classifyExisting(ctx context.Context, in engine.RunInput) (engine.StartResult, error) {
	describer, ok := s.client.(workflowDescriber)
	if !ok {
		return engine.StartResult{}, errors.New("engine: cloud starter cannot describe existing execution")
	}
	description, err := describer.DescribeWorkflowExecution(ctx, in.RunID, "")
	if err != nil {
		return engine.StartResult{}, err
	}
	if description == nil || description.GetWorkflowExecutionInfo() == nil {
		return engine.StartResult{}, errors.New("engine: existing execution description is incomplete")
	}
	if description.GetWorkflowExecutionInfo().GetType().GetName() != "Run" {
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, NotMine: true}, nil
	}
	fields := description.GetWorkflowExecutionInfo().GetMemo().GetFields()
	payload, exists := fields[StartOwnerMemoKey]
	if !exists {
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, NotMine: true}, nil
	}
	var owner StartOwnerMemo
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &owner); err != nil {
		return engine.StartResult{}, errors.New("engine: existing start-owner memo is invalid")
	}
	expected, err := startOwner(in)
	if err != nil {
		return engine.StartResult{}, err
	}
	if owner.InstanceID != expected.InstanceID || owner.InputDigest != expected.InputDigest {
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, NotMine: true}, nil
	}
	if in.OwnerEpoch != 0 && (owner.AcceptanceID == "" || owner.OwnerEpoch < 1 || owner.StarterID == "") {
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, NotMine: true}, nil
	}
	switch {
	case owner.AcceptanceID == in.AcceptanceID && owner.OwnerEpoch == in.OwnerEpoch && owner.StarterID == in.StarterID:
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true}, nil
	case owner.AcceptanceID == in.AcceptanceID && owner.OwnerEpoch < in.OwnerEpoch:
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, Adopt: true}, nil
	default:
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, NotMine: true}, nil
	}
}

// isAlreadyStarted reports whether err is Temporal's "already started" error.
func isAlreadyStarted(err error) bool {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &already)
}

// runMemo builds a run's Goobers-identity Memo (#2911): gaggle, workflow,
// version, and — when the run has one — the driving backlog item. Each field
// is independently omittable so a run with no backlog item (a manual or
// scheduled trigger) carries no RunBacklogItemMemoKey at all rather than an
// empty placeholder.
func runMemo(in engine.RunInput) (map[string]interface{}, error) {
	owner, err := startOwner(in)
	if err != nil {
		return nil, err
	}
	memo := map[string]interface{}{
		RunGaggleMemoKey:          in.Gaggle,
		RunWorkflowMemoKey:        in.WorkflowName,
		RunWorkflowVersionMemoKey: strconv.Itoa(in.Version),
		StartOwnerMemoKey:         owner,
	}
	if in.Item != nil {
		memo[RunBacklogItemMemoKey] = BacklogItemMemo{
			ID: in.Item.ID, Provider: string(in.Item.Provider), URL: in.Item.URL,
		}
	}
	return memo, nil
}

// startOwner binds immutable input identity while allowing a successor epoch.
func startOwner(in engine.RunInput) (StartOwnerMemo, error) {
	owner := StartOwnerMemo{AcceptanceID: in.AcceptanceID, OwnerEpoch: in.OwnerEpoch, StarterID: in.StarterID, InstanceID: in.InstanceID}
	in.OwnerEpoch, in.StarterID = 0, ""
	raw, err := json.Marshal(in)
	if err != nil {
		return StartOwnerMemo{}, err
	}
	sum := sha256.Sum256(raw)
	owner.InputDigest = "sha256:" + hex.EncodeToString(sum[:])
	return owner, nil
}

// New requires a durable admission adapter before any Temporal operation.
func New(c client.Client, taskQueue string, admission engine.StartAdmission) *TemporalStarter {
	return &TemporalStarter{client: c, taskQueue: taskQueue, admission: admission}
}
func (s *TemporalStarter) admit(ctx context.Context, in engine.RunInput) (engine.RunInput, error) {
	if s.admission == nil {
		return engine.RunInput{}, errors.New("cloud start requires durable acceptance admission")
	}
	return s.admission.AdmitStart(ctx, in)
}
func (s *TemporalStarter) confirm(ctx context.Context, in engine.RunInput, result engine.StartResult) (engine.StartResult, error) {
	return result, s.admission.ConfirmStart(ctx, in, result)
}
