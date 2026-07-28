package main

import (
	"context"
	"flag"
	"io"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/service"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/winsvc"
)

const serviceSuperviseHelp = "Usage: goobers __service-supervise [path]\n\n" +
	"Internal service entrypoint. Launches the mutable instance binary and owns\n" +
	"graceful self-update handoff, health monitoring, rollback, and escalation.\n"

func runServiceSupervise(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("__service-supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { pf(stderr, "%s", serviceSuperviseHelp) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if _, err := os.Stat(instance.NewLayout(root).ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", instance.NewLayout(root).ConfigFile())
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		pf(stderr, "error: resolve supervisor executable: %v\n", err)
		return 1
	}
	run := func(ctx context.Context) int {
		err := selfupdate.RunSupervisor(ctx, selfupdate.SupervisorOptions{
			Root:           root,
			HostExecutable: executable,
			Escalator:      selfUpdateEscalator{root: root},
			Stdout:         stdout,
			Stderr:         stderr,
		})
		if err != nil {
			pf(stderr, "error: supervise daemon: %v\n", err)
			return 1
		}
		return 0
	}
	isService, err := winsvc.IsWindowsService()
	if err != nil {
		pf(stderr, "error: detect Windows service context: %v\n", err)
		return 1
	}
	if isService {
		code, err := winsvc.Run(service.Name, run)
		if err != nil {
			pf(stderr, "error: run Windows service: %v\n", err)
			return 1
		}
		return code
	}
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return run(ctx)
}
