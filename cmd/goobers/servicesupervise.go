package main

import (
	"context"
	"io"
	"os"

	"github.com/goobers/goobers/internal/daemonlog"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/service"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/winsvc"
)

func runServiceSupervise(args []string, stdout, stderr io.Writer) int {
	return runServiceSuperviseWith(args, stdout, stderr, serviceSuperviseDeps{
		runSupervisor:    selfupdate.RunSupervisor,
		isWindowsService: winsvc.IsWindowsService,
		runWindowsService: func(name string, run func(context.Context) int) (int, error) {
			return winsvc.Run(name, run)
		},
		setupSignalContext: signals.SetupSignalContext,
	})
}

type serviceSuperviseDeps struct {
	runSupervisor      func(context.Context, selfupdate.SupervisorOptions) error
	isWindowsService   func() (bool, error)
	runWindowsService  func(string, func(context.Context) int) (int, error)
	setupSignalContext func() (context.Context, func())
}

func runServiceSuperviseWith(args []string, stdout, stderr io.Writer, deps serviceSuperviseDeps) int {
	if len(args) > 1 {
		pf(stderr, "Usage: goobers __service-supervise [path]\n")
		return 2
	}
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", layout.ConfigFile())
		return 2
	}
	run := func(ctx context.Context, supervisorStdout, supervisorStderr io.Writer) int {
		err := deps.runSupervisor(ctx, selfupdate.SupervisorOptions{
			Root:      root,
			Escalator: selfUpdateEscalator{root: root},
			Stdout:    supervisorStdout,
			Stderr:    supervisorStderr,
		})
		if err != nil {
			pf(supervisorStderr, "error: supervise daemon: %v\n", err)
			return 1
		}
		return 0
	}
	isService, err := deps.isWindowsService()
	if err != nil {
		pf(stderr, "error: detect Windows service context: %v\n", err)
		return 1
	}
	if isService {
		// A Windows service's stdout/stderr are not an interactive console a
		// human is watching live, so redirecting the SCM-captured streams to
		// separate stdout/stderr files (as the previous behavior did) loses
		// their relative ordering. Give both the same MergedWriter over one
		// file instead (#4368): writes from the supervisor and its child
		// daemon interleave in the order they actually happened, each
		// timestamped, in one place an operator can tail.
		logFile, err := os.OpenFile(layout.DaemonLogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			pf(stderr, "error: open daemon log %s: %v\n", layout.DaemonLogFile(), err)
			return 1
		}
		merged := daemonlog.NewMergedWriter(logFile)
		defer func() {
			if closeErr := merged.Close(); closeErr != nil {
				pf(stderr, "error: close daemon log %s: %v\n", layout.DaemonLogFile(), closeErr)
			}
		}()
		code, err := deps.runWindowsService(service.Name, func(ctx context.Context) int {
			return run(ctx, merged, merged)
		})
		if err != nil {
			pf(stderr, "error: run Windows service: %v\n", err)
			return 1
		}
		return code
	}
	ctx, stop := deps.setupSignalContext()
	defer stop()
	return run(ctx, stdout, stderr)
}
