// Command goobers is the tier 1-2 instance CLI (INST-012, DEP-021/022):
// `goobers init` scaffolds an instance root, `goobers validate` checks it,
// `goobers up`/`run` operate it, and `goobers status`/`trace` inspect it
// (ARCHITECTURE.md §6).
package main

import (
	"fmt"
	"io"
	"os"
	_ "time/tzdata"
)

// runProcessExits is true only for the real CLI entrypoint. In-process callers
// keep standalone asynchronous runs alive in their host process instead.
var runProcessExits bool

func main() {
	runProcessExits = true
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
	if command, ok := findCLICommand(args[0]); ok {
		return command.dispatch(args[1:], stdout, stderr)
	}
	pf(stderr, "goobers: unknown command %q\n\n", args[0])
	usage(stderr)
	return 2
}

// usage renders the core operator surface. Advanced operator and runner-invoked
// stage commands remain available through explicit help views.
func usage(w io.Writer) {
	pf(w, usageHeader)
	writeSynopses(w, cliCommands, cliTierCore)
	pf(w, usageFooter)
}

func usageAll(w io.Writer) {
	pf(w, usageAllHeader)
	writeSynopsisGroup(w, "Core commands:", cliTierCore)
	writeSynopsisGroup(w, "Advanced operator commands:", cliTierAdvanced)
	writeSynopsisGroup(w, "Workflow-stage and connector commands:", cliTierStage)
	pf(w, usageAllFooter)
}

func usageStages(w io.Writer) {
	pf(w, usageStagesHeader)
	writeSynopses(w, cliCommands, cliTierStage)
	pf(w, usageStagesFooter)
}

func writeSynopsisGroup(w io.Writer, heading string, tier cliCommandTier) {
	pln(w, heading)
	writeSynopses(w, cliCommands, tier)
	pln(w, "")
}

// writeSynopses walks the registry in declaration order and emits entries from
// one tier. Commands without a synopsis are skipped.
func writeSynopses(w io.Writer, commands []cliCommand, tier cliCommandTier) {
	for _, command := range commands {
		if command.synopsis != "" && command.tier == tier {
			pf(w, "%s", command.synopsis)
		}
		writeSynopses(w, command.subcommands, tier)
	}
}

const usageHeader = `goobers — tier 1-2 local instance CLI

Usage:
`

const usageFooter = `
path defaults to the current directory. Exit codes: 0 = OK, 1 = validation/
business errors, 2 = usage/IO error. After waiting for a run, run/signal use
0 = completed, 1 = failed/aborted, and 3 = escalated; successful submission-only
modes exit 0 before a terminal outcome is known.

Use ` + "`goobers help all`" + ` for advanced operator commands or
` + "`goobers help stages`" + ` for runner-invoked workflow internals.
Quickstart guide: docs/guides/quickstart.md
DSL entry points: ` + "`goobers schema`" + ` and ` + "`goobers examples`" + `
Troubleshooting: ` + "`goobers status`" + `, ` + "`goobers trace`" + `, and ` + "`goobers escalations`" + `
`

const usageAllHeader = `goobers — complete command reference

Usage:

`

const usageAllFooter = `path defaults to the current directory.
`

const usageStagesHeader = `goobers — workflow-stage and connector commands

These commands are invoked by the runner, not typically by hand. They read
their run context from GOOBERS_* environment variables and retain their
existing standalone invocation behavior.

Usage:
`

const usageStagesFooter = `
See ` + "`goobers --help`" + ` for core operator commands.
`
