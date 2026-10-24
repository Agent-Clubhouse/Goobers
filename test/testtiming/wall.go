package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
)

// runWall records elapsed job time from the Actions API's started_at, including
// service setup, checkout and compilation rather than only Go test execution.
func runWall(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	flags := flag.NewFlagSet("wall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	job := flags.String("job", "", "stable job name")
	startedAt := flags.String("started-at", "", "Actions job started_at timestamp (RFC3339)")
	output := flags.String("out", "", "timing artifact path")
	budget := flags.String("budget", "", "timing budget path")
	summary := flags.String("summary", os.Getenv("GITHUB_STEP_SUMMARY"), "optional GitHub step summary path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *job == "" || *output == "" || *budget == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "testtiming wall: -job, -started-at, -out and -budget are required")
		return 2
	}
	start, err := time.Parse(time.RFC3339, *startedAt)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming wall: invalid job start: %v\n", err)
		return 2
	}
	elapsed := now().Sub(start).Seconds()
	if !validNonNegative(elapsed) {
		_, _ = fmt.Fprintln(stderr, "testtiming wall: job start is in the future")
		return 2
	}
	result := artifact{SchemaVersion: schemaVersion, Job: *job, Platform: runtime.GOOS,
		Architecture: runtime.GOARCH, ElapsedSeconds: elapsed}
	if err := writeArtifact(*output, result); err != nil {
		_, _ = fmt.Fprintf(stderr, "testtiming wall: write artifact: %v\n", err)
		return 1
	}
	return runReport([]string{"-budget", *budget, "-current", *output, "-summary", *summary}, stdout, stderr)
}
