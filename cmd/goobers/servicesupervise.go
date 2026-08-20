package main

import (
	"context"
	"io"
	"os"

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
	if _, err := os.Stat(instance.NewLayout(root).ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", instance.NewLayout(root).ConfigFile())
		return 2
	}
	run := func(ctx context.Context) int {
		err := deps.runSupervisor(ctx, selfupdate.SupervisorOptions{
			Root:      root,
			Escalator: selfUpdateEscalator{root: root},
			Stdout:    stdout,
			Stderr:    stderr,
		})
		if err != nil {
			pf(stderr, "error: supervise daemon: %v\n", err)
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
		code, err := deps.runWindowsService(service.Name, run)
		if err != nil {
			pf(stderr, "error: run Windows service: %v\n", err)
			return 1
		}
		return code
	}
	ctx, stop := deps.setupSignalContext()
	defer stop()
	return run(ctx)
}
