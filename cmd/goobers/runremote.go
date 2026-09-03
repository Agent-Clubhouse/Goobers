package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

// Remote trigger submission (#3279). `goobers run` asks a live daemon to
// dispatch a workflow by dropping a request file into
// <SchedulerDir>/pending-triggers/, which only reaches the daemon when the
// caller shares its filesystem — in a cluster, that means the daemon's own
// pod. Anywhere else the file lands where nothing sweeps it.
//
// The daemon already serves the same mutation over its authenticated HTTP API
// (apicontract.TriggerIngestPath, registered in internal/httpapi/writeplanes.go),
// so the missing half was purely client-side: a way to name a daemon that is
// not on this filesystem. --api (or $GOOBERS_DAEMON_API) selects the API path;
// with no endpoint configured the local lock/file-drop path is unchanged, which
// keeps pending-triggers/ the local fallback rather than the only way in.

// remoteDaemonAPIEnv names the daemon API base URL when --api is not given.
// It is the same variable a stage pod already receives
// (internal/dispatcher.EnvDaemonAPI), so a CI job, a webhook receiver, and a
// stage all point at the daemon the same way.
const remoteDaemonAPIEnv = "GOOBERS_DAEMON_API"

// remoteTriggerTimeout bounds one submission. The daemon answers a trigger
// after admitting it, not after the run finishes, so this is a transport
// bound rather than a run bound.
const remoteTriggerTimeout = 30 * time.Second

const maxRemoteTriggerResponseBody = 1 << 20

// remoteDaemonAPIBase resolves the configured daemon API base URL, preferring
// the flag over the environment. An empty return means "no remote endpoint
// configured" — the caller keeps its local behavior.
func remoteDaemonAPIBase(flagValue string) (string, error) {
	raw := strings.TrimSpace(flagValue)
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(remoteDaemonAPIEnv))
	}
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid daemon API base URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("daemon API base URL %q must use http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("daemon API base URL %q names no host", raw)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// runRemoteTrigger submits one trigger to a daemon reached over HTTP and
// reports what the daemon minted. Exit codes match the intervention commands:
// 0 = accepted, 1 = refused by the daemon, 2 = usage/transport error.
func runRemoteTrigger(
	ctx context.Context,
	endpoint string,
	target runTarget,
	requestID string,
	noWait bool,
	stdout, stderr io.Writer,
) int {
	if target.PR > 0 {
		pf(stderr, "error: --pr is not supported over the daemon API; run it from the daemon's own instance root\n")
		return 2
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		generated, err := newRemoteTriggerRequestID()
		if err != nil {
			pf(stderr, "error: generate trigger request id: %v\n", err)
			return 2
		}
		requestID = generated
	}
	if len(requestID) > httpapi.MaxTriggerRequestIDBytes {
		pf(stderr, "error: --request-id must be no longer than %d bytes\n", httpapi.MaxTriggerRequestIDBytes)
		return 2
	}

	response, apiErr, err := submitRemoteTrigger(ctx, endpoint, httpapi.TriggerRequest{
		Gaggle:    target.Gaggle,
		Workflow:  target.Workflow,
		RequestID: requestID,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if apiErr != nil {
		pf(stderr, "error: %s: %s\n", apiErr.Code, apiErr.Message)
		return 1
	}

	switch {
	case response.Duplicate && response.RunID == "":
		pf(stdout, "trigger request %s was already accepted (workflow=%s, dispatched via daemon API); its run is still being minted\n",
			requestID, target.Workflow)
	case response.Duplicate:
		pf(stdout, "trigger request %s already dispatched run %s (workflow=%s, dispatched via daemon API)\n",
			requestID, response.RunID, target.Workflow)
	case response.RunID == "":
		pf(stdout, "accepted trigger request %s (workflow=%s, dispatched via daemon API)\n", requestID, target.Workflow)
	default:
		pf(stdout, "created run %s (workflow=%s, dispatched via daemon API)\n", response.RunID, target.Workflow)
	}
	if !noWait {
		// The waiting path reads the run's journal on the daemon's own
		// filesystem, which is exactly what a remote caller does not have.
		// Saying so is the point: the failure this command used to have was a
		// silent one.
		pf(stderr, "note: a remote trigger returns once the daemon accepts it; this client cannot watch the run's journal\n")
	}
	return 0
}

// submitRemoteTrigger POSTs one trigger to the daemon's trigger plane. It
// returns exactly one of a decoded response, a daemon error envelope, or a
// transport error.
func submitRemoteTrigger(
	ctx context.Context,
	endpoint string,
	input httpapi.TriggerRequest,
) (httpapi.TriggerResponse, *apicontract.APIError, error) {
	route, ok := apicontract.V1Route(apicontract.RouteTriggerIngest)
	if !ok {
		return httpapi.TriggerResponse{}, nil, fmt.Errorf("API route %q is not registered", apicontract.RouteTriggerIngest)
	}
	body, err := json.Marshal(input)
	if err != nil {
		return httpapi.TriggerResponse{}, nil, fmt.Errorf("encode trigger request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, route.Method, endpoint+route.Path, bytes.NewReader(body))
	if err != nil {
		return httpapi.TriggerResponse{}, nil, fmt.Errorf("build trigger request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: remoteTriggerTimeout}
	response, err := client.Do(request)
	if err != nil {
		return httpapi.TriggerResponse{}, nil, fmt.Errorf("call daemon API %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxRemoteTriggerResponseBody))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope apicontract.ErrorEnvelope
		if err := decoder.Decode(&envelope); err != nil {
			return httpapi.TriggerResponse{}, nil, fmt.Errorf("daemon API returned %s with an invalid error body: %w", response.Status, err)
		}
		return httpapi.TriggerResponse{}, &envelope.Error, nil
	}
	var decoded httpapi.TriggerResponse
	if err := decoder.Decode(&decoded); err != nil {
		return httpapi.TriggerResponse{}, nil, fmt.Errorf("decode daemon trigger response: %w", err)
	}
	return decoded, nil, nil
}

// newRemoteTriggerRequestID mints the delivery identity the daemon dedupes on.
// A fresh invocation means a fresh run, so this is random by default; a caller
// that retries an ambiguous submission passes its own --request-id instead and
// gets the original run back rather than a second one.
func newRemoteTriggerRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "cli-" + fmt.Sprintf("%x", id[:]), nil
}
