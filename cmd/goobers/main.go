// Command goobers is the tier 1-2 instance CLI (INST-012, DEP-021/022):
// `goobers init` scaffolds an instance root, `goobers validate` checks it,
// `goobers up`/`run` operate it, and `goobers status`/`trace` inspect it
// (ARCHITECTURE.md §6).
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/clidocs"
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

// usage renders the top-level help: the operator-facing command surface only.
// Its command list is assembled from each command's registry synopsis
// (cmd/goobers/runtime_capabilities.go) rather than a hand-written block, so
// the top-level surface cannot drift from the per-command help (#1095,
// CLI-1). The header and footer are the only hand-written prose here —
// everything between them derives from the registry.
//
// #2012: the ~35 built-in workflow-stage/connector commands (runner
// plumbing, never typically run by hand) are deliberately excluded here — a
// first-time user asking "what can I do with goobers" was drowning in
// entries like `elect-lander`/`gather-sibling-context` alongside `init`/
// `run`/`status`. They remain fully documented and reachable via
// `goobers help stages`, not removed — see usageStages below.
func usage(w io.Writer) {
	pf(w, usageHeader)
	writeSynopses(w, cliCommands, false)
	pf(w, usageFooter)
}

// usageStages renders the built-in workflow-stage and connector command
// surface usage() omits — the explicit, documented escape hatch #2012's
// acceptance criteria requires ("stage internals remain reachable").
func usageStages(w io.Writer) {
	pf(w, usageStagesHeader)
	writeSynopses(w, cliCommands, true)
	pf(w, usageStagesFooter)
}

// writeSynopses walks the command registry in declaration order and emits
// each command's (and subcommand's) top-level usage entry whose tier matches
// stagesOnly — false for the operator-facing surface, true for workflow-stage
// and connector commands (classified by clidocs.IsWorkflowStage, the same
// signal the generated CLI reference and man pages split on, so all three
// surfaces can never disagree about which bucket a command is in). A command
// with no synopsis — an internal entrypoint, a flag alias — is always
// skipped, regardless of tier.
func writeSynopses(w io.Writer, commands []cliCommand, stagesOnly bool) {
	for _, command := range commands {
		if command.synopsis != "" && clidocs.IsWorkflowStage(command.short) == stagesOnly {
			pf(w, "%s", command.synopsis)
		}
		writeSynopses(w, command.subcommands, stagesOnly)
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

goobers help stages lists the built-in workflow-stage and connector commands
the runner invokes directly as a deterministic stage's shell command — not
typically run by hand.
`

const usageStagesHeader = `goobers — workflow-stage and connector commands

These are the built-in provider-chain and connector stage kinds
(ARCHITECTURE.md §7, issues #12/#13/#27/#148/#237/#359/#360/#361/#362/#363/
#364/#392/#939/#942/#945): invoked by the runner as a deterministic stage's
shell command, not typically run by hand. They read their run context
(instance root, run id, workflow, declared Task.Inputs, and injected
credentials) from GOOBERS_* environment variables the runner sets — see
internal/executor/env.go — falling back to an optional trailing [path]
argument (default ".") for standalone/manual invocation.
gather-implement-context uses the same deterministic stage contract to supply
first-pass review and hot-file evidence.

Usage:
`

const usageStagesFooter = `
See ` + "`goobers --help`" + ` for the operator-facing command surface.
`
