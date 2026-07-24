// Seeded by a one-time migration from the original main.go usage() block
// (#1095). There is no live generator yet (that is CLI-2's man/reference-gen
// work), so until then this map is maintained by hand alongside the command
// registry in runtime_capabilities.go: add a command there, add its synopsis
// line here. TestCLIVerbCoverage enforces that the two stay in sync.
package main

const gatherContextID = "gather-implement-context"

// synopsisByID holds each command's verbatim entry in the top-level usage()
// list, keyed by invocation-path id. usage() assembles these via the command
// registry so the top-level surface cannot drift from per-command help (#1095).
var synopsisByID = map[string]string{
	"version":                "  goobers version [--json]      print build version, commit, and date (--json for structured output)\n",
	"versions":               "  goobers versions [--json]     print the supported DSL, Go toolchain, and OS/arch matrix\n",
	"init":                   "  goobers init [--guided | --demo | --template=quickstart] [path]\n                                scaffold an instance root\n",
	"scaffold":               "  goobers scaffold goober|workflow [--force] <name> [path]\n                                scaffold a goober or workflow in a gaggle\n",
	"validate":               "  goobers validate [flags] [path]  validate an instance or checked-in config source tree\n",
	"lint":                   "  goobers lint [flags] [path]   lint config via the single authoritative validation engine (alias for validate)\n",
	"doctor":                 "  goobers doctor --k8s [--report json] [--oidc-issuer <url>] [--registry <host>] [--egress <host:port,...>]\n                                preflight a Kubernetes cluster against docs/design/k8s-infra-shape.md\n",
	"config":                 "  goobers config show|diff [flags] [path]\n                                inspect instance config or compare workflows with canonical definitions\n",
	"up":                     "  goobers up [--quiet] [--notify[=all]] [--skip-preflight] [path]\n                                run the daemon (scheduler + runner + loopback HTTP API)\n",
	"service":                "  goobers service install|uninstall|status [path]\n                                install and manage the platform-supervised daemon\n",
	"worker":                 "  goobers worker [--task-queue <q>]... [--temporal-hostport h:p] [--drain-timeout <dur>]\n                                host a Temporal engine worker (tier-3, experimental)\n",
	"dashboard":              "  goobers dashboard [--port=<port|auto>] [--no-open] [path]\n                                serve and open the local operations portal\n",
	"run":                    "  goobers run <workflow> [--no-wait] [path]\n                                trigger a run manually (still honors run conditions)\n",
	"run abort":              "  goobers run abort <run-id> [path]  mark a stuck non-terminal run aborted\n",
	"run cancel":             "  goobers run cancel <run-id> [path]  cancel a live in-flight run via the daemon\n",
	"signal":                 "  goobers signal <name> [path]  fire an external signal, dispatching every\n                                subscribed type=signal-trigger workflow\n",
	"workflow show":          "  goobers workflow show <name> [path]  show a workflow as a text DAG\n",
	"runs list":              "  goobers runs list [--json] [--phase=...] [--workflow=...] [--limit=N] [path]\n                                alias for the status run table (same flags, no --watch)\n",
	"runs du":                "  goobers runs du [--json] [path]       report per-run journal and artifact bytes\n",
	"status":                 "  goobers status [--daemon] [--json] [--phase=...] [--workflow=...] [--limit=N] [--watch [--interval=2s]] [path]\n                                validate config, show warnings, list runs newest first, or report daemon health with --daemon\n",
	"stats":                  "  goobers stats [--since <duration>] [--json] [path]\n                                show the instance lifetime summary card\n",
	"features":               "  goobers features [--dsl-version <version>] [--used] [path]\n                                list the workflow-DSL features this build supports\n",
	"reset-rate-limit":       "  goobers reset-rate-limit [path]  clear the hourly run-rate budget without deleting runs/\n",
	"blocked list":           "  goobers blocked list [--json] [path]   print the learned blocked-item ledger (scheduler/blocked.json)\n",
	"blocked clear":          "  goobers blocked clear <item-id> [path]  safely remove one blocked-item record, under claims.lock\n",
	"claims list":            "  goobers claims list [--json] [--stale] [--gaggle=name] [--provider=name] [path]\n                                print current claim leases, optionally only expired leases\n",
	"claims release":         "  goobers claims release [--force] [--gaggle=name --provider=name] <item-id> [path]\n                                force-release a claim through the live daemon or claims.lock\n",
	"trace":                  "  goobers trace [--json] [--follow] [--transcripts | --transcript=<stage>] <run-id> [path]\n                                show a run's journal events, follow a live run, or show recorded agent transcripts\n",
	"escalations":            "  goobers escalations [--json] [path]\n                                list escalated runs newest first\n",
	"escalations show":       "  goobers escalations show [--json] <run-id> [path]\n                                show escalation cause + per-stage artifact timeline\n",
	"completion":             "  goobers completion bash|zsh|fish  generate a shell completion script\n",
	"telemetry":              "  goobers telemetry stats|errors|export|prune [flags] [path]\n                                query, export, or prune run telemetry\n",
	"journal redact":         "  goobers journal redact --run <id> --path <blob> --reason <text> [path]\n                                remove a leaked secret from a stored blob (SEC-041)\n",
	"backlog-dedupe":         "  goobers backlog-dedupe                 surface ranked duplicate candidates for curator judgment (a workflow stage)\n",
	"backlog-query":          "  goobers backlog-query [--claim]        query/claim one eligible backlog item (a workflow stage)\n",
	"reconcile-branches":     "  goobers reconcile-branches [--delete] [--max N] [--min-age D] [--after BRANCH]\n                                report bounded stale goobers/* branch candidates; --delete opts into removal (a workflow stage)\n",
	"push-branch":            "  goobers push-branch                    push the worktree's checked-out branch to origin (a workflow stage)\n",
	"open-pr":                "  goobers open-pr                        open or update the run's PR (a workflow stage)\n",
	"issue-close-out":        "  goobers issue-close-out                comment + close out the claimed issue (a workflow stage)\n",
	"set-milestone":          "  goobers set-milestone --item ID --milestone N [path]\n                                assign an existing milestone to an issue (a workflow stage)\n",
	"merge-pr":               "  goobers merge-pr                       conjunctive auto-merge \u2014 verdict=pass + CI green + not-draft + SHA-pin valid; lands via direct-merge or merge-queue-enqueue per the repo's detected merge policy (a workflow stage)\n",
	"merge-queue-poll":       "  goobers merge-queue-poll               watch an enqueued pull request until the merge queue merges or evicts it, labeling an eviction for remediation (a workflow stage)\n",
	"reconcile-post-merge":   "  goobers reconcile-post-merge [--max N] [--lookback D]\n                                reconcile bounded late merge-queue merges through post-merge bookkeeping (a workflow stage)\n",
	"record-merge-refusal":   "  goobers record-merge-refusal           record a merge refusal and demote a persistently-stuck lander (a workflow stage)\n",
	"post-merge":             "  goobers post-merge                     post-merge fan-out (label behind PRs) + close the referenced issue (a workflow stage)\n",
	"telemetry-query":        "  goobers telemetry-query [--window <d>] [--aggregate <name>] [--threshold <k=v>] [--format candidate-findings]\n                                emit versioned candidate findings (a connector stage)\n",
	"docs-churn":             "  goobers docs-churn [--repo <dir>] [--since <d>] [--buffer-multiplier <f>] [--format churn-digest]\n                                emit the docs-drift churn digest since the watermark (a connector stage)\n",
	"pr-select":              "  goobers pr-select                      select one eligible open PR for merge-review (a workflow stage)\n",
	"gather-sibling-context": "  goobers gather-sibling-context         load other open PRs' files/state as review evidence (a workflow stage)\n",
	gatherContextID:          "  goobers gather-implement-context       load first-pass verdict taxonomy and hot-file context (a workflow stage)\n",
	"apply-verdict":          "  goobers apply-verdict                  publish a merge-review verdict as a native review (a workflow stage)\n",
	"elect-lander":           "  goobers elect-lander                   elect the landing PR among a merge-review cohort (a workflow stage)\n",
	"update-behind-pr":       "  goobers update-behind-pr               API-update a clean behind-base PR, otherwise route to full remediation (a workflow stage)\n",
	"gather-pr-context":      "  goobers gather-pr-context              pr-remediation entrypoint: select a needs-remediation PR, check out its branch, load verdict/thread/behind-base context (a workflow stage)\n",
	"gather-issue-context":   "  goobers gather-issue-context           add originating issue bodies to a remediation brief (a workflow stage)\n",
	"gather-ci-failures":     "  goobers gather-ci-failures             add failing check summaries and annotations to a remediation brief (a workflow stage)\n",
	"rebase-pr":              "  goobers rebase-pr                      rebase-first, finding-driven routing: clean+no-substantive force-pushes and clears the label, else defers to agentic remediation (a workflow stage)\n",
	"remediation-checkpoint": "  goobers remediation-checkpoint [--budget N] [--escalate <reason>]  durable per-PR repass budget + same-diff escalation (a workflow stage)\n",
	"push-remediated":        "  goobers push-remediated                force-push the remediated branch to the claimed PR and clear needs-remediation (a workflow stage)\n",
	"respond-to-findings":    "  goobers respond-to-findings            post a validated per-finding remediation response to the claimed PR (a workflow stage)\n",
}
