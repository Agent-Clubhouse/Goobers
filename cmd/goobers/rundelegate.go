package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/stateclient"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// rundelegate.go implements #343: when a short-lived `goobers run` process
// finds a live `goobers up` daemon already holding this instance's up.lock,
// it no longer just fails — it hands the trigger off to that daemon through
// a small file-based request/response protocol under
// <SchedulerDir>/pending-triggers/, and the daemon's own periodic sweep
// (wired in up.go) dispatches it through the exact same Scheduler.Trigger
// path a local `goobers run` would have used itself.
//
// This is deliberately NOT built on #169's planned daemon HTTP API — #169 is
// unbuilt and explicitly gated ("do not dispatch until its design review"),
// so depending on it would either mean taking on unreviewed V1 design work
// or inventing a parallel ad-hoc HTTP surface that risks conflicting with
// #169's eventual shape. Reusing the daemon's own already-safe-for-
// concurrent-calls Scheduler (Trigger/Tick already interleave safely under
// its internal mutex — see scheduler.go's Tick doc comment) and a periodic
// filesystem sweep (the same idle-between-ticks philosophy the scheduler
// loop itself uses, no busy-polling) needs no new server, port, or auth
// surface at all.

// pendingTriggersDir is the SchedulerDir subdirectory delegated and internal
// priority-trigger request/response files live under.
const pendingTriggersDir = "pending-triggers"

// maxOutstandingTriggerRequestsPerIdentity bounds how many not-yet-dispatched
// requests for the same (gaggle, workflow, PR, priority, sourceRun) identity
// sweepPendingTriggers will actually dispatch in one pass; the rest are
// answered immediately as bounded-out, without touching sched.Trigger* or the
// instance journal (#4323/#4326). This exists because a misbehaving external
// caller can drop pending-trigger request files directly under
// SchedulerDir()/pending-triggers — bypassing writeTriggerRequestPayload and
// any dedup a well-behaved client would apply on the write side — so the
// bound has to hold at sweep time, unconditionally, regardless of how a
// request file got there. #4326's incident was exactly this: a recurring
// automation generated five duplicate same-identity requests every 15
// minutes for ~59 hours with no accounting for already-pending ones,
// producing 1,177 duplicates. Deliberately generous (not 1): an occasional
// legitimate back-to-back retrigger of the same workflow must not be refused,
// only a runaway flood. Var, not const, so a test can shrink it.
var maxOutstandingTriggerRequestsPerIdentity = 5

// maxTriggerSweepEntriesPerCycle bounds how many pending-trigger request
// files a single sweepPendingTriggers call examines. Before this bound, a
// large backlog (however it accumulated) was processed in one synchronous
// pass — each real dispatch can journal one or more events, and
// journal.InstanceLog.Append rereads the whole event log on every call
// (#1914), so an unbounded backlog turned one sweep call into a
// multi-minute-or-worse blocking operation on the delegation-ticker
// goroutine. Bounding it here means a backlog drains progressively across
// several delegationSweepInterval cycles instead of stalling one. Var, not
// const, so a test can shrink it.
var maxTriggerSweepEntriesPerCycle = 500

// triggerIdentity groups pending trigger requests that target the same
// nominal work, for maxOutstandingTriggerRequestsPerIdentity's bound.
type triggerIdentity struct {
	Gaggle    string
	Workflow  string
	PR        int
	Priority  bool
	SourceRun string
}

func identityOf(req triggerRequest) triggerIdentity {
	return triggerIdentity{
		Gaggle: req.Gaggle, Workflow: req.Workflow, PR: req.PR,
		Priority: req.Priority, SourceRun: req.SourceRun,
	}
}

// identityLabel renders a triggerRequest's identity for an operator-facing
// bounded-out error, mirroring how `goobers run` itself names a target.
func identityLabel(req triggerRequest) string {
	label := req.Workflow
	if req.Gaggle != "" {
		label = req.Gaggle + "/" + label
	}
	if req.PR > 0 {
		label = fmt.Sprintf("%s (PR #%d)", label, req.PR)
	}
	return label
}

// pendingTriggerRequest is one *.request.json file read (not yet consumed)
// during a sweepPendingTriggers pass.
type pendingTriggerRequest struct {
	id       string
	path     string
	data     []byte
	req      triggerRequest
	parseErr error
}

// suppressExcessOutstanding returns the request ids among parsed that exceed
// maxOutstandingTriggerRequestsPerIdentity within their identity group — the
// oldest (by CreatedAt) requests in each group are kept, so a caller
// legitimately waiting on an early request is never the one bounded out by
// requests submitted after it. Requests that fail to parse or carry no
// CreatedAt are excluded from grouping; sweepPendingTriggers already refuses
// those on their own terms.
func suppressExcessOutstanding(parsed []*pendingTriggerRequest) map[string]bool {
	type candidate struct {
		id        string
		createdAt time.Time
	}
	groups := make(map[triggerIdentity][]candidate)
	for _, p := range parsed {
		if p.parseErr != nil || p.req.CreatedAt.IsZero() {
			continue
		}
		key := identityOf(p.req)
		groups[key] = append(groups[key], candidate{id: p.id, createdAt: p.req.CreatedAt})
	}
	suppressed := make(map[string]bool)
	for _, candidates := range groups {
		if len(candidates) <= maxOutstandingTriggerRequestsPerIdentity {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].createdAt.Before(candidates[j].createdAt) })
		for _, c := range candidates[maxOutstandingTriggerRequestsPerIdentity:] {
			suppressed[c.id] = true
		}
	}
	return suppressed
}

// triggerRequest is one request for the daemon-owned scheduler to trigger a
// workflow. Priority requests are internal, targeted, and fire-and-forget;
// ordinary delegated requests retain the request/response protocol.
type triggerRequest struct {
	Workflow  string    `json:"workflow"`
	Gaggle    string    `json:"gaggle,omitempty"`
	PR        int       `json:"pr,omitempty"`
	SourceRun string    `json:"sourceRun,omitempty"`
	Priority  bool      `json:"priority,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Deadline  time.Time `json:"deadline,omitempty"`
}

// triggerResponse is what the daemon writes back once it has acted on a
// triggerRequest — exactly one of RunID/Error is set (mirroring
// Scheduler.Trigger's own (runID, err) return shape).
type triggerResponse struct {
	RunID string `json:"runId,omitempty"`
	Error string `json:"error,omitempty"`
}

// requestSuffix/responseSuffix name a request/response file pair sharing one
// request id: "<id>.request.json" / "<id>.response.json".
const (
	requestSuffix  = ".request.json"
	responseSuffix = ".response.json"
)

// writeTriggerRequest drops a new delegation request file under
// schedulerDir/pending-triggers and returns its request id (derived from the
// unique temp name os.CreateTemp mints, so concurrent `goobers run`
// invocations against the same instance never collide without needing any
// extra locking of their own). The request is published atomically — written
// to a hidden temp that does NOT match requestSuffix, then renamed into place —
// so the daemon's sweep can never observe (and reject as malformed) a
// half-written request, the same torn-read guard claims.go and runcancel.go
// use. Before this was atomic, os.CreateTemp minted the request file already
// named *.request.json, so a sweep landing between create and write read empty
// bytes and failed the delegation.
func writeTriggerRequestContext(ctx context.Context, schedulerDir, gaggle, workflow string) (requestID string, err error) {
	createdAt, deadline := triggerRequestLifetime(ctx, triggerDelegationTimeout)
	return writeTriggerRequestPayload(schedulerDir, triggerRequest{
		Workflow:  workflow,
		Gaggle:    gaggle,
		CreatedAt: createdAt,
		Deadline:  deadline,
	})
}

func writeTargetedTriggerRequestContext(ctx context.Context, schedulerDir, gaggle, workflow string, pr int) (requestID string, err error) {
	createdAt, deadline := triggerRequestLifetime(ctx, triggerDelegationTimeout)
	return writeTriggerRequestPayload(schedulerDir, triggerRequest{
		Workflow:  workflow,
		Gaggle:    gaggle,
		PR:        pr,
		CreatedAt: createdAt,
		Deadline:  deadline,
	})
}

// writePriorityTriggerRequest queues a fire-and-forget re-tick for one exact
// gaggle/workflow after sourceRun makes new durable selection state visible.
// The daemon still routes it through ordinary scheduler admission, so budgets
// and concurrency limits bound the resulting chain.
func writePriorityTriggerRequest(schedulerDir, gaggle, workflow, sourceRun string) (requestID string, err error) {
	if gaggle == "" || workflow == "" || sourceRun == "" {
		return "", errors.New("delegate: priority trigger requires gaggle, workflow, and source run")
	}
	createdAt, deadline := triggerRequestLifetime(context.Background(), priorityTriggerTimeout)
	return writeTriggerRequestPayload(schedulerDir, triggerRequest{
		Workflow:  workflow,
		Gaggle:    gaggle,
		SourceRun: sourceRun,
		Priority:  true,
		CreatedAt: createdAt,
		Deadline:  deadline,
	})
}

func triggerRequestLifetime(ctx context.Context, timeout time.Duration) (time.Time, time.Time) {
	createdAt := delegationNow().UTC()
	deadline := createdAt.Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline.UTC()
	}
	return createdAt, deadline
}

func writeTriggerRequestPayload(schedulerDir string, req triggerRequest) (requestID string, err error) {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("delegate: create pending-triggers dir: %w", err)
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("delegate: create trigger request: %w", err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	data, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("delegate: write trigger request: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("delegate: close trigger request: %w", err)
	}
	requestID = strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	finalPath := filepath.Join(reqDir, requestID+requestSuffix)
	if err := durability.ReplaceFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("delegate: publish trigger request: %w", err)
	}
	return requestID, nil
}

// pollTriggerResponse waits for schedulerDir/pending-triggers/<requestID>
// .response.json to appear (the daemon's sweep writes it once it has
// dispatched — or failed to dispatch — the request), consumes it, and
// returns the same (runID, err) shape Scheduler.Trigger itself returns. A
// timeout — not an indefinite wait — bounds the case where no live daemon is
// actually picking requests up (e.g. it exited between this process
// observing up.lock held and writing its request).
func pollTriggerResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (runID string, err error) {
	respPath := filepath.Join(schedulerDir, pendingTriggersDir, requestID+responseSuffix)
	deadline := delegationNow().Add(timeout)
	for {
		if data, rerr := os.ReadFile(respPath); rerr == nil {
			// The writer (sweepPendingTriggers / a test responder) publishes via
			// journal.WriteFileAtomic (hidden temp + rename), so a torn read here
			// should not occur in practice — this stays tolerant of an unparseable
			// read as defense in depth rather than failing the whole delegation on
			// it: consuming (removing) the file before a clean parse would strand
			// the real response so the next poll could never see it. Only remove
			// once we have a complete, parseable response. The deadline still
			// bounds a genuinely stuck writer.
			var resp triggerResponse
			if jerr := json.Unmarshal(data, &resp); jerr == nil {
				_ = os.Remove(respPath)
				if resp.Error != "" {
					return "", errors.New(resp.Error)
				}
				return resp.RunID, nil
			}
		}
		if delegationNow().After(deadline) {
			return "", fmt.Errorf("delegate: timed out after %s waiting for the live `goobers up` daemon to pick up the trigger request "+
				"(request left at %s — is the daemon still running and healthy?)", timeout, filepath.Join(schedulerDir, pendingTriggersDir, requestID+requestSuffix))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
}

// delegationPollInterval bounds how often pollTriggerResponse re-checks for
// a response file. Var, not const, so tests aren't slow.
var delegationPollInterval = 100 * time.Millisecond

// triggerDelegationTimeout bounds pollTriggerResponse's total wait. Var, not
// const, for the same reason. 30s comfortably exceeds delegationSweepInterval
// (up.go) by a wide margin under any normal daemon load.
var triggerDelegationTimeout = 30 * time.Second

var delegationNow = time.Now

// priorityTriggerTimeout keeps an internally-requested re-tick alive while the
// source workflow's concurrent runs finish. Unlike an interactive delegation,
// no client is waiting on a 30-second response deadline.
const priorityTriggerTimeout = time.Hour

func triggerRequestTimeout(req triggerRequest) time.Duration {
	if req.Priority {
		return priorityTriggerTimeout
	}
	return triggerDelegationTimeout
}

func triggerRequestDeadline(req triggerRequest) time.Time {
	maxDeadline := req.CreatedAt.Add(triggerRequestTimeout(req))
	if !req.Deadline.IsZero() && req.Deadline.Before(maxDeadline) {
		return req.Deadline
	}
	return maxDeadline
}

// sweepPendingTriggers is the daemon-side half of #343's delegation
// protocol, called at startup and periodically from runUpContext's sweep
// goroutine
// (mirroring the existing claim-recovery ticker's shape). It dispatches
// every pending request through sched — the exact same Scheduler.Trigger a
// local `goobers run` invocation would call directly — and writes back a
// response for pollTriggerResponse to consume.
//
// A request file is removed BEFORE dispatch, not after: if the daemon
// crashed mid-dispatch, a still-present request file would replay on the
// next process's startup sweep and double-trigger the same nominal request;
// removing first means a lost response in that narrow window fails the
// waiting `goobers run` closed (timeout) rather than risking a duplicate
// run — the same "don't replay an ambiguous firing" principle Scheduler's
// own trigger.fired-before-dispatch ordering already applies (see dispatch's
// doc comment in scheduler.go).
func sweepPendingTriggers(ctx context.Context, schedulerDir string, sched *localscheduler.Scheduler, now func() time.Time) error {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	entries, exists, err := readDirectory(reqDir)
	if !exists {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delegate: read pending triggers: %w", err)
	}
	var sweepErr error
	var requestEntries []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), responseSuffix) {
			info, err := e.Info()
			if err == nil && now().Sub(info.ModTime()) > triggerDelegationTimeout {
				_ = os.Remove(filepath.Join(reqDir, e.Name()))
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), requestSuffix) {
			continue
		}
		requestEntries = append(requestEntries, e)
	}
	// os.ReadDir (readDirectory's underlying call) returns entries sorted by
	// filename, so bounding here consistently defers the same (lexically
	// later) requests to the next cycle rather than starving them at random —
	// #4323's fix for a backlog too large to examine in one pass.
	if len(requestEntries) > maxTriggerSweepEntriesPerCycle {
		requestEntries = requestEntries[:maxTriggerSweepEntriesPerCycle]
	}

	// Read every candidate request before dispatching any of them so
	// suppressExcessOutstanding can bound outstanding requests per identity
	// across the whole batch, not just entries seen so far.
	parsed := make([]*pendingTriggerRequest, 0, len(requestEntries))
	for _, e := range requestEntries {
		requestID := strings.TrimSuffix(e.Name(), requestSuffix)
		reqPath := filepath.Join(reqDir, e.Name())
		data, err := os.ReadFile(reqPath)
		if err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: read trigger request %s: %w", requestID, err))
			}
			continue
		}
		p := &pendingTriggerRequest{id: requestID, path: reqPath, data: data}
		p.parseErr = json.Unmarshal(data, &p.req)
		parsed = append(parsed, p)
	}
	suppressed := suppressExcessOutstanding(parsed)

	for _, p := range parsed {
		requestID, reqPath := p.id, p.path
		if err := os.Remove(reqPath); err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: consume trigger request %s: %w", requestID, err))
			}
			continue
		}

		req := p.req
		data := p.data
		resp := triggerResponse{}
		switch {
		case p.parseErr != nil:
			resp.Error = fmt.Sprintf("delegate: malformed trigger request: %v", p.parseErr)
		case req.CreatedAt.IsZero():
			resp.Error = fmt.Sprintf("delegate: trigger request %s has no creation time; refusing to dispatch", requestID)
			sched.RecordTriggerRefusal(req.Workflow, resp.Error)
		case suppressed[requestID]:
			// Deliberately skipped: no sched.Trigger* call and no
			// RecordTriggerRefusal journal write. Both are what a runaway
			// duplicate producer must not be able to multiply — this branch
			// costs only the file remove and (for a non-priority caller
			// waiting on a response) one small atomic write, however large
			// the backlog behind it is (#4326).
			resp.Error = fmt.Sprintf(
				"delegate: %d requests already outstanding for %s (bounded to %d); rejected without dispatch",
				maxOutstandingTriggerRequestsPerIdentity+1, identityLabel(req), maxOutstandingTriggerRequestsPerIdentity,
			)
		default:
			sweepTime := now()
			requestDeadline := triggerRequestDeadline(req)
			requestLifetime := requestDeadline.Sub(req.CreatedAt)
			if !sweepTime.Before(requestDeadline) {
				resp.Error = fmt.Sprintf(
					"delegate: stale trigger request %s reached its %s deadline (created at %s, lifetime %s); refusing to dispatch",
					requestID, requestDeadline.Format(time.RFC3339Nano), req.CreatedAt.Format(time.RFC3339Nano), requestLifetime,
				)
				sched.RecordTriggerRefusal(req.Workflow, resp.Error)
			} else {
				requestCtx, cancelRequest := context.WithDeadline(ctx, requestDeadline)
				var runID string
				var terr error
				if req.Priority {
					runID, terr = sched.TriggerPriorityWithDispatchContext(requestCtx, ctx, localscheduler.WorkflowIdentity{
						Gaggle: req.Gaggle, Workflow: req.Workflow,
					}, req.SourceRun, sweepTime)
				} else if req.Gaggle != "" {
					identity := localscheduler.WorkflowIdentity{Gaggle: req.Gaggle, Workflow: req.Workflow}
					if req.PR > 0 {
						runID, terr = sched.TriggerSignalExactWithDispatchContext(requestCtx, ctx, identity, webhookhttp.SignalName("pull_request"),
							webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: req.PR}), sweepTime)
					} else {
						runID, terr = sched.TriggerExactWithDispatchContext(requestCtx, ctx, identity, sweepTime)
					}
				} else {
					if req.PR > 0 {
						runID, terr = sched.TriggerSignalWithDispatchContext(requestCtx, ctx, req.Workflow,
							webhookhttp.SignalName("pull_request"),
							webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: req.PR}), sweepTime)
					} else {
						runID, terr = sched.TriggerWithDispatchContext(requestCtx, ctx, req.Workflow, sweepTime)
					}
				}
				cancelRequest()
				var rejected *localscheduler.TriggerRejectedError
				switch {
				case terr != nil && errors.As(terr, &rejected) && rejected.Transient():
					// A capacity refusal is held by a run that is already
					// finishing, so answering the client with a hard error
					// turns a moment of contention into a failed command. Put
					// the request back, untouched, and let the next sweep try
					// again: CreatedAt is preserved, so the staleness check
					// above still bounds the wait, and the client's own
					// pollTriggerResponse deadline bounds it independently.
					// Requeued atomically (hidden temp + rename) so a
					// concurrent sweep/inspection can never observe a
					// truncated live request file mid-rewrite.
					if rerr := journal.WriteFileAtomic(reqPath, data, 0o644); rerr != nil {
						sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: requeue trigger request %s: %w", requestID, rerr))
						resp.Error = terr.Error()
						break
					}
					continue
				case terr != nil:
					resp.Error = terr.Error()
					if req.Priority && !errors.As(terr, &rejected) {
						sched.RecordTriggerRefusal(req.Workflow, resp.Error)
						sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: dispatch priority trigger %s: %w", requestID, terr))
					}
				default:
					resp.RunID = runID
					sched.RecordRecoveredTrigger(requestID, req.Workflow, runID)
				}
			}
		}

		if req.Priority {
			continue
		}
		respData, err := json.Marshal(resp)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: encode trigger response %s: %w", requestID, err))
			continue
		}
		if err := journal.WriteFileAtomic(filepath.Join(reqDir, requestID+responseSuffix), respData, 0o644); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: write trigger response %s: %w", requestID, err))
		}
	}
	return sweepErr
}

// startPeriodicSweep runs sweep on a fresh goroutine every interval until ctx
// is done, then stops the ticker and closes the returned channel. Factored
// out (rather than inlined at each call site, as runUpContextWithForce's
// older tickers are) so adding one more periodic sweep — the claims ticker
// #4323 splits off the trigger-delegation ticker — doesn't grow that
// already-large function's cyclomatic complexity.
func startPeriodicSweep(ctx context.Context, interval time.Duration, sweep func()) (done chan struct{}) {
	ticker := time.NewTicker(interval)
	done = make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
	return done
}

// pendingTriggerQueueStats reports how many *.request.json files currently
// sit under schedulerDir/pending-triggers and the age of the oldest one, so
// an operator (`goobers status`) can see a growing backlog before it turns
// into #4323-style starvation instead of only after health checks start
// failing. depth is 0 and oldestAge is zero when there is no pending-triggers
// directory yet (nothing has ever delegated) or it is empty.
func pendingTriggerQueueStats(schedulerDir string, now time.Time) (depth int, oldestAge time.Duration, err error) {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	entries, exists, err := readDirectory(reqDir)
	if !exists {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("delegate: read pending triggers: %w", err)
	}
	var oldest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), requestSuffix) {
			continue
		}
		depth++
		info, err := e.Info()
		if err != nil {
			continue
		}
		if oldest.IsZero() || info.ModTime().Before(oldest) {
			oldest = info.ModTime()
		}
	}
	if depth == 0 {
		return 0, 0, nil
	}
	return depth, now.Sub(oldest), nil
}

// dispatchPriorityTrigger routes apply-verdict's crowned-lander re-tick to
// whichever half of the trigger seam this stage can actually reach (#3878).
//
// A stage pod has no pending-triggers directory the daemon sweeps — the one it
// can see is its own container's, so the file drop below is written and then
// discarded with the pod. When the scheduler plane is selected, the re-tick
// goes to the daemon's trigger route instead, under the same pod principal and
// the same gaggle containment the scheduler-state route applies. Everywhere
// else (a self runner, a local mode) the file drop is still the right and only
// mechanism.
func dispatchPriorityTrigger(ctx context.Context, l instance.Layout, gaggle, workflow, sourceRun string) (string, error) {
	if gaggle == "" || workflow == "" || sourceRun == "" {
		return "", errors.New("delegate: priority trigger requires gaggle, workflow, and source run")
	}
	store, err := openStageStateStore(l)
	if err != nil {
		return "", err
	}
	triggerer, ok := store.(stateclient.PriorityTriggerer)
	if !ok || !statePlaneSelected() {
		return writePriorityTriggerRequest(l.SchedulerDir(), gaggle, workflow, sourceRun)
	}
	return triggerer.PriorityTrigger(ctx, workflow, sourceRun)
}
