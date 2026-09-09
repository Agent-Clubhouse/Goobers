package main

import (
	"errors"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
)

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
	return daemonAPIScheme(config) + "://" + address, nil
}

func tryLocalAPICancel(layout instance.Layout, runID, requestID string, noAPI bool, stdout, stderr io.Writer) (bool, int) {
	if noAPI {
		return false, 0
	}
	running, _, err := inspectDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: inspect daemon: %v\n", err)
		return true, 2
	}
	if !running {
		return false, 0
	}
	endpoint, err := localDaemonAPIBase(layout)
	if err != nil {
		pf(stderr, "error: resolve daemon API: %v; use --no-api only for explicit file delegation\n", err)
		return true, 2
	}
	return true, runRemoteCancelWithKey(endpoint, runID, "cancelled", requestID, stdout, stderr)
}
