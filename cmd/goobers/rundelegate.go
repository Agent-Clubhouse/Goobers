package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/stateclient"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// Same-root callers can submit ordinary and priority starts through request
// files when the daemon owns the instance lock. The production sweep transfers
// those files to the shared durable start ledger; files remain the client's
// acknowledgment/response transport. The older direct sweep is retained only
// for compatibility callers and uncertain legacy-file recovery.

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

type pendingTriggerCandidate struct {
	id   string
	path string
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
	QueueTransfer bool   `json:"queueTransfer,omitempty"`
	AcceptanceID  string `json:"acceptanceId,omitempty"`
	Workflow      string `json:"workflow"`
	Gaggle        string `json:"gaggle,omitempty"`
	PR            int    `json:"pr,omitempty"`
	Force         bool   `json:"force,omitempty"`
	SourceRun     string `json:"sourceRun,omitempty"`
	Priority      bool   `json:"priority,omitempty"`
	// Key is the idempotency key (#4326). When set, the request FILE is named
	// from it, so two producers submitting the same logical ask publish to one
	// path and the atomic rename collapses them. Empty on delegated requests,
	// whose caller is blocked on a per-id response file.
	Key           string    `json:"key,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	Deadline      time.Time `json:"deadline,omitempty"`
	AcceptedAt    time.Time `json:"acceptedAt,omitempty"`
	DispatchRunID string    `json:"dispatchRunId,omitempty"`
}

// triggerResponse is what the daemon writes back once it has acted on a
// triggerRequest — exactly one of RunID/Error is set (mirroring
// Scheduler.Trigger's own (runID, err) return shape).
type triggerResponse struct {
	RunID     string `json:"runId,omitempty"`
	Error     string `json:"error,omitempty"`
	State     string `json:"state,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// requestSuffix/responseSuffix name a request/response file pair sharing one
// request id: "<id>.request.json" / "<id>.response.json".
const (
	requestSuffix   = ".request.json"
	responseSuffix  = ".response.json"
	ackSuffix       = ".ack.json"
	activeSuffix    = ".active.json"
	abandonedSuffix = ".abandoned.json"

	triggerResponseQueued = "queued"
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
	return writeTriggerRequestContextOptions(ctx, schedulerDir, gaggle, workflow, false)
}

func writeTriggerRequestContextOptions(ctx context.Context, schedulerDir, gaggle, workflow string, force bool) (requestID string, err error) {
	createdAt, deadline := triggerRequestLifetime(ctx, triggerDelegationTimeout)
	return writeTriggerRequestPayload(schedulerDir, triggerRequest{
		Workflow:  workflow,
		Gaggle:    gaggle,
		Force:     force,
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
		Key:       triggerRequestIdempotencyKey(gaggle, workflow, sourceRun),
		CreatedAt: createdAt,
		Deadline:  deadline,
	})
}

func triggerRequestLifetime(ctx context.Context, timeout time.Duration) (time.Time, time.Time) {
	createdAt := delegationNow().UTC()
	var deadline time.Time
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(createdAt.Add(timeout)) {
		deadline = contextDeadline.UTC()
	}
	return createdAt, deadline
}

func writeTriggerRequestPayload(schedulerDir string, req triggerRequest) (requestID string, err error) {
	// A keyed request's id — and therefore its published filename — is derived
	// from the key, so repeat submissions of one logical ask converge on a
	// single file instead of accumulating (#4326).
	keyedID := ""
	if req.Key != "" {
		keyedID = keyedRequestID(req.Key)
	}
	return writeDelegateRequestWithID(
		schedulerDir,
		triggerDelegateFileProtocol(),
		keyedID,
		req,
		nil,
		func(reqDir string) error { return admitTriggerSubmission(reqDir, keyedID) },
	)
}

func triggerDelegateFileProtocol() delegateFileProtocol {
	return delegateFileProtocol{
		pendingDir:      pendingTriggersDir,
		requestSuffix:   requestSuffix,
		responseSuffix:  responseSuffix,
		errorPrefix:     "delegate",
		requestDirLabel: "pending-triggers dir",
		requestLabel:    "trigger request",
		staleAfter:      triggerDelegationTimeout,
	}
}

// pollTriggerResponse waits for schedulerDir/pending-triggers/<requestID>
// .response.json to appear (the daemon's sweep writes it once it has
// dispatched — or failed to dispatch — the request), consumes it, and
// returns the same (runID, err) shape Scheduler.Trigger itself returns. A
// timeout — not an indefinite wait — bounds the case where no live daemon is
// actually picking requests up (e.g. it exited between this process
// observing up.lock held and writing its request).
func pollTriggerResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (runID string, err error) {
	for {
		resp, err := pollTriggerResponseEvent(ctx, schedulerDir, requestID, timeout, false)
		if err != nil {
			return "", err
		}
		if resp.State == triggerResponseQueued {
			continue
		}
		if resp.Error != "" {
			return "", errors.New(resp.Error)
		}
		return resp.RunID, nil
	}
}

func pollTriggerResponseEvent(ctx context.Context, schedulerDir, requestID string, timeout time.Duration, withdrawOnTimeout bool) (triggerResponse, error) {
	respPath := filepath.Join(schedulerDir, pendingTriggersDir, requestID+responseSuffix)
	ackPath := filepath.Join(schedulerDir, pendingTriggersDir, requestID+ackSuffix)
	deadline := delegationNow().Add(timeout)
	awaitingClaimAck := false
	for {
		if resp, ok := readTriggerResponseFile(respPath); ok {
			_ = os.Remove(ackPath)
			return resp, nil
		}
		if resp, ok := readTriggerResponseFile(ackPath); ok {
			return resp, nil
		}
		if delegationNow().After(deadline) {
			if withdrawOnTimeout && !awaitingClaimAck {
				withdrawn, werr := withdrawTriggerRequest(schedulerDir, requestID)
				if werr != nil {
					return triggerResponse{}, werr
				}
				if !withdrawn {
					if resp, ok := readTriggerResponseFile(respPath); ok {
						return resp, nil
					}
					if resp, ok := readTriggerResponseFile(ackPath); ok {
						return resp, nil
					}
					awaitingClaimAck = true
					deadline = delegationNow().Add(triggerClaimAckGrace)
					continue
				}
			}
			if resp, ok := readTriggerResponseFile(respPath); ok {
				_ = os.Remove(ackPath)
				return resp, nil
			}
			if resp, ok := readTriggerResponseFile(ackPath); ok {
				return resp, nil
			}
			// Reaching here now means the daemon never answered at all, not
			// merely that it was slow: the wait outlives the request's own
			// deadline, so a daemon that swept at any point would have written
			// either a dispatch or a stale refusal. Say which of the two
			// remaining explanations it is rather than asking the operator
			// (#2974).
			if triggerResponseTimeoutHook != nil {
				triggerResponseTimeoutHook(requestID)
				if resp, ok := readTriggerResponseFile(respPath); ok {
					_ = os.Remove(ackPath)
					return resp, nil
				}
				if resp, ok := readTriggerResponseFile(ackPath); ok {
					return resp, nil
				}
			}
			return triggerResponse{}, fmt.Errorf("delegate: timed out after %s waiting for the `goobers up` daemon to answer the trigger request "+
				"(request left at %s). %s", timeout,
				filepath.Join(schedulerDir, pendingTriggersDir, requestID+requestSuffix),
				schedulerLivenessEvidence(schedulerDir))
		}
		select {
		case <-ctx.Done():
			return triggerResponse{}, ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
}

func readTriggerResponseFile(path string) (triggerResponse, bool) {
	// The writer publishes via journal.WriteFileAtomic (hidden temp + rename),
	// so a torn read should not occur in practice. Stay tolerant anyway:
	// consuming before a clean parse would strand the real response.
	return readAndRemoveDelegateJSON[triggerResponse](path)
}

func withdrawTriggerRequest(schedulerDir, requestID string) (bool, error) {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	reqPath := filepath.Join(reqDir, requestID+requestSuffix)
	abandonedPath := filepath.Join(reqDir, requestID+abandonedSuffix)
	if err := os.Rename(reqPath, abandonedPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("delegate: withdraw trigger request %s: %w", requestID, err)
	}
	raw, readErr := os.ReadFile(abandonedPath)
	var req triggerRequest
	if readErr == nil {
		readErr = json.Unmarshal(raw, &req)
	}
	// Unknown custody cannot be reported as a successful withdrawal.
	if readErr != nil || req.AcceptanceID != "" || req.QueueTransfer {
		return false, errors.Join(readErr, os.Rename(abandonedPath, reqPath))
	}
	return true, nil
}

// delegationPollInterval bounds how often pollTriggerResponse re-checks for
// a response file. Var, not const, so tests aren't slow.
var delegationPollInterval = 100 * time.Millisecond

var triggerClaimAckGrace = 5 * time.Second

// triggerDelegationTimeout bounds pollTriggerResponse's total wait. Var, not
// const, for the same reason. 30s comfortably exceeds delegationSweepInterval
// (up.go) by a wide margin under any normal daemon load.
var triggerDelegationTimeout = 30 * time.Second

// triggerResponseGrace is how much longer the CLIENT waits than the request it
// submitted is allowed to live (#2974).
//
// The two windows used to be one number, and that is what produced a failure
// with no answer in it. On a Windows daemon restart the client waited its 30
// seconds, reported "timed out ... is the daemon still running and healthy?",
// and the daemon swept the request about six seconds later and — correctly,
// under #537 — refused it as stale. The daemon was healthy, the request was
// definitively resolved, and the operator was told neither of those things.
//
// Ordering the windows fixes that without touching the safety property. The
// request still expires exactly when it always did, so a request the operator
// believes failed can still never be dispatched later; the client simply stays
// long enough afterwards to collect the daemon's own verdict. The grace covers
// the sweep interval plus the write, so the only way to reach a bare timeout
// now is a daemon that is not sweeping at all — which is the one case that
// message was ever meant to describe.
//
// Var, not const, so tests need not sleep.
var triggerResponseGrace = 15 * time.Second

// triggerResponseWait is how long a submitting client listens for a verdict.
func triggerResponseWait() time.Duration {
	return triggerDelegationTimeout + triggerResponseGrace
}

var delegationNow = time.Now

var acceptedTriggerQueueLifetime = func() time.Duration { return 10 * triggerResponseWait() }

var triggerResponseTimeoutHook func(requestID string)

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

func triggerRequestQueueDeadline(req triggerRequest, staleLegacyMissingDeadline bool) (time.Time, bool) {
	if req.Priority {
		return triggerRequestDeadline(req), true
	}
	if req.Deadline.IsZero() {
		if staleLegacyMissingDeadline {
			return req.CreatedAt.Add(triggerRequestTimeout(req)), true
		}
		return time.Time{}, false
	}
	maxDeadline := req.CreatedAt.Add(triggerRequestTimeout(req))
	if req.Deadline.Before(maxDeadline) {
		return req.Deadline, true
	}
	return time.Time{}, false
}

func triggerAttemptContext(ctx context.Context, req triggerRequest, startedAt time.Time) (context.Context, context.CancelFunc, time.Time) {
	deadline := startedAt.UTC().Add(triggerRequestTimeout(req))
	maxDeadline := req.CreatedAt.Add(triggerRequestTimeout(req))
	if !req.Deadline.IsZero() && req.Deadline.Before(maxDeadline) && req.Deadline.Before(deadline) {
		deadline = req.Deadline
	}
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	return requestCtx, cancel, deadline
}

type triggerSweepOptions struct {
	staleLegacyMissingDeadline bool
	recoverActiveRequests      bool
}

func claimTriggerRequest(reqDir, requestID string) (string, bool, error) {
	reqPath := filepath.Join(reqDir, requestID+requestSuffix)
	activePath := filepath.Join(reqDir, requestID+activeSuffix)
	if err := os.Rename(reqPath, activePath); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("delegate: claim trigger request %s: %w", requestID, err)
	}
	return activePath, true, nil
}

func writeTriggerResponse(reqDir, requestID string, resp triggerResponse) error {
	resp.RequestID = requestID
	if err := writeDelegateJSON(filepath.Join(reqDir, requestID+responseSuffix), resp); err != nil {
		var encodeErr *delegateJSONEncodeError
		if errors.As(err, &encodeErr) {
			return fmt.Errorf("delegate: encode trigger response %s: %w", requestID, err)
		}
		return fmt.Errorf("delegate: write trigger response %s: %w", requestID, err)
	}
	_ = os.Remove(filepath.Join(reqDir, requestID+ackSuffix))
	return nil
}

func writeTriggerAck(reqDir, requestID string, resp triggerResponse) error {
	resp.RequestID = requestID
	if err := writeDelegateJSON(filepath.Join(reqDir, requestID+ackSuffix), resp); err != nil {
		var encodeErr *delegateJSONEncodeError
		if errors.As(err, &encodeErr) {
			return fmt.Errorf("delegate: encode trigger ack %s: %w", requestID, err)
		}
		return fmt.Errorf("delegate: write trigger ack %s: %w", requestID, err)
	}
	return nil
}

func acknowledgeTriggerRequest(reqDir, requestID string, req *triggerRequest, now time.Time) error {
	if req.Priority || !req.AcceptedAt.IsZero() {
		return nil
	}
	req.AcceptedAt = now.UTC()
	return writeTriggerAck(reqDir, requestID, triggerResponse{State: triggerResponseQueued})
}

func acceptedTriggerExpired(req triggerRequest, now time.Time) bool {
	return !req.Priority && !req.AcceptedAt.IsZero() && !now.Before(req.AcceptedAt.Add(acceptedTriggerQueueLifetime()))
}

func acceptedTriggerExpiredResponse(requestID string, req triggerRequest) triggerResponse {
	deadline := req.AcceptedAt.Add(acceptedTriggerQueueLifetime())
	return triggerResponse{
		Error: fmt.Sprintf(
			"delegate: accepted trigger request %s waited until %s without capacity; retry the trigger",
			requestID, deadline.Format(time.RFC3339Nano),
		),
		Retryable: true,
	}
}

func requeueTriggerRequest(reqPath string, req triggerRequest) error {
	return writeDelegateJSON(reqPath, req)
}

func recoverActiveTriggerRequest(schedulerDir, reqDir, requestID string) (bool, error) {
	activePath := filepath.Join(reqDir, requestID+activeSuffix)
	requestPath := filepath.Join(reqDir, requestID+requestSuffix)
	responsePath := filepath.Join(reqDir, requestID+responseSuffix)
	if _, err := os.Stat(responsePath); err == nil {
		_ = os.Remove(filepath.Join(reqDir, requestID+ackSuffix))
		_ = os.Remove(activePath)
		return false, nil
	} else if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("delegate: inspect active trigger response %s: %w", requestID, err)
	}
	activeData, err := os.ReadFile(activePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("delegate: read active trigger request %s: %w", requestID, err)
	}
	var req triggerRequest
	if err := json.Unmarshal(activeData, &req); err == nil && req.DispatchRunID != "" && req.AcceptanceID == "" {
		root := filepath.Dir(schedulerDir)
		if _, err := instance.NewLayout(root).FindRunDir(req.DispatchRunID); err == nil {
			if err := writeTriggerResponse(reqDir, requestID, triggerResponse{RunID: req.DispatchRunID}); err != nil {
				return false, err
			}
		} else {
			resp := triggerResponse{
				Error: fmt.Sprintf(
					"delegate: trigger request %s may have dispatched run %s before daemon shutdown; refusing to replay",
					requestID, req.DispatchRunID,
				),
			}
			if err := writeTriggerResponse(reqDir, requestID, resp); err != nil {
				return false, err
			}
		}
		_ = os.Remove(activePath)
		return false, nil
	}
	if err := os.Rename(activePath, requestPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if errors.Is(err, os.ErrExist) {
			_ = os.Remove(activePath)
			return false, nil
		}
		return false, fmt.Errorf("delegate: recover active trigger request %s: %w", requestID, err)
	}
	return true, nil
}

func newDelegatedDispatchRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	for raw == [16]byte{} {
		if _, err := rand.Read(raw[:]); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(raw[:]), nil
}

func discoverPendingTriggerCandidates(schedulerDir, reqDir string, entries []os.DirEntry, now func() time.Time, options triggerSweepOptions) ([]pendingTriggerCandidate, error) {
	var candidates []pendingTriggerCandidate
	var discoverErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch {
		case strings.HasSuffix(e.Name(), responseSuffix), strings.HasSuffix(e.Name(), ackSuffix), strings.HasSuffix(e.Name(), abandonedSuffix):
			info, err := e.Info()
			if err == nil {
				removeExpiredDelegateArtifact(filepath.Join(reqDir, e.Name()), info, now(), triggerDelegationTimeout)
			}
		case strings.HasSuffix(e.Name(), activeSuffix):
			if !options.recoverActiveRequests {
				continue
			}
			requestID := strings.TrimSuffix(e.Name(), activeSuffix)
			recovered, err := recoverActiveTriggerRequest(schedulerDir, reqDir, requestID)
			if err != nil {
				discoverErr = errors.Join(discoverErr, err)
				continue
			}
			if recovered {
				candidates = append(candidates, pendingTriggerCandidate{
					id:   requestID,
					path: filepath.Join(reqDir, requestID+requestSuffix),
				})
			}
		case strings.HasSuffix(e.Name(), requestSuffix):
			requestID := strings.TrimSuffix(e.Name(), requestSuffix)
			candidates = append(candidates, pendingTriggerCandidate{
				id:   requestID,
				path: filepath.Join(reqDir, e.Name()),
			})
		}
	}
	return candidates, discoverErr
}

func readPendingTriggerRequests(requestEntries []pendingTriggerCandidate) ([]*pendingTriggerRequest, error) {
	parsed := make([]*pendingTriggerRequest, 0, len(requestEntries))
	var readErr error
	for _, e := range requestEntries {
		requestID := e.id
		reqPath := e.path
		data, err := os.ReadFile(reqPath)
		if err != nil {
			if !os.IsNotExist(err) {
				readErr = errors.Join(readErr, fmt.Errorf("delegate: read trigger request %s: %w", requestID, err))
			}
			continue
		}
		p := &pendingTriggerRequest{id: requestID, path: reqPath, data: data}
		p.parseErr = json.Unmarshal(data, &p.req)
		parsed = append(parsed, p)
	}
	return parsed, readErr
}

// sweepPendingTriggers preserves the legacy direct sweep. The daemon calls
// sweepPendingTriggersWithAdmission, which first adopts any existing durable
// receipt and transfers new requests before execution. Legacy dispatch markers
// remain fail-closed; they cannot opt a new production request out of the queue.
func sweepPendingTriggers(ctx context.Context, schedulerDir string, log *journal.InstanceLog, sched *localscheduler.Scheduler, now func() time.Time) error {
	return sweepPendingTriggersWithOptions(ctx, schedulerDir, log, sched, now, triggerSweepOptions{})
}

func sweepPendingTriggersWithOptions(ctx context.Context, schedulerDir string, log *journal.InstanceLog, sched *localscheduler.Scheduler, now func() time.Time, options triggerSweepOptions) error {
	return sweepPendingTriggersWithAdmission(ctx, schedulerDir, log, sched, now, options, nil)
}

func sweepPendingTriggersWithAdmission(ctx context.Context, schedulerDir string, log *journal.InstanceLog, sched *localscheduler.Scheduler, now func() time.Time, options triggerSweepOptions, admission delegatedAdmission) error {
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	entries, exists, err := readDirectory(reqDir)
	if !exists {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delegate: read pending triggers: %w", err)
	}
	var sweepErr error
	requestEntries, err := discoverPendingTriggerCandidates(schedulerDir, reqDir, entries, now, options)
	sweepErr = errors.Join(sweepErr, err)
	// Total depth is reported from the entries this sweep already read, so the
	// visibility costs no extra directory walk, and BEFORE the per-cycle bound
	// truncates the batch — the number that matters is the backlog the
	// producer built, not the slice this pass happens to examine (#4326).
	reportPendingTriggerDepth(log, len(requestEntries))
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
	parsed, err := readPendingTriggerRequests(requestEntries)
	sweepErr = errors.Join(sweepErr, err)
	suppressed := suppressExcessOutstanding(parsed)

	for _, p := range parsed {
		requestID, reqPath := p.id, p.path
		activePath, claimed, err := claimTriggerRequest(reqDir, requestID)
		if err != nil {
			sweepErr = errors.Join(sweepErr, err)
			continue
		}
		if !claimed {
			continue
		}

		req := p.req
		if handled, err := recoverDelegatedAdmission(ctx, admission, p.parseErr, reqDir, requestID, activePath, &req); handled {
			sweepErr = errors.Join(sweepErr, err)
			continue
		}
		resp := triggerResponse{}
		switch {
		case p.parseErr != nil:
			resp.Error = fmt.Sprintf("delegate: malformed trigger request: %v", p.parseErr)
		case req.CreatedAt.IsZero():
			resp.Error = fmt.Sprintf("delegate: trigger request %s has no creation time; refusing to dispatch", requestID)
			sched.RecordTriggerRefusal(req.Workflow, resp.Error)
		case admission != nil && req.DispatchRunID != "":
			resp.Error = "delegate: legacy uncertain dispatch marker requires restart reconciliation; refusing direct replay"
		case req.Force && (req.PR > 0 || req.Priority):
			resp.Error = "delegate: force is only valid for an explicit manual trigger"
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
			if requestDeadline, ok := triggerRequestQueueDeadline(req, options.staleLegacyMissingDeadline); ok && !sweepTime.Before(requestDeadline) {
				requestLifetime := requestDeadline.Sub(req.CreatedAt)
				resp.Error = fmt.Sprintf(
					"delegate: stale trigger request %s reached its %s deadline (created at %s, lifetime %s); refusing to dispatch",
					requestID, requestDeadline.Format(time.RFC3339Nano), req.CreatedAt.Format(time.RFC3339Nano), requestLifetime,
				)
				sched.RecordTriggerRefusal(req.Workflow, resp.Error)
			} else if acceptedTriggerExpired(req, sweepTime) {
				resp = acceptedTriggerExpiredResponse(requestID, req)
				sched.RecordTriggerRefusal(req.Workflow, resp.Error)
			} else {
				if admission != nil && req.DispatchRunID == "" {
					_, err := admission(ctx, reqDir, requestID, activePath, &req, false)
					sweepErr = errors.Join(sweepErr, err)
					continue
				}
				if req.DispatchRunID == "" {
					runID, err := newDelegatedDispatchRunID()
					if err != nil {
						sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: allocate dispatch run id for %s: %w", requestID, err))
						_ = os.Remove(activePath)
						continue
					}
					req.DispatchRunID = runID
				}
				if err := requeueTriggerRequest(activePath, req); err != nil {
					sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: persist dispatch marker for %s: %w", requestID, err))
					_ = os.Remove(activePath)
					continue
				}
				requestCtx, cancelRequest, _ := triggerAttemptContext(ctx, req, sweepTime)
				var runID string
				var terr error
				if req.Priority {
					runID, terr = sched.TriggerPriorityWithDispatchRunID(requestCtx, ctx, localscheduler.WorkflowIdentity{
						Gaggle: req.Gaggle, Workflow: req.Workflow,
					}, req.SourceRun, sweepTime, req.DispatchRunID)
				} else if req.Gaggle != "" {
					identity := localscheduler.WorkflowIdentity{Gaggle: req.Gaggle, Workflow: req.Workflow}
					if req.PR > 0 {
						runID, terr = sched.TriggerSignalExactWithDispatchContextOptions(requestCtx, ctx, identity, webhookhttp.SignalName("pull_request"),
							webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: req.PR}), sweepTime, localscheduler.SignalTriggerOptions{
								RunID: req.DispatchRunID,
							})
					} else {
						runID, terr = sched.TriggerExactWithDispatchContextOptions(requestCtx, ctx, identity, sweepTime, localscheduler.ManualTriggerOptions{
							BypassCadenceBudgets: req.Force,
							RunID:                req.DispatchRunID,
						})
					}
				} else {
					if req.PR > 0 {
						runID, terr = sched.TriggerSignalWithDispatchContextOptions(requestCtx, ctx, req.Workflow,
							webhookhttp.SignalName("pull_request"),
							webhookhttp.TriggerRef(webhookhttp.Delivery{Event: "pull_request", PullNumber: req.PR}), sweepTime, localscheduler.SignalTriggerOptions{
								RunID: req.DispatchRunID,
							})
					} else {
						runID, terr = sched.TriggerWithDispatchContextOptions(requestCtx, ctx, req.Workflow, sweepTime, localscheduler.ManualTriggerOptions{
							BypassCadenceBudgets: req.Force,
							RunID:                req.DispatchRunID,
						})
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
					// again. The next attempt receives its own validation and
					// dispatch budget; a busy daemon's queue wait must not
					// spend the provider call's deadline.
					// Requeued atomically (hidden temp + rename) so a
					// concurrent sweep/inspection can never observe a
					// truncated live request file mid-rewrite.
					if err := acknowledgeTriggerRequest(reqDir, requestID, &req, sweepTime); err != nil {
						sweepErr = errors.Join(sweepErr, err)
						_ = os.Remove(activePath)
						continue
					}
					req.DispatchRunID = ""
					if rerr := requeueTriggerRequest(reqPath, req); rerr != nil {
						sweepErr = errors.Join(sweepErr, fmt.Errorf("delegate: requeue trigger request %s: %w", requestID, rerr))
						resp.Error = terr.Error()
						break
					}
					_ = os.Remove(activePath)
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
			_ = os.Remove(activePath)
			continue
		}
		if err := writeTriggerResponse(reqDir, requestID, resp); err != nil {
			sweepErr = errors.Join(sweepErr, err)
		}
		_ = os.Remove(activePath)
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

// schedulerLivenessEvidence describes what the scheduler heartbeat says, for
// the one case a delegation can still time out (#2974).
//
// The heartbeat is refreshed by a completed scheduler tick, so it distinguishes
// exactly the two states the old message conflated: a daemon whose API is up
// but whose scheduler is not yet ticking — the startup window this issue was
// reported from — and no live daemon at all. Those call for opposite responses,
// and the difference is observable, so the operator should not have to guess
// which one they are in.
//
// Diagnostic only: it never changes the outcome, and an unreadable heartbeat
// says so plainly rather than asserting either state.
func schedulerLivenessEvidence(schedulerDir string) string {
	lastTick, err := daemonstate.Read(filepath.Join(schedulerDir, "up.lock"))
	if err != nil {
		return "The scheduler heartbeat could not be read (" + err.Error() +
			"), so whether a daemon is live is unknown from here."
	}
	age := delegationNow().UTC().Sub(lastTick)
	if age < 0 {
		age = 0
	}
	if age <= schedulerHeartbeatFreshFor {
		return fmt.Sprintf(
			"The scheduler last ticked %s ago, so a daemon is live but its trigger sweep did not reach this "+
				"request; retry once startup settles.", age.Truncate(time.Second))
	}
	return fmt.Sprintf(
		"The scheduler has not ticked for %s, so no live daemon is sweeping triggers; start or restart `goobers up`.",
		age.Truncate(time.Second))
}

// schedulerHeartbeatFreshFor bounds how old a scheduler tick may be while the
// daemon still counts as live for this diagnostic. Generous on purpose: this
// only chooses which sentence an operator reads.
const schedulerHeartbeatFreshFor = 2 * time.Minute
