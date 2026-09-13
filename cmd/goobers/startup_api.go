package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

func startStartupAPI(
	config *instance.Config,
	probes *daemonProbeState,
	tracker *startupPhaseTracker,
	addressPath string,
	stdout, stderr io.Writer,
) (*httpapi.SwitchHandler, *httpapi.Server, *log.Logger, error) {
	apiLog := log.New(stderr, "http API: ", log.LstdFlags)
	startingHandler := httpapi.WrapWithProbes(
		http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "daemon is starting", http.StatusServiceUnavailable)
		}),
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
