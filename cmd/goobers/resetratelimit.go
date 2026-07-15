package main

import (
	"flag"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// runResetRateLimit implements `goobers reset-rate-limit [path]` (issue #315):
// the blessed, narrow way to clear an instance's hourly run-rate budget
// (maxRunsPerHour) so runs can start again immediately — WITHOUT deleting
// runs/, the append-only run journals that are V0's durable execution record.
//
// It writes a marker under scheduler/ that raises the rate window's floor to
// now; Scheduler.Reconcile (run at the start of `goobers up`/`goobers run`)
// then ignores run.started history at or before that floor. It never touches
// runs/, unlike the `rm -rf <instance>` habit this replaces, which reset the
// rate window only as a side effect of destroying the whole instance.
func runResetRateLimit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset-rate-limit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers reset-rate-limit [path]\n\n"+
			"Reset an instance's hourly run-rate budget (maxRunsPerHour) so runs can\n"+
			"start again immediately, WITHOUT deleting runs/ (the append-only run\n"+
			"journals — the durable execution record). This is the blessed narrow\n"+
			"reset: it writes a marker under scheduler/ that moves the rate window's\n"+
			"floor to now, and never touches runs/ — unlike `rm -rf <instance>`,\n"+
			"which clears the rate window only by destroying everything with it.\n"+
			"Takes effect on the next `goobers up`/`goobers run` (stop the daemon\n"+
			"first if one is running; the reset is applied when the scheduler next\n"+
			"reconstructs its budget window). Default path \".\".\n"+
			"Exit codes: 0 = reset written, 2 = usage/IO error.\n")
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
	now := time.Now()
	if err := localscheduler.WriteRateReset(l.SchedulerDir(), now); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stdout, "rate-limit budget reset at %s — runs/ preserved; takes effect on the next `goobers up`/`goobers run`\n",
		now.UTC().Format(time.RFC3339))
	return 0
}
