package main

import (
	"context"
	"fmt"
	"time"
)

// applyrequest.go implements #459's `goobers apply`: a live, one-shot
// "reconcile now" trigger for a running `goobers up` daemon's workflow
// definitions, sourced from the instance's configured workflowSource,
// instead of waiting for the daemon's own poll/watch interval. Mirrors
// #831's cancel-request file protocol (runcancel.go) rather than inventing
// a third file-based request/response mechanism.

// pendingApplyDir is the SchedulerDir subdirectory apply request/response
// files live under.
const pendingApplyDir = "pending-applies"

const (
	applyRequestSuffix  = ".request.json"
	applyResponseSuffix = ".response.json"
)

// applyDelegationTimeout bounds both the daemon-side staleness check and the
// CLI's wait. A reconcile pass is a git fetch plus a config validate/reload —
// comfortably faster than a run cancellation, but generous enough to absorb a
// slow remote fetch. Var, not const, so tests aren't slow.
var applyDelegationTimeout = 60 * time.Second

func applyDelegateFileProtocol() delegateFileProtocol {
	return delegateFileProtocol{
		pendingDir:     pendingApplyDir,
		requestSuffix:  applyRequestSuffix,
		responseSuffix: applyResponseSuffix,
		errorPrefix:    "apply delegate",
		staleAfter:     applyDelegationTimeout,
	}
}

type applyRequest struct {
	CreatedAt time.Time `json:"createdAt"`
}

// applyResponse reports one reconcile attempt's outcome. Applied is true only
// when the daemon's live definitions actually changed to the newly pulled
// revision. Rejected carries a config-validation failure message (the
// daemon kept its last-known-good definitions, matching the same-process
// hand-edit reload's existing reject semantics); Error is a distinct
// operational failure (e.g. the git fetch itself failed) that isn't a
// judgment about the pulled config's validity.
type applyResponse struct {
	Applied   bool   `json:"applied"`
	OldDigest string `json:"oldDigest,omitempty"`
	NewDigest string `json:"newDigest,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Rejected  string `json:"rejected,omitempty"`
	Error     string `json:"error,omitempty"`
}

// writeApplyRequest publishes an apply request atomically (hidden temp then
// rename) so the daemon's sweep never reads a torn request, returning the
// request id that names its response file.
func writeApplyRequest(schedulerDir string) (string, error) {
	return writeDelegateRequest(schedulerDir, applyDelegateFileProtocol(), applyRequest{}, func(req *applyRequest) {
		req.CreatedAt = time.Now().UTC()
	})
}

// pollApplyResponse waits for the daemon's sweep to answer requestID,
// tolerant of torn reads, bounded by timeout.
func pollApplyResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (applyResponse, error) {
	return pollDelegateResponse[applyResponse](
		ctx, schedulerDir, requestID, applyDelegateFileProtocol(), timeout,
		func(requestPath string) string {
			return fmt.Sprintf(
				"apply delegate: timed out after %s waiting for the live `goobers up` daemon to reconcile "+
					"(request left at %s — is the daemon still running and healthy?)",
				timeout, requestPath,
			)
		},
	)
}

// applyReconciler performs one on-demand reconcile pass: syncing the tracked
// workflowSource (a no-op for a local-dir source, since there's nothing to
// pull) and then running exactly one config-reload check, exactly as if an
// operator had hand-edited a file and the reloader's own ticker had just
// fired.
type applyReconciler func(ctx context.Context, now time.Time) applyResponse

// sweepPendingApplyRequests is the daemon-side half: for each request it runs
// one reconcile pass and writes back the outcome. A request file is removed
// BEFORE dispatch so a crash mid-reconcile cannot replay it, mirroring the
// cancel sweep's crash-safety.
func sweepPendingApplyRequests(ctx context.Context, schedulerDir string, reconcile applyReconciler, now func() time.Time) error {
	return sweepDelegateRequests(
		schedulerDir,
		applyDelegateFileProtocol(),
		now,
		func(requestID string, req applyRequest, decodeErr error) (applyResponse, bool) {
			switch {
			case decodeErr != nil:
				return applyResponse{Error: "apply delegate: malformed request"}, false
			case req.CreatedAt.IsZero():
				return applyResponse{Error: fmt.Sprintf("apply delegate: request %s has no creation time; refusing to dispatch", requestID)}, false
			case now().Sub(req.CreatedAt) > applyDelegationTimeout:
				return applyResponse{Error: fmt.Sprintf("apply delegate: stale request %s; refusing to dispatch", requestID)}, false
			default:
				return applyResponse{}, true
			}
		},
		func(applyRequest) applyResponse {
			return reconcile(ctx, now())
		},
	)
}
