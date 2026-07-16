// Command goobers is the tier 1-2 instance CLI (INST-012, DEP-021/022):
// `goobers init` scaffolds an instance root, `goobers validate` checks it,
// `goobers up`/`run` operate it, and `goobers status`/`trace` inspect it
// (ARCHITECTURE.md §6).
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/version"
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
	case "scaffold":
		return runScaffold(args[1:], stdout, stderr)
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "up":
		return runUp(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "signal":
		return runSignal(args[1:], stdout, stderr)
	case "workflow":
		return runWorkflow(args[1:], stdout, stderr)
	case "runs":
		return runRuns(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "stats":
		return runStats(args[1:], stdout, stderr)
	case "trace":
		return runTrace(args[1:], stdout, stderr)
	case "telemetry":
		return runTelemetry(args[1:], stdout, stderr)
	case "telemetry-query":
		return runTelemetryQuery(args[1:], stdout, stderr)
	case "journal":
		return runJournal(args[1:], stdout, stderr)
	case "backlog-query":
		return runBacklogQuery(args[1:], stdout, stderr)
	case "push-branch":
		return runPushBranch(args[1:], stdout, stderr)
	case "open-pr":
		return runOpenPR(args[1:], stdout, stderr)
	case "issue-close-out":
		return runIssueCloseOut(args[1:], stdout, stderr)
	case "reset-rate-limit":
		return runResetRateLimit(args[1:], stdout, stderr)
	case "merge-pr":
		return runMergePR(args[1:], stdout, stderr)
	case "pr-select":
		return runPRSelect(args[1:], stdout, stderr)
	case "gather-sibling-context":
		return runGatherSiblingContext(args[1:], stdout, stderr)
	case "apply-verdict":
		return runApplyVerdict(args[1:], stdout, stderr)
	case "post-merge":
		return runPostMerge(args[1:], stdout, stderr)
	case "gather-pr-context":
		return runGatherPRContext(args[1:], stdout, stderr)
	case "rebase-pr":
		return runRebasePR(args[1:], stdout, stderr)
	case "remediation-checkpoint":
		return runRemediationCheckpoint(args[1:], stdout, stderr)
	case "--version", "version":
		pf(stdout, "goobers %s\n", version.Get())
		return 0
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
  goobers --version             print build version, commit, and date
  goobers init [path]           scaffold an instance root
  goobers scaffold goober|workflow [--force] <name> [path]
                                scaffold a goober or workflow in a gaggle
  goobers validate [path]       validate an instance's instance.yaml + config/
  goobers up [--quiet] [path]   run the daemon (scheduler + runner)
  goobers run <workflow> [path] trigger a run manually (still honors run conditions)
  goobers run abort <run-id> [path]  mark a stuck non-terminal run aborted
  goobers signal <name> [path]  fire an external signal, dispatching every
                                subscribed type=signal-trigger workflow
  goobers workflow show <name> [path]  show a workflow as a text DAG
  goobers runs list [--json] [--limit=N] [path]  list runs, most-recent first
  goobers status [--json] [--phase=...] [--workflow=...] [--limit=N] [path]
                                list runs and their current phase
  goobers stats [--since <duration>] [--json] [path]
                                show the instance lifetime summary card
  goobers reset-rate-limit [path]  clear the hourly run-rate budget without deleting runs/
  goobers trace [--json] <run-id> [path] show a run's journal events (+ spans if rolled up)
  goobers telemetry stats|errors [--json] [path]  success rate/duration or recent-error aggregates
  goobers journal redact --run <id> --path <blob> --reason <text> [path]
                                remove a leaked secret from a stored blob (SEC-041)
  goobers backlog-query [--claim]        query/claim one eligible backlog item (a workflow stage)
  goobers push-branch                    push the worktree's checked-out branch to origin (a workflow stage)
  goobers open-pr                        open or update the run's PR (a workflow stage)
  goobers issue-close-out                comment + close out the claimed issue (a workflow stage)
  goobers merge-pr                       conjunctive auto-merge — verdict=pass + CI green + not-draft + SHA-pin valid (a workflow stage)
  goobers post-merge                     post-merge fan-out (label behind PRs) + close the referenced issue (a workflow stage)
  goobers telemetry-query [--window <d>] emit telemetry signals JSON over a window (a workflow stage)
  goobers pr-select                      select one eligible open PR for merge-review (a workflow stage)
  goobers gather-sibling-context         load other open PRs' files/state as review evidence (a workflow stage)
  goobers apply-verdict                  apply a merge-review verdict's label + comment (a workflow stage)
  goobers gather-pr-context              pr-remediation entrypoint: select a needs-remediation PR, check out its branch, load verdict/thread/behind-base context (a workflow stage)
  goobers rebase-pr                      rebase-first, finding-driven routing: clean+no-substantive force-pushes and clears the label, else defers to agentic remediation (a workflow stage)
  goobers remediation-checkpoint [--budget N]  durable per-PR repass budget + same-diff escalation (a workflow stage)

path defaults to the current directory. Exit codes: 0 = OK, 1 = validation/
business errors, 2 = usage/IO error. After waiting for a run, run/signal use
0 = completed, 1 = failed/aborted, and 3 = escalated; successful submission-only
modes exit 0 before a terminal outcome is known.

backlog-query/push-branch/open-pr/issue-close-out/merge-pr/pr-select/
gather-sibling-context/apply-verdict/post-merge/gather-pr-context/
rebase-pr/remediation-checkpoint are the built-in provider-chain stage
kinds (ARCHITECTURE.md §7, issues #12/#13/#27/#237/#359/#360/#361/#362/
#363/#364): invoked by the runner as a deterministic
stage's shell command, not
typically run by hand. They read their run context (instance root, run id,
workflow, declared Task.Inputs, and injected credentials) from GOOBERS_*
environment variables the runner sets — see internal/executor/env.go —
falling back to an optional trailing [path] argument (default ".") for
standalone/manual invocation.
`)
}
