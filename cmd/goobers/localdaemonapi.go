package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/instance"
)

var errWildcardTLSDaemonAPI = errors.New("daemon TLS API published only a wildcard bind address")

func prepareLocalDaemonRoot(ctx context.Context, layout instance.Layout, endpoint string, diagnostic io.Writer) error {
	id, err := instance.ReadRootIdentity(layout.Root)
	if err != nil {
		return err
	}
	return prepareRemoteRootForInstance(ctx, endpoint, id, diagnostic)
}

func isNoAPIFlag(arg string) bool {
	return arg == "--no-api" || arg == "-no-api" || strings.HasPrefix(arg, "--no-api=") || strings.HasPrefix(arg, "-no-api=")
}

func requestedDaemonAPI(explicit string, noAPI bool) (string, error) {
	if noAPI {
		if explicit != "" {
			return "", errors.New("--api and --no-api cannot be combined")
		}
		return "", nil
	}
	return remoteDaemonAPIBase(explicit)
}

func localDaemonAPIBase(layout instance.Layout) (string, error) {
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return "", err
	}
	address, err := dashboardDaemonAPIAddress(layout, apiListenAddress(config))
	if err != nil {
		return "", err
	}
	scheme := daemonAPIScheme(config)
	if scheme == "https" && wildcardDaemonAPIAddress(address) {
		return "", fmt.Errorf("%w %q", errWildcardTLSDaemonAPI, address)
	}
	return scheme + "://" + address, nil
}

func wildcardDaemonAPIAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func tryLocalAPICancel(layout instance.Layout, runID, requestID string, noAPI bool, stdout, stderr io.Writer) (handled, fileFallback bool, code int) {
	if noAPI {
		return false, false, 0
	}
	running, _, err := inspectDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: inspect daemon: %v\n", err)
		return true, false, 2
	}
	if !running {
		return false, false, 0
	}
	endpoint, err := localDaemonAPIBase(layout)
	if err != nil {
		if errors.Is(err, errWildcardTLSDaemonAPI) && requestID == "" {
			pf(stderr, "note: %v; using same-root file delegation\n", err)
			return false, true, 0
		}
		pf(stderr, "error: resolve daemon API: %v; use --no-api only for explicit file delegation\n", err)
		return true, false, 2
	}
	id, err := instance.ReadRootIdentity(layout.Root)
	if err != nil {
		pf(stderr, "error: read local instance identity: %v\n", err)
		return true, false, 2
	}
	return true, false, runRemoteCancelForInstance(endpoint, runID, "cancelled", requestID, id, stdout, stderr)
}
