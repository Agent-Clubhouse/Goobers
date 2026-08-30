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
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
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

// OpenRun identifies one open engine workflow by both of its names: the
// Goobers run id its journal is written under and the Temporal workflow id
// the daemon must describe or wait on.
type OpenRun struct {
	// RunID is the Goobers run id — for a direct run, the workflow id
	// itself; for a scheduled run, the RunID() hash RunScheduled rewrote it
	// to. It is the key runs/<id>/ is stored under.
	RunID string
	// WorkflowID is the Temporal workflow id. It differs from RunID for
	// scheduled runs, which is precisely why this type exists: the daemon
	// cannot reattach to a run whose workflow it cannot name.
	WorkflowID string
	// Gaggle is the run's gaggle, decoded from RunGaggleMemoKey.
	Gaggle string
	// Workflow is the run's workflow name, decoded from RunWorkflowMemoKey
	// when present.
	Workflow string
}

// OpenRuns enumerates the open engine workflows belonging to the named
// gaggles, keyed by Goobers run id.
//
// This is scanOpenWorkflows' INVERSE. The DS6 liveness probe only ever needs
// to know THAT a run id maps to some open workflow, so it discards the
// workflow id and keeps a set. The daemon's boot reattach (decision 005 D1's
// start-to-first-emit closure) needs the opposite: given that an engine run
// was admitted, it must find and wait on the workflow, which means it needs
// the workflow id back. Retaining the mapping here — rather than in a second
// scan in cmd/goobers — keeps the direct/scheduled id derivation in the one
// place that already owns it, and lets the two callers share the enumeration.
//
// gaggles filters by RunGaggleMemoKey, which is how a daemon avoids
// reattaching to a sibling instance's runs when several share a Temporal
// namespace. An empty set matches nothing (fail-closed): a caller that has
// not said which gaggles it owns must not be told about anyone's runs.
//
// Unlike scanOpenWorkflows, the derivation here is EXACT rather than a
// superset. A liveness set may over-approximate, since an extra entry only
// ever fails live; this map is read as "run <id> is executing as workflow
// <wid>", so a phantom entry would name a run directory that does not exist
// and be reported as an orphan.
//
// Bounded exactly as scanOpenWorkflows is; exceeding the caps is an error
// (the mapping is UNKNOWN), never a short answer, because a boot scan that
// silently returned a partial set would drop exactly the runs it exists to
// recover.
func (p *WorkflowLiveness) OpenRuns(ctx context.Context, gaggles map[string]struct{}) (map[string]OpenRun, error) {
	out := make(map[string]OpenRun)
	if len(gaggles) == 0 {
		return out, nil
	}
	// A Schedule claim and its child both map to RunID(claimID), and only the
	// child is the workflow that actually executes the run — so claims are
	// deferred and applied only where no child was found. Visibility returns
	// executions in no defined order, which is why this cannot be a
	// last-writer-wins overwrite.
	var claims []OpenRun
	err := p.eachOpenWorkflow(ctx, func(info *workflowpb.WorkflowExecutionInfo, workflowID string) {
		gaggle, ok := memoString(info, RunGaggleMemoKey)
		if !ok {
			return
		}
		if _, owned := gaggles[gaggle]; !owned {
			return
		}
		workflowName, _ := memoString(info, RunWorkflowMemoKey)
		run := OpenRun{WorkflowID: workflowID, Gaggle: gaggle, Workflow: workflowName}
		switch {
		case isScheduledRunWorkflowID(workflowID):
			// A scheduled run's child: RunScheduled rewrote its RunID to the
			// hash of the claim id, so that hash is the key runs/<id>/ used.
			claimID, _ := strings.CutSuffix(workflowID, scheduledRunWorkflowIDSuffix)
			run.RunID = RunID(claimID)
			out[run.RunID] = run
		case looksLikeScheduleClaimID(workflowID):
			// The claim workflow itself, still open because its child has not
			// finished. Same run id; a worse workflow to wait on.
			run.RunID = RunID(workflowID)
			claims = append(claims, run)
		default:
			// A directly started run. TemporalStarter.Start uses
			// WorkflowID == RunID, so the two names coincide. Unlike
			// scanOpenWorkflows — which may over-approximate freely, because a
			// liveness set that is a superset only ever fails live — this map
			// must NOT also register RunID(workflowID) here: that phantom key
			// names no run directory, and reportOrphanedEngineRuns would
			// report every healthy direct run as an orphan.
			run.RunID = workflowID
			out[workflowID] = run
		}
	})
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		if _, hasChild := out[claim.RunID]; !hasChild {
			out[claim.RunID] = claim
		}
	}
	return out, nil
}

// isScheduledRunWorkflowID reports whether workflowID names a scheduled run's
// child workflow (scheduledRunWorkflowID's output).
func isScheduledRunWorkflowID(workflowID string) bool {
	claimID, ok := strings.CutSuffix(workflowID, scheduledRunWorkflowIDSuffix)
	return ok && claimID != ""
}

// looksLikeScheduleClaimID reports whether workflowID has the shape Temporal
// gives a Schedule action: the configured action id, "-", and the nominal
// fire time in RFC3339 (the same encoding scheduledFireTime parses back out).
//
// The timestamp itself contains '-', so the split point is found by trying
// each '-' from the right — cheap on ids this short, and exact, which matters
// because the alternative reading of a suffix-less workflow id is "a direct
// run", and the two derive completely different run ids.
func looksLikeScheduleClaimID(workflowID string) bool {
	for i := len(workflowID) - 1; i > 0; i-- {
		if workflowID[i] != '-' {
			continue
		}
		if _, err := time.Parse(time.RFC3339, workflowID[i+1:]); err == nil {
			return true
		}
	}
	return false
}

// memoString decodes one string-valued memo field, reporting whether it was
// present and decodable. A memo that fails to decode is treated as absent:
// every caller here is filtering, and a filter must not admit a value it
// could not read.
func memoString(info *workflowpb.WorkflowExecutionInfo, key string) (string, bool) {
	payload := info.GetMemo().GetFields()[key]
	if payload == nil {
		return "", false
	}
	var value string
	if err := converter.GetDefaultDataConverter().FromPayload(payload, &value); err != nil {
		return "", false
	}
	return value, value != ""
}

// scanOpenWorkflows enumerates open workflow executions and returns the set
// of Goobers RunIDs they could be executing: RunID(workflowID) for a Schedule
// claim workflow, RunID(workflowID minus scheduledRunWorkflowIDSuffix) for
// its child. Bounded by openWorkflowScanTimeout and maxOpenWorkflowScanPages;
// exceeding either is an error (unknown, fail-live), not an empty set.
func (p *WorkflowLiveness) scanOpenWorkflows(ctx context.Context) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	err := p.eachOpenWorkflow(ctx, func(_ *workflowpb.WorkflowExecutionInfo, workflowID string) {
		live[RunID(workflowID)] = struct{}{}
		if claimID, ok := strings.CutSuffix(workflowID, scheduledRunWorkflowIDSuffix); ok && claimID != "" {
			live[RunID(claimID)] = struct{}{}
		}
	})
	if err != nil {
		return nil, err
	}
	return live, nil
}

// eachOpenWorkflow runs the bounded visibility enumeration both open-workflow
// consumers share, calling visit once per execution that has a workflow id.
func (p *WorkflowLiveness) eachOpenWorkflow(ctx context.Context, visit func(info *workflowpb.WorkflowExecutionInfo, workflowID string)) error {
	ctx, cancel := context.WithTimeout(ctx, openWorkflowScanTimeout)
	defer cancel()
	var pageToken []byte
	for page := 0; ; page++ {
		if page >= maxOpenWorkflowScanPages {
			return fmt.Errorf("engine: open-workflow scan exceeded %d pages; scheduled-run liveness unknown", maxOpenWorkflowScanPages)
		}
		resp, err := p.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace:     p.namespace,
			PageSize:      openWorkflowScanPageSize,
			NextPageToken: pageToken,
			Query:         "ExecutionStatus = 'Running'",
		})
		if err != nil {
			return fmt.Errorf("engine: list open workflows: %w", err)
		}
		for _, info := range resp.GetExecutions() {
			workflowID := info.GetExecution().GetWorkflowId()
			if workflowID == "" {
				continue
			}
			visit(info, workflowID)
		}
		pageToken = resp.GetNextPageToken()
		if len(pageToken) == 0 {
			return nil
		}
	}
}
