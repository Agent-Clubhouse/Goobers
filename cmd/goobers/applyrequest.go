package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
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
	reqDir := filepath.Join(schedulerDir, pendingApplyDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("apply delegate: create request dir: %w", err)
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("apply delegate: create request: %w", err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	req := applyRequest{CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("apply delegate: write request: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("apply delegate: close request: %w", err)
	}
	requestID := strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	finalPath := filepath.Join(reqDir, requestID+applyRequestSuffix)
	if err := durability.ReplaceFile(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("apply delegate: publish request: %w", err)
	}
	return requestID, nil
}

// pollApplyResponse waits for the daemon's sweep to answer requestID,
// tolerant of torn reads, bounded by timeout.
func pollApplyResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (applyResponse, error) {
	respPath := filepath.Join(schedulerDir, pendingApplyDir, requestID+applyResponseSuffix)
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(respPath); err == nil {
			var resp applyResponse
			if err := json.Unmarshal(data, &resp); err == nil {
				_ = os.Remove(respPath)
				return resp, nil
			}
		}
		if time.Now().After(deadline) {
			return applyResponse{}, fmt.Errorf(
				"apply delegate: timed out after %s waiting for the live `goobers up` daemon to reconcile "+
					"(request left at %s — is the daemon still running and healthy?)",
				timeout, filepath.Join(schedulerDir, pendingApplyDir, requestID+applyRequestSuffix),
			)
		}
		select {
		case <-ctx.Done():
			return applyResponse{}, ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
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
	reqDir := filepath.Join(schedulerDir, pendingApplyDir)
	entries, err := os.ReadDir(reqDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("apply delegate: read pending requests: %w", err)
	}

	var sweepErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(reqDir, entry.Name())
		if strings.HasSuffix(entry.Name(), applyResponseSuffix) {
			info, err := entry.Info()
			if err == nil && now().Sub(info.ModTime()) > applyDelegationTimeout {
				_ = os.Remove(path)
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), applyRequestSuffix) {
			continue
		}
		requestID := strings.TrimSuffix(entry.Name(), applyRequestSuffix)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("apply delegate: read request %s: %w", requestID, err))
			}
			continue
		}
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("apply delegate: consume request %s: %w", requestID, err))
			}
			continue
		}

		var req applyRequest
		var resp applyResponse
		switch {
		case json.Unmarshal(data, &req) != nil:
			resp.Error = "apply delegate: malformed request"
		case req.CreatedAt.IsZero():
			resp.Error = fmt.Sprintf("apply delegate: request %s has no creation time; refusing to dispatch", requestID)
		case now().Sub(req.CreatedAt) > applyDelegationTimeout:
			resp.Error = fmt.Sprintf("apply delegate: stale request %s; refusing to dispatch", requestID)
		default:
			resp = reconcile(ctx, now())
		}

		respData, err := json.Marshal(resp)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("apply delegate: encode response %s: %w", requestID, err))
			continue
		}
		if err := journal.WriteFileAtomic(filepath.Join(reqDir, requestID+applyResponseSuffix), respData, 0o644); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("apply delegate: write response %s: %w", requestID, err))
		}
	}
	return sweepErr
}
