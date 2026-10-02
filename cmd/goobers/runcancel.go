package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/cancelreceipt"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// runcancel.go implements #831's live `goobers run cancel <id>`. Unlike
// `run abort` — which appends a terminal event straight to a run's journal and
// is the daemon-DOWN repair path — a cancel targets a run a live `goobers up`
// daemon is actively executing. Only that daemon process holds the in-memory
// handle (Runner.CancelRun) that can stop the active stage, so the short-lived
// `run cancel` process hands the request off through the same file-based
// request/response protocol #343/#384 established for trigger and claim
// delegation (see rundelegate.go / claims.go), and the daemon's periodic sweep
// (wired in up.go) resolves the owning Runner and cancels the run. When no
// daemon is running there is nothing in flight to cancel, so cancel refuses and
// points the operator at `run abort` rather than editing a journal.

// pendingCancelsDir is the SchedulerDir subdirectory cancel request/response
// files live under.
const pendingCancelsDir = "pending-cancels"

const (
	cancelRequestSuffix  = ".request.json"
	cancelResponseSuffix = ".response.json"
)

// Cancel response codes let the CLI map a daemon outcome to a stable exit code
// and message without string-matching.
const (
	cancelCodeAborted    = "aborted"          // the run was cancelled and finalized aborted
	cancelCodeTerminal   = "already_terminal" // the run finished on its own before the cancel landed
	cancelCodeNotRunning = "not_running"      // no live owner: this daemon is not executing the run
)

// cancelDelegationTimeout bounds both the daemon-side staleness check and the
// CLI's wait. It comfortably exceeds the runner's worst-case cancellation grace
// plus terminalization grace (StalledCancellationGrace + Stalled-
// TerminalizationGrace) so a run whose stage ignores cancellation still resolves
// through the watchdog-style takeover before the client gives up. Var, not
// const, so tests aren't slow.
var cancelDelegationTimeout = 60 * time.Second

func cancelDelegateFileProtocol() delegateFileProtocol {
	return delegateFileProtocol{
		pendingDir:     pendingCancelsDir,
		requestSuffix:  cancelRequestSuffix,
		responseSuffix: cancelResponseSuffix,
		errorPrefix:    "cancel delegate",
		staleAfter:     cancelDelegationTimeout,
	}
}

type cancelRequest struct {
	RunID     string    `json:"runId"`
	Workflow  string    `json:"workflow,omitempty"`
	Gaggle    string    `json:"gaggle,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// cancelResponse mirrors CancelRun's outcome: Code names the disposition, Phase
// is the terminal phase reached (aborted on success), and exactly one of a
// success Code or Error is meaningful.
type cancelResponse struct {
	Phase string `json:"phase,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// writeCancelRequest publishes a cancel request atomically (hidden temp then
// rename) so the daemon's sweep never reads a torn request, returning the
// request id that names its response file.
func writeCancelRequest(schedulerDir string, req cancelRequest) (string, error) {
	return writeDelegateRequest(schedulerDir, cancelDelegateFileProtocol(), req, func(req *cancelRequest) {
		req.CreatedAt = time.Now().UTC()
	})
}

// pollCancelResponse waits for the daemon's sweep to answer requestID, tolerant
// of torn reads, bounded by timeout.
func pollCancelResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (cancelResponse, error) {
	return pollDelegateResponse[cancelResponse](
		ctx, schedulerDir, requestID, cancelDelegateFileProtocol(), timeout,
		func(requestPath string) string {
			return fmt.Sprintf(
				"cancel delegate: timed out after %s waiting for the live `goobers up` daemon to cancel run %s "+
					"(request left at %s — is the daemon still running and healthy?)",
				timeout, requestID, requestPath,
			)
		},
	)
}

// sweepPendingCancelRequests is the daemon-side half of #831's cancel protocol,
// called at startup and periodically from up.go. For each request it resolves
// the Runner that owns the target run and calls CancelRun; a run this daemon is
// not actively executing answers not_running (its journal must not be edited
// behind a would-be owner's back — that is `run abort`'s job when the daemon is
// down). A request file is removed BEFORE dispatch so a crash mid-cancel cannot
// replay it, mirroring the trigger sweep's crash-safety.
func sweepPendingCancelRequests(
	schedulerDir string,
	runners *daemonRunnerRegistry,
	log *journal.InstanceLog,
	release func(runID, workflow string),
	now func() time.Time,
) error {
	return sweepDelegateRequests(
		schedulerDir,
		cancelDelegateFileProtocol(),
		now,
		func(requestID string, req cancelRequest, decodeErr error) (cancelResponse, bool) {
			switch {
			case decodeErr != nil:
				return cancelResponse{Error: "cancel delegate: malformed request"}, false
			case req.CreatedAt.IsZero():
				return cancelResponse{Error: fmt.Sprintf("cancel delegate: request %s has no creation time; refusing to dispatch", requestID)}, false
			case now().Sub(req.CreatedAt) > cancelDelegationTimeout:
				return cancelResponse{Error: fmt.Sprintf("cancel delegate: stale request %s; refusing to dispatch", requestID)}, false
			default:
				return cancelResponse{}, true
			}
		},
		func(req cancelRequest) cancelResponse {
			return executeCancelRequest(runners, release, req, now())
		},
	)
}

// executeCancelRequest resolves the owning Runner and cancels the run. It frees
// the scheduler's in-memory concurrency slot (release) only after CancelRun
// returns, i.e. after the run's terminal run.finished is durable and its claim
// released — so a replacement run for the same backlog item can never be
// admitted mid-cancel.
func executeCancelRequest(
	runners *daemonRunnerRegistry,
	release func(runID, workflow string),
	req cancelRequest,
	now time.Time,
) cancelResponse {
	owner, liveOwner := runners.Resolve(req.RunID, req.Gaggle, nil)
	if !liveOwner || owner == nil {
		return cancelResponse{
			Code:  cancelCodeNotRunning,
			Error: fmt.Sprintf("run %s is not currently running under this daemon", req.RunID),
		}
	}
	result, cancelled, err := owner.CancelRun(req.RunID, now)
	switch {
	case err != nil:
		return cancelResponse{Error: err.Error()}
	case cancelled:
		if release != nil {
			release(req.RunID, req.Workflow)
		}
		return cancelResponse{Code: cancelCodeAborted, Phase: string(result.Phase)}
	case result.Phase != "" && result.Phase != journal.PhaseRunning:
		return cancelResponse{Code: cancelCodeTerminal, Phase: string(result.Phase)}
	default:
		// Owner disappeared between Resolve and CancelRun (finished on its own).
		return cancelResponse{
			Code:  cancelCodeNotRunning,
			Error: fmt.Sprintf("run %s is no longer running under this daemon", req.RunID),
		}
	}
}

// daemonCancelService preserves the local Runner/file-drop cancellation path
// and routes retained engine runs through the engine's own cancellation guard.
type daemonCancelService struct {
	runners  *daemonRunnerRegistry
	engine   *daemonEngineCancelService
	auditLog *journal.InstanceLog
	receipts *cancelreceipt.Store

	mu      sync.RWMutex
	release func(runID, workflow string)
}

func newDaemonCancelService(runners *daemonRunnerRegistry) *daemonCancelService {
	return &daemonCancelService{runners: runners}
}

// AttachRelease supplies the scheduler's slot release once the scheduler
// exists. The API handler is built before it, exactly as the HITL deliverer is
// attached after the fact.
func (s *daemonCancelService) AttachRelease(release func(runID, workflow string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release = release
}

func (s *daemonCancelService) Cancel(ctx context.Context, input httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
	if s.receipts != nil {
		return s.cancelWithReceipt(ctx, input)
	}
	return s.cancelOnce(ctx, input)
}

func (s *daemonCancelService) cancelOnce(ctx context.Context, input httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
	if err := s.auditCancellation(input); err != nil {
		return httpapi.CancelRunResult{}, err
	}
	s.mu.RLock()
	release := s.release
	s.mu.RUnlock()

	if _, local := s.runners.Resolve(input.RunID, input.Gaggle, nil); !local && s.engine != nil {
		if result, handled, err := s.engine.cancel(ctx, input); handled {
			return result, err
		}
	}

	workflow := strings.TrimSpace(input.Workflow)
	if workflow == "" {
		// A remote caller has no journal to read the run's workflow from, so
		// the daemon supplies it from its own registry; without it the
		// scheduler's concurrency slot would leak.
		for _, run := range s.runners.ActiveRuns() {
			if run.RunID == input.RunID {
				workflow = run.Workflow
				break
			}
		}
	}
	resp := executeCancelRequest(s.runners, release, cancelRequest{
		RunID:    input.RunID,
		Workflow: workflow,
		Gaggle:   input.Gaggle,
		Actor:    input.Actor,
	}, time.Now())
	return httpapi.CancelRunResult{Phase: resp.Phase, Code: resp.Code, Error: resp.Error}, nil
}

// Record the attempt before either runner or engine side effects. This is an
// attribution record, not a completion receipt: a failed or interrupted cancel
// must never look like confirmed termination in the journal.
func (s *daemonCancelService) auditCancellation(input httpapi.CancelRunRequest) error {
	if s.auditLog == nil {
		return nil // Low-level service tests may omit the production audit sink.
	}
	return s.auditLog.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: input.RunID, Workflow: input.Workflow, Gaggle: input.Gaggle,
		Actor: input.Actor, Reason: "run cancellation requested",
		Runner: map[string]any{"note": "run.cancel.requested", "idempotencyKey": input.IdempotencyKey},
	})
}

// runRemoteCancel cancels a run on a daemon that does not share this
// filesystem (#3807). The local paths resolve the run's directory, journal
// identity, and daemon lock under an instance root the caller does not have
// here, so a remote cancel names the run by its full id and lets the daemon
// resolve the owning runner or retained engine identity. Existing local
// dispositions keep their exit codes; accepted engine cancellation reports a
// request without claiming a terminal outcome.
func runRemoteCancel(endpoint, runID, action string, stdout, stderr io.Writer) int {
	return runRemoteCancelWithKey(endpoint, runID, action, "", stdout, stderr)
}

func runRemoteCancelWithKey(endpoint, runID, action, key string, stdout, stderr io.Writer) int {
	return runRemoteCancelForInstance(endpoint, runID, action, key, "", stdout, stderr)
}

func runRemoteCancelForInstance(endpoint, runID, action, key, expectedID string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(endpoint) == "" {
		pf(stderr, "error: no daemon API endpoint configured\n")
		return 2
	}
	if err := prepareRemoteRootForInstance(context.Background(), endpoint, expectedID, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	actor, err := defaultInterventionActor()
	if err != nil {
		actor = "cli"
	}
	if key == "" {
		key, err = newInterventionIdempotencyKey()
		if err != nil {
			pf(stderr, "error: generate cancellation request ID: %v\n", err)
			return 2
		}
	}
	call := func(ctx context.Context) (httpapi.CancelRunResult, *apicontract.APIError, error) {
		return callDaemonMutationAPIWithKeyContext[httpapi.CancelRunRequest, httpapi.CancelRunResult](
			ctx, instance.NewLayout("."), endpoint, apicontract.RouteCancelRun,
			map[string]string{"{run}": runID}, httpapi.CancelRunRequest{Actor: actor}, key,
		)
	}
	result, apiErr, err := call(context.Background())
	if cancelAnswerMayBeLost(err) || cancelInFlight(apiErr) {
		// #5118: the request may have landed even though its answer did not,
		// or an earlier delivery of this key is still in flight. Re-ask under
		// the same key before declaring the outcome unknown.
		if apiErr != nil {
			err = fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
		}
		pf(stderr, "warning: %v; confirming cancellation outcome with request ID %q\n", err, key)
		result, apiErr, err = reconcileRemoteCancel(call, err)
	}
	if err != nil {
		pf(stderr, "error: %v; cancellation outcome may be unknown; retry run cancel with --request-id=%q and the same target\n", err, key)
		return 2
	}
	if apiErr != nil {
		pf(stderr, "error: %s: %s\n", apiErr.Code, apiErr.Message)
		pf(stderr, "cancellation request ID: %q (reuse --request-id with the same target to reconcile)\n", key)
		return 1
	}
	switch {
	case result.Error != "":
		pf(stderr, "error: %s\n", result.Error)
		return 1
	case result.Code == httpapi.CancelCodeRequested:
		pf(stdout, "requested cancellation of engine-driven run %s via daemon API; the engine reports the terminal outcome\n", runID)
		return 0
	case result.Code == httpapi.CancelCodeAborted:
		pf(stdout, "%s run %s (aborted via daemon API)\n", action, runID)
		return 0
	case result.Code == httpapi.CancelCodeTerminal:
		pf(stderr, "error: run %s finished before it could be cancelled (phase=%s)\n", runID, result.Phase)
		return 1
	case result.Code == httpapi.CancelCodeNotRunning:
		pf(stderr, "error: run %s is not currently running under that daemon\n", runID)
		return 1
	default:
		pf(stderr, "error: unexpected cancel response for run %s\n", runID)
		return 1
	}
}

// cancelOutcomeUnknownCode is the daemon's answer to a replayed cancellation
// key whose first delivery has not finished: the cancel is still in flight.
// cancelReceiptUnavailableCode means the daemon could not record the outcome
// it reached and asks for a replay under the same key.
const (
	cancelOutcomeUnknownCode     = "cancel_outcome_unknown"
	cancelReceiptUnavailableCode = "cancel_receipt_unavailable"
)

// cancelReconcileWindow bounds how long `run cancel` keeps re-asking the daemon
// under the original request ID after a lost or late response, and
// cancelReconcileInterval spaces those asks. Vars, not consts, so tests aren't
// slow.
var (
	cancelReconcileWindow   = 30 * time.Second
	cancelReconcileInterval = 2 * time.Second
)

// reconcileRemoteCancel (#5118) re-issues a cancellation whose response was
// lost (client timeout, connection dropped after sending) under the same
// idempotency key. A daemon with a cancellation receipt store (every `goobers
// up` daemon) answers a completed key with the original outcome and an
// in-flight key with cancel_outcome_unknown, so the replay does not cancel
// twice; it only learns what the first request did. It returns the
// first definite answer, or lastErr once the window closes without one.
func reconcileRemoteCancel(
	call func(context.Context) (httpapi.CancelRunResult, *apicontract.APIError, error),
	lastErr error,
) (httpapi.CancelRunResult, *apicontract.APIError, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelReconcileWindow)
	defer cancel()
	ticker := time.NewTicker(cancelReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return httpapi.CancelRunResult{}, nil, lastErr
		case <-ticker.C:
		}
		result, apiErr, err := call(ctx)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				lastErr = err
			}
		case cancelInFlight(apiErr):
			lastErr = fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
		default:
			return result, apiErr, nil
		}
	}
}

// cancelInFlight reports whether the daemon asked for the same key to be
// replayed because the cancellation's outcome is not yet recorded.
func cancelInFlight(apiErr *apicontract.APIError) bool {
	return apiErr != nil && (apiErr.Code == cancelOutcomeUnknownCode || apiErr.Code == cancelReceiptUnavailableCode)
}

// cancelAnswerMayBeLost reports whether err means the cancel request may have
// reached the daemon but its answer never came back: a client timeout or a
// connection dropped after sending. A failed dial never sent the request, and
// a received-but-malformed answer is not improved by asking again.
func cancelAnswerMayBeLost(err error) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	var opErr *net.OpError
	return !errors.As(err, &opErr) || opErr.Op != "dial"
}
