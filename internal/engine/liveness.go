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
// is still open, and — its D2 half — WHICH Temporal workflow id that run is
// executing under. It is the engine half of DS6's claim-renewal liveness
// signal (docs/design/distributed-state-and-coordination.md §6/§10) and the
// daemon's only way to name a scheduled run's workflow. It covers both wired
// start paths:
//
//   - TemporalStarter.Start begins every directly started run's workflow with
//     WorkflowID == RunID, so a claim's RunID IS the workflow id to describe.
//     Nothing here is consulted for one: the describe answers first.
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
// ONE bounded enumeration serves every consumer (#3877): the DS6 liveness
// probe (RunLive), the daemon's boot reattach enumeration (OpenRuns) and the
// run guards' NotFound resolution (ResolveWorkflowID) all read the same
// TTL-cached openWorkflowIndex, so a namespace is paged at most once per
// openWorkflowScanCacheTTL no matter how many callers ask.
//
// Enumeration failure — including exceeding the page cap — is UNKNOWN
// (an error, so renewal fails live and a guard holds its slot), never
// definitively not-live and never an empty answer.
// It satisfies localscheduler.RunLivenessProbe; the dependency points from
// the composition root (cmd/goobers), never from localscheduler to here.
type WorkflowLiveness struct {
	client    workflowLivenessClient
	namespace string

	// Open-workflow scan cache: one visibility enumeration serves every
	// hash-shaped RunID probed in the same renewal pass instead of one scan
	// per holder. TTL-bounded (openWorkflowScanCacheTTL); mu also serializes
	// concurrent refreshes so parallel probes share a single scan.
	mu        sync.Mutex
	index     *openWorkflowIndex
	scannedAt time.Time
}

// NewWorkflowLiveness builds the probe over a Temporal client. namespace is
// the Temporal namespace the client is bound to; the open-workflow scan's
// list requests must name it explicitly.
func NewWorkflowLiveness(c workflowLivenessClient, namespace string) *WorkflowLiveness {
	return &WorkflowLiveness{client: c, namespace: namespace}
}

// ErrRunNotOpen reports that no open workflow in the namespace maps to a run
// id. It is a DEFINITE answer — the run is not executing on the engine — as
// distinct from an enumeration that could not complete, which is an error of
// its own and means "unknown".
var ErrRunNotOpen = errors.New("engine: no open workflow maps to this run id")

// ErrAmbiguousRunID reports that MORE THAN ONE unrelated open workflow claims
// one run id. It is never silently resolved: picking one would be a coin flip
// between describing (or cancelling) the right workflow and the wrong one,
// and the wrong one is another gaggle's or another run's. Callers treat it
// exactly as they treat an enumeration failure — unknown, hold everything —
// and it is loud so an operator sees the collision.
var ErrAmbiguousRunID = errors.New("engine: run id maps to more than one open workflow")

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
//
// It reads the SUPERSET presence set, not the exact inverse: a liveness
// answer may over-approximate freely, because an extra entry only ever fails
// live, and it is deliberately NOT gaggle-filtered — a claim held by a run
// this daemon does not own is still a claim that must not be stolen.
func (p *WorkflowLiveness) scheduledRunLive(ctx context.Context, runID string) (bool, error) {
	index, err := p.openWorkflows(ctx)
	if err != nil {
		return false, err
	}
	_, open := index.live[runID]
	return open, nil
}

// OpenRun identifies one open engine workflow by both of its names: the
// Goobers run id its journal is written under and the Temporal workflow id
// the daemon must describe, wait on, or cancel.
type OpenRun struct {
	// RunID is the Goobers run id — for a direct run, the workflow id
	// itself; for a scheduled run, the RunID() hash RunScheduled rewrote it
	// to. It is the key runs/<id>/ is stored under.
	RunID string
	// WorkflowID is the Temporal workflow id. It differs from RunID for
	// scheduled runs, which is precisely why this type exists: the daemon
	// cannot reattach to, or cancel, a run whose workflow it cannot name.
	WorkflowID string
	// Gaggle is the run's gaggle, decoded from RunGaggleMemoKey.
	Gaggle string
	// Workflow is the run's workflow name, decoded from RunWorkflowMemoKey
	// when present.
	Workflow string
}

// openRunKind distinguishes the three shapes an open workflow id can have,
// because the three derive their run ids completely differently and a
// scheduled run's claim and child must be ranked against each other.
type openRunKind int

const (
	openRunDirect openRunKind = iota // WorkflowID == RunID
	openRunClaim                     // a Schedule action workflow
	openRunChild                     // its "-run" child, the executing workflow
)

// openRunCandidate is one open workflow offered as the executor of a run id.
type openRunCandidate struct {
	run OpenRun
	// claimID is the Schedule action id a claim/child pair shares. Empty for
	// a direct run. It is what proves a claim and a child are the SAME
	// scheduled run rather than two workflows colliding on one run id.
	claimID string
	kind    openRunKind
}

// openWorkflowIndex is one bounded enumeration's result, in the two shapes
// its consumers need:
//
//   - live is the SUPERSET presence set DS6 liveness reads. It may
//     over-approximate, since an extra entry only ever fails live.
//   - candidates is the EXACT inverse (#3877): every open workflow that could
//     be executing a run id, kept with the workflow id itself so the daemon
//     can name it. It is resolved — claim versus child, gaggle ownership,
//     ambiguity — at READ time rather than scan time, so one unfiltered scan
//     can serve callers that own different gaggles without ever letting one
//     daemon's ownership widen another's.
type openWorkflowIndex struct {
	live       map[string]struct{}
	candidates map[string][]openRunCandidate
}

// OpenRuns enumerates the open engine workflows belonging to the named
// gaggles, keyed by Goobers run id.
//
// This is the liveness set's INVERSE. The DS6 probe only ever needs to know
// THAT a run id maps to some open workflow, so it reads a set. The daemon's
// boot reattach (decision 005 D1's start-to-first-emit closure) needs the
// opposite: given that an engine run was admitted, it must find and wait on
// the workflow, which means it needs the workflow id back.
//
// gaggles filters by RunGaggleMemoKey, which is how a daemon avoids
// reattaching to a sibling instance's runs when several share a Temporal
// namespace. An empty set matches nothing (fail-closed): a caller that has
// not said which gaggles it owns must not be told about anyone's runs.
//
// Unlike the liveness set, the derivation here is EXACT rather than a
// superset. A liveness set may over-approximate, since an extra entry only
// ever fails live; this map is read as "run <id> is executing as workflow
// <wid>", so a phantom entry would name a run directory that does not exist
// and be reported as an orphan. A run id two unrelated workflows both claim
// is therefore OMITTED rather than guessed at — ResolveWorkflowID reports the
// same collision as ErrAmbiguousRunID, which is where a caller acting on one
// specific run hears about it.
//
// Bounded exactly as the scan is; exceeding the caps is an error (the mapping
// is UNKNOWN), never a short answer, because a boot scan that silently
// returned a partial set would drop exactly the runs it exists to recover.
func (p *WorkflowLiveness) OpenRuns(ctx context.Context, gaggles map[string]struct{}) (map[string]OpenRun, error) {
	out := make(map[string]OpenRun)
	if len(gaggles) == 0 {
		return out, nil
	}
	index, err := p.openWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	for runID, candidates := range index.candidates {
		run, err := resolveOpenRun(candidates, gaggles)
		if err != nil {
			continue
		}
		out[runID] = run
	}
	return out, nil
}

// ResolveWorkflowID names the Temporal workflow a run id is executing as.
//
// This is what closes decision 003's engine_run_unresolvable case (#3877):
// the daemon's guards describe a run under its own id first — correct, and
// free, for every direct run — and reach here only when that describe
// returned NotFound. Three answers, and the caller must keep them apart:
//
//   - (id, nil): the run is executing as id. Describe, wait on, or cancel it.
//   - ("", ErrRunNotOpen): DEFINITE. No open workflow maps to this run, so
//     nothing on the engine is driving it.
//   - ("", anything else): UNKNOWN — an enumeration that could not complete,
//     or ErrAmbiguousRunID. The caller must hold whatever it is holding; a
//     run whose workflow cannot be named is not a run that has ended.
func (p *WorkflowLiveness) ResolveWorkflowID(ctx context.Context, runID string, gaggles map[string]struct{}) (string, error) {
	if len(gaggles) == 0 {
		return "", ErrRunNotOpen
	}
	index, err := p.openWorkflows(ctx)
	if err != nil {
		return "", err
	}
	run, err := resolveOpenRun(index.candidates[runID], gaggles)
	if err != nil {
		return "", err
	}
	return run.WorkflowID, nil
}

// resolveOpenRun picks the one workflow that is executing a run id, from the
// candidates the scan recorded for it, restricted to the caller's gaggles.
//
// A scheduled run legitimately offers TWO candidates while it executes — the
// Schedule claim workflow and its child — and only the child is the workflow
// the run actually runs as; waiting on or cancelling the claim is not
// equivalent. Visibility returns executions in no defined order, so this
// cannot be a last-writer-wins overwrite. Anything else — two children, two
// claim ids, a direct run colliding with a scheduled one — is a genuine
// collision and is reported rather than guessed.
func resolveOpenRun(candidates []openRunCandidate, gaggles map[string]struct{}) (OpenRun, error) {
	var direct, claims, children []openRunCandidate
	for _, candidate := range candidates {
		if _, owned := gaggles[candidate.run.Gaggle]; !owned {
			continue
		}
		switch candidate.kind {
		case openRunChild:
			children = append(children, candidate)
		case openRunClaim:
			claims = append(claims, candidate)
		default:
			direct = append(direct, candidate)
		}
	}
	switch {
	case len(direct)+len(claims)+len(children) == 0:
		return OpenRun{}, ErrRunNotOpen
	case len(direct) > 0 && len(claims)+len(children) > 0,
		distinctWorkflowIDs(direct) > 1,
		distinctWorkflowIDs(children) > 1,
		distinctClaimIDs(append(append([]openRunCandidate(nil), claims...), children...)) > 1:
		return OpenRun{}, ErrAmbiguousRunID
	case len(children) > 0:
		return children[0].run, nil
	case len(claims) > 0:
		// The claim workflow, still open because its child has not started
		// (or has already finished). Same run; a worse workflow to wait on,
		// and the only one there is.
		return claims[0].run, nil
	default:
		return direct[0].run, nil
	}
}

func distinctWorkflowIDs(candidates []openRunCandidate) int {
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		seen[candidate.run.WorkflowID] = struct{}{}
	}
	return len(seen)
}

func distinctClaimIDs(candidates []openRunCandidate) int {
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		seen[candidate.claimID] = struct{}{}
	}
	return len(seen)
}

// openWorkflows returns the cached open-workflow index, rescanning when it is
// missing or older than openWorkflowScanCacheTTL.
//
// The lock is held across the scan on purpose: parallel probes in one renewal
// pass must SHARE one enumeration rather than each start their own, which is
// the entire reason the cache exists (the DS6 open-workflow scan budget risk
// finding 002 names). A scan is bounded by openWorkflowScanTimeout, so the
// wait is bounded too.
func (p *WorkflowLiveness) openWorkflows(ctx context.Context) (*openWorkflowIndex, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.index != nil && time.Since(p.scannedAt) <= openWorkflowScanCacheTTL {
		return p.index, nil
	}
	index, err := p.scanOpenWorkflows(ctx)
	if err != nil {
		// A failed refresh does NOT fall back to the stale index: every
		// consumer treats an error as unknown and fails safe, whereas a stale
		// map read as authoritative names workflows that may have closed.
		return nil, err
	}
	p.index = index
	p.scannedAt = time.Now()
	return index, nil
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

// scanOpenWorkflows performs the one bounded visibility enumeration every
// consumer shares, building both the superset liveness set and the exact
// run-id -> workflow-id inverse. Bounded by openWorkflowScanTimeout and
// maxOpenWorkflowScanPages; exceeding either is an error (unknown, fail-live),
// not a partial index.
func (p *WorkflowLiveness) scanOpenWorkflows(ctx context.Context) (*openWorkflowIndex, error) {
	index := &openWorkflowIndex{
		live:       make(map[string]struct{}),
		candidates: make(map[string][]openRunCandidate),
	}
	err := p.eachOpenWorkflow(ctx, func(info *workflowpb.WorkflowExecutionInfo, workflowID string) {
		// The liveness half over-approximates deliberately: RunID(workflowID)
		// covers a Schedule claim, RunID(claimID) covers its child, and an
		// extra entry only ever fails live.
		index.live[RunID(workflowID)] = struct{}{}
		if claimID, ok := strings.CutSuffix(workflowID, scheduledRunWorkflowIDSuffix); ok && claimID != "" {
			index.live[RunID(claimID)] = struct{}{}
		}

		gaggle, _ := memoString(info, RunGaggleMemoKey)
		workflowName, _ := memoString(info, RunWorkflowMemoKey)
		candidate := openRunCandidate{
			run: OpenRun{WorkflowID: workflowID, Gaggle: gaggle, Workflow: workflowName},
		}
		switch {
		case isScheduledRunWorkflowID(workflowID):
			// A scheduled run's child: RunScheduled rewrote its RunID to the
			// hash of the claim id, so that hash is the key runs/<id>/ uses.
			claimID, _ := strings.CutSuffix(workflowID, scheduledRunWorkflowIDSuffix)
			candidate.kind, candidate.claimID = openRunChild, claimID
			candidate.run.RunID = RunID(claimID)
		case looksLikeScheduleClaimID(workflowID):
			candidate.kind, candidate.claimID = openRunClaim, workflowID
			candidate.run.RunID = RunID(workflowID)
		default:
			// A directly started run. TemporalStarter.Start uses
			// WorkflowID == RunID, so the two names coincide. Unlike the
			// liveness set above — which may over-approximate freely — this
			// must NOT also register RunID(workflowID): that phantom key
			// names no run directory, and reportOrphanedEngineRuns would
			// report every healthy direct run as an orphan.
			candidate.kind = openRunDirect
			candidate.run.RunID = workflowID
		}
		index.candidates[candidate.run.RunID] = append(index.candidates[candidate.run.RunID], candidate)
	})
	if err != nil {
		return nil, err
	}
	return index, nil
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
