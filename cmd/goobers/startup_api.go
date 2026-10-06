package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

// daemonStartingMessage is the body of every reply served before startup
// installs the full API. The CLI recognizes it as "not ready yet" (#5897), so
// both sides use this constant; older daemons send the same text.
const daemonStartingMessage = "daemon is starting"

// serveDaemonStarting answers a request that arrives before the full API is
// installed: HTTP 503 with a Retry-After hint, because the condition clears on
// its own once startup finishes.
func serveDaemonStarting(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set(httpapi.HeaderRetryAfterSeconds, strconv.Itoa(httpapi.NotReadyRetryAfterSeconds))
	http.Error(response, daemonStartingMessage, http.StatusServiceUnavailable)
}

func startStartupAPI(
	config *instance.Config,
	probes *daemonProbeState,
	tracker *startupPhaseTracker,
	addressPath string,
	stdout, stderr io.Writer,
) (*httpapi.SwitchHandler, *httpapi.Server, *log.Logger, error) {
	apiLog := log.New(stderr, "http API: ", log.LstdFlags)
	startingHandler := httpapi.WrapWithProbes(
		http.HandlerFunc(serveDaemonStarting),
		probes.liveness,
		probes.readiness,
	)
	handler, err := httpapi.NewSwitchHandler(
		startingHandler,
		config.API.Auth != nil || !instance.IsLoopbackListenAddress(apiListenAddress(config)),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("initialize startup HTTP API: %w", err)
	}
	var options []httpapi.ServerOption
	if tlsConfig := config.API.TLS; tlsConfig != nil {
		options = append(options, httpapi.WithTLS(tlsConfig.CertFile, tlsConfig.KeyFile))
	}
	server, err := httpapi.NewServer(apiListenAddress(config), handler, apiLog, options...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("initialize HTTP API: %w", err)
	}
	if err := runStartupPhase(stdout, tracker, "api-bind", apiListenAddress(config), server.Start); err != nil {
		return nil, nil, nil, fmt.Errorf("start HTTP API: %w", err)
	}
	if err := publishDaemonAPIAddress(addressPath, server.Address()); err != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		return nil, nil, nil, err
	}
	return handler, server, apiLog, nil
}
