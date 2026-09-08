package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

const rootsHelp = "Usage: goobers roots <discover | decommission> [flags] [path]\n\nDiscover likely instance roots or mark a stopped root as historical without deleting data.\n"
const rootsDecommissionHelp = "Usage: goobers roots decommission --reason=<text> [path]\n\n" +
	"Persist a historical-root marker bound to the instance ID. Refuses an active\n" +
	"daemon or another manual lock holder. Does not delete runs or configuration.\n" +
	"Repeated calls preserve the first marker. Exit codes: 0 = marked, 2 = error.\n"

func runRoots(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			pf(stdout, "%s", rootsHelp)
			return 0
		default:
			pf(stderr, "goobers roots: unknown subcommand %q\n\n", args[0])
		}
	}
	pf(stderr, "%s", rootsHelp)
	return 2
}

func runRootsDecommission(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("roots decommission", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reason := fs.String("reason", "", "why this root is historical (required)")
	fs.Usage = helpUsage(stderr, "roots decommission")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *reason == "" || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	layout := instance.NewLayout(root)
	if _, err := instance.LoadConfig(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := instance.EnsureRootIdentity(ctx, root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if _, err := fmt.Fprintf(stdout, "Instance root: %q; instance ID: %q\n", canonicalStatusRoot(root), id); err != nil {
		pf(stderr, "error: display instance identity: %v\n", err)
		return 2
	}
	release, err := acquireInstanceLock(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: root must be stopped before decommission: %v\n", err)
		return 2
	}
	defer release()
	marker, err := instance.DecommissionRoot(ctx, root, *reason, time.Now())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stdout, "Historical root; do not use. Marked %s: %q. No data was deleted.\n", marker.At.Format(time.RFC3339Nano), marker.Reason)
	return 0
}
