package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

// workflowLivenessClient is the slice of the Temporal client the DS6 claim
// liveness probe needs. client.Client satisfies it; tests provide a fake.
type workflowLivenessClient interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// describeLivenessTimeout bounds one liveness describe so a wedged frontend
// degrades that probe call to "unknown" (fail-live at the renewal layer)
// instead of stalling the daemon's whole claim tick.
const describeLivenessTimeout = 15 * time.Second

// openWorkflowScanTimeout bounds the whole open-workflow enumeration a
// scheduled-run liveness resolution performs (all pages together), for the
// same reason describeLivenessTimeout bounds one describe.
const openWorkflowScanTimeout = 15 * time.Second

// openWorkflowScanPageSize and maxOpenWorkflowScanPages bound the visibility
// enumeration: past maxOpenWorkflowScanPages pages the mapping is UNKNOWN —
// returned as an error so the renewal layer fails live — never definitively
// not-live, because an un-enumerated open workflow may be the holder.
const (
	openWorkflowScanPageSize = 100
	maxOpenWorkflowScanPages = 10
	openWorkflowScanCacheTTL = 10 * time.Second
)

// WorkflowLiveness reports whether the engine workflow for a Goobers run id
// is still open — the engine half of DS6's claim-renewal liveness signal
// (docs/design/distributed-state-and-coordination.md §6/§10). It covers both
// wired start paths:
//
//   - TemporalStarter.Start begins every directly started run's workflow with
//     WorkflowID == RunID, so a claim's RunID IS the workflow id to describe.
//   - A scheduled run executes under DIFFERENT workflow ids: the Schedule
//     action workflow's id is the claim id, its child's is claimID+"-run"
//     (scheduledRunWorkflowID), and RunScheduled rewrites the run's RunID to
//     RunID(claimID) — a hash describe can never find (the projection layer
//     special-cases the same mapping in ProjectCompletedScheduledRun). For a
//     RunID no workflow exists under, the probe therefore enumerates OPEN
//     workflows and matches the recorded RunID against RunID(workflowID) and
//     RunID(strings.TrimSuffix(workflowID, "-run")) — the exact inverse of the
//     scheduled path's rewrite. By the time a scheduled run's claim exists in
//     the ledger the run has already executed at least its claiming stage, so
//     its open claim/child workflows are long past visibility propagation.
//
// Enumeration failure — including exceeding the page cap — is UNKNOWN
// liveness (an error, so renewal fails live), never definitively not-live.
// It satisfies localscheduler.RunLivenessProbe; the dependency points from
// the composition root (cmd/goobers), never from localscheduler to here.
type WorkflowLiveness struct {
	client    workflowLivenessClient
	namespace string

	// Open-workflow scan cache: one visibility enumeration serves every
	// hash-shaped RunID probed in the same renewal pass instead of one scan
	// per holder. TTL-bounded (openWorkflowScanCacheTTL); mu also serializes
	// concurrent refreshes so parallel probes share a single scan.
	mu            sync.Mutex
	scheduledLive map[string]struct{}
	scannedAt     time.Time
}

// NewWorkflowLiveness builds the probe over a Temporal client. namespace is
// the Temporal namespace the client is bound to; the open-workflow scan's
// list requests must name it explicitly.
func NewWorkflowLiveness(c workflowLivenessClient, namespace string) *WorkflowLiveness {
	return &WorkflowLiveness{client: c, namespace: namespace}
}

// RunLive reports (true, nil) for an open workflow and (false, nil) —
// definitively not live — for a closed or vanished one: a closed workflow's
// lease lapses normally, and NotFound falls through to the scheduled-run
// mapping above before it, too, may definitively answer not-live. Any error
// is UNKNOWN liveness and returned so the renewal layer can fail live.
func (p *WorkflowLiveness) RunLive(ctx context.Context, runID string) (bool, error) {
	describeCtx, cancel := context.WithTimeout(ctx, describeLivenessTimeout)
	defer cancel()
	resp, err := p.client.DescribeWorkflowExecution(describeCtx, runID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			// No workflow ever ran under this id — either a vanished direct
			// run (definitively not live) or a scheduled run, whose RunID is
			// a hash of the claim workflow's id. Resolve via the open-workflow
			// scan before answering.
			return p.scheduledRunLive(ctx, runID)
		}
		return false, fmt.Errorf("engine: describe workflow %s: %w", runID, err)
	}
	return resp.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}

// scheduledRunLive answers whether runID is the rewritten RunID of a
// scheduled run whose claim (or child) workflow is still open.
func (p *WorkflowLiveness) scheduledRunLive(ctx context.Context, runID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scheduledLive == nil || time.Since(p.scannedAt) > openWorkflowScanCacheTTL {
		live, err := p.scanOpenWorkflows(ctx)
		if err != nil {
			return false, err
		}
		p.scheduledLive = live
		p.scannedAt = time.Now()
	}
	_, open := p.scheduledLive[runID]
	return open, nil
}

// scanOpenWorkflows enumerates open workflow executions and returns the set
// of Goobers RunIDs they could be executing: RunID(workflowID) for a Schedule
// claim workflow, RunID(workflowID minus scheduledRunWorkflowIDSuffix) for
// its child. Bounded by openWorkflowScanTimeout and maxOpenWorkflowScanPages;
// exceeding either is an error (unknown, fail-live), not an empty set.
func (p *WorkflowLiveness) scanOpenWorkflows(ctx context.Context) (map[string]struct{}, error) {
	ctx, cancel := context.WithTimeout(ctx, openWorkflowScanTimeout)
	defer cancel()
	live := make(map[string]struct{})
	var pageToken []byte
	for page := 0; ; page++ {
		if page >= maxOpenWorkflowScanPages {
			return nil, fmt.Errorf("engine: open-workflow scan exceeded %d pages; scheduled-run liveness unknown", maxOpenWorkflowScanPages)
		}
		resp, err := p.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace:     p.namespace,
			PageSize:      openWorkflowScanPageSize,
			NextPageToken: pageToken,
			Query:         "ExecutionStatus = 'Running'",
		})
		if err != nil {
			return nil, fmt.Errorf("engine: list open workflows: %w", err)
		}
		for _, info := range resp.GetExecutions() {
			workflowID := info.GetExecution().GetWorkflowId()
			if workflowID == "" {
				continue
			}
			live[RunID(workflowID)] = struct{}{}
			if claimID, ok := strings.CutSuffix(workflowID, scheduledRunWorkflowIDSuffix); ok && claimID != "" {
				live[RunID(claimID)] = struct{}{}
			}
		}
		pageToken = resp.GetNextPageToken()
		if len(pageToken) == 0 {
			return live, nil
		}
	}
}
