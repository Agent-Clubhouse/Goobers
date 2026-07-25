package main

import (
	"flag"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

const telemetryPruneOrphansHelp = "Usage: goobers telemetry prune-orphans [--delete] [--min-age=D] [path]\n\n" +
	"Report run directories that have no run.yaml and whose directory contents have\n" +
	"not changed for at least 24h. The default is a dry-run report; --delete opts\n" +
	"into deletion. --min-age may raise but never lower the 24h safety threshold.\n" +
	"Valid run journals, recent directories, files, and symlinks are always preserved.\n" +
	"Exit codes: 0 = OK, 1 = cleanup error, 2 = usage/config error.\n"

func runTelemetryPruneOrphans(args []string, stdout, stderr io.Writer) int {
	return runTelemetryPruneOrphansAt(args, stdout, stderr, time.Now())
}

func runTelemetryPruneOrphansAt(args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("telemetry prune-orphans", flag.ContinueOnError)
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
		pf(stdout, "no orphan run directories inactive for at least %s\n", *minAge)
		return 0
	}
	verb := "would delete"
	if *deleteOrphans {
		verb = "deleted"
	}
	for _, result := range results {
		pf(stdout, "%s orphan=%q path=%q lastModified=%s\n",
			verb, result.Name, result.RunDir, result.LastModified.UTC().Format(time.RFC3339))
	}
	return 0
}
