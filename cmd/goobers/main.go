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

func usage(w io.Writer) {
	pf(w, `goobers — tier 1-2 local instance CLI

Usage:
  goobers --version             print build version, commit, and date
  goobers init [--demo] [path]  scaffold an instance root
  goobers scaffold goober|workflow [--force] <name> [path]
                                scaffold a goober or workflow in a gaggle
  goobers validate [flags] [path]  validate an instance or checked-in config source tree
  goobers up [--quiet] [--notify[=all]] [path]
                                run the daemon (scheduler + runner + loopback HTTP API)
  goobers dashboard [--port=<port|auto>] [--no-open] [path]
                                serve and open the local operations portal
  goobers run <workflow> [--no-wait] [path]
                                trigger a run manually (still honors run conditions)
  goobers run abort <run-id> [path]  mark a stuck non-terminal run aborted
  goobers signal <name> [path]  fire an external signal, dispatching every
                                subscribed type=signal-trigger workflow
  goobers workflow show <name> [path]  show a workflow as a text DAG
  goobers runs list [--json] [--phase=...] [--workflow=...] [--limit=N] [path]
                                alias for the status run table (same flags, no --watch)
  goobers runs du [--json] [path]       report per-run journal and artifact bytes
  goobers status [--daemon] [--json] [--phase=...] [--workflow=...] [--limit=N] [--watch [--interval=2s]] [path]
                                validate config, show warnings, list runs newest first, or report daemon health with --daemon
  goobers stats [--since <duration>] [--json] [path]
                                show the instance lifetime summary card
  goobers reset-rate-limit [path]  clear the hourly run-rate budget without deleting runs/
  goobers blocked list [--json] [path]   print the learned blocked-item ledger (scheduler/blocked.json)
  goobers blocked clear <item-id> [path]  safely remove one blocked-item record, under claims.lock
  goobers claims list [--json] [--stale] [--gaggle=name] [--provider=name] [path]
                                print current claim leases, optionally only expired leases
  goobers claims release [--force] [--gaggle=name --provider=name] <item-id> [path]
                                force-release a claim through the live daemon or claims.lock
  goobers trace [--json] [--follow] [--transcripts | --transcript=<stage>] <run-id> [path]
                                show a run's journal events, follow a live run, or show recorded agent transcripts
  goobers escalations [--json] [path]
                                list escalated runs newest first
  goobers escalations show [--json] <run-id> [path]
                                show escalation cause + per-stage artifact timeline
  goobers completion bash|zsh|fish  generate a shell completion script
  goobers telemetry stats|errors [flags] [path]  success rate/duration or recent-error aggregates
  goobers journal redact --run <id> --path <blob> --reason <text> [path]
                                remove a leaked secret from a stored blob (SEC-041)
  goobers backlog-query [--claim]        query/claim one eligible backlog item (a workflow stage)
  goobers push-branch                    push the worktree's checked-out branch to origin (a workflow stage)
  goobers open-pr                        open or update the run's PR (a workflow stage)
  goobers issue-close-out                comment + close out the claimed issue (a workflow stage)
  goobers merge-pr                       conjunctive auto-merge — verdict=pass + CI green + not-draft + SHA-pin valid; lands via direct-merge or merge-queue-enqueue per the repo's detected merge policy (a workflow stage)
  goobers merge-queue-poll               watch an enqueued pull request until the merge queue merges or evicts it, labeling an eviction for remediation (a workflow stage)
  goobers post-merge                     post-merge fan-out (label behind PRs) + close the referenced issue (a workflow stage)
  goobers telemetry-query [--window <d>] [--aggregate <name>] [--threshold <k=v>] [--format candidate-findings]
                                emit versioned candidate findings (a connector stage)
  goobers pr-select                      select one eligible open PR for merge-review (a workflow stage)
  goobers gather-sibling-context         load other open PRs' files/state as review evidence (a workflow stage)
  goobers apply-verdict                  publish a merge-review verdict as a native review (a workflow stage)
  goobers gather-pr-context              pr-remediation entrypoint: select a needs-remediation PR, check out its branch, load verdict/thread/behind-base context (a workflow stage)
  goobers rebase-pr                      rebase-first, finding-driven routing: clean+no-substantive force-pushes and clears the label, else defers to agentic remediation (a workflow stage)
  goobers remediation-checkpoint [--budget N] [--escalate <reason>]  durable per-PR repass budget + same-diff escalation (a workflow stage)
  goobers push-remediated                force-push the remediated branch to the claimed PR and clear needs-remediation (a workflow stage)

path defaults to the current directory. Exit codes: 0 = OK, 1 = validation/
business errors, 2 = usage/IO error. After waiting for a run, run/signal use
0 = completed, 1 = failed/aborted, and 3 = escalated; successful submission-only
modes exit 0 before a terminal outcome is known.

backlog-query/telemetry-query/push-branch/open-pr/issue-close-out/merge-pr/merge-queue-poll/
pr-select/gather-sibling-context/apply-verdict/post-merge/gather-pr-context/
rebase-pr/remediation-checkpoint/push-remediated are the built-in provider-chain
and connector stage kinds (ARCHITECTURE.md §7, issues #12/#13/#27/#148/#237/
#359/#360/#361/#362/#363/#364/#392): invoked by the runner as a deterministic
stage's shell command, not
typically run by hand. They read their run context (instance root, run id,
workflow, declared Task.Inputs, and injected credentials) from GOOBERS_*
environment variables the runner sets — see internal/executor/env.go —
falling back to an optional trailing [path] argument (default ".") for
standalone/manual invocation.
`)
}
