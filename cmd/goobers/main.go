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

// usage renders the top-level help. Its command list is assembled from each
// command's registry synopsis (cmd/goobers/runtime_capabilities.go) rather than
// a hand-written block, so the top-level surface cannot drift from the
// per-command help (#1095, CLI-1). The header and footer are the only
// hand-written prose here — everything between them derives from the registry.
func usage(w io.Writer) {
	pf(w, usageHeader)
	writeSynopses(w, cliCommands)
	pf(w, usageFooter)
}

// writeSynopses walks the command registry in declaration order and emits each
// command's (and subcommand's) top-level usage entry. A command with no
// synopsis — an internal stage worker, a flag alias — is silently skipped.
func writeSynopses(w io.Writer, commands []cliCommand) {
	for _, command := range commands {
		if command.synopsis != "" {
			pf(w, "%s", command.synopsis)
		}
		writeSynopses(w, command.subcommands)
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

backlog-query/reconcile-branches/telemetry-query/push-branch/open-pr/issue-close-out/set-milestone/merge-pr/merge-queue-poll/
pr-select/gather-sibling-context/gather-implement-context/apply-verdict/post-merge/update-behind-pr/gather-pr-context/
rebase-pr/remediation-checkpoint/push-remediated/respond-to-findings are the built-in provider-chain
and connector stage kinds (ARCHITECTURE.md §7, issues #12/#13/#27/#148/#237/
#359/#360/#361/#362/#363/#364/#392/#942): invoked by the runner as a deterministic
stage's shell command, not
typically run by hand. They read their run context (instance root, run id,
workflow, declared Task.Inputs, and injected credentials) from GOOBERS_*
environment variables the runner sets — see internal/executor/env.go —
falling back to an optional trailing [path] argument (default ".") for
standalone/manual invocation.
gather-implement-context uses the same deterministic stage contract to supply
first-pass review and hot-file evidence.
`
