package main

import (
	"flag"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/selfupdate"
)

// down.go implements #2072: a CLI-native way to ask a live `goobers up`
// daemon to perform the same graceful drain-shutdown SIGINT/SIGTERM already
// trigger, from a separate terminal or from automation, without terminal or
// process access.
//
// This reuses the self-update supervisor's existing stop-request file
// (internal/selfupdate.RequestDaemonStop / ConsumeStopRequest) rather than
// building a second file-based protocol: `goobers up`'s supervisorStop
// goroutine (up.go) already polls for that exact file every
// delegationSweepInterval and, on finding it, calls stopDaemon() — driving
// the identical drain path SIGINT/SIGTERM would. Nothing about "this is a
// restart" is encoded in the stop-request file itself (that orchestration
// lives entirely in the self-update supervisor process), so a plain,
// non-restarting `goobers down` reusing the same file is exactly correct,
// not a repurposing of unrelated machinery.
//
// Deliberately NOT built on #169's planned daemon HTTP API, for the same
// reason rundelegate.go (#343) isn't: #169 is unbuilt and gated, so
// depending on it now would mean either unreviewed V1 design work or a
// parallel ad-hoc HTTP surface risking conflict with its eventual shape.

const downHelp = "Usage: goobers down [path]\n\n" +
	"Request a live `goobers up` daemon for this instance to perform the\n" +
	"same graceful drain-shutdown SIGINT/SIGTERM already trigger, from a\n" +
	"separate terminal or from automation. No daemon HTTP API, port, or\n" +
	"auth surface is required or added. Exits 0 once the shutdown request\n" +
	"has been delivered — the daemon picks it up and begins draining on its\n" +
	"next sweep. With no live daemon for this instance, fails fast with a\n" +
	"clear message rather than hanging. Exit codes: 0 = shutdown requested,\n" +
	"1 = no live daemon found, 2 = usage/IO error.\n"

func runDown(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "down")
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
	running, identity, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if !running {
		pf(stderr, "error: no live `goobers up` daemon found for %s\n", root)
		return 1
	}

	if err := selfupdate.RequestDaemonStop(root); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if identity != nil {
		pf(stdout, "shutdown requested for daemon pid %d; it will begin draining within %s\n", identity.PID, delegationSweepInterval)
	} else {
		pf(stdout, "shutdown requested; the daemon will begin draining within %s\n", delegationSweepInterval)
	}
	return 0
}
