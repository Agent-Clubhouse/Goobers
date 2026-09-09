package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

func runLocalTriggerSubmission(ctx context.Context, layout instance.Layout, target runTarget, root, requestID string, noWait, noAPI bool, timeout time.Duration, stdout, stderr io.Writer) int {
	if noAPI {
		if requestID != "" {
			pf(stderr, "error: --request-id cannot be used with --no-api file delegation\n")
			return 2
		}
		return runDelegatedTrigger(ctx, layout, target, root, noWait, stdout, stderr)
	}
	running, _, err := inspectDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil || !running {
		pf(stderr, "error: instance is locked but no live daemon API was identified: %v; retry after the current owner releases the lock\n", err)
		return 2
	}
	endpoint, err := localDaemonAPIBase(layout)
	if err != nil {
		pf(stderr, "error: resolve daemon API: %v; use --no-api for explicit file delegation\n", err)
		return 2
	}
	if timeout <= 0 || target.PR > 0 {
		pf(stderr, "error: local API submission requires a positive --api-timeout; --pr currently requires explicit --no-api delegation\n")
		return 2
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID, err = newRemoteTriggerRequestID()
	}
	if err != nil || len(requestID) > httpapi.MaxTriggerRequestIDBytes {
		pf(stderr, "error: invalid trigger request identity (maximum %d bytes): %v\n", httpapi.MaxTriggerRequestIDBytes, err)
		return 2
	}
	admission, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := prepareLocalDaemonRoot(admission, layout, endpoint, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	response, apiErr, err := submitRemoteTrigger(admission, endpoint, httpapi.TriggerRequest{Workflow: target.Workflow, Gaggle: target.Gaggle, Force: target.Force, RequestID: requestID})
	if err != nil {
		pf(stderr, "error: trigger acceptance is unknown: %v; retry with --request-id %q and the same workflow/options\n", err, requestID)
		return 2
	}
	if apiErr != nil {
		pf(stderr, "error: %s: %s (request ID %q)\n", apiErr.Code, apiErr.Message, requestID)
		return 1
	}
	if response.AcceptanceID == "" {
		pf(stderr, "error: daemon returned no durable acceptance identity; outcome is unknown, retry with --request-id %q\n", requestID)
		return 2
	}
	pf(stdout, "accepted trigger %s (request=%s, workflow=%s, state=%s)\n", response.AcceptanceID, requestID, target.Workflow, response.State)
	if noWait {
		return 0
	}
	return waitLocalAPITrigger(ctx, layout, endpoint, root, response, stdout, stderr)
}

func waitLocalAPITrigger(ctx context.Context, layout instance.Layout, endpoint, root string, response httpapi.TriggerResponse, stdout, stderr io.Writer) int {
	runID := response.RunID
	if response.AcceptanceID != "" {
		status, err := waitAcceptedTriggerDispatch(ctx, endpoint, response.AcceptanceID)
		if err != nil {
			pf(stderr, "error: trigger %s remains accepted; could not observe dispatch: %v\n", response.AcceptanceID, err)
			return 2
		}
		if status.State == "rejected" {
			pf(stderr, "trigger %s was accepted but dispatch was rejected: %s\n", response.AcceptanceID, status.Reason)
			return 1
		}
		runID = status.RunID
	}
	if runID == "" {
		pf(stderr, "error: trigger was accepted but no observable dispatch identity was returned\n")
		return 2
	}
	pf(stdout, "created run %s (dispatched via live daemon API)\n", runID)
	phase, err := waitForRunTerminalInLayoutWithProgress(ctx, layout, runID, stderr)
	if err != nil {
		pf(stderr, "error: run %s was dispatched; monitoring stopped: %v\n", runID, err)
		return 2
	}
	pf(stdout, "finished: phase=%s\ninspect with: goobers trace %s %s\n", phase, runID, root)
	return exitForPhase(phase)
}

func waitAcceptedTriggerDispatch(ctx context.Context, endpoint, id string) (httpapi.TriggerStatusResponse, error) {
	for {
		status, err := readAcceptedTriggerStatus(ctx, endpoint, id)
		if err != nil {
			return status, err
		}
		switch status.State {
		case "dispatched":
			if status.RunID == "" {
				return status, errors.New("dispatched trigger has no run identity")
			}
			return status, nil
		case "rejected":
			return status, nil
		case "accepted", "dispatching":
		default:
			return status, fmt.Errorf("unknown trigger state %q", status.State)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(runPollInterval):
		}
	}
}

func readAcceptedTriggerStatus(ctx context.Context, endpoint, id string) (httpapi.TriggerStatusResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteTriggerTimeout)
	defer cancel()
	route, _ := apicontract.V1Route(apicontract.RouteTriggerStatus)
	path := strings.ReplaceAll(route.Path, "{acceptance}", url.PathEscape(id))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
	if err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return httpapi.TriggerStatusResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return httpapi.TriggerStatusResponse{}, fmt.Errorf("trigger status returned HTTP %d", response.StatusCode)
	}
	var status httpapi.TriggerStatusResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxRemoteTriggerResponseBody)).Decode(&status); err != nil {
		return status, err
	}
	if status.AcceptanceID != id {
		return httpapi.TriggerStatusResponse{}, errors.New("trigger status returned another acceptance identity")
	}
	return status, nil
}
