package main

import (
	"flag"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

const telemetryPruneOrphansHelp = "Usage: goobers telemetry prune-orphans [--delete] [--min-age=D] [path]\n\n" +
	"Report directories without run.yaml from both published run roots and unpublished\n" +
	"creation staging roots after at least 24h of inactivity. The default is a dry-run\n" +
	"report; --delete opts into deletion. --min-age may raise but never lower\n" +
	"the 24h safety threshold. Valid run journals, recent or active directories, files,\n" +
	"and symlinks are always preserved.\n" +
	"Exit codes: 0 = OK, 1 = cleanup error, 2 = usage/config error.\n"

func runTelemetryPruneOrphans(args []string, stdout, stderr io.Writer) int {
	return runTelemetryPruneOrphansAt(args, stdout, stderr, time.Now())
}

// pruneOrphansAtStartup runs PruneOrphans once at daemon startup (#2035) so a
// crash-abandoned orphan run or run-creation staging directory left before
// this process started doesn't sit until an operator happens to run `goobers
// telemetry prune-orphans` — mirroring the same startup-housekeeping
// precedent already established for worktree.Reap and telemetry retention
// (up.go). MinimumOrphanAge (24h) and PruneOrphans' own lock-awareness are
// what keep a directory belonging to an in-flight creation from ever being
// swept; this wrapper doesn't add or relax either guard.
func pruneOrphansAtStartup(layout instance.Layout, now time.Time) ([]retention.OrphanResult, error) {
	return retention.PruneOrphans(layout, retention.OrphanOptions{
		Now:    now,
		MinAge: retention.MinimumOrphanAge,
		Delete: true,
	})
}

func runTelemetryPruneOrphansAt(args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := newCLIFlagSet("telemetry prune-orphans", flag.ContinueOnError)
	fs.SetOutput(stderr)
	deleteOrphans := fs.Bool("delete", false, "delete eligible orphan directories (opt-in; default is dry-run)")
	minAge := fs.Duration("min-age", retention.MinimumOrphanAge, "minimum inactivity age (at least 24h)")
	fs.Usage = helpUsage(stderr, "telemetry prune-orphans")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || *minAge < retention.MinimumOrphanAge {
		if *minAge < retention.MinimumOrphanAge {
			pf(stderr, "error: --min-age must be at least %s\n", retention.MinimumOrphanAge)
		} else {
			fs.Usage()
		}
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
	results, err := retention.PruneOrphans(layout, retention.OrphanOptions{
		Now: now, MinAge: *minAge, Delete: *deleteOrphans,
	})
	if err != nil {
		pf(stderr, "error: prune orphan runs: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		pf(stdout, "no orphan run or creation-stage directories inactive for at least %s\n", *minAge)
		return 0
	}
	verb := "would delete"
	if *deleteOrphans {
		verb = "deleted"
	}
	for _, result := range results {
		source := "run"
		if result.CreationStage {
			source = "creation-stage"
		}
		pf(stdout, "%s orphan=%q source=%s path=%q lastModified=%s\n",
			verb, result.Name, source, result.RunDir, result.LastModified.UTC().Format(time.RFC3339))
	}
	return 0
}
