package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// RunGaggleMemoKey identifies engine runs in Temporal visibility without
// requiring a namespace-specific search attribute registration.
const RunGaggleMemoKey = "goobers.run.gaggle.v1"

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

// StartResult reports the outcome of starting a run.
type StartResult struct {
	// RunID is the run/workflow id the run executes under.
	RunID string
	// AlreadyRunning is true when a run with the same id was already claimed,
	// whether it is still running or closed, so this Start was a no-op. This is
	// the engine's exactly-once guarantee: a deterministic RunID makes a
	// duplicate Start idempotent rather than launching a second run.
	AlreadyRunning bool
}

// Starter is the engine's start API: it begins a workflow run for a pinned
// RunInput. The scheduler (M11) depends on this; the runtime/operator provides a
// Temporal-backed implementation.
type Starter interface {
	Start(ctx context.Context, in RunInput) (StartResult, error)
}

// RunID builds a deterministic OpenTelemetry trace ID from its non-empty parts.
// Using the same parts (e.g. gaggle, workflow, item id) yields the same id, so a
// second Start for the same unit of work is rejected as already-running and all
// telemetry for that run can share the same trace.
func RunID(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(cleaned, "/")))
	return hex.EncodeToString(sum[:16])
}

// workflowStarter is the slice of the Temporal client the TemporalStarter needs.
// client.Client satisfies it; tests provide a fake.
type workflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// TemporalStarter starts engine runs on a Temporal task queue. It sets the run's
// WorkflowID to RunInput.RunID and asks Temporal to reject reuse whether the
// first run is open or closed, mapping that rejection to AlreadyRunning.
type TemporalStarter struct {
	client    workflowStarter
	taskQueue string
}

// NewTemporalStarter builds a TemporalStarter for the given Temporal client and
// task queue.
func NewTemporalStarter(c client.Client, taskQueue string) *TemporalStarter {
	return &TemporalStarter{client: c, taskQueue: taskQueue}
}

// Start launches the engine workflow for in, idempotently on in.RunID.
func (s *TemporalStarter) Start(ctx context.Context, in RunInput) (StartResult, error) {
	if in.RunID == "" {
		return StartResult{}, errors.New("engine: RunInput.RunID is required to start a run")
	}
	opts := client.StartWorkflowOptions{
		ID:                                       in.RunID,
		TaskQueue:                                s.taskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
		Memo:                                     runMemo(in),
	}
	run, err := s.client.ExecuteWorkflow(ctx, opts, Run, in)
	if err != nil {
		if isAlreadyStarted(err) {
			return StartResult{RunID: in.RunID, AlreadyRunning: true}, nil
		}
		return StartResult{}, err
	}
	return StartResult{RunID: run.GetID()}, nil
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
func runMemo(in RunInput) map[string]interface{} {
	memo := map[string]interface{}{
		RunGaggleMemoKey:          in.Gaggle,
		RunWorkflowMemoKey:        in.WorkflowName,
		RunWorkflowVersionMemoKey: strconv.Itoa(in.Version),
	}
	if in.Item != nil {
		memo[RunBacklogItemMemoKey] = BacklogItemMemo{
			ID: in.Item.ID, Provider: string(in.Item.Provider), URL: in.Item.URL,
		}
	}
	return memo
}
