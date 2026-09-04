package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
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
	reqDir := filepath.Join(schedulerDir, pendingCancelsDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("cancel delegate: create request dir: %w", err)
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("cancel delegate: create request: %w", err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	req.CreatedAt = time.Now().UTC()
	data, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("cancel delegate: write request: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("cancel delegate: close request: %w", err)
	}
	requestID := strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	finalPath := filepath.Join(reqDir, requestID+cancelRequestSuffix)
	if err := durability.ReplaceFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("cancel delegate: publish request: %w", err)
	}
	return requestID, nil
}

// pollCancelResponse waits for the daemon's sweep to answer requestID, tolerant
// of torn reads, bounded by timeout.
func pollCancelResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (cancelResponse, error) {
	respPath := filepath.Join(schedulerDir, pendingCancelsDir, requestID+cancelResponseSuffix)
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(respPath); err == nil {
			var resp cancelResponse
			if err := json.Unmarshal(data, &resp); err == nil {
				_ = os.Remove(respPath)
				return resp, nil
			}
		}
		if time.Now().After(deadline) {
			return cancelResponse{}, fmt.Errorf(
				"cancel delegate: timed out after %s waiting for the live `goobers up` daemon to cancel run %s "+
					"(request left at %s — is the daemon still running and healthy?)",
				timeout, requestID, filepath.Join(schedulerDir, pendingCancelsDir, requestID+cancelRequestSuffix),
			)
		}
		select {
		case <-ctx.Done():
			return cancelResponse{}, ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
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
	reqDir := filepath.Join(schedulerDir, pendingCancelsDir)
	entries, err := os.ReadDir(reqDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cancel delegate: read pending requests: %w", err)
	}

	var sweepErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(reqDir, entry.Name())
		if strings.HasSuffix(entry.Name(), cancelResponseSuffix) {
			info, err := entry.Info()
			if err == nil && now().Sub(info.ModTime()) > cancelDelegationTimeout {
				_ = os.Remove(path)
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), cancelRequestSuffix) {
			continue
		}
		requestID := strings.TrimSuffix(entry.Name(), cancelRequestSuffix)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("cancel delegate: read request %s: %w", requestID, err))
			}
			continue
		}
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("cancel delegate: consume request %s: %w", requestID, err))
			}
			continue
		}

		var req cancelRequest
		resp := cancelResponse{}
		switch {
		case json.Unmarshal(data, &req) != nil:
			resp.Error = "cancel delegate: malformed request"
		case req.CreatedAt.IsZero():
			resp.Error = fmt.Sprintf("cancel delegate: request %s has no creation time; refusing to dispatch", requestID)
		case now().Sub(req.CreatedAt) > cancelDelegationTimeout:
			resp.Error = fmt.Sprintf("cancel delegate: stale request %s; refusing to dispatch", requestID)
		default:
			resp = executeCancelRequest(runners, release, req, now())
		}

		respData, err := json.Marshal(resp)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("cancel delegate: encode response %s: %w", requestID, err))
			continue
		}
		if err := journal.WriteFileAtomic(filepath.Join(reqDir, requestID+cancelResponseSuffix), respData, 0o644); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("cancel delegate: write response %s: %w", requestID, err))
		}
	}
	return sweepErr
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

// daemonCancelService is the run-control plane's daemon-side half (#3807): it
// runs the SAME executeCancelRequest the pending-cancels sweep runs, so a
// cancel that arrives over HTTP and one that arrives as a file drop resolve
// the owning Runner, stop the active stage, and free the scheduler slot
// identically. Only the way in differs — which is the point, since the file
// drop only reaches a daemon that shares the caller's filesystem.
type daemonCancelService struct {
	runners *daemonRunnerRegistry

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

func (s *daemonCancelService) Cancel(_ context.Context, input httpapi.CancelRunRequest) (httpapi.CancelRunResult, error) {
	s.mu.RLock()
	release := s.release
	s.mu.RUnlock()

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

// runRemoteCancel cancels a run on a daemon that does not share this
// filesystem (#3807). The local paths resolve the run's directory, journal
// identity, and daemon lock under an instance root the caller does not have
// here, so a remote cancel names the run by its full id and lets the daemon
// answer from its own registry: the disposition codes, and the exit codes they
// map to, are the file-drop path's unchanged.
func runRemoteCancel(endpoint, runID, action string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(endpoint) == "" {
		pf(stderr, "error: no daemon API endpoint configured\n")
		return 2
	}
	actor, err := defaultInterventionActor()
	if err != nil {
		actor = "cli"
	}
	var result httpapi.CancelRunResult
	apiErr, err := callDaemonMutationAPI(
		instance.NewLayout("."), endpoint, apicontract.RouteCancelRun,
		map[string]string{"{run}": runID}, httpapi.CancelRunRequest{Actor: actor}, &result,
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if apiErr != nil {
		pf(stderr, "error: %s: %s\n", apiErr.Code, apiErr.Message)
		return 1
	}
	switch {
	case result.Error != "":
		pf(stderr, "error: %s\n", result.Error)
		return 1
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
