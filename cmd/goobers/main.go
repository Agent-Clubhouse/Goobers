// Command goobers is the tier 1-2 instance CLI (INST-012, DEP-021/022):
// `goobers init` scaffolds an instance root, `goobers validate` checks it,
// `goobers up`/`run` operate it, and `goobers status`/`trace` inspect it
// (ARCHITECTURE.md §6).
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// pf/pln are thin print helpers that discard the write error — these are
// terminal CLI writes to stdout/stderr where a failed write is not
// actionable.
func pf(w io.Writer, format string, a ...interface{}) { _, _ = fmt.Fprintf(w, format, a...) }
func pln(w io.Writer, s string)                       { _, _ = fmt.Fprintln(w, s) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "up":
		return runUp(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "trace":
		return runTrace(args[1:], stdout, stderr)
	case "telemetry":
		return runTelemetry(args[1:], stdout, stderr)
	case "journal":
		return runJournal(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		pf(stderr, "goobers: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	pf(w, `goobers — tier 1-2 local instance CLI

Usage:
  goobers init [path]           scaffold an instance root
  goobers validate [path]       validate an instance's instance.yaml + config/
  goobers up [path]             run the daemon (scheduler + runner)
  goobers run <workflow> [path] trigger a run manually (still honors run conditions)
  goobers run abort <run-id> [path]  mark a stuck non-terminal run aborted
  goobers status [path]         list runs and their current phase
  goobers trace <run-id> [path] show a run's journal events (+ spans if rolled up)
  goobers telemetry stats|errors [path]  success rate/duration or recent-error aggregates
  goobers journal redact --run <id> --path <blob> --reason <text> [path]
                                remove a leaked secret from a stored blob (SEC-041)

path defaults to the current directory. Exit codes: 0 = OK, 1 = validation/
business errors, 2 = usage/IO error.
`)
}
