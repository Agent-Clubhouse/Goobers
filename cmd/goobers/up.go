package main

import (
	"flag"
	"io"
	"os"

	"github.com/goobers/goobers/internal/instance"
)

func runUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers up [path]\n\n"+
			"Run the daemon: the embedded scheduler (cron triggers + run conditions)\n"+
			"plus the local runner loop (default path \".\"). Exit codes: 0 = OK,\n"+
			"1 = daemon unavailable, 2 = usage/IO error.\n")
	}
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

	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	if _, _, err := instance.LoadConfigDir(l.ConfigDir()); err != nil {
		pf(stderr, "error: config directory invalid: %v\n", err)
		return 1
	}

	// TODO(#17/#21): the embedded scheduler (#21) and local runner (#17) are
	// not yet merged, so there is no daemon loop to run yet — coordinated in
	// #mission-scheduler / #mission-runner-core. Validate the instance and
	// stop rather than block forever on a daemon that doesn't exist; once
	// both land this becomes scheduler.New(...).Serve(ctx, events, handler).
	pf(stdout, "instance at %s is valid, but the daemon (scheduler #21 + runner #17) is not yet wired into the CLI\n", root)
	pln(stdout, "use `goobers run <workflow>` to trigger a run manually until then")
	return 1
}
