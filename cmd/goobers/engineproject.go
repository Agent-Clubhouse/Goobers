package main

import (
	"context"
	"flag"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

// runEngineProject writes an engine run's journal into the instance.
//
// This is the other end of the same gap `engine-start` filled. The projection
// itself is complete and has been for some time — engine.ProjectCompletedRun
// queries a run's JournalProjection from Temporal, replays it, and writes the
// standard runs/<id>/ layout — but nothing outside tests ever called it. So an
// engine run executed correctly and then left NO trace in any surface the
// product actually has: no `goobers journal`, nothing for the portal to read,
// nothing for `goobers telemetry export` to ship.
//
// That is a worse failure than it sounds. It is not that the journal is
// missing; it is that the tier-3 path looked untrustworthy for a reason that
// had nothing to do with tier 3. The run was durable in Temporal the whole
// time.
//
// WHY IT IS A SEPARATE COMMAND rather than the tail of `engine-start`. The
// projection is a function of history (#629), so it can be run at any point
// after the workflow closes, by any process that can reach Temporal, as many
// times as you like — the query is read-only. Binding it to the starter would
// tie a run's journal to the liveness of whatever fired it, which is exactly
// the coupling the engine exists to remove.
const engineProjectHelp = "Usage: goobers engine-project [flags] <run-id> [path]\n\n" +
	"Write a completed engine run's journal into the instance (experimental):\n" +
	"query the run's journal projection from Temporal, replay it, and write the\n" +
	"standard runs/<id>/ layout. The projection is a function of workflow\n" +
	"history, so this is read-only against Temporal and may be run at any time\n" +
	"after the run closes.\n\n" +
	"Fails closed if the run directory already exists: a journal is authored\n" +
	"once, and overwriting one would destroy the record it exists to be.\n\n" +
	"Exit codes: 0 = projected, 1 = query/write failure, 2 = usage/config error.\n"

func runEngineProject(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("engine-project", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the run; its runs directory receives the journal")
	hostPort := fs.String("temporal-hostport", workerEnvOr("GOOBERS_TEMPORAL_HOSTPORT", "127.0.0.1:7233"), "Temporal frontend host:port")
	namespace := fs.String("temporal-namespace", workerEnvOr("GOOBERS_TEMPORAL_NAMESPACE", "default"), "Temporal namespace")
	fs.Usage = helpUsage(stderr, "engine-project")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		pf(stderr, "usage: goobers engine-project [flags] <run-id> [path]\n")
		return 2
	}
	runID := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	if *gaggle == "" {
		pf(stderr, "error: --gaggle is required; the journal is written under that gaggle's runs directory\n")
		return 2
	}

	l := instance.NewLayout(root)
	if _, err := instance.LoadConfig(l.ConfigFile()); err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 2
	}
	runsDir := l.ForGaggle(*gaggle).RunsDir()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: *hostPort, Namespace: *namespace})
	if err != nil {
		pf(stderr, "error: dial temporal at %s: %v\n", *hostPort, err)
		return 1
	}
	defer c.Close()

	dir, err := engine.ProjectCompletedRun(ctx, c, runID, runsDir)
	if err != nil {
		pf(stderr, "error: project run %s: %v\n", runID, err)
		return 1
	}
	pf(stdout, "journal written: %s\n", dir)
	return 0
}
