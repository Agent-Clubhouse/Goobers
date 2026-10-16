package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/startuphint"
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
		daemonStartingHandler(tracker),
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

// noteProgress records that a startup phase reported progress (for example
// read-model build or re-projection counts), so the starting daemon's 503
// tells a waiting worker it is still advancing (#6895).
func (t *startupPhaseTracker) noteProgress() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.progress++
}

// startupHints is what a starting daemon advertises about itself: the time
// left in its derived startup budget and its progress token.
func (t *startupPhaseTracker) startupHints(now time.Time) startuphint.Hints {
	budget := t.budgetSnapshot(now)
	t.mu.Lock()
	progress := t.progress
	t.mu.Unlock()
	hints := startuphint.Hints{Progress: strconv.FormatUint(progress, 10), Daemon: t.daemonID}
	if budget.Budget > 0 {
		hints.HasBudget = true
		hints.BudgetRemaining = budget.Budget - budget.Elapsed
	}
	return hints
}

// setStartupHints writes the daemon's current startup hints onto h. It
// decorates both the pre-API "daemon is starting" 503 and the recovery
// gate's 503, since the longest startup phases run behind the latter.
func (t *startupPhaseTracker) setStartupHints(h http.Header) {
	if t != nil {
		startuphint.Set(h, t.startupHints(time.Now()))
	}
}

// trackStartupProgress counts each startup progress report on tracker before
// passing it on.
func trackStartupProgress(tracker *startupPhaseTracker, report func(string)) func(string) {
	return func(message string) {
		tracker.noteProgress()
		report(message)
	}
}

// daemonStartingHandler is serveDaemonStarting plus the daemon's startup
// hints, from which a waiting worker derives how long to wait.
func daemonStartingHandler(tracker *startupPhaseTracker) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		tracker.setStartupHints(response.Header())
		serveDaemonStarting(response, request)
	}
}
