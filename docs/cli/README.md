# `goobers` CLI reference

<!-- Generated from the command registry (cmd/goobers/runtime_capabilities.go) by `make docs`. Do not edit by hand; edits are overwritten and the CI drift guard will fail. -->

`goobers` is the tier 1-2 local instance CLI. This reference is generated from the CLI command registry, so it always matches the shipped binary.

## Core commands

| Command | Description |
| --- | --- |
| [`goobers completion`](#goobers-completion) | generate a shell completion script |
| [`goobers connect`](#goobers-connect) | connect an instance to your own GitHub repository |
| [`goobers dashboard`](#goobers-dashboard) | serve and open the local operations portal |
| [`goobers down`](#goobers-down) | request a live daemon's graceful drain-shutdown from a separate terminal |
| [`goobers escalations`](#goobers-escalations) | list escalated runs newest first |
| [`goobers examples`](#goobers-examples) | browse canonical workflow examples embedded in the binary |
| [`goobers getting-started`](#goobers-getting-started) | serve and open the guided portal Getting Started walkthrough |
| [`goobers help`](#goobers-help) | show command or concept help |
| [`goobers init`](#goobers-init) | scaffold an instance root |
| [`goobers run`](#goobers-run) | trigger a run manually (still honors run conditions) |
| [`goobers scaffold`](#goobers-scaffold) | scaffold a goober, workflow, or gaggle |
| [`goobers service`](#goobers-service) | install and manage the platform-supervised daemon |
| [`goobers signal`](#goobers-signal) | fire an external signal to subscribed workflows |
| [`goobers stats`](#goobers-stats) | show the instance lifetime summary card |
| [`goobers status`](#goobers-status) | validate config, show warnings, list runs, report daemon health, or list live agentic stages |
| [`goobers trace`](#goobers-trace) | show a run's journal events or review verdicts, follow a live run, or show transcripts |
| [`goobers up`](#goobers-up) | run the daemon (scheduler + runner + loopback HTTP API) |
| [`goobers validate`](#goobers-validate) | validate an instance or checked-in config source tree |
| [`goobers version`](#goobers-version) | print build version, commit, and date (--json for structured output) |
| [`goobers workflow`](#goobers-workflow) | inspect workflows |
| [`goobers workflow show`](#goobers-workflow-show) | show a workflow as a text DAG |

## Advanced operator commands

Less-common commands for configuration, maintenance, and diagnostics.

| Command | Description |
| --- | --- |
| [`goobers agent-kit`](#goobers-agent-kit) | install, inspect, or update the release-matched agent toolkit |
| [`goobers agent-kit check`](#goobers-agent-kit-check) | report agent toolkit version and drift |
| [`goobers agent-kit install`](#goobers-agent-kit-install) | install the release-matched agent toolkit |
| [`goobers agent-kit update`](#goobers-agent-kit-update) | review or explicitly apply an agent toolkit update |
| [`goobers apply`](#goobers-apply) | reconcile a live daemon's workflow definitions now |
| [`goobers approve`](#goobers-approve) | approve a paused or escalated gate |
| [`goobers blocked`](#goobers-blocked) | inspect and clear the learned blocked-item ledger |
| [`goobers blocked clear`](#goobers-blocked-clear) | safely remove one blocked-item record, under claims.lock |
| [`goobers blocked list`](#goobers-blocked-list) | print the learned blocked-item ledger (scheduler/blocked.json) |
| [`goobers claims`](#goobers-claims) | inspect and force-release claim leases |
| [`goobers claims list`](#goobers-claims-list) | print current claim leases, optionally only expired leases |
| [`goobers claims release`](#goobers-claims-release) | force-release a claim through the live daemon or claims.lock |
| [`goobers completion bash`](#goobers-completion-bash) | generate a bash completion script |
| [`goobers completion fish`](#goobers-completion-fish) | generate a fish completion script |
| [`goobers completion powershell`](#goobers-completion-powershell) | generate a PowerShell completion script |
| [`goobers completion zsh`](#goobers-completion-zsh) | generate a zsh completion script |
| [`goobers config`](#goobers-config) | inspect, materialize, and compare instance configuration |
| [`goobers config diff`](#goobers-config-diff) | compare active workflows with canonical definitions |
| [`goobers config materialize`](#goobers-config-materialize) | apply the recorded checked-in source to the runtime instance |
| [`goobers config show`](#goobers-config-show) | render the effective instance config (secrets redacted) |
| [`goobers doctor`](#goobers-doctor) | preflight a Kubernetes cluster, repository forge policy, or Windows antivirus exclusions |
| [`goobers e2e`](#goobers-e2e) | check the Goobernetes distributed e2e proof harness's assertions against a recorded run |
| [`goobers e2e kill-inject`](#goobers-e2e-kill-inject) | perform one live S6 kill-matrix cell (pod-kill) against a real cluster |
| [`goobers e2e verify`](#goobers-e2e-verify) | verify the Goobernetes S1-S9 e2e proof harness against one completed run's recorded data |
| [`goobers engine-project`](#goobers-engine-project) | write a completed engine run's journal into the instance (experimental) |
| [`goobers engine-queues`](#goobers-engine-queues) | report which workers poll this instance's engine and dispatch task queues (experimental) |
| [`goobers engine-start`](#goobers-engine-start) | dispatch one run onto the tier-3 engine via Temporal (experimental) |
| [`goobers escalations show`](#goobers-escalations-show) | show escalation cause, verdict, and per-stage artifact timeline |
| [`goobers examples list`](#goobers-examples-list) | list canonical embedded workflow examples |
| [`goobers examples show`](#goobers-examples-show) | print a canonical embedded workflow example |
| [`goobers explain`](#goobers-explain) | project field facts from an embedded JSON Schema |
| [`goobers features`](#goobers-features) | list the workflow-DSL features this build supports |
| [`goobers fix`](#goobers-fix) | mechanically migrate workflows to a target dslVersion, one step at a time (DVL-6) |
| [`goobers fleet`](#goobers-fleet) | associate this instance with a Fleet service |
| [`goobers fleet join`](#goobers-fleet-join) | discover and enroll this instance with a Fleet service |
| [`goobers fleet leave`](#goobers-fleet-leave) | remove this instance's Fleet association and protected secrets |
| [`goobers fleet status`](#goobers-fleet-status) | show durable Fleet registration and connection state |
| [`goobers journal`](#goobers-journal) | the one sanctioned edit to the append-only journal |
| [`goobers journal redact`](#goobers-journal-redact) | remove a leaked secret from a stored blob (SEC-041) |
| [`goobers lint`](#goobers-lint) | lint config via the single authoritative validation engine (alias for validate) |
| [`goobers netpol-render`](#goobers-netpol-render) | render per-runner-class NetworkPolicy reference manifests from the runners: inventory |
| [`goobers onboarding`](#goobers-onboarding) | run non-interactive onboarding actions |
| [`goobers onboarding stub-agent-instructions`](#goobers-onboarding-stub-agent-instructions) | install agent-instruction assets into a config source |
| [`goobers onboarding stub-sample`](#goobers-onboarding-stub-sample) | materialize and optionally seed the disposable Getting Started target |
| [`goobers override`](#goobers-override) | override a nondeterministic gate with a rationale |
| [`goobers preflight`](#goobers-preflight) | check WSL full-isolation readiness and optionally hand off a command |
| [`goobers rerun-stage`](#goobers-rerun-stage) | rerun a stage with a recorded instruction addendum |
| [`goobers reset-rate-limit`](#goobers-reset-rate-limit) | clear the hourly run-rate budget without deleting runs/ |
| [`goobers run abort`](#goobers-run-abort) | mark a stuck non-terminal run aborted |
| [`goobers run cancel`](#goobers-run-cancel) | cancel a live in-flight run via the daemon |
| [`goobers runs`](#goobers-runs) | list runs and report per-run disk usage |
| [`goobers runs du`](#goobers-runs-du) | report per-run journal and artifact bytes |
| [`goobers runs list`](#goobers-runs-list) | alias for the status run table (same flags, no --watch) |
| [`goobers scaffold gaggle`](#goobers-scaffold-gaggle) | scaffold a gaggle, or rename one with --from |
| [`goobers scaffold goober`](#goobers-scaffold-goober) | scaffold a goober in a gaggle |
| [`goobers scaffold workflow`](#goobers-scaffold-workflow) | scaffold a workflow in a gaggle |
| [`goobers schema`](#goobers-schema) | emit a JSON Schema embedded in this build |
| [`goobers self-update`](#goobers-self-update) | stage and request a supervised binary update |
| [`goobers service install`](#goobers-service-install) | install, enable, and start the supervised daemon |
| [`goobers service start`](#goobers-service-start) | resume an installed-but-stopped daemon |
| [`goobers service status`](#goobers-service-status) | report whether the supervised daemon is installed and running |
| [`goobers service stop`](#goobers-service-stop) | halt the running daemon without disabling or removing it |
| [`goobers service uninstall`](#goobers-service-uninstall) | gracefully stop and remove the supervised daemon |
| [`goobers speech`](#goobers-speech) | preflight and test local speech notifications |
| [`goobers speech preflight`](#goobers-speech-preflight) | check the configured local speech engine without emitting sound |
| [`goobers speech test`](#goobers-speech-test) | speak the fixed local readiness phrase |
| [`goobers telemetry`](#goobers-telemetry) | query, export, prune, or compact run telemetry |
| [`goobers telemetry compact`](#goobers-telemetry-compact) | drop aged scheduler journal/rollup rows and reclaim disk (VACUUM) |
| [`goobers telemetry errors`](#goobers-telemetry-errors) | recent errors across runs, by class, with run/stage refs |
| [`goobers telemetry export`](#goobers-telemetry-export) | re-emit a span-start-time window from journaled OTLP/JSON |
| [`goobers telemetry prune`](#goobers-telemetry-prune) | remove terminal runs outside configured retention bounds |
| [`goobers telemetry prune-orphans`](#goobers-telemetry-prune-orphans) | report or delete old orphan and unfinished run directories |
| [`goobers telemetry stats`](#goobers-telemetry-stats) | success rate and duration aggregates per workflow and stage |
| [`goobers versions`](#goobers-versions) | print the supported DSL, Go toolchain, and OS/arch matrix (--json for structured output) |
| [`goobers worker`](#goobers-worker) | host a Temporal engine worker: task queues, graceful drain, versioned identity (tier-3, experimental) |
| [`goobers workspace`](#goobers-workspace) | explicitly recover pinned repository workspaces |
| [`goobers workspace reset`](#goobers-workspace-reset) | tear down and re-materialize a pinned repository workspace |

## Workflow-stage and connector commands

Runner-invoked workflow internals; these remain directly invocable but are not typically run by hand.

| Command | Description |
| --- | --- |
| [`goobers apply-verdict`](#goobers-apply-verdict) | publish a managed or advisory merge-review verdict (a workflow stage) |
| [`goobers backlog-assignment`](#goobers-backlog-assignment) | assign eligible backlog items from a configured roster (a workflow stage) |
| [`goobers backlog-dedupe`](#goobers-backlog-dedupe) | surface ranked duplicate candidates for curator judgment (a workflow stage) |
| [`goobers backlog-health`](#goobers-backlog-health) | snapshot ready-pool depth and age (a workflow stage) |
| [`goobers backlog-query`](#goobers-backlog-query) | query/claim one eligible backlog item (a workflow stage) |
| [`goobers check-fail-first`](#goobers-check-fail-first) | enforce fail-first evidence for a new workflow gate (a workflow stage) |
| [`goobers check-issue-staleness`](#goobers-check-issue-staleness) | route a PR to remediation if its linked issue changed since implementation began (a workflow stage) |
| [`goobers docs-churn`](#goobers-docs-churn) | emit the docs-drift churn digest since the watermark (a connector stage) |
| [`goobers elect-lander`](#goobers-elect-lander) | elect the landing PR among a merge-review cohort (a workflow stage) |
| [`goobers file-issues`](#goobers-file-issues) | file a validated nominations artifact as deduped, budgeted issues (a workflow stage) |
| [`goobers gate-removal-guard`](#goobers-gate-removal-guard) | block a tutor run that removes/loosens its own flagged gate without proof (a workflow stage) |
| [`goobers gather-ci-failures`](#goobers-gather-ci-failures) | add failing CI diagnostics to a remediation brief (a workflow stage) |
| [`goobers gather-implement-context`](#goobers-gather-implement-context) | load first-pass implementation review and hot-file context (a workflow stage) |
| [`goobers gather-issue-context`](#goobers-gather-issue-context) | add originating issue bodies to a remediation brief (a workflow stage) |
| [`goobers gather-pr-context`](#goobers-gather-pr-context) | pr-remediation entrypoint: select and load a PR's context (a workflow stage) |
| [`goobers gather-review-threads`](#goobers-gather-review-threads) | add native reviews and anchored inline threads to a remediation brief (a workflow stage) |
| [`goobers gather-sibling-context`](#goobers-gather-sibling-context) | load other open PRs as review evidence (a workflow stage) |
| [`goobers ios-simulator-test`](#goobers-ios-simulator-test) | run XCUITest on an iOS simulator and parse its xcresult (a workflow stage) |
| [`goobers issue-close-out`](#goobers-issue-close-out) | comment + close out the claimed issue (a workflow stage) |
| [`goobers mcp-io`](#goobers-mcp-io) | run the generic publish/read/list MCP server the harness spawns for a goober (a workflow stage) |
| [`goobers merge-pr`](#goobers-merge-pr) | conjunctive auto-merge via direct-merge or merge-queue (a workflow stage) |
| [`goobers merge-queue-poll`](#goobers-merge-queue-poll) | watch an enqueued PR until merged, evicted, timed out, or opted out (a workflow stage) |
| [`goobers open-pr`](#goobers-open-pr) | open or update the run's PR (a workflow stage) |
| [`goobers post-merge`](#goobers-post-merge) | post-merge fan-out + close the referenced issue (a workflow stage) |
| [`goobers pr-claim`](#goobers-pr-claim) | check PR liveness or release its remediation claim (a workflow stage) |
| [`goobers pr-comment-watch`](#goobers-pr-comment-watch) | label open goober PRs carrying unaddressed human comments (a workflow stage) |
| [`goobers pr-select`](#goobers-pr-select) | select one managed or advisory open PR for merge-review (a workflow stage) |
| [`goobers publish-batch`](#goobers-publish-batch) | publish a verified decomposition batch behind one eligibility barrier (a workflow stage) |
| [`goobers push-branch`](#goobers-push-branch) | push the worktree's checked-out branch to origin (a workflow stage) |
| [`goobers push-remediated`](#goobers-push-remediated) | force-push the remediated branch and clear needs-remediation (a workflow stage) |
| [`goobers rebase-pr`](#goobers-rebase-pr) | rebase-first, finding-driven remediation routing (a workflow stage) |
| [`goobers reconcile-branches`](#goobers-reconcile-branches) | report bounded stale goobers/* branch candidates (a workflow stage) |
| [`goobers reconcile-post-merge`](#goobers-reconcile-post-merge) | reconcile late merge-queue merges (a workflow stage) |
| [`goobers record-merge-refusal`](#goobers-record-merge-refusal) | record a merge refusal and demote a persistently-stuck lander (a workflow stage) |
| [`goobers remediation-checkpoint`](#goobers-remediation-checkpoint) | durable per-cause attempt budgets + same-diff escalation (a workflow stage) |
| [`goobers report-pr-status`](#goobers-report-pr-status) | publish goobers' verdict + CI evidence as a policy-gate-able PR status (a workflow stage) |
| [`goobers resolve-review-threads`](#goobers-resolve-review-threads) | reply to and resolve remediated native review threads (a workflow stage) |
| [`goobers respond-to-findings`](#goobers-respond-to-findings) | post a validated per-finding remediation response to the claimed PR (a workflow stage) |
| [`goobers select-source`](#goobers-select-source) | select and claim an unconsumed L6 decomposition disposition (a workflow stage) |
| [`goobers set-milestone`](#goobers-set-milestone) | assign an existing milestone to an issue (a workflow stage) |
| [`goobers telemetry-query`](#goobers-telemetry-query) | emit versioned candidate findings (a connector stage) |
| [`goobers update-behind-pr`](#goobers-update-behind-pr) | API-update a clean behind-base PR, else route to remediation (a workflow stage) |
| [`goobers validate-plan`](#goobers-validate-plan) | validate a decomposition plan against its selector artifact and the live parent (a workflow stage) |

## `goobers agent-kit`

install, inspect, or update the release-matched agent toolkit

~~~text
Usage: goobers agent-kit <subcommand> [flags] [path]

Install, inspect, or explicitly update the release-matched Goobers agent
toolkit in a checked-in configuration repository.

Subcommands:
  install  install product-owned assets and a minimal harness reference
  check    report installed version, drift, missing files, and updates
  update   show a reviewable diff, then write only with --write

Default path is ".". Targets must be repository roots and may not traverse
symbolic links or parent path segments.
~~~

**Examples**

~~~console
$ goobers agent-kit install --harness generic ./config-repo
$ goobers agent-kit check ./config-repo
~~~

## `goobers agent-kit check`

report agent toolkit version and drift

~~~text
Usage: goobers agent-kit check [path]

Compare the installed manifest and product-owned file digests with the toolkit
bundled in this binary. Report the exact bundle and binary release identities,
modified or missing owned files, and whether an explicit update is available.

Exit codes: 0 = current, 1 = drift, missing manifest, available update, or
inspection error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers agent-kit check ./config-repo
~~~

## `goobers agent-kit install`

install the release-matched agent toolkit

~~~text
Usage: goobers agent-kit install [--harness copilot|claude|generic] [path]

Install the toolkit bundled with this Goobers binary beneath the product-owned
`.goobers/agent-toolkit/` boundary and add a minimal adapter reference to the
selected harness instruction file. Existing instructions are preserved; install
only appends a clearly delimited managed reference when one is not already present.
Skills and other repository content are never overwritten.

Exit codes: 0 = installed or already current, 1 = unsafe target, collision, or
write error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers agent-kit install --harness copilot ./config-repo
~~~

## `goobers agent-kit update`

review or explicitly apply an agent toolkit update

~~~text
Usage: goobers agent-kit update [--dry-run | --write [--replace-modified]] [path]

Show a reviewable diff from the repository's current files to the toolkit
bundled in this binary. The default and --dry-run never write. --write applies
only manifest-owned changes and preserves user-created files. If the installed
manifest has semantic drift or an owned file differs from its installed digest,
--replace-modified is also required.

Exit codes: 0 = diff shown or update written, 1 = unsafe target, ownership
collision, unacknowledged modification, or write error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers agent-kit update ./config-repo
$ goobers agent-kit update --write ./config-repo
~~~

## `goobers apply`

reconcile a live daemon's workflow definitions now

~~~text
Usage: goobers apply [path]

Ask a live `goobers up` daemon to reconcile its workflow definitions
now, instead of waiting for the configured source's poll interval. For
a git-tracked workflowSource, first pulls
the tracked ref's latest commit; for a local-dir source, just forces an
immediate validate-and-reload of the config directory as it stands.

On success the daemon's live definitions swap to the new commit/edit
immediately. An invalid pulled/edited config is rejected: the daemon
keeps running its last-known-good definitions, and this command reports
the validation failure. Exit codes: 0 = applied or already current,
1 = no live daemon found or the pulled/edited config was rejected,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers apply
$ goobers apply ./instance
~~~

## `goobers apply-verdict`

publish a managed or advisory merge-review verdict (a workflow stage)

~~~text
Usage: goobers apply-verdict [--gate name] [path]

Read the holistic review gate's Verdict from this run's own journal,
cross-check its optional SHA echo against the deterministic review
pin, re-check that pin against the PR's current head/base, and — if
still valid — publish the verdict. Managed PRs receive a native GitHub
review plus the PR-comment handoff; non-pass verdicts also receive a
remediation label. advisoryMode=true publishes only the non-blocking
comment, without closing, labeling, electing, or merging the PR. A
stale SHA pin voids the verdict: no comment, no label, exit 0 (this
cycle's work is simply moot, not an error — merge-review re-reviews
next tick). Requires selectedNumber, selectedHeadSha, and
selectedBaseSha from Task.InputsFrom. Exit codes: 0 = applied (or
voided), 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers apply-verdict
~~~

## `goobers approve`

approve a paused or escalated gate

~~~text
Usage: goobers approve [--decision=pass] [--actor=<identity>] <run-id> <gate> [path]

Approve a paused human gate or an escalated human/reviewer gate. The daemon
records the authenticated actor, decision, and resulting resume in the run
journal. GOOBERS_API_TOKEN supplies a bearer token when API auth is enabled.

Exit codes: 0 = action accepted, 1 = action refused, 2 = usage/transport error.
~~~

**Examples**

~~~console
$ goobers approve <run-id> <gate>
~~~

## `goobers backlog-assignment`

assign eligible backlog items from a configured roster (a workflow stage)

~~~text
Usage: goobers backlog-assignment [path]

Assign eligible, open, unassigned backlog items using the configured
strategy and JSON roster. Supported strategies are constant-cap and
round-robin. The trustLabel input and a valid, non-empty roster are
required; invalid configuration fails before any provider mutation.

Exit codes: 0 = assignment pass completed, 1 = configuration/provider/IO
error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers backlog-assignment
~~~

## `goobers backlog-dedupe`

surface ranked duplicate candidates for curator judgment (a workflow stage)

~~~text
Usage: goobers backlog-dedupe [path]

Surface ranked likely-duplicate pairs for the current curation run. The
command compares this run's claimed issues against every open backlog item
using title/body similarity, shared closing references, external references,
and links. It writes a structured candidate artifact for curator judgment;
it never changes or closes an issue. A candidate's closeEligibleId is present
only when its newer issue belongs to this run's claimed, trusted batch;
unclaimed comparison issues are read-only evidence.

The maxCandidates stage input defaults to 20 and must be between 1 and 100.

Exit codes: 0 = candidate artifact written, 1 = config/credential/provider
error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers backlog-dedupe
~~~

## `goobers backlog-health`

snapshot ready-pool depth and age (a workflow stage)

~~~text
Usage: goobers backlog-health [--feedback] [path]

Snapshot ready-pool depth and age from provider label-event timestamps, and
persist the paginated ready-transition ledger for telemetry rollups.
The ledger resumes from a durable per-repo/label event cursor; a full history
scan runs only on the first cycle or an integrity mismatch, bounded by the
transitionScanMaxPages input. Below the transitionScanQuotaFloor fraction of
the provider rate-limit window the scan defers to the next cycle rather than
spending the shared credential to zero.
--feedback instead de-readies items whose consecutive failed/escalated
implementation runs meet the implementationFailureThreshold input (minimum 2).
Exit codes: 0 = OK, 1 = provider/IO error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers backlog-health
~~~

## `goobers backlog-query`

query/claim one eligible backlog item (a workflow stage)

~~~text
Usage: goobers backlog-query [--debug] [--read-only | --claim | --reconcile | --release] [path]

Query the provider for eligible backlog items — labeled with trustLabel
(SEC-047: required on public repos, since backlog content is untrusted
input otherwise), requireLabels, excludeLabels, and the optional
labelPredicate CEL expression. CEL supports string membership in `labels`
combined with &&, ||, and !. fieldPredicate adds typed comparisons against
provider-native scalar fields using fields["name"]; unavailable fields fail
explicitly. With --claim, claims
exactly one via the local claim ledger (source of truth) mirrored to a
provider-visible marker, and writes it to the declared result file.
trustLabel is required with --claim (SEC-047 fails closed, not open) —
a plain list (no --claim) does not require it. --read-only also bypasses
claim locks, blocked-record reconciliation, scan cursors, and read caches,
and uses only the github:issues:read capability.

--debug writes candidate eligibility, exclusion, and claim-loss details to
stderr. Diagnostics contain item IDs and selection metadata only; normal
output and claim behavior are unchanged.

With --release, removes the provider-visible claim marker and then releases
every claim this run holds in the local ledger (issues #234/#1003). A
workflow that only reads/labels an item, never opening a PR or closing the
issue — e.g. backlog-curation — must release its own claim explicitly,
since issue-close-out's release is reached only by implementation. Claims
require github:issues:write so the label mirror stays symmetric with the
ledger. Idempotent: releasing claims this run does not hold (already
released, e.g. re-run after a crash) is a no-op success, not an error.
With --reconcile, repairs drifted backlog labels against the claim ledger
and live issue/child state, then writes the actual correction count to the
declared result file. --claim, --reconcile, and --release are mutually
exclusive.

With --claim, contested-file dispatch awareness (#1085) deprioritizes
claiming an issue whose referenced files are already contested by
contestedFileMinPRs+ (default 2) open PRs, so new work isn't fed into an
overlap cluster faster than merge-review can drain it. It only reorders
candidates (never drops one — an all-contested cycle still claims FIFO)
and falls back to FIFO on any provider error. Disable with input
deprioritizeContestedFiles=false.

selectionPriority (#1335) is an opt-in, ordered comma-separated label
list (highest priority first, e.g. "security,bug") applied before FIFO:
eligible items carrying an earlier-listed label claim ahead of items
carrying only a later one or none at all; FIFO still breaks ties within
a priority tier. An item carrying more than one listed label ranks by
whichever appears earliest in selectionPriority. Unset (the default)
preserves plain FIFO exactly. fieldOrder is an optional comma-separated
field[:asc|desc] list applied within each label-priority tier before FIFO.

backlog-curation may opt into a bounded ready-item re-sweep with
resweepMaxItems. Forward candidates always consume maxItems first; a
re-sweep uses only leftover capacity, no more often than resweepInterval
(default 24h), and rotates within selectionPriority tiers. Ready items
already in implementation/review are emitted as read-only context and are
never claimed.

respectAssignee (#1820) is an opt-in claim-scoping flag, default off (the
assignee field is ignored entirely, exactly as today). Once true,
assignedTo selects the mode: a real identity (either a literal login, or
left undeclared so the runner's configured self identity fills it in at
run time) makes only items assigned to that identity eligible — assigned
to anyone else, or unassigned, is excluded. assignedTo declared with no
value is the null/unassigned-only mode: only items with no assignee at
all are eligible.

Exit codes: 0 = eligible item found (and claimed, if --claim) / released
(--release), 1 = business error (no eligible/claimable item, missing
trustLabel with --claim, config/credential/provider error), 2 =
usage/IO error.
~~~

**Examples**

~~~console
$ goobers backlog-query
$ goobers backlog-query --claim
~~~

## `goobers blocked`

inspect and clear the learned blocked-item ledger

~~~text
Usage: goobers blocked <command> [flags] [path]

Inspect and clear scheduler/blocked.json, the learned dependency-block
ledger (#552). Reads and writes go through the same claims.lock the
daemon holds, so they are safe against a concurrent tick.

Commands:
  list     print the current blocked-item records
  clear    remove one blocked-item record by item id
~~~

## `goobers blocked clear`

safely remove one blocked-item record, under claims.lock

~~~text
Usage: goobers blocked clear <item-id> [path]

Remove one scheduler/blocked.json record by item id (e.g. "955" or
"pr/955"), or by the owner/repo#N ref shown by `blocked list`
when the id is ambiguous. Default path is ".". Exit codes: 0 =
cleared, 1 = no unique record, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers blocked clear 955
~~~

## `goobers blocked list`

print the learned blocked-item ledger (scheduler/blocked.json)

~~~text
Usage: goobers blocked list [--json] [path]

Print one dependency line per scheduler/blocked.json record, snapshotted
under claims.lock. A single-repository ledger uses #N (or PR #N); when
multiple repositories are present, refs use owner/repo#N. --json emits a
stable ordered array with ref, kind (issue or pull_request), and blockedBy.
Default path is ".". Exit codes: 0 = printed, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers blocked list
$ goobers blocked list --json
~~~

## `goobers check-fail-first`

enforce fail-first evidence for a new workflow gate (a workflow stage)

~~~text
Usage: goobers check-fail-first [path]

Enforce TUT-A2's fail-first validation-authorship contract (#1214): any
new Workflow gate this run's branch adds under workflows/*.yaml must be
accompanied by fail-first evidence — a JSON file (default
fail-first-evidence.json, override with the evidenceFile input) proving
the new gate fails against the pre-change config and passes against the
post-change config. A branch that adds no gate passes trivially.
[path] defaults to the current directory (the stage's worktree).
Exit codes: 0 = no new gate, or every new gate has valid fail-first
evidence; 1 = a new gate lacks evidence; 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers check-fail-first
~~~

## `goobers check-issue-staleness`

route a PR to remediation if its linked issue changed since implementation began (a workflow stage)

~~~text
Usage: goobers check-issue-staleness [path]

Re-fetch the PR's pinned linked issue and compare its title and body
against the snapshot taken when the PR was opened. If the issue spec
changed since implementation began, label the PR goobers:needs-
remediation and post an explanatory comment instead of letting review
proceed against stale copied criteria. A PR with no pin (predates this
feature, or its linked issue never resolved an updatedAt) is never
considered stale — there is nothing to compare against. Declared
inputs: pullNumber (required), head/base/advisoryMode (passed through
unchanged for the next stage's inputsFrom). Writes
issueStale/number/head/base/advisoryMode to the declared result file.
Exit codes: 0 = evaluated, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers check-issue-staleness
~~~

## `goobers claims`

inspect and force-release claim leases

~~~text
Usage: goobers claims <command> [flags] [path]

Inspect and force-release scheduler/claims.json without racing a live
daemon. Operations delegate to `goobers up` when it is running.

Commands:
  list       print current claim leases
  release    force-release one item by id
~~~

## `goobers claims list`

print current claim leases, optionally only expired leases

~~~text
Usage: goobers claims list [--json] [--stale] [--gaggle=name] [--provider=name] [path]

Print item id, gaggle, provider, run id, workflow, claimed-at, and
expires-at for each claim. Filters may be combined. Default path is ".".
~~~

**Examples**

~~~console
$ goobers claims list
$ goobers claims list --stale
~~~

## `goobers claims release`

force-release a claim through the live daemon or claims.lock

~~~text
Usage: goobers claims release [--force] [--gaggle=name --provider=name] <item-id> [path]

Print the claim holder, age, and expiry, then force-release the item.
--force is required while the holding run is non-terminal. The override
is recorded as claim.force_released in the instance journal. Default
path is ".". Scope flags are required when the item id exists in more
than one namespace. Exit codes: 0 = released, 1 = refused/not found/
ambiguous, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers claims release 955
$ goobers claims release --force 955
~~~

## `goobers completion`

generate a shell completion script

~~~text
Usage: goobers completion <bash|zsh|fish|powershell>

Generate a shell completion script. Source the output in the target shell.
~~~

**Examples**

~~~console
$ goobers completion bash
$ goobers completion zsh
$ goobers completion powershell
~~~

## `goobers completion bash`

generate a bash completion script

~~~text
Usage: goobers completion <bash|zsh|fish|powershell>

Generate a shell completion script. Source the output in the target shell.
~~~

## `goobers completion fish`

generate a fish completion script

~~~text
Usage: goobers completion <bash|zsh|fish|powershell>

Generate a shell completion script. Source the output in the target shell.
~~~

## `goobers completion powershell`

generate a PowerShell completion script

~~~text
Usage: goobers completion <bash|zsh|fish|powershell>

Generate a shell completion script. Source the output in the target shell.
~~~

## `goobers completion zsh`

generate a zsh completion script

~~~text
Usage: goobers completion <bash|zsh|fish|powershell>

Generate a shell completion script. Source the output in the target shell.
~~~

## `goobers config`

inspect, materialize, and compare instance configuration

~~~text
Usage: goobers config <subcommand> [flags] [path]

Inspect, materialize, and compare instance configuration.

Subcommands:
  show         render the effective instance config (secrets redacted)
  materialize  apply the recorded checked-in source to the runtime instance
  diff         compare active workflows with the shipped canonical workflows

Run `goobers config show -h`, `goobers config materialize -h`, or
`goobers config diff -h` for details.
Default path is ".".
~~~

**Examples**

~~~console
$ goobers config show
$ goobers config materialize ./instance
$ goobers config diff ./instance
~~~

## `goobers config diff`

compare active workflows with canonical definitions

~~~text
Usage: goobers config diff [--against <canonical-root>] [instance-root]

Compare the active workflows under <instance-root>/config with a canonical
config source tree. The canonical root defaults to ./reference-workflows; use --against
when running outside the Goobers source checkout or comparing another set.

Schedule, maxConcurrentRuns, maxRunsPerHour, maxRunsPerDay, maxOpenPRs,
and trigger presence (enablement) are operational tuning: they are printed
as INFO and do not fail the command. Every other workflow difference is
structural drift, printed as ERROR with active and canonical values.

Exit codes: 0 = structurally identical (informational tuning is allowed),
1 = structural drift or invalid config, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers config diff ./instance
$ goobers config diff --against ./reference-workflows ./instance
~~~

## `goobers config materialize`

apply the recorded checked-in source to the runtime instance

~~~text
Usage: goobers config materialize [instance-root]

Validate and apply the checked-in local config source recorded in
<instance-root>/instance.yaml. The source's instance.yaml.example becomes
the runtime instance.yaml (with workflowSource retained), and manifest.yaml
plus gaggles/ replace the runtime config/ tree. Journals, scheduler data,
workcopies, credential values, and telemetry remain untouched.

Stop the daemon before materializing, then restart it to use the new
definitions. GitHub-backed guided sources use their local checkout, so pull
the desired revision into that checkout first.

Exit codes: 0 = materialized, 1 = invalid source or apply error, 2 = usage
error or not an instance root.
~~~

**Examples**

~~~console
$ goobers config materialize
$ goobers config materialize ./instance
~~~

## `goobers config show`

render the effective instance config (secrets redacted)

~~~text
Usage: goobers config show [--json] [path]

Render the effective instance configuration loaded from <path>/instance.yaml
(repos, API/webhook bind config, run conditions, credential grants, timezone),
as YAML by default or as JSON with --json.

Secrets are redacted by construction: a token reference is only ever a
locator — the env var name or file path the secret is read from at runtime
(CFG-009/SEC-010 forbid inline values) — so `config show` prints where each
credential comes from and never reads or prints the secret value itself.

Exit codes: 0 = OK, 1 = load/render error, 2 = usage error or not an
instance root.
~~~

**Examples**

~~~console
$ goobers config show
$ goobers config show --json
~~~

## `goobers connect`

connect an instance to your own GitHub repository

~~~text
Usage: goobers connect <owner>/<repo> [--token-env NAME] [--seed] [--replace] [--json] [path]

Connect an instance to your own GitHub repository — the connect rung of the
onboarding ladder. The command rewrites the template placeholders
(your-org/your-repo) in instance.yaml repos[] and in every materialized
gaggle's project and backlog under config/gaggles/, then validates the
result in-process. Configuration already pointing at a real repository is
left alone (and reported skipped) unless --replace is set.

Credentials are recorded by NAME only: --token-env stores the environment
variable name (default GOOBERS_GITHUB_TOKEN) in the repo's token
reference. Token values never pass through this command; a value that looks
like a pasted token is rejected.

Only GitHub repositories can be connected. An Azure DevOps identity (an
organization/project/repository slug or a dev.azure.com URL) is refused
with the instance.yaml block to write by hand — see
docs/guides/ado-authentication.md.

--seed derives two label sets from the connected gaggles and idempotently
ensures every one of them exists on the repository: the backlog SELECTORS
(backlog labels plus each workflow's trustLabel/requireLabels inputs) and
the labels those workflows WRITE or exclude on (the goobers:claimed claim
mirror, each issue-close-out stage's park/status label, readyLabel inputs,
and every excludeLabels entry) — a run dies at its first park or close-out
when those do not exist yet. The one safe starter issue it files carries
the selector labels only, never the lifecycle ones. Seeding uses the same
--token-env; when that variable is unset the issue is reported pending and
the local rewrite still completes.

When the token variable is set, the target repository's reachability is
checked with the exact credential path a real run would use BEFORE any
file is written, and a failed connect leaves the instance exactly as it
was. After a successful connect the same credential reports how many of
the repository's open issues your backlog selectors currently match.

Flags:
  --token-env <name>  repository token environment variable name (default GOOBERS_GITHUB_TOKEN)
  --seed              ensure selector labels + one starter issue on the repository
  --replace           also rewrite entries already pointing at a real repository
  --json              emit the versioned onboarding action envelope

Exit codes: 0 = connected (or already connected), 1 = refusal, validation,
or provider error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers connect acme/web ./my-instance
$ goobers connect acme/web --token-env MY_GITHUB_TOKEN --seed ./my-instance
$ goobers connect acme/web --json ./my-instance
~~~

## `goobers dashboard`

serve and open the local operations portal

~~~text
Usage: goobers dashboard [--port=<port|auto>] [--listen=<host:port>] [--wait-for-daemon[=<duration>]] [--no-open] [--dev-assets=<dir>] [path]

Serve the embedded portal against the live daemon when `goobers up` is
running, or against a standalone read-only service otherwise. The default
port is 8081; --port=auto increments from there until a port is available.
--wait-for-daemon optionally waits up to 30s for a concurrently starting
daemon; use --wait-for-daemon=<duration> to choose another bound.
--listen overrides the full bind address (host:port) and takes the place
of --port when given; binding a non-loopback host requires api.auth to be
configured in instance.yaml (SEC-043) — there is no insecure override.
Blocks until interrupted. Exit codes: 0 = clean shutdown, 1 = service or
browser failure, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers dashboard
$ goobers dashboard --port=auto --no-open
~~~

## `goobers docs-churn`

emit the docs-drift churn digest since the watermark (a connector stage)

~~~text
Usage: goobers docs-churn [--repo <dir>] [--workflow <name>] [--gaggle <name>] [--since <duration>] [--buffer-multiplier <float>] [--format churn-digest] [path]

Report the code churn since docs were last refreshed for the docs-updater
workflow (#1015). Reads and advances a durable per-(gaggle,workflow)
watermark under the instance's scheduler dir, and writes a versioned
churn-digest to GOOBERS_INPUT_resultFile when declared, else stdout.
[path] is the instance root (default $GOOBERS_INSTANCE_ROOT, else ".").

Exit codes: 0 = OK (including a clean no-work result), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers docs-churn --format churn-digest
~~~

## `goobers doctor`

preflight a Kubernetes cluster, repository forge policy, or Windows antivirus exclusions

~~~text
Usage: goobers doctor --k8s [--kubeconfig <path>] [--context <name>] [--report text|json]
                          [--oidc-issuer <url>] [--registry <host>] [--egress <host:port,...>]
                          [--timeout <duration>]
       goobers doctor --repo [--report text|json] [instance-root]
       goobers doctor --av-exclusions [--report text|json] [--work-root <dir>] [instance-root]

--k8s preflights a target Kubernetes cluster against the documented
infrastructure shape (docs/design/k8s-infra-shape.md) before installing
Goobers on it — the install-time enforcement of that document (#668).

The --k8s check set, each row citing the shape-doc section it enforces:

  cluster-version    required  §1     cluster reachable, supported version
  networkpolicy-api  required  §5     NetworkPolicy API served (warn: enforcement unverified)
  rbac-install       required  §1/§3  permissions to install goobers-system
  rbac-gaggle        required  §3/§5  permissions to stamp per-gaggle namespaces
  storage-rwx        required  §4     ReadWriteMany-capable StorageClass exists
  mixed-os-placement required  §7     Linux workloads cannot land on Windows nodes
  oidc-issuer        required* §1/§3  issuer discovery document reachable
  egress             required* §1/§5  outbound targets reachable from this host
  registry           optional  §1     registry reachable (host-side sanity)

Checks marked required* apply when their probe target is configured; left
unconfigured they report a skipped warn. Every check is read-only: nothing is
created on the cluster, and a check that cannot run reports fail with the
reason — never a silent pass. Reference manifests expressing the same
requirements live under deploy/reference/ (#663).

networkpolicy-api warns even when the API is served: a served API is only a
correlate of enforcement — a CNI can serve it and still ignore policies
silently. This check is API-discovery only; enforcement can only be proven
by a denied attempt from an in-cluster negative control, never by doctor
--k8s alone.

--repo diffs each configured repo's declared forge-policy manifest
(<instance-root>/instance.yaml repos[].policy: required merge method,
merge-queue requirement, required status checks — issue #916, Tier 4 of
#903) against its live GitHub state. Repos with no policy declared are
skipped. Token-scope introspection is reported as unavailable when GitHub
does not expose it (fine-grained PAT / GitHub App tokens) — never inferred
from a failed call. instance-root defaults to ".".

--av-exclusions lists every directory Goobers writes and immediately reads
back — the set real-time antivirus scanning on Windows must exclude, or a
scan holding a handle on a just-written file surfaces minutes later as an
unrelated git "Permission denied" (#3480, #3161–#3164). The list is
derived from the same path code the daemon (instance root, run journals,
scheduler ledger, blob store, workcopies, TEMP), the worker (--work-root,
which applies to this mode only, and its workcopies/scratch subtrees) and
a Windows stage pod (C:\workspace, the tmp:ephemeral TEMP, the container
user's profile) actually use, so it cannot drift from what the binary
writes. Each gaggle's own spec.workcopies.root is enumerated separately,
since it beats the instance-wide one and may name any drive; when config/
cannot be read, that is reported rather than passed off as no gaggles.
On a Windows host it also reads
Microsoft Defender's exclusion list (Get-MpPreference, read-only) and
reports each directory as excluded, not-excluded, or unknown; elsewhere
it lists the set and reports unknown. ADVISORY: exit 0 whatever the
coverage — an organisation-wide AV policy is the operator's to set, and
nothing here changes it. Declare the answer on each windows runner as
provides.windows.avExclusionsVerified (validate warns RNR006 without it).

--report json emits the stable machine-readable report; text (default)
prints a human-readable table (--k8s), per-repo findings (--repo), or the
per-directory coverage list (--av-exclusions).

Exit codes: 0 = conformant (warns allowed for --k8s; always for
--av-exclusions), 1 = a required check failed or drift was found,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers doctor --k8s
$ goobers doctor --k8s --report json --oidc-issuer https://login.example.com/tenant/v2.0
$ goobers doctor --av-exclusions --report json ./instance
~~~

## `goobers down`

request a live daemon's graceful drain-shutdown from a separate terminal

~~~text
Usage: goobers down [path]

Request a live `goobers up` daemon for this instance to perform the
same graceful drain-shutdown SIGINT/SIGTERM already trigger, from a
separate terminal or from automation. No daemon HTTP API, port, or
auth surface is required or added. Exits 0 once the shutdown request
has been delivered — the daemon picks it up and begins draining on its
next sweep. With no live daemon for this instance, fails fast with a
clear message rather than hanging. Exit codes: 0 = shutdown requested,
1 = no live daemon found, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers down
$ goobers down ./instance
~~~

## `goobers e2e`

check the Goobernetes distributed e2e proof harness's assertions against a recorded run

~~~text
Usage: goobers e2e verify [flags]
       goobers e2e kill-inject [flags]

verify:      check the Goobernetes S1-S9 distributed e2e proof harness's
             assertions (#3517) against one already-completed run's
             recorded data.
kill-inject: perform one live S6 kill-matrix cell (pod-kill) against a
             real cluster (#3513).
~~~

## `goobers e2e kill-inject`

perform one live S6 kill-matrix cell (pod-kill) against a real cluster

~~~text
Usage: goobers e2e kill-inject --run <run-id> --stage <stage-name>
       --stage-class builtin --namespace <k8s-namespace>
       [--poll-timeout <duration>] [--out <record.json>]

Perform ONE live S6 kill-matrix cell (#3513/#3517, goobernetes-smoke.md §4
S6) against a real cluster: pod-kill only, one stage class per invocation.
This is the minimal live CellDriver implementation — it does not run the
full six-cell matrix (RunKillMatrix), and it does not implement node-kill.

--stage names the REAL workflow stage (e.g. "probe-builtin") whose currently
running pod should be killed; --stage-class records which of S6's three
abstract classes (builtin, agentic, local-ci) that stage plays, for the
emitted record only.

Finds the target pod by label selector (goobers.dev/run=<run-id>,
goobers.dev/stage=<stage>) — the same labels every dispatcher-created stage
pod carries (internal/dispatcher.LabelRun/LabelStage) — NOT by asking the
daemon: its read API defaults to loopback-only and cannot be reached from
outside its own pod. No kubectl exec anywhere.

Procedure:
  1. Poll for a Running pod matching the label selector.
  2. Delete it via the Kubernetes API (client-go — in-cluster config when
     running as a pod, else standard kubeconfig loading), in --namespace.
  3. Poll the same selector for a DIFFERENT pod (the dispatcher's retry) to
     appear.
  4. Emit one internal/e2e.CellInjectionRecord as JSON — to --out, or
     stdout. InterruptedAttempt/SuccessorAttempt are built from pod data
     only (name, labels, phase, node) — thinner than the journal-sourced
     StageAttempt goobers e2e verify produces, and RunCompletedSuccessfully
     is always false here: no pod carries the overall run's terminal phase,
     only its own stage's outcome, so this command does not guess it.
     Rendering S6's actual pass/fail verdict needs this record combined with
     the run's journal-sourced data separately.

--poll-timeout bounds BOTH polling phases independently (default 10m each) —
this command fails loudly on timeout rather than hanging forever; a partial
record (injection performed, no successor observed yet) is still emitted
with RunCompletedSuccessfully=false, since D5 requires every injection be
recorded even when the retry hasn't landed by the time this command gives up.

Exit codes:
  0 = injection performed and a successor pod was observed
  1 = injection performed but no successor pod appeared within
      --poll-timeout — the record is still emitted
  2 = usage / IO / Kubernetes API error, or the target pod never appeared
      within --poll-timeout (nothing was injected, no record produced)
~~~

**Examples**

~~~console
$ goobers e2e kill-inject --run <run-id> --stage probe-builtin --stage-class builtin --namespace gaggle-goobers
~~~

## `goobers e2e verify`

verify the Goobernetes S1-S9 e2e proof harness against one completed run's recorded data

~~~text
Usage: goobers e2e verify --run <run-id> [--gaggle <name>] [--expected <topology.json>] [--out <bundle.json>] [path]
       goobers e2e verify --print-runner-class <restriction[,restriction...]>

Verify the Goobernetes S1-S9 distributed e2e proof harness's assertions
(#3517, docs/design/goobernetes-smoke.md) against one already-completed
run's recorded data — StageAttempt placement provenance and the run
journal, fetched the same way `goobers trace` and `goobers runs list` do. This
command drives no cluster, applies no workflow, and kills nothing. It
verifies a run that already happened; a separate, infra-side orchestration
(outside this repo, likely shell) produces the run and calls this command
afterward.

PURE FUNCTION, RE-RUNNABLE FROM ITSELF (goobernetes-smoke.md §5 rule 4):
every verdict is computed from the run's on-disk journal (via the read
side), --expected, and whatever captures --expected inlines — never a
cluster, network, or other live dependency. The emitted bundle records
--expected's own path (Collateral.S8CapturePath/S9ProbeOutputPath, when
those items were checked) alongside the run's journal directory
(Collateral.RunJournalPaths), so every verdict is re-derivable offline
from the bundle plus those two paths alone.

Always checked, from the run's recorded data alone:
  S1        fresh pod per stage attempt, never reused
  S2        at least one Linux and one Windows stage attempt, run completed

Checked only when --expected supplies the needed data; skipped (not
recorded as a failure) otherwise:
  S8        a live portal/SSE observation of a stage transition, timestamped
            before the run's terminal event (needs "liveVisibility"). A
            JOURNAL timestamp cannot stand in for this: a closed-run
            projection has the same early stage.started/stage.finished
            times a genuine live capture would, so it is blind to exactly
            the failure mode S8 exists to catch (terminal-only visibility)
            — per goobernetes-smoke.md §5 rule 2, an observer that was not
            actually exercised is never a pass. Supply the consumer's own
            recorded portal/SSE capture, or S8 is skipped, never passed.
  ARCH11-7  no ledger-touching stage attempt placed on Windows (needs
            "ledgerTouchingStages")
  ARCH11-8  a declared capability gap was caught (needs "capabilityGap")
  S9        the network:allowlist negative-control triple (needs
            "negativeControl")

--expected DECLARES INTENDED TOPOLOGY — what the operator meant to deploy,
or what the workflow's own DSL declares should happen — never a restatement
of the run's actual recorded data (that would be circular: checking the
run against a description of itself). A mismatch between --expected and
the recorded run is a real FINDING (an item FAILS), never a usage error:

  {
    "liveVisibility": {
      "source": "portal",
      "observations": [
        {"stage": "implement", "transition": "started", "observedAt": "2026-08-01T12:00:01Z"}
      ]
    },
    "ledgerTouchingStages": ["implement"],
    "capabilityGap": {
      "wantUnsatStage": "windows-only-stage",
      "unsatisfiableStages": [
        {"stage": "windows-only-stage", "kind": "requirement", "diagnostic": "..."}
      ]
    },
    "negativeControl": {
      "denial":                            {"endpoint": "blocked.example.com:443", "exitCode": 28},
      "positiveControl":                   {"endpoint": "allowed.example.com:443", "exitCode": 0},
      "modelEndpoints":                    ["api.anthropic.com"],
      "controlVantage":                    {"endpoint": "blocked.example.com:443", "exitCode": 0},
      "restrictedRunnerRestrictions":      ["network:allowlist"],
      "controlVantageRunnerRestrictions":  []
    }
  }

negativeControl's two "...Restrictions" fields name the RESTRICTION EFFECTS
(the same closed vocabulary a runners: inventory entry declares) of the
Denial/PositiveControl leg's restricted runner and the ControlVantage leg's
second runner, not a runner-class LABEL VALUE — this command GENERATES that
value itself via internal/runnercap.RunnerClassValue (delivery decision
015, the dispatcher's own stamping function), the same way
--print-runner-class does, so a topology file never hand-transcribes a
class string that could drift from what the dispatcher actually stamps.
Use --print-runner-class <restriction[,restriction...]> to see the derived
value before authoring a topology file; it needs no --run, no instance
root, and touches no cluster.

S3-S7 (declared-edge handoff, artifact materialization, repass, write-API
trigger/escalation, and the S6 kill matrix) need a live topology-driving
orchestration to produce their evidence and are not checked by this
command — see internal/e2e's per-item doc comments.

--out writes the evidence bundle (internal/e2e.Bundle, JSON) there; default
is stdout. Exit codes distinguish "the design/system is broken" from "the
observer machinery is broken" — opposite responses (D4/§5 rule 2's
invalid-is-never-a-pass rule applied to the driver's own control flow):
  0 = every checked item PASSED
  1 = at least one item FAILED (act on the design — this wins even if an
      INVALID item is also present; it still appears in the bundle)
  2 = usage / IO / --expected parse error / run or gaggle did not resolve
      (the command itself could not run at all — no bundle was produced)
  3 = at least one item was INVALID and none FAILED (the observer
      machinery broke, not necessarily the product — fix instrumentation
      and re-run; includes an item --expected asked this invocation to
      check but that came back unproven)
~~~

**Examples**

~~~console
$ goobers e2e verify --run <run-id>
$ goobers e2e verify --run <run-id> --expected topology.json --out bundle.json
$ goobers e2e verify --print-runner-class network:allowlist
~~~

## `goobers elect-lander`

elect the landing PR among a merge-review cohort (a workflow stage)

~~~text
Usage: goobers elect-lander [--gate name] [path]

Read the holistic review gate's Verdict from this run's journal and, when
it is entirely cross-PR-ordering asks and the selected PR is the elected
lander of its overlap cluster (lowest PR number), emit elected=true to
route the PR into merge-pr; otherwise emit elected=false to route it to
apply-verdict. Advisory-mode PRs are never elected. Requires selectedNumber
and advisoryMode (inputsFrom gather-sibling-context).
Exit codes: 0 = decided (elected or not — both normal), 1 = business
error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers elect-lander
~~~

## `goobers engine-project`

write a completed engine run's journal into the instance (experimental)

~~~text
Usage: goobers engine-project [flags] <run-id> [path]

Write a completed engine run's standard journal into the instance.

Exit codes: 0 = projected or already present, 1 = query/write failure,
2 = usage/config error.
~~~

**Examples**

~~~console
$ goobers engine-project --gaggle example <run-id>
~~~

## `goobers engine-queues`

report which workers poll this instance's engine and dispatch task queues (experimental)

~~~text
Usage: goobers engine-queues [flags] [path]

Report which workers poll this instance's engine task queues (experimental).
The queue set is derived, not typed: the engine's workflow queue plus every
goobers-dispatch.<gaggle>.<runner> queue the runners: inventory and the
declared gaggles imply — the same derivation `goobers worker
--dispatch-namespace` serves. Each queue is described for both task types
(workflow and activity) and every poller's Temporal identity is printed.

This is the evidence for the queue-ownership check: goobers-worker must poll
the workflow queue and every dispatch queue, and goobers-api must poll none
of them — the daemon dispatches as a Temporal client and is never a pod
creator.

Flags:
  --temporal-hostport <h:p>  Temporal frontend (default engine.hostPort)
  --temporal-namespace <ns>  Temporal namespace (default engine.namespace)
  --task-queue <queue>       workflow queue to describe (default
                             engine.taskQueue)
  --timeout <duration>       bound on the whole describe (default 30s)
  --json                     emit the report as JSON

Exit codes: 0 = described, 1 = describe/connection failure, 2 = usage/config
error.
~~~

**Examples**

~~~console
$ goobers engine-queues
$ goobers engine-queues --json
~~~

## `goobers engine-start`

dispatch one run onto the tier-3 engine via Temporal (experimental)

~~~text
Usage: goobers engine-start [flags] <workflow> [path]

Dispatch one run onto the tier-3 engine (experimental).

When a `goobers up` daemon holds this instance's lock the dispatch is
DELEGATED to it: the daemon admits the run through the scheduler (so it
takes a concurrency slot, records an instance-log run.started, reserves
the run journal and fires the terminal hooks on completion) and starts
the workflow through its own engine starter. The run id is the
scheduler's, and --dedupe-key is refused, because the daemon mints a
fresh run id per admission.

--direct bypasses the daemon and starts the workflow straight on
Temporal with REJECT_DUPLICATE, deriving the run id from gaggle,
workflow and --dedupe-key. That is the only mode in which --dedupe-key
means anything: a direct start's run id IS its dedupe unit, whereas a
delegated dispatch dedupes DELIVERIES (by request id) and not work.
A direct start takes no scheduler slot and fires no terminal hooks.
--direct is implied when no daemon is running.

--live-journal pins live journal authorship into the run: workers emit
journal events through the daemon's journal plane as they happen, so the
run is visible mid-flight; without it the journal is projected from
history at close, as before. Requires the daemon's write API to be
reachable from every worker serving the run (worker --daemon-api).

Exit codes: 0 = started, 1 = dispatch failure, 2 = usage/config error.
~~~

**Examples**

~~~console
$ goobers engine-start default-implement
~~~

## `goobers escalations`

list escalated runs newest first

~~~text
Usage: goobers escalations [--json] [path]
       goobers escalations show [--json] [--include-verdict] <run-id> [path]

List escalated runs newest first. Use `escalations show` to inspect an
escalation cause and the artifacts available before and after each stage.
~~~

**Examples**

~~~console
$ goobers escalations
$ goobers escalations --json
~~~

## `goobers escalations show`

show escalation cause, verdict, and per-stage artifact timeline

~~~text
Usage: goobers escalations show [--json] [--include-verdict] <run-id> [path]

Show an escalation's structured cause and per-stage artifact timeline.
Use --include-verdict to include reviewer verdict rationale and findings.
~~~

**Examples**

~~~console
$ goobers escalations show <run-id>
$ goobers escalations show --include-verdict <run-id>
~~~

## `goobers examples`

browse canonical workflow examples embedded in the binary

~~~text
Usage: goobers examples <list|show> [name]

Browse the canonical workflow examples embedded in this binary. No source
checkout or instance root is required.

Commands:
  list         print the available example names
  show <name>  print an example's exact Workflow YAML

Run `goobers examples list -h` or `goobers examples show -h` for details.
~~~

**Examples**

~~~console
$ goobers examples list
$ goobers examples show implementation
~~~

## `goobers examples list`

list canonical embedded workflow examples

~~~text
Usage: goobers examples list

Print the names of the canonical embedded workflow examples, one per line.
Pass one of these names to `goobers examples show` to print its YAML.

Exit codes: 0 = listed, 1 = embedded catalog error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers examples list
~~~

## `goobers examples show`

print a canonical embedded workflow example

~~~text
Usage: goobers examples show <name>

Print the exact canonical Workflow YAML embedded in this binary. Use
`goobers examples list` to discover names. No source checkout or instance
root is required.

Exit codes: 0 = printed, 1 = unknown name or embedded catalog error,
2 = usage error.
~~~

**Examples**

~~~console
$ goobers examples show implementation
~~~

## `goobers explain`

project field facts from an embedded JSON Schema

~~~text
Usage: goobers explain [--human] <selector>

Project field guidance from the embedded schema and feature registries using
a dotted or slash-
separated selector such as goober.spec.capabilities or
workflow/spec/gates[]/evaluator. Array elements use []. Output includes the
field purpose, type, allowed values, lifecycle, and a schema-grounded example.
JSON is the default; --human prints a terminal rendering. Exit codes: 0 = OK,
1 = unknown selector or output error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers explain goober.spec.capabilities
$ goobers explain --human workflow.spec.gates[].evaluator
~~~

## `goobers features`

list the workflow-DSL features this build supports

~~~text
Usage: goobers features [--json] [--dsl-version <version>] [--used] [path]

List the workflow-DSL features this build understands by DSL version,
including each feature's support level (preview/ga/deprecated/removed).
Use --dsl-version to scope the matrix to one declared version. This reads
the same registry and SupportMatrix the committed
docs/feature-matrix.md is generated from, so the two never disagree.

With --json, emit a versioned feature-discovery envelope instead of the
human-readable table. With --used, list only the features the instance at path (default ".")
actually references across its workflows and goobers — the subset that
instance's config exercises. Exit codes: 0 = OK, 1 = invalid instance
config, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers features
$ goobers features --json --dsl-version 2.0
$ goobers features --used
~~~

## `goobers file-issues`

file a validated nominations artifact as deduped, budgeted issues (a workflow stage)

~~~text
Usage: goobers file-issues [--check] [path]

file-issues is the nomination workflows' deterministic issue filer —
TBH-1 #2251's first slice, built on the decomposition binding (a typed
goobers.dev/nominations/v1 artifact, a digest-bound check, a publisher
that owns every goobers:* label). The finder proposes area/type labels
and evidence; this stage dedupes by body marker against every issue
carrying the nominated label, excludes anything flake-watch already
fingerprints, enforces maxPerRun, and creates issues with a retry-safe
idempotency key.

goobers:approved (the SEC-047 trust label) is applied on one condition
only (decision 004): the nomination's evidence names a finding — a go
vet diagnostic, a golangci-lint issue (linter + file + line) or a go
test failure (package + test) — that the deterministic signalsStage's
stdout artifact of THIS run contains byte for byte, as parsed by this
stage from the run journal; plus riskClass low, type:bug, one package,
no load-bearing path, no needs-human trigger, and no open or recently
closed nominated issue naming the same finding (a body marker the
filer computes from the finding, not the model's dedupeKey), nor an
earlier nomination of the same artifact naming it. autoApprove=
deterministic-only (exactly; default never) opts in and the label is
added with the github:issues:approve credential only.
Everything else files unapproved with the reasons in the result. On a
stage pod the run journal is unreachable, so nothing is approved.

With --check, only validate the artifact and run the read-only dedupe
scan (github:issues:read); nothing is created. The write path must be
bound to a --check that marked this artifact valid: wire the check
stage's nominationsDigest output to the checkDigest input (inputsFrom),
or point checkFile at its result, or run on a self runner where the
checkStage's recorded result is in the run journal.

Inputs: nominationsFile (nominations.json), producerStage (triage),
backlogLabel (required), partitionLabel (required), maxPerRun (3),
dedupeWindowDays (21), nominatedLabel, autoApprove (never), signalsStage
(collect-repo-signals), checkDigest, checkFile (nomination-check.json),
checkStage (validate-nominations), resultFile (filed-nominations.json).
Exit codes: 0 = filed or checked / 1 = business or provider error / 2 = usage error.
~~~

**Examples**

~~~console
$ goobers file-issues --check
$ goobers file-issues
~~~

## `goobers fix`

mechanically migrate workflows to a target dslVersion, one step at a time (DVL-6)

~~~text
Usage: goobers fix --to <version> [--write] [path]

Mechanically migrate every workflow in a config directory (default path
".") from its current dslVersion to <version>, one registered version
step at a time (DVL-6). Prints a reviewable unified diff per changed
workflow file by default; --write applies the diff to each file in
place instead. Refuses a workflow that is already at <version>, and
refuses any jump for which no direct one-step migration is registered —
chain multiple `fix` invocations for a multi-step upgrade, never a
silent multi-step rewrite. Never runs automatically; this is always an
author-run, reviewable change. Exit codes: 0 = migrated (or nothing to
migrate), 1 = one or more workflows could not be migrated,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers fix --to 2.0
$ goobers fix --to 2.0 --write ./instance
~~~

## `goobers fleet`

associate this instance with a Fleet service

~~~text
Usage: goobers fleet <join|status|leave> [flags] [path]

Associate a local Goobers instance with a Fleet service, inspect its durable
connection state, or remove the association. Fleet identity and credentials
are stored outside the instance root so copying an instance does not clone its
identity.
~~~

**Examples**

~~~console
$ goobers fleet join --url https://fleet.example
$ goobers fleet status
$ goobers fleet leave
~~~

## `goobers fleet join`

discover and enroll this instance with a Fleet service

~~~text
Usage: goobers fleet join --url <url> [--enrollment-token-file <path>] [--grant-local-admin | --no-grant-local-admin] [path]

Discover and enroll an instance with a Fleet service. By default the one-time
enrollment grant is read from a protected terminal prompt and never accepted
as a command-line value. --enrollment-token-file supports automation and is
accepted only when the file is private to its owner.

When discovery advertises a local administrator principal, interactive use
offers an explicit instance:read self-grant. Noninteractive use must choose
--grant-local-admin or --no-grant-local-admin. An empty ACL always requires an
explicit opt-out or interactive confirmation.
~~~

**Examples**

~~~console
$ goobers fleet join --url https://fleet.example
$ goobers fleet join --url https://fleet.example --enrollment-token-file ./grant.txt --grant-local-admin
~~~

## `goobers fleet leave`

remove this instance's Fleet association and protected secrets

~~~text
Usage: goobers fleet leave [path]

Remove the Fleet association, private key, and bearer credential. A running
daemon observes the removal and stops reconnecting.
~~~

**Examples**

~~~console
$ goobers fleet leave
~~~

## `goobers fleet status`

show durable Fleet registration and connection state

~~~text
Usage: goobers fleet status [--json] [path]

Show the durable Fleet registration, connection, heartbeat, ACL version, and
credential expiry state. Private key and bearer credential material are never
printed.
~~~

**Examples**

~~~console
$ goobers fleet status
$ goobers fleet status --json
~~~

## `goobers gate-removal-guard`

block a tutor run that removes/loosens its own flagged gate without proof (a workflow stage)

~~~text
Usage: goobers gate-removal-guard [path]

Block a tutor run whose drafted change removes or loosens the specific
gate its own finding flagged as noisy, unless the finding cites
independent proof the gate is dead. Runs after draft-change, before
validate-config, reading the analyze stage's finding.md from the run
journal and diffing every changed workflow YAML file against base.
A finding unrelated to gate noise, or a diff that doesn't touch the
named gate, is a no-op pass-through.
Exit codes: 0 = clear (or nothing to check), 1 = blocked, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers gate-removal-guard
~~~

## `goobers gather-ci-failures`

add failing CI diagnostics to a remediation brief (a workflow stage)

~~~text
Usage: goobers gather-ci-failures [path]

Enrich this run's remediation brief with failing check names,
conclusions, summaries, and annotations. Passing CI leaves the brief
unchanged and performs no provider API calls. Raw job logs are never
fetched: their explicit per-check volume bound is 0 bytes. [path] is
the instance root, defaulting to GOOBERS_INSTANCE_ROOT. Exit codes:
0 = evidence gathered (or passing-CI no-op), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers gather-ci-failures
~~~

## `goobers gather-implement-context`

load first-pass implementation review and hot-file context (a workflow stage)

~~~text
Usage: goobers gather-implement-context [path]

Emit bounded first-pass implementation context: the shipped merge-review
verdict taxonomy and a hot-file map aggregated from currently-open goober
pull requests plus exact base-sync conflict files journaled in the last 30
days. The result is a workflow-stage artifact carried to implement
through the runner's ordinary contextPointers path. maxHotFiles defaults
to 100 and is capped at 500. Exit codes: 0 = context gathered (an empty
hot-file map is valid), 1 = business/provider/journal error, 2 = usage/IO
error.
~~~

**Examples**

~~~console
$ goobers gather-implement-context
~~~

## `goobers gather-issue-context`

add originating issue bodies to a remediation brief (a workflow stage)

~~~text
Usage: goobers gather-issue-context [path]

Read this run's latest remediation brief, resolve the selected PR's
Fixes/Closes/Resolves issue references, and replace only the brief's
gatherIssueContext section with the originating issue bodies. Missing
PRs, absent references, and referenced issues that no longer resolve
produce an empty issues list rather than failing the remediation cycle.
[path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = issue context
gathered (possibly empty), 1 = business/provider/journal error, 2 =
usage/IO error.
~~~

**Examples**

~~~console
$ goobers gather-issue-context
~~~

## `goobers gather-pr-context`

pr-remediation entrypoint: select and load a PR's context (a workflow stage)

~~~text
Usage: goobers gather-pr-context [path]

Select one open, goober-authored PR labeled goobers:needs-remediation
or reporting failing CI, falling back to a PR behind its base only when
neither stronger signal is present. Check out its branch into this
stage's worktree and load the latest merge-review verdict + PR-thread
comments + whether the base has advanced since this PR branched, writing
the versioned remediation-brief artifact to the declared result file.
[path] is the instance root (matching
pr-select/apply-verdict), defaulting to GOOBERS_INSTANCE_ROOT; git
operations run against the stage's actual worktree (the process's
current directory), not path — same split push-branch already relies
on. Exit codes: 0 = context gathered (or no-work if no PR is eligible),
1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers gather-pr-context
~~~

## `goobers gather-review-threads`

add native reviews and anchored inline threads to a remediation brief (a workflow stage)

~~~text
Usage: goobers gather-review-threads [path]

Read this run's latest remediation brief and replace only its
gatherReviewThreads section with native review bodies and inline review
comments. File, line, side, diff-hunk, resolved, and outdated metadata
are preserved so the remediator can distinguish live feedback from stale
threads. [path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = review
context gathered (possibly empty), 1 = business/provider/journal error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers gather-review-threads
~~~

## `goobers gather-sibling-context`

load other open PRs as review evidence (a workflow stage)

~~~text
Usage: goobers gather-sibling-context [--no-cache] [--no-verdict-cache] [path]

Load the other open PRs' touched files + state as
evidence for the holistic review (a workflow stage, follows
pr-select). authorScope=any permits an outside-headPrefixes selection and
requires pr-select's advisoryMode output. Managed selections retain their
namespace-wide goober sibling set; advisory selections see every open PR.
Requires selectedNumber from Task.InputsFrom. Sibling files are memoized
per head SHA under the instance scheduler dir, while check state is
always refreshed; --no-cache bypasses the file memo entirely (neither
read nor written) to force a fully fresh gather. Separately, this stage
also computes the selected PR's
reviewDigest and checks the PR's own most recent verdict comment for a
matching one (issue #523's verdict-level cache) — a match is emitted as
cachedVerdictJson, letting the runner skip the reviewer gate's LLM call
entirely. For a managed PR, however, a matching fail verdict is marked
stale and not emitted when an operator has cleared goobers:merge-escalated,
so the stage forces a fresh review instead; --no-verdict-cache skips that
lookup, always forcing a fresh review. Exit codes: 0 = context gathered
(possibly empty — no siblings is not an error), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers gather-sibling-context
~~~

## `goobers getting-started`

serve and open the guided portal Getting Started walkthrough

~~~text
Usage: goobers getting-started [--port=<port|auto>] [--no-open] [--workdir <dir>]

Serve and open the portal's guided Getting Started walkthrough: two paths
from an empty working directory to a first autonomous pull request. The
recommended path connects a repository you already work in — goobers init,
goobers connect --json, goobers validate --json --check-harness
--check-repos, and goobers run default-implement — and ends with a real PR
against your own backlog. The alternative walks a disposable
getting-started-task-api sample instead — goobers onboarding stub-sample,
goobers init --template=quickstart, the same validate call, and goobers run
quickstart. Every write action the guide offers is a thin wrapper over
these documented CLI commands; it never scaffolds, connects, or validates
on its own. The manual steps stay yours, and the guide states each one
explicitly: exporting your token for the own-repository path, or creating
the disposable GitHub repository, pushing the sample, and exporting tokens
for the sample path. Time to First PR is computed locally and reported
only to you; nothing leaves your machine.

--workdir (default ".") holds the sample checkout and the tutorial
instance; no instance root needs to exist yet. The default --port is auto,
incrementing from 8081 until a port is available. Blocks until interrupted.
Exit codes: 0 = clean shutdown, 1 = service or browser failure, 2 =
usage/IO error.
~~~

**Examples**

~~~console
$ goobers getting-started
$ goobers getting-started --no-open --workdir ~/goobers-tutorial
~~~

## `goobers help`

show command or concept help

~~~text
Usage: goobers help [all|stages|COMMAND|CONCEPT]

Show core command help with no topic, the complete or workflow-stage command
list with all or stages, a command's full help, or one of these concepts:
instance, gaggle, goober, workflow, stage, gate, harness, capability.
~~~

## `goobers init`

scaffold an instance root

~~~text
Usage: goobers init [--guided | --demo [--insecure] | --template=quickstart [--source-tree <path> [--json]]] [path]

Scaffold an instance root at path (default "."): instance.yaml, config/
(seeded with a starter example), gaggles/, scheduler/, and a telemetry.db
placeholder. The daemon creates per-gaggle runs/ and workcopies/ under
gaggles/<gaggle>/ at runtime. Re-running without --guided is safe — existing
pieces are left untouched. --guided is first-run only and refuses a target
with instance.yaml or a populated config/ before prompting. It separately
selects a checked-in config source and target GitHub application repository,
then validates both. It can optionally preview and install the release-matched
agent toolkit into that config source after an explicit harness and destination
choice. The source may be new or existing locally, cloned from GitHub, or
optionally backed by a newly confirmed GitHub repository.
--template=quickstart seeds the versioned onboarding workflow; it is
intentionally not production-safe. With --source-tree <path>, it instead
seeds the checked-in source layout (instance.yaml.example, manifest.yaml,
and gaggles/) without runtime state. The source-tree action is non-interactive,
preserves every existing file, and reports each created or skipped path;
--json emits its versioned machine-readable result envelope. --demo seeds a hermetic mock-provider full-loop tour
requiring no repo, provider credentials, model tokens, or network writes. The
demo is supported on Linux and macOS, where network isolation is enforced; it is
fail-closed on Windows (no enforced network:none equivalent exists there) unless
--insecure is also given, which scaffolds the demo anyway and reports the
isolation limitation — an explicit, narrowly-scoped opt-in that does not alter
the general Windows sandbox policy (#651). Use `goobers preflight` to check and
launch the fully isolated WSL 2 route instead. --insecure requires --demo.
~~~

**Examples**

~~~console
$ goobers init
$ goobers init --template=quickstart ./tutorial
$ goobers init --template=quickstart --source-tree ./tutorial-config --json
$ goobers init --guided ./my-instance
$ goobers init --demo ./demo
~~~

## `goobers ios-simulator-test`

run XCUITest on an iOS simulator and parse its xcresult (a workflow stage)

~~~text
Usage: goobers ios-simulator-test (--project <path> | --workspace <path>) --scheme <name> [flags]

Run an XCUITest scheme on an available iPhone simulator, parse the xcresult
summary, and write flat workflow outputs to GOOBERS_INPUT_RESULTFILE. A workflow
using this command must declare runner requirements os=darwin and xcode so
scheduling rejects incompatible hosts before invocation. The result records the
selected Xcode, simulator device, and runtime versions; failure output includes
the parsed xcresult test diagnostics.

Flags:
  --project <path>         Xcode project path (mutually exclusive with --workspace)
  --workspace <path>       Xcode workspace path (mutually exclusive with --project)
  --scheme <name>          shared test scheme to run
  --device <name>          exact simulator device name (default: first available iPhone)
  --runtime <version>      iOS runtime version, name, or identifier (default: latest available)
  --only-testing <target>  optional xcodebuild only-testing selector
  --result-bundle <path>   relative xcresult bundle path (default: ios-simulator.xcresult)
~~~

**Examples**

~~~console
$ goobers ios-simulator-test --project App.xcodeproj --scheme AppUITests
~~~

## `goobers issue-close-out`

comment + close out the claimed issue (a workflow stage)

~~~text
Usage: goobers issue-close-out [path]

Comment on the issue this run claimed, linking its PR, and mark it done;
status=in-review leaves it open for merge-review. status=needs-human parks
it by replacing goobers:ready with goobers:needs-human (a decision only a
human can make); status=needs-remediation parks it with goobers:needs-
remediation instead (a mechanical failure needing a fix, not a decision —
#2028). Release the claim ledger lease early rather than waiting for it to
expire. Exit codes: 0 = done, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers issue-close-out
~~~

## `goobers journal`

the one sanctioned edit to the append-only journal

~~~text
Usage: goobers journal <redact> [flags]

redact: remove a leaked secret from a run's stored blob and append a
        redaction event — the one sanctioned edit to the append-only
        journal (ARCHITECTURE.md §4, SEC-041).
~~~

## `goobers journal redact`

remove a leaked secret from a stored blob (SEC-041)

~~~text
Usage: goobers journal redact --run <id> --path <blob> --reason <text> [--secret-file <f>] [instance-path]

Registers the leaked secret with the run's scrubber, rewrites the target blob
with the secret redacted, removes the leaked bytes from rest, and appends a
redaction event recording old→new digests (ARCHITECTURE.md §4, SEC-041).

The secret's exact bytes are read from stdin (or --secret-file) — never a flag —
so the value is not exposed in the process table or shell history. It must match
the leaked bytes exactly (no trailing newline unless the secret has one):

  printf %s "$LEAKED_TOKEN" | goobers journal redact --run <id> \
      --path inputs/creds.env --reason 'token pasted into the issue body'

Exit codes: 0 = redacted, 1 = nothing redacted / business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ printf %s "$LEAKED" | goobers journal redact --run <id> --path inputs/creds.env --reason 'leak'
~~~

## `goobers lint`

lint config via the single authoritative validation engine (alias for validate)

~~~text
Usage: goobers lint [--json] [--github-annotations] [--check-harness] [--check-repos] [--source-tree] [--strict] [path]

Lint an instance's instance.yaml and config/ directory (default path
".") against the single authoritative validation engine. This is an
alias for `goobers validate`: identical flags, identical checks, and
identical exit codes, so CI and local development share one validation
path instead of drifting between ad-hoc checks. --source-tree lints a
checked-in config source tree using instance.yaml.example and the path
itself as config/. --json emits the same versioned findings envelope as
`goobers validate --json`. --github-annotations writes each finding to
stderr as a GitHub Actions file annotation (#687), for use as a
config-repo PR check. --strict treats warnings as validation errors. --check-harness additionally preflights every agent
harness referenced by a goober (GBO-011). --check-repos resolves each
target repository's token and verifies authenticated git access. Exit
codes: 0 = clean, 1 = findings, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers lint
$ goobers lint --json
$ goobers lint --check-harness --check-repos
~~~

## `goobers mcp-io`

run the generic publish/read/list MCP server the harness spawns for a goober (a workflow stage)

~~~text
Usage: goobers mcp-io --config <path>

Run the goobers-io MCP server over stdio: publish_output, list_inputs,
read_input, and grep_input — the generic replacement for writing an
agentic stage's declared output with a file-editing tool (#2406). Not
meant to be run interactively — the harness spawns this automatically
via --additional-mcp-config for any eligible stage (one with a declared
artifactFile and/or upstream context); nothing in a goober's own YAML
needs to name it. --config points at the workspace-relative runtime
config (workspace, declared artifactFile, available inputs) the
harness writes before invocation — deliberately not $COPILOT_HOME-
relative, so this works whether or not the invocation has Copilot's
stored-login auth or any other MCP server configured.
Exit codes: 0 = the stdio session ended cleanly (stdin closed),
1 = missing or invalid configuration, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers mcp-io --config .goobers/mcp-io/goobers-io-config.json
~~~

## `goobers merge-pr`

conjunctive auto-merge via direct-merge or merge-queue (a workflow stage)

~~~text
Usage: goobers merge-pr [path]

Merge a pull request, but only when every independent conjunct holds:
verdict=pass, CI green, not a draft, the SHA-pin still matches the PR's
live head/base, and — for a sibling-overlap PR — completed single-lander
election evidence (elected:true, #1071) — never a bare self-approval.
Declared inputs: pullNumber, verdict, headSha, baseSha (all required),
verdictAuthor (required for the default commit message; supplied by
apply-verdict), advisoryMode (default false — report only, no merge
attempted), mergeMethod (merge/squash/rebase; default squash),
commitMessage (default: PR title + review rationale + referenced
issues), resultFile (default merge-result.json). Successful merges
also report headBranch and branchCleanup (deleted, skipped-stacked, or
failed). Exit codes: 0 = evaluated (merged or not — see the result
file's "merged" field), 1 = business error (missing capability/config,
malformed inputs, provider failure), 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers merge-pr
~~~

## `goobers merge-queue-poll`

watch an enqueued PR until merged, evicted, timed out, or opted out (a workflow stage)

~~~text
Usage: goobers merge-queue-poll [path]

Watch a pull request already enqueued to its repo's merge queue (issue
#758's Land, in merge-queue-enqueue policy) until the queue merges or
evicts it, or this stage's own poll times out. Declared inputs:
pullNumber (required), pollIntervalSeconds/pollMaxIntervalSeconds/
pollTimeoutSeconds (time.ParseDuration strings, default to
internal/executor's ci-poll defaults), resultFile (default
queue-result.json). An eviction or timeout applies
goobers:needs-remediation plus an explanatory comment before reporting
its queueOutcome — a failure to apply that trail is a stage failure,
not a swallowed warning. A live goobers:no-merge-review opt-out dequeues
the pull request and reports skipped without remediation. Exit codes:
0 = evaluated (merged, evicted, skipped, or still-pending-timeout —
see the result file's queueOutcome field),
1 = business error (missing capability/config,
provider failure), 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers merge-queue-poll
~~~

## `goobers netpol-render`

render per-runner-class NetworkPolicy reference manifests from the runners: inventory

~~~text
Usage: goobers netpol-render [--out <dir>] [--check] [--baseline <path>]
                             [--write-baseline] [--timeout <duration>]
                             [--print-blob-endpoint] [instance-root]

Render the per-runner-class NetworkPolicy reference manifests from the
runners: inventory — the decision-016 single source of the network reference
manifests (issue #3568, docs/design/goobernetes-restrictions.md §6/§7).

One manifest set is emitted per DISTINCT restriction set (runner class). Every
policy selects on goobers.dev/runner-class derived by the SAME shared function
the dispatcher stamps stage pods with, so selector and stamp agree by
construction. Each policy also carries the goobers.dev/runner-class-restrictions
ANNOTATION — the human-readable restriction set behind the (possibly opaque)
class value, so `kubectl get netpol -o yaml` answers "which class is this".

Per class: a network:none class gets only DNS and the blob-endpoint data path;
every other class additionally gets the instance-configured egress.allowlist
CIDR groups (instance.yaml egress: — operator-supplied; the render REFUSES
CHANGE-ME documentation placeholders rather than emitting a stub). Every class,
restricted included, carries the blob-endpoint egress row: it is the class's
own artifact data path, and each cross-namespace grant is composed as
namespaceSelector AND podSelector in a single peer element.

--out writes one file per class plus a kustomization.yaml; without it the
manifests stream to stdout.

--check validates instead of writing:
  - provenance drift: every egress.allowlist group with a source URL is
    re-fetched and EVERY sourceSHA256 marker compared (a stale CIDR set
    otherwise fails mid-run as a connect timeout indistinguishable from a
    correct denial);
  - coverage ratchet: per-class model-endpoint coverage, measured in
    ADDRESSES (never CIDR-block counts), compared against the committed
    baseline — failing on a rise or an unfrozen class;
  - output freshness: with --out, the on-disk manifests must match a fresh
    render.

--write-baseline freezes the current per-class coverage into the baseline
file (--baseline; defaults to <out>/coverage-baseline.json).

--print-blob-endpoint prints the blob endpoint (namespace, pod labels,
container port) as JSON and exits, touching no cluster and needing no instance
root. The goobers-system ingress half of the blob grant (#3585) lives in a
separate repo and renders its ingress peer FROM this value rather than
restating it, so the endpoint is derived once and cannot drift.

The rendered manifests are REFERENCE manifests the cluster operator applies;
this command never touches a cluster, and rendering says nothing about
enforcement — `goobers doctor --k8s` owns enforcement honesty (D12).

An instance with no pod-hosted runners (no runners: block, or self-only)
renders nothing and exits 0.

Exit codes: 0 = OK, 1 = refusal or check failure, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers netpol-render --out ./deploy/netpol
$ goobers netpol-render --out ./deploy/netpol --write-baseline
$ goobers netpol-render --out ./deploy/netpol --check
~~~

## `goobers onboarding`

run non-interactive onboarding actions

~~~text
Usage: goobers onboarding <command> [flags]

Run non-interactive, machine-readable onboarding actions. Actions never
prompt, write secrets, create a remote, or touch a repository that was not
explicitly named.

Commands:
  stub-agent-instructions  install agent assets into a config source
  stub-sample              materialize the disposable Getting Started target

Run `goobers onboarding <command> -h` for action flags.
~~~

**Examples**

~~~console
$ goobers onboarding stub-agent-instructions --source-tree ./config-repo --harness copilot --json
$ goobers onboarding stub-sample --destination ./getting-started-task-api --json
~~~

## `goobers onboarding stub-agent-instructions`

install agent-instruction assets into a config source

~~~text
Usage: goobers onboarding stub-agent-instructions --source-tree <path> [--harness <name>] [--json]

Install the release-matched Goobers agent toolkit and the selected harness
instruction reference into a checked-in config source repository. This action
delegates to `agent-kit install`: product-owned toolkit files are installed
beneath `.goobers/agent-toolkit/`, existing user instructions are preserved,
and collisions or drift fail without overwriting files.

Flags:
  --source-tree <path>  required config source repository root
  --harness <name>      required: copilot, claude, or generic
  --json                emit the versioned config-source action envelope

Exit codes: 0 = installed or already current, 1 = unsafe target, collision,
drift, or write error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers onboarding stub-agent-instructions --source-tree ./config-repo --harness copilot --json
~~~

## `goobers onboarding stub-sample`

materialize and optionally seed the disposable Getting Started target

~~~text
Usage: goobers onboarding stub-sample --destination <path> [--work-tracking <owner/repo>] [--token-env <name>] [--force] [--json]

Materialize the embedded getting-started-task-api sample at an explicitly
named destination. Existing matching files are skipped. A conflicting file
fails the complete preflight without changing the destination unless --force
is set; symbolic links are always refused.

With --work-tracking owner/repo, seed the catalog's missing GitHub labels and
issues using the token named by --token-env. If the token is unset, report
the issues pending and complete the local materialization without network
access. No remote repository is created or pushed.

Flags:
  --destination <path>      required sample destination
  --work-tracking <repo>    optional GitHub owner/repo to seed
  --token-env <name>        issue token environment variable (default GOOBERS_GITHUB_ISSUES_TOKEN)
  --force                   replace conflicting regular files
  --json                    emit the versioned action envelope

Exit codes: 0 = materialized, 1 = conflict/provider error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers onboarding stub-sample --destination ./getting-started-task-api --json
$ goobers onboarding stub-sample --destination ./getting-started-task-api --work-tracking my-org/tutorial
~~~

## `goobers open-pr`

open or update the run's PR (a workflow stage)

~~~text
Usage: goobers open-pr [path]

Open the run's PR — or, on a repass through this stage, find and update
the PR it already opened (idempotent: the run's branch name is stable
across repasses, providers.BranchName). Writes prNumber/pull-request-url
to the declared result file for a downstream stage's Task.InputsFrom.
Exit codes: 0 = opened/updated, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers open-pr
~~~

## `goobers override`

override a nondeterministic gate with a rationale

~~~text
Usage: goobers override --rationale=<text> [--decision=pass] [--actor=<identity>] <run-id> <gate> [path]

Override a nondeterministic gate on an escalated or failed run and continue
from the selected configured branch. The rationale and authenticated actor
are recorded in the run journal. GOOBERS_API_TOKEN supplies a bearer token
when API auth is enabled.

Exit codes: 0 = action accepted, 1 = action refused, 2 = usage/transport error.
~~~

**Examples**

~~~console
$ goobers override --rationale="accepted risk" <run-id> <gate>
~~~

## `goobers post-merge`

post-merge fan-out + close the referenced issue (a workflow stage)

~~~text
Usage: goobers post-merge [path]

Run the two actions that follow a successful merge: triage every
other open PR targeting the same base branch and label ONLY the
conflicted or file-overlapping ones goobers:needs-remediation, recording
the merged PR and overlapping paths on each affected PR (issue
#715 — a clean disjoint sibling is left untouched), and mark each
issue the merged PR's body references (Fixes/Closes/Resolves #N)
done. Declared input: pullNumber (required — the just-merged PR).
Exit codes: 0 = done (even if the PR body references no issue, or
there are no other open PRs — both are normal outcomes, not
errors), 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers post-merge
~~~

## `goobers pr-claim`

check PR liveness or release its remediation claim (a workflow stage)

~~~text
Usage: goobers pr-claim [--release] [path]

At a pr-remediation stage boundary, verify that this run's claimed pull
request is still open. If it has merged or closed, release the claim and
return a structured no-work result so the runner stops the workflow.
With --release, explicitly release the run's PR claim without querying the
provider. Releasing an already-released claim is an idempotent success.

Exit codes: 0 = PR open, terminal no-work, or released; 1 = business error;
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers pr-claim
$ goobers pr-claim --release
~~~

## `goobers pr-comment-watch`

label open goober PRs carrying unaddressed human comments (a workflow stage)

~~~text
Usage: goobers pr-comment-watch [path]

Scan open goober-authored PRs (head under the gaggle branch namespace)
and label any whose newest human comment is newer than the bot's own
newest comment with goobers:needs-remediation, so pr-remediation updates
that PR in place. A PR parked for a human (needs-human / merge-escalated)
is un-parked when a fresh human comment lands: the park label is cleared
and needs-remediation added in one mutation, since the human the PR was
parked for has now weighed in. The bot identity is the token's own login
(AuthenticatedLogin) — a dedicated bot account is required for signal;
with a shared human identity the stage never fires. Comments landing
mid-remediation after the brief snapshot can be masked by the bot's
response comment until the human comments again (accepted v1 limit).

Inputs: maxPullRequests (default 20), headPrefixes (default the branch
namespace), base (default the gaggle base branch), excludeLabels (labels
that hard-exclude a PR from the scan), unparkLabels (park labels a fresh
human comment clears while routing, default needs-human,merge-escalated),
excludeAuthors (extra bot logins to ignore, e.g. Gitea CI bots),
resultFile (default comment-watch-result.json).
Exit codes: 0 = scanned (labeled zero or more), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers pr-comment-watch
~~~

## `goobers pr-select`

select one managed or advisory open PR for merge-review (a workflow stage)

~~~text
Usage: goobers pr-select [path]

Select at most one open, non-draft, green-CI PR for merge-review to
evaluate this cycle (a workflow stage). authorScope defaults to goobers;
set it to any to admit PRs outside headPrefixes as advisory-only. PRs
may be filtered by exact author, assignee, and requestedReviewer inputs.
PRs labeled goobers:no-merge-review or goobers:run-aborted are always
excluded. Before selection,
park narrower PRs behind open PRs that clearly dominate a shared-file
rewrite or deletion. Writes the
selected PR's number/head/base/headSha/baseSha/url/advisoryMode to the declared
result file. Exit codes: 0 = selected (or no-work), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers pr-select
~~~

## `goobers preflight`

check WSL full-isolation readiness and optionally hand off a command

~~~text
Usage: goobers preflight [--distro <name>] [--launch-wsl -- <goobers-command> [args...]]

On Windows, verify that the selected or default WSL distro can run the full
isolated Goobers workflow. Readiness requires WSL 2, a runnable distro, a Linux
goobers binary, Bubblewrap, and working unprivileged user + network namespaces.

With --launch-wsl, run the trailing Goobers command through that distro after
the checks pass. Arguments are forwarded directly without shell evaluation,
the WSL process starts in the current Windows directory, and its exit code is
returned. Use paths relative to that directory or Linux paths in forwarded
arguments.

Exit codes: 0 = ready/forwarded command succeeded, 1 = not ready or forwarded
command failed, 2 = usage error. A forwarded command's non-zero exit code is
preserved.
~~~

**Examples**

~~~console
$ goobers preflight
$ goobers preflight --distro Ubuntu-24.04
$ goobers preflight --launch-wsl -- run implementation .
~~~

## `goobers publish-batch`

publish a verified decomposition batch behind one eligibility barrier (a workflow stage)

~~~text
Usage: goobers publish-batch [path]

publish-batch is the decomposition workflow's deterministic publication
stage (docs/design/decomposition-workflow.md §5.2-5.3, DEC-4). It consumes
a validated decomposition plan, resumes the prepare/publish protocol by
stable markers, verifies the complete batch, commits eligibility through
the parent published record, and releases the parent claim.

Exit codes: 0 = batch published / 1 = business or provider error / 2 = usage error.
~~~

**Examples**

~~~console
$ goobers publish-batch
~~~

## `goobers push-branch`

push the worktree's checked-out branch to origin (a workflow stage)

~~~text
Usage: goobers push-branch [path]

Push the worktree's checked-out branch to origin, authenticated via the
configured repository credential — never the host's ambient git
credentials, and never persisted to .git/config.
A push rejected as a ref race (non-fast-forward, "failed to push some
refs") fetches the remote tip, rebases the local branch onto it, and
retries up to 2 more times before failing, so a fully-validated diff is
not discarded because a concurrent writer advanced the branch (#3366).
[path] defaults to the current directory (the stage's worktree).
Exit codes: 0 = pushed, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers push-branch
~~~

## `goobers push-remediated`

force-push the remediated branch and clear needs-remediation (a workflow stage)

~~~text
Usage: goobers push-remediated [path]

Force-push (with lease) the remediated branch to the claimed PR's head
and clear goobers:needs-remediation so merge-review re-evaluates it.
Recovers the PR from this run's own claim ledger entry and the lease
expectation from the head SHA remediation-checkpoint recorded on the
PR's sticky state comment — neither can be threaded here, since the
agentic chain sits between this stage and the one that selected the PR.
Exit codes: 0 = pushed (or an idempotent no-op), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers push-remediated
~~~

## `goobers rebase-pr`

rebase-first, finding-driven remediation routing (a workflow stage)

~~~text
Usage: goobers rebase-pr [path]

Check out the selected PR's branch, attempt a rebase onto its base
(force-with-lease is mandatory for the eventual push — never a bare
push), and route on the result: a clean rebase with no detected,
policy-allowed cause force-pushes and clears goobers:needs-remediation;
a detected cause the declared policy allows needs the agentic remediation
chain, reported via the needsAgent output for the workflow to route on; a
detected cause the policy excludes is left untouched for a human
(policyExcluded/policyExcludedReason outputs — #941/PRR-6). Requires
selectedNumber/head/base (Task.InputsFrom gather-pr-context's own
outputs) and hasSubstantiveFindings/hasFailingCI.

remediate (input, default "conflict,substantive,failing-ci,behind-base,
sibling-overlap,human-comment") is a comma-separated policy naming which
detected causes are allowed to trigger remediation; the shipped default is
fully liberal. behind-base is accepted vocabulary but cannot fire yet (no
detection reaches this stage's decision today). human-comment fires when a
genuinely new human comment postdates the watermark recorded in the sticky
remediation-state comment; detection runs only when the declared policy
names human-comment, so an old pinned policy is unaffected.

Exit codes: 0 = routed, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers rebase-pr
~~~

## `goobers reconcile-branches`

report bounded stale goobers/* branch candidates (a workflow stage)

~~~text
Usage: goobers reconcile-branches [--delete] [--max N] [--min-age D] [--after BRANCH] [path]

Inspect a bounded page of remote goobers/* branches. The default is a
dry-run: no branch is deleted. --delete opts into deletion only for a
branch whose local run journal proves ownership, has been terminal for
at least 168h by default, has no remote activity in that window, and has
no open pull request. Every candidate
decision and deletion outcome is appended to scheduler/events.jsonl.
The default batch is 25 and the hard ceiling is 100 candidates. Task inputs deleteBranches,
maxBranches, minimumAge, and after provide the same workflow-stage
configuration surface. Exit codes: 0 = sweep completed, 1 = business or
provider error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers reconcile-branches
$ goobers reconcile-branches --delete --max 5
~~~

## `goobers reconcile-post-merge`

reconcile late merge-queue merges (a workflow stage)

~~~text
Usage: goobers reconcile-post-merge [--max N] [--lookback D] [path]

Inspect a bounded batch of merge-queue entries whose queue-watch stage
timed out. A pull request that has since merged receives branch cleanup,
issue close-out, and sibling fan-out through the normal post-merge path;
an open or unmerged pull request remains pending. Completed entries are
durably skipped on later runs. Task inputs maxPullRequests and lookback
set the same bounds (defaults: 10 and 168h; hard maximum: 100).
Exit codes: 0 = sweep completed, 1 = business/provider error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers reconcile-post-merge
~~~

## `goobers record-merge-refusal`

record a merge refusal and demote a persistently-stuck lander (a workflow stage)

~~~text
Usage: goobers record-merge-refusal [path]

Record a merge-pr refusal against the selected PR's durable demotion
counter (#950). After a bounded number of refusals at an unchanged
head, apply goobers:merge-demoted so the election crowns a sibling
instead and the cluster drains around the stuck PR. Runs on
merge-gate's fail branch. Exit codes: 0 = recorded (or nothing to
do), 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers record-merge-refusal
~~~

## `goobers remediation-checkpoint`

durable per-cause attempt budgets + same-diff escalation (a workflow stage)

~~~text
Usage: goobers remediation-checkpoint [--budget N] [--escalate <reason> [--escalation-outcome <outcome>]] [path]

Re-checkout the PR's own branch (this stage gets its own fresh
worktree), read pr-remediation's durable per-cause attempt counters + last
diff digest back from a sticky PR comment, compare this cycle's
actual diff (git diff base...HEAD) against it, and either
escalate (goobers:merge-escalated, clearing needs-remediation) when
the active cause exhausts its DSL-declared budget or on a byte-identical
repeat, or record the advanced
state as a new sticky comment. Requires selectedNumber (inputsFrom
gather-pr-context's selectedNumber output), remediationCauses, and the
five per-cause budget inputs (humanCommentBudget defaults to 2 when
undeclared). --budget overrides every declared cause
for standalone diagnostics. --escalation-outcome classifies a forced
--escalate as did-not-converge (the default), budget-exhausted, or infrastructure-failure.
Escalations persist a machine-readable `escalationOutcome`
(`did-not-converge`, `budget-exhausted`, `policy-excluded`, or `infrastructure-failure`), whether
repair was attempted, and the attempted causes in both the sticky comment
and stage result. Exit codes: 0 = checkpoint
recorded (escalated or not — both are normal outcomes), 1 = business
error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers remediation-checkpoint
~~~

## `goobers report-pr-status`

publish goobers' verdict + CI evidence as a policy-gate-able PR status (a workflow stage)

~~~text
Usage: goobers report-pr-status [path]

Publish a provider-native pull-request status (Azure DevOps PR status) as
goobers' own evidence — the agentic reviewer verdict and the local-CI
result — so a repository's status-check branch policy can gate on it and
the validation loop can prove PR correctness against the repo's required
policies (#772). Reaching this stage on the happy path is itself the
evidence: the run only advances here after the review gate and local-CI
gate both pass, so the default published state is `succeeded`.

Inputs (Task.Inputs / inputsFrom): prNumber (required, from open-pr),
statusName (default "validation"), statusGenre (default "goobers"),
state (succeeded|failed|pending, default succeeded), description,
targetUrl (default the PR url), resultFile (default status-result.json).
Exit codes: 0 = published, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers report-pr-status
~~~

## `goobers rerun-stage`

rerun a stage with a recorded instruction addendum

~~~text
Usage: goobers rerun-stage --addendum=<text> [--actor=<identity>] <run-id> <stage> [path]

Rerun one agentic task or reviewer gate on an escalated run with a one-off
instruction addendum. The actor, addendum, target, and human attempt are
recorded in the run journal; the workflow definition is not changed.
GOOBERS_API_TOKEN supplies a bearer token when API auth is enabled.

Exit codes: 0 = action accepted, 1 = action refused, 2 = usage/transport error.
~~~

**Examples**

~~~console
$ goobers rerun-stage --addendum="use the parser seam" <run-id> <stage>
~~~

## `goobers reset-rate-limit`

clear the hourly run-rate budget without deleting runs/

~~~text
Usage: goobers reset-rate-limit [path]

Reset an instance's hourly run-rate budget (maxRunsPerHour) so runs can
start again immediately, WITHOUT deleting runs/ (the append-only run
journals — the durable execution record). This is the blessed narrow
reset: it writes a marker under scheduler/ that moves the rate window's
floor to now, and never touches runs/ — unlike `rm -rf <instance>`,
which clears the rate window only by destroying everything with it.
Takes effect on the next `goobers up`/`goobers run` (stop the daemon
first if one is running; the reset is applied when the scheduler next
reconstructs its budget window). Default path ".".
Exit codes: 0 = reset written, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers reset-rate-limit
~~~

## `goobers resolve-review-threads`

reply to and resolve remediated native review threads (a workflow stage)

~~~text
Usage: goobers resolve-review-threads [path]

Validate the implementer's threadResponses against every gathered live review
thread, reply to each thread, resolve addressed threads after the reply is
visible, and re-query the published PR head. Exit codes: 0 = responses
applied and verified, 1 = business/provider error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers resolve-review-threads
~~~

## `goobers respond-to-findings`

post a validated per-finding remediation response to the claimed PR (a workflow stage)

~~~text
Usage: goobers respond-to-findings [--check] [path]

Read the claimed PR's original remediation verdict and the latest
implement stage's findingResponses output from this run's journal.
Require exactly one addressed/declined disposition with a non-empty
detail for every finding, post the resulting changelog to the PR, and
write the complete structured response to the declared result file.
With --check, only validate the response before publication; do not
require push-remediated or post to the PR.
Retries reconcile one run-scoped comment instead of appending duplicates.
If push-remediated skipped a closed PR, records the unposted account
without claiming those local changes landed.
[path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = response
processed, 1 = business error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers respond-to-findings
~~~

## `goobers run`

trigger a run manually (still honors run conditions)

~~~text
Usage: goobers run [--gaggle <name>] [--pr <number>] <workflow> [--no-wait] [path]
       goobers run <gaggle>/<workflow> [--pr <number>] [--no-wait] [path]
       goobers run abort <run-id> [path]
       goobers run continue --from <run-id> --terminal-seq <seq> --target <state> --operator <id> [path]
       goobers run cancel <run-id> [path]

Trigger a run of a config/ workflow manually, through the same scheduler
(run conditions, instance journal, single-instance lock) a live `goobers up`
daemon uses, then wait for it to reach a terminal state unless
--no-wait is set (default path "."). Use --gaggle or the qualified
<gaggle>/<workflow> form when multiple gaggles share a workflow name.
If a live `goobers up` daemon already
holds the instance lock,
delegates the trigger to it instead of failing (#343) — dispatched through
the same Scheduler.Trigger path either way. Exit codes after waiting: 0 =
completed, 1 = failed/aborted or business error (unknown workflow, invalid
config, run conditions rejected the trigger), 2 = usage/IO error, 3 =
escalated. A successful submission-only mode (such as --no-wait, once
available) exits 0 because it does not observe a terminal phase.
`run abort` marks a stuck non-terminal run aborted directly in its own
journal — recovery for a run resumeInterruptedRuns can't resolve on its own.
If a live `goobers up` daemon already holds that run's journal lock, abort
delegates to it instead of failing (#2270), the same way `run <workflow>`
delegates triggering (#343) — dispatched through the live-cancel path
either way. `run cancel` instead asks a live daemon to stop a run it is
actively executing (active-stage cancel + worktree/claim teardown +
aborted) — the live counterpart to `run abort`'s daemon-down journal
repair.
~~~

**Examples**

~~~console
$ goobers run default-implement
$ goobers run --gaggle example default-implement
$ goobers run example/default-implement --no-wait
~~~

## `goobers run abort`

mark a stuck non-terminal run aborted

~~~text
Usage: goobers run abort <run-id> [path]

Mark a stuck non-terminal run aborted by appending a terminal
run.finished(status=aborted) event to its own journal (default path
"."). Exit codes: 0 = aborted, 1 = business error (run already terminal),
2 = usage/IO error (unknown run).
~~~

**Examples**

~~~console
$ goobers run abort <run-id>
~~~

## `goobers run cancel`

cancel a live in-flight run via the daemon

~~~text
Usage: goobers run cancel <run-id> [path]

Ask the live `goobers up` daemon to stop a run it is actively executing
(default path "."): it cancels the active stage, tears down the run
worktree, releases the backlog claim so the item can be re-queued, and
records terminal phase aborted — without stopping the daemon or editing a
journal behind its back. Use `run abort` instead when no daemon is running
(that path finalizes a stuck run's journal directly). Exit codes: 0 =
cancelled, 1 = business error (already terminal, not currently running, or
no daemon to cancel it), 2 = usage/IO error (unknown run).
~~~

**Examples**

~~~console
$ goobers run cancel <run-id>
~~~

## `goobers runs`

list runs and report per-run disk usage

~~~text
Usage: goobers runs <command> [flags] [path]

Commands:
  list    alias for the goobers status run table (same flags)
  du      report per-run journal and artifact bytes, largest first
~~~

## `goobers runs du`

report per-run journal and artifact bytes

~~~text
Usage: goobers runs du [--json] [path]

Report file bytes used by each run, split between journal/state data
and artifacts, largest first (default path ".").
Exit codes: 0 = OK, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers runs du
$ goobers runs du --json
~~~

## `goobers runs list`

alias for the status run table (same flags, no --watch)

~~~text
Usage: goobers runs list [--json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--gaggle=<name>] [--limit=N] [path]

Alias for the goobers status run table, with the same flags (minus --daemon/--watch).
Validate active config, show warnings, and list runs under an instance's
runs/ directory with their current phase, newest first (default path ".").
Exit codes: 0 = OK, 1 = validation errors, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers runs list
$ goobers runs list --json --limit=20
~~~

## `goobers scaffold`

scaffold a goober, workflow, or gaggle

~~~text
Usage: goobers scaffold goober [--force] <name> [path]
       goobers scaffold workflow [--force] <name> [path]
       goobers scaffold gaggle [--force] <name> [path]
       goobers scaffold gaggle --from <existing-gaggle> <name> [path]

Generate a valid goober, workflow, or gaggle. For goober/workflow, path
may be an instance root or a gaggle directory and defaults to ".". For
gaggle, path is always an instance root. Existing files are never
replaced unless --force is set.

`scaffold gaggle` with no --from creates an empty gaggle: gaggle.yaml
plus its manifest.yaml registration, ready for scaffold goober/workflow.
`scaffold gaggle --from <existing-gaggle>` instead renames that gaggle to
<name>: it moves gaggles/<existing-gaggle> to gaggles/<name> and rewrites
every identity reference — gaggle.yaml's metadata.name and
isolation.namespace, the manifest.yaml gaggles list entry, and every
contained goober/workflow's spec.gaggle — while project, backlog,
ciCommand, instructions, and workflow bodies move untouched. --force is
not accepted with --from.
~~~

**Examples**

~~~console
$ goobers scaffold goober my-coder
$ goobers scaffold workflow my-flow
$ goobers scaffold gaggle ledger --from example
~~~

## `goobers scaffold gaggle`

scaffold a gaggle, or rename one with --from

~~~text
Usage: goobers scaffold gaggle [--force | --from <existing-gaggle>] <name> [path]

Create a new gaggle in the instance at path (an instance root, default
"."). With no --from, scaffolds an empty gaggle: gaggle.yaml plus its
manifest.yaml registration, ready for `goobers scaffold goober`/
`scaffold workflow`. Existing files are never replaced unless --force is
set.

--from <existing-gaggle> instead renames that gaggle to <name>: it moves
gaggles/<existing-gaggle> to gaggles/<name> and rewrites every identity
reference in place — gaggle.yaml's metadata.name and isolation.namespace,
the manifest.yaml gaggles list entry, and every contained goober/workflow's
spec.gaggle back-reference — while leaving project, backlog, ciCommand,
instructions, and workflow bodies untouched. --force is not accepted
with --from: a rename never overwrites an existing gaggle directory.
~~~

## `goobers scaffold goober`

scaffold a goober in a gaggle

~~~text
Usage: goobers scaffold goober [--force] <name> [path]
       goobers scaffold workflow [--force] <name> [path]
       goobers scaffold gaggle [--force] <name> [path]
       goobers scaffold gaggle --from <existing-gaggle> <name> [path]

Generate a valid goober, workflow, or gaggle. For goober/workflow, path
may be an instance root or a gaggle directory and defaults to ".". For
gaggle, path is always an instance root. Existing files are never
replaced unless --force is set.

`scaffold gaggle` with no --from creates an empty gaggle: gaggle.yaml
plus its manifest.yaml registration, ready for scaffold goober/workflow.
`scaffold gaggle --from <existing-gaggle>` instead renames that gaggle to
<name>: it moves gaggles/<existing-gaggle> to gaggles/<name> and rewrites
every identity reference — gaggle.yaml's metadata.name and
isolation.namespace, the manifest.yaml gaggles list entry, and every
contained goober/workflow's spec.gaggle — while project, backlog,
ciCommand, instructions, and workflow bodies move untouched. --force is
not accepted with --from.
~~~

## `goobers scaffold workflow`

scaffold a workflow in a gaggle

~~~text
Usage: goobers scaffold goober [--force] <name> [path]
       goobers scaffold workflow [--force] <name> [path]
       goobers scaffold gaggle [--force] <name> [path]
       goobers scaffold gaggle --from <existing-gaggle> <name> [path]

Generate a valid goober, workflow, or gaggle. For goober/workflow, path
may be an instance root or a gaggle directory and defaults to ".". For
gaggle, path is always an instance root. Existing files are never
replaced unless --force is set.

`scaffold gaggle` with no --from creates an empty gaggle: gaggle.yaml
plus its manifest.yaml registration, ready for scaffold goober/workflow.
`scaffold gaggle --from <existing-gaggle>` instead renames that gaggle to
<name>: it moves gaggles/<existing-gaggle> to gaggles/<name> and rewrites
every identity reference — gaggle.yaml's metadata.name and
isolation.namespace, the manifest.yaml gaggles list entry, and every
contained goober/workflow's spec.gaggle — while project, backlog,
ciCommand, instructions, and workflow bodies move untouched. --force is
not accepted with --from.
~~~

## `goobers schema`

emit a JSON Schema embedded in this build

~~~text
Usage: goobers schema [--human] <kind>
       goobers schema --list [--human]

Emit a canonical JSON Schema embedded in this build, or list every available
schema kind. JSON is the default and includes the build version, commit, and
DSL version. --human prints a labeled terminal rendering. No network lookup
or fallback to another release is performed. Exit codes: 0 = OK, 1 = unknown
kind or output error, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers schema --list
$ goobers schema workflow
$ goobers schema --human goober
~~~

## `goobers select-source`

select and claim an unconsumed L6 decomposition disposition (a workflow stage)

~~~text
Usage: goobers select-source [path]

select-source is the decomposition workflow's `select-source` stage
(docs/design/decomposition-workflow.md §3.2, DEC-1). It scans escalated
runs for an unconsumed, non-retryable ISSUE_OVER_SCOPE/NEEDS_DECOMPOSITION
disposition (#415), resolves the oldest eligible one's claimed parent issue,
and — if it is still open, maintainer-approved, not already claimed or
decomposed — claims it in the local claim ledger and writes the immutable
selection artifact to the declared result file. The maintainer approval
label is configured by the required trustLabel input (SEC-047).

Exit codes: 0 = a source was selected (or none was eligible — a no-work
result, not an error) / 1 = business error (provider/credential/config
error) / 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers select-source
~~~

## `goobers self-update`

stage and request a supervised binary update

~~~text
Usage: goobers self-update [flags] [path]

Stage and smoke-check a binary, then request supervised activation. Policies
are manual, on-release (default), and on-main. Manual requires a release tag;
on-main builds the configured branch. Config is never changed.
~~~

**Examples**

~~~console
$ goobers self-update --policy on-release
$ goobers self-update --policy manual --target v0.1.0
~~~

## `goobers service`

install and manage the platform-supervised daemon

~~~text
Usage: goobers service <subcommand> [path]

Install and manage the goobers daemon under the current platform's user
supervisor: systemd on Linux, launchd on macOS, or the Windows Service
Control Manager. The stable service host launches the instance's mutable
binary, receives the same graceful-shutdown trigger as a foreground daemon,
and owns validated self-update handoff, health checks, and rollback.

Subcommands:
  install     install, enable, and start the service
  uninstall   gracefully stop, disable, and remove the service
  start       resume an installed-but-stopped service
  stop        halt the running service without disabling or removing it
  status      report whether the service is installed and running

Run `goobers service install -h`, `goobers service uninstall -h`,
`goobers service start -h`, `goobers service stop -h`, or
`goobers service status -h` for details. Default path is ".".
~~~

**Examples**

~~~console
$ goobers service install
$ goobers service status
$ goobers service uninstall
~~~

## `goobers service install`

install, enable, and start the supervised daemon

~~~text
Usage: goobers service install [path]

Install, enable, and start the goobers daemon for the instance at <path>.
Linux and macOS install a per-user service so provider credentials retain
the current user's ownership. Windows installation must run from an
elevated terminal. An existing installation is never overwritten; uninstall
it first when changing the stable host binary or instance path. Product
binary updates use the self-update workflow instead.

Exit codes: 0 = installed and running, 1 = installation/start error,
2 = usage error or not an instance root.
~~~

**Examples**

~~~console
$ goobers service install
$ goobers service install ./instance
~~~

## `goobers service start`

resume an installed-but-stopped daemon

~~~text
Usage: goobers service start [path]

Resume an installed-but-stopped goobers daemon (#2073) without
re-registering it. Starting an already-running service is a successful
no-op.

Exit codes: 0 = running, 1 = not installed/start error,
2 = usage error or not an instance root.
~~~

**Examples**

~~~console
$ goobers service start
$ goobers service start ./instance
~~~

## `goobers service status`

report whether the supervised daemon is installed and running

~~~text
Usage: goobers service status [--json] [path]

Report the current platform supervisor, registration path (when applicable),
and whether the goobers daemon is installed, loaded, and running.

Exit codes: 0 = running, 1 = stopped/not installed/query error,
2 = usage error or not an instance root.
~~~

**Examples**

~~~console
$ goobers service status
$ goobers service status --json
~~~

## `goobers service stop`

halt the running daemon without disabling or removing it

~~~text
Usage: goobers service stop [path]

Halt the running goobers daemon without disabling or removing its
supervisor registration (#2073) — distinct from uninstall, which folds
stop, disable, and removal into one step. `goobers service status` then
reports "installed, not running"; `goobers service start` resumes it.
Stopping an already-stopped service is a successful no-op.

Exit codes: 0 = stopped (or already stopped), 1 = not installed/stop
error, 2 = usage error or not an instance root.
~~~

**Examples**

~~~console
$ goobers service stop
$ goobers service stop ./instance
~~~

## `goobers service uninstall`

gracefully stop and remove the supervised daemon

~~~text
Usage: goobers service uninstall [path]

Gracefully stop the managed daemon, disable it, and remove its supervisor
registration. Uninstalling an absent service is a successful no-op.

Exit codes: 0 = absent after the operation, 1 = stop/removal error,
2 = usage error or not an instance root.
~~~

**Examples**

~~~console
$ goobers service uninstall
$ goobers service uninstall ./instance
~~~

## `goobers set-milestone`

assign an existing milestone to an issue (a workflow stage)

~~~text
Usage: goobers set-milestone --item ID --milestone N [path]

Assign an existing GitHub milestone number to an issue. Task inputs itemID
and milestone provide the same workflow-stage configuration as the flags.
Requires the github:milestones:write capability; github:issues:write alone
does not authorize this command. Exit codes: 0 = assigned, 1 = business or
provider error, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers set-milestone --item 1227 --milestone 22
~~~

## `goobers signal`

fire an external signal to subscribed workflows

~~~text
Usage: goobers signal <name> [path]

Fire an external signal by name, dispatching every workflow with a
type=signal trigger subscribed to it, through the same scheduler (run
conditions, instance journal, single-instance lock) a live `goobers up`
daemon uses (default path "."). A signal may match zero, one, or many
workflows; waits for every dispatched run to reach a terminal state or
pause before returning (same blocking UX as `goobers run`).
Exit codes after waiting: 0 = every admitted run completed (also used when
none were admitted), 1 = any run failed/aborted or a business error, 2 =
usage/IO error, 3 = any run escalated. Escalation takes precedence for
mixed outcomes; successful submission-only modes exit 0 because they do
not observe a terminal phase.
~~~

**Examples**

~~~console
$ goobers signal deploy-approved
~~~

## `goobers speech`

preflight and test local speech notifications

~~~text
Usage: goobers speech <command> [flags] [path]

Check and test the configured local speech notification sink without cloud
credentials. Both commands work while speech.enabled is false so audio can
be verified before monitoring is enabled.

Commands:
  preflight  report the selected engine and local prerequisites without sound
  test       speak the fixed Goobers readiness phrase
~~~

**Examples**

~~~console
$ goobers speech preflight
$ goobers speech test
~~~

## `goobers speech preflight`

check the configured local speech engine without emitting sound

~~~text
Usage: goobers speech preflight [--json] [path]

Report the selected engine and executable, effective voice, language, and
rate, plus the required local audio session. This command emits no sound.
Exit codes: 0 = available, 1 = unavailable or invalid config, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers speech preflight
$ goobers speech preflight --json ./instance
~~~

## `goobers speech test`

speak the fixed local readiness phrase

~~~text
Usage: goobers speech test [--json] [path]

Run preflight, then speak the fixed phrase "Goobers speech notifications are ready." using
the configured local engine. Arbitrary text is not accepted. The delivery
receipt is appended under scheduler/speech-receipts.jsonl.
Exit codes: 0 = delivered, 1 = unavailable or failed, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers speech test
$ goobers speech test --json ./instance
~~~

## `goobers stats`

show the instance lifetime summary card

~~~text
Usage: goobers stats [--since <duration>] [--json] [path]

Show the instance's run outcomes, provider mutations, busiest workflow,
and agentic-stage durations (default path "."). Exit codes: 0 = OK,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers stats
$ goobers stats --since 24h --json
~~~

## `goobers status`

validate config, show warnings, list runs, report daemon health, or list live agentic stages

~~~text
Usage: goobers status [--daemon | --agents | --json] [--phase=<phase>[,<phase>...]] [--workflow=<name>] [--gaggle=<name>] [--limit=N] [--watch [--interval=2s]] [path]

Validate active config, show warnings, and list runs under an instance's
runs/ directory with their current phase, newest first (default path ".").
Each run includes work identity, stage liveness, PR trajectory, claim drift, latest error, and review rationale.
Status also reports workflow health and separate blocked-on-sibling/merge-escalated PR counts.
It lists parked backlog items too — open issues carrying a park disposition without
goobers:ready, which backlog selection can no longer see and no workflow re-readies.
With --daemon, report daemon health, identity, and effective behavior settings instead.
With --agents, list only the agentic stages in flight right now, by role and run id.
The --agents answer comes from the runner's own journals, never from a process table,
so it can never match the process asking (no `ps | grep` self-match), and it drops the
invoking run when it is itself a stage. It needs no credentials and makes no provider
calls, so it is safe to run from inside a container during a deploy window. Combine it
with --json for scripting, or --workflow/--gaggle to scope it; --phase, --limit and
--watch are refused because the probe reports only the live moment.
Exit codes: 0 = OK, 1 = validation errors, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers status
$ goobers status --daemon
$ goobers status --watch
$ goobers status --agents
$ goobers status --agents --json
~~~

## `goobers telemetry`

query, export, prune, or compact run telemetry

~~~text
Usage: goobers telemetry <stats|errors|export|prune|prune-orphans|compact> [flags] [path]

stats:  run/stage outcomes, curation actions, and ready-pool health
errors: recent errors across runs, by class, with run/stage refs
export: re-emit a span-start-time window from journaled OTLP/JSON
prune:   remove terminal runs outside the configured retention bounds
prune-orphans: report or delete old run directories that lack run.yaml
compact: drop aged scheduler journal/rollup rows and reclaim disk (VACUUM)
~~~

**Examples**

~~~console
$ goobers telemetry stats
$ goobers telemetry errors
$ goobers telemetry export --since=2026-07-01T00:00:00Z
$ goobers telemetry prune --dry-run
~~~

## `goobers telemetry compact`

drop aged scheduler journal/rollup rows and reclaim disk (VACUUM)

~~~text
Usage: goobers telemetry compact [--dry-run] [path]

Reclaim disk from a bloated instance: drop scheduler journal records
(scheduler/events.jsonl) and rollup scheduler rows older than
telemetry.retention.window, then VACUUM the rollup database and truncate its
write-ahead log so freed pages return to the filesystem. The daemon must be
stopped first — compaction rewrites files a running daemon holds open.
--dry-run reports what would be reclaimed without changing anything. The
default 90d window applies when none is configured. Exit codes: 0 = OK,
1 = compaction error, 2 = usage/config error.
~~~

**Examples**

~~~console
$ goobers telemetry compact --dry-run
$ goobers telemetry compact
~~~

## `goobers telemetry errors`

recent errors across runs, by class, with run/stage refs

~~~text
Usage: goobers telemetry errors [--json] [--workflow=name] [--gaggle=name] [--class=name] [--since=RFC3339] [--until=RFC3339] [--limit=N] [--rebuild] [path]

Recent errors across every run, newest first, with run/stage refs
(default path "."). Exit codes: 0 = OK, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers telemetry errors
$ goobers telemetry errors --limit=50
~~~

## `goobers telemetry export`

re-emit a span-start-time window from journaled OTLP/JSON

~~~text
Usage: goobers telemetry export --since=RFC3339 [--until=RFC3339] [path]

Re-emit journaled OTLP/JSON trace requests to stdout. --since is inclusive;
--until is exclusive when set. Window membership uses each span's start time.
Every discovered run journal is validated before output; missing, corrupt, or
unsupported OTLP data emits nothing and exits non-zero. Exit codes: 0 = OK,
1 = journal data error, 2 = usage/output error.
~~~

**Examples**

~~~console
$ goobers telemetry export --since=2026-07-01T00:00:00Z
$ goobers telemetry export --since=2026-07-01T00:00:00Z --until=2026-07-02T00:00:00Z
~~~

## `goobers telemetry prune`

remove terminal runs outside configured retention bounds

~~~text
Usage: goobers telemetry prune [--dry-run] [path]

Remove terminal run journals and all of their SQLite rollup rows when either
telemetry.retention.window or telemetry.retention.maxRuns is exceeded. Active
and paused runs are never removed. The configured 90d/500-run defaults apply
when a bound is omitted. This explicit command works even when automatic
retention is disabled. Exit codes: 0 = OK, 1 = prune error, 2 = usage/config error.
~~~

**Examples**

~~~console
$ goobers telemetry prune --dry-run
$ goobers telemetry prune
~~~

## `goobers telemetry prune-orphans`

report or delete old orphan and unfinished run directories

~~~text
Usage: goobers telemetry prune-orphans [--delete] [--min-age=D] [path]

Report directories without run.yaml from both published run roots and unpublished
creation staging roots after at least 24h of inactivity. The default is a dry-run
report; --delete opts into deletion. --min-age may raise but never lower
the 24h safety threshold. Valid run journals, recent or active directories, files,
and symlinks are always preserved.
Exit codes: 0 = OK, 1 = cleanup error, 2 = usage/config error.
~~~

**Examples**

~~~console
$ goobers telemetry prune-orphans
$ goobers telemetry prune-orphans --delete
~~~

## `goobers telemetry stats`

success rate and duration aggregates per workflow and stage

~~~text
Usage: goobers telemetry stats [--json] [--workflow=name] [--gaggle=name] [--branch=id] [--model=id] [--harness-version=version] [--group-by=branch|model|harness-version]... [--since=RFC3339] [--until=RFC3339] [--rebuild] [path]

Success rate and duration aggregates per workflow and stage, plus curation
actions and ready-pool health for unfiltered workflow views,
across every run (default path "."). Agent filters retain matching agentic
stage attempts; --branch filters stage/usage rows by journal branch id.
A run that used multiple grouped agent cohorts appears in each.
Exit codes: 0 = OK, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers telemetry stats
$ goobers telemetry stats --json
~~~

## `goobers telemetry-query`

emit versioned candidate findings (a connector stage)

~~~text
Usage: goobers telemetry-query [--window <duration>] [--aggregate <name>]... [--learning-action <name>]... [--threshold <k=v>]... [--format candidate-findings|effective-version-efficacy|tutor-live-verification] [--gaggle <name>] [--workflow <name>] [path]

Query the instance telemetry rollup for threshold-crossing failure and gate
patterns. The built-in connector stage writes a versioned candidate-findings
artifact to GOOBERS_INPUT_resultFile when declared, or to stdout otherwise.
With no --aggregate, all supported aggregates are evaluated. --learning-action
filters learning-episode findings to governed action families. Threshold rates
are fractions from 0 through 1; count thresholds are positive integers.

--format effective-version-efficacy (requires --workflow) instead assesses
the workflow's most recent EffectiveVersion transition — the version-
segmented cohort key (workflow digest + goober digest + model + harness
version) from the Tutor v2 design — and emits a helped/regressed/no-change/
insufficient-data verdict. --format tutor-live-verification evaluates all
durable mandatory Tutor holdouts for the current gaggle from each PR's
merge time and first observed post-merge configuration transition. Exact
EffectiveVersion cohorts verify transitions and additions; a removal is
verified when the workflow is absent from the live reconciled config.

Exit codes: 0 = OK (including a clean no-work result), 1 = business error,
2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers telemetry-query --window 24h --format candidate-findings
~~~

## `goobers trace`

show a run's journal events or review verdicts, follow a live run, or show transcripts

~~~text
Usage: goobers trace [--json] [--follow] [--summary | --verdicts] [--transcripts | --transcript=<stage>] <run-id> [path]

Show a run's journal events and, if the telemetry rollup has ingested it,
its trace spans. Use --transcripts to show all recorded agent transcripts,
or --transcript to select one stage. Use --summary for run metadata and
review verdicts, or --verdicts for verdicts alone. With --follow, stream a live run's
events until it finishes; --json --follow emits JSON Lines (default path
"."). Remediation escalations include the typed outcome, attempted flag,
and attempted causes in the text summary and JSON `escalation.remediation`
object. Exit codes: 0 = OK, 1 = run/transcript not found, 2 = usage/IO
error, 130 = interrupted while following.
~~~

**Examples**

~~~console
$ goobers trace <run-id>
$ goobers trace --summary <run-id>
$ goobers trace --verdicts <run-id>
$ goobers trace --follow <run-id>
$ goobers trace --transcripts <run-id>
~~~

## `goobers up`

run the daemon (scheduler + runner + loopback HTTP API)

~~~text
Usage: goobers up [--quiet] [--diagnostics] [--notify[=all]] [--watch-config] [--drain-timeout duration] [--skip-preflight] [--cleanup-spans-only-runs] [--disable-read-model-reads] [path]

Run the daemon: the embedded scheduler (cron triggers + run conditions)
plus the local runner, loopback HTTP API, and configured GitHub webhook
listener (default path "."). Blocks
until interrupted (SIGINT/SIGTERM), then drains in-flight runs indefinitely
by default. --drain-timeout forces shutdown after a deadline; a repeated
signal always forces shutdown without prompting. Interrupted runs resume
from their last durable checkpoints on the next startup before
exiting. Exit codes: 0 = clean shutdown, 1 = daemon/API failure,
2 = usage/IO error.

Legacy spans-only run directories are reported as cleanup candidates
and preserved by default. --cleanup-spans-only-runs deletes them at
startup after reporting each candidate.

Startup validates the resolved instance config and refuses to run on
errors. --skip-preflight bypasses that refusal with a prominent warning.

A Git workflowSource continuously reconciles its tracked ref. Local Git
ref changes wake the loop immediately; periodic fetch-and-compare polling
is always active, and authenticated GitHub push deliveries wake it when
webhook.secret is configured. Invalid revisions are rejected with the
last-known-good definitions left running. --watch-config separately watches
direct edits to the materialized config directory.

--diagnostics turns on deep, opt-in capture for hard hangs: any
deterministic stage still running past a couple of minutes gets a
periodic native process sample + process tree + open-fd (lsof)
snapshot recorded as a run artifact, and stage stdout/stderr are kept
un-truncated. Verbose and slightly heavier; leave off for normal runs.

--disable-read-model-reads is the design's §6.6 read-model rollback: it
forces every list request to scan the authoritative journals for this
run, bypassing both read.db and telemetry.db as run-candidate indexes.
This can be slow on a large history. A flag flip and a restart, not a
deploy — use it if the read-model list path is ever suspected of serving
wrong or incomplete results.

These five behavior controls are intentionally flag-only: --watch-config
selects a process-local development watcher, --diagnostics is temporary
debug capture, --drain-timeout applies only after this process receives a
shutdown signal, --skip-preflight is an unsafe startup escape hatch, and
--disable-read-model-reads is an emergency rollback. Keeping them out of
instance.yaml prevents temporary operational overrides from becoming
durable policy. `goobers status --daemon` reports their effective values.
~~~

**Examples**

~~~console
$ goobers up
$ goobers up --quiet --notify=all
~~~

## `goobers update-behind-pr`

API-update a clean behind-base PR, else route to remediation (a workflow stage)

~~~text
Usage: goobers update-behind-pr [path]

Update one behind-base PR through GitHub's update-branch API when it
is mergeable, CI-clean, and carries no substantive findings. Other
candidates are routed to full remediation. Exit codes: 0 = updated,
routed, or no-work; 1 = business error; 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers update-behind-pr
~~~

## `goobers validate`

validate an instance or checked-in config source tree

~~~text
Usage: goobers validate [--json] [--github-annotations] [--check-harness] [--check-repos] [--source-tree] [--strict] [path]

Validate an instance's instance.yaml and config/ directory (default
path "."). Placement findings (RNR001/RNR003) are errors when
instance.yaml declares a runners: inventory that cannot satisfy some
stage, and warnings otherwise. --source-tree validates a checked-in
config source tree using instance.yaml.example and the path itself as
config/; because the tree carries no real instance.yaml, its placement
solve runs against the example inventory and is advisory-only
(warnings, never errors). --strict treats config warnings as validation errors. --json emits a versioned findings envelope instead of human-readable output. --github-annotations additionally writes each finding to stderr as a
GitHub Actions ::error/::warning file annotation (#687), so a
config-repo PR check surfaces failures directly on the PR diff; composes with --json since stdout stays untouched. --check-harness additionally preflights every agent harness
referenced by a goober (GBO-011) — installed, signed in, actionable
guidance otherwise. --check-repos resolves each target repository's
token, verifies authenticated git access, and (GitHub only) warns when
a repository is larger than the checkout-size threshold. Exit codes:
0 = valid, 1 = validation errors, 2 = usage/IO error.
~~~

**Examples**

~~~console
$ goobers validate
$ goobers validate --json
$ goobers validate --check-harness --check-repos
~~~

## `goobers validate-plan`

validate a decomposition plan against its selector artifact and the live parent (a workflow stage)

~~~text
Usage: goobers validate-plan [path]

validate-plan is the decomposition workflow's `validate-plan` stage
(docs/design/decomposition-workflow.md §3.2/§4, DEC-2). It reads the
decomposition plan design-slices produced and the selection artifact
select-source produced, checks the plan's schema version, its binding to
the selector artifact, every child's shape, the dependency graph, and the
label allowlist, then re-fetches the live parent to detect whether it
changed since selection. It never mutates the provider.

Exit codes: 0 = the plan is valid, or invalid/conflicting (the result file
reports which — not a business error) / 1 = business error (provider/
credential/config/IO error) / 2 = usage error.
~~~

**Examples**

~~~console
$ goobers validate-plan
~~~

## `goobers version`

print build version, commit, and date (--json for structured output)

~~~text
Usage: goobers version [--json]
       goobers --version [--json]

Print the build version, commit, build date, Go toolchain, and platform.

Default output is a single human-readable line. --json emits a structured
object with keys: version, commit, date, goVersion, platform — the same
fields, machine-readable for scripts and support bundles.

Exit codes: 0 = OK, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers --version
$ goobers version --json
~~~

## `goobers versions`

print the supported DSL, Go toolchain, and OS/arch matrix (--json for structured output)

~~~text
Usage: goobers versions [--json]

Print the version-support matrix this build of goobers declares: supported
DSL versions and lifecycle levels, the minimum Go toolchain, supported
OS/arch targets, and where the current host stands in that matrix.

The matrix is host-declared — a build-time constant, not a live probe — so it
answers "what does this binary claim to support?" for operators and support
bundles. Distinct from `goobers version`, which reports this build's own
version/commit/date.

Default output is human-readable. --json emits a structured object with keys:
minGoVersion, dslVersions[] (version, level, unsupportedAfter, replacement,
history[] (level, sinceVersion)),
platforms[] (os, arch, tier), and host — machine-readable for scripts.

Exit codes: 0 = OK, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers versions
$ goobers versions --json
~~~

## `goobers worker`

host a Temporal engine worker: task queues, graceful drain, versioned identity (tier-3, experimental)

~~~text
Usage: goobers worker [--task-queue <queue>]... [flags]

Host a Temporal worker for the tier-3 engine (experimental): connect to the
configured Temporal frontend, register the engine workflow and activities,
and serve the named task queue(s) until SIGTERM/SIGINT, then drain — stop
polling and let in-flight activities finish within --drain-timeout.

The tier-3 engine is not on the local (V0) execution path; this command is
the deployable worker shape for the cloud ladder. Automated gate checks and
workspace provisioning (git worktrees + scratch dirs under --work-root) are
wired. With --instance, the worker also wires the same agentic and
deterministic executors as the local runner; without it, stages needing
those executors fail closed with a clear error.

Flags:
  --instance <dir>           instance root; wires the real agentic and
                             deterministic executors (default
                             $GOOBERS_INSTANCE_ROOT)
  --blob-store <dir>         directory backing the fleet-wide
                             content-addressed artifact store; required
                             for a run whose stages are served by more
                             than one worker (default $GOOBERS_BLOB_STORE)
  --task-queue <queue>       task queue to serve; repeatable (default
                             engine.taskQueue, with env override)
  --temporal-hostport <h:p>  Temporal frontend (default engine.hostPort,
                             with env override)
  --temporal-namespace <ns>  Temporal namespace (default engine.namespace,
                             with env override)
  --drain-timeout <dur>      graceful-drain bound after a shutdown signal
                             (default 30s)
  --work-root <dir>          root for stage workspaces (default: a
                             goobers-worker dir under the OS temp dir)
  --daemon-api <url>         daemon write API base URL; wires live journal
                             emission through the journal plane, with the
                             per-run bearer from $GOOBERS_POD_TOKEN when
                             set (default $GOOBERS_DAEMON_API)
  --config-reload-interval <dur>
                             how often to re-read the instance config
                             tree and rebuild the gaggle seams whose
                             config changed, without a restart; 0
                             disables reload and freezes the worker on
                             its boot-time tree (default 10s; requires
                             --instance)
  --dispatch-namespace <ns>  namespace to create mode-3 stage pods in;
                             wires the dispatcher behind the stage-dispatch
                             seam and serves the per-(gaggle x runner)
                             dispatch queues derived from the instance's
                             runners: inventory. Requires --instance and
                             --blob-store (the surrender plane rides the
                             same volume); cluster access uses in-cluster
                             credentials or the standard kubeconfig rules
                             (default $GOOBERS_DISPATCH_NAMESPACE)

The worker identity reported to Temporal is versioned
(goobers-worker/<build>@<host>#<pid>) so visibility alone answers which
build serves a queue.

With --instance, the worker re-reads its config tree on
--config-reload-interval and atomically replaces the gaggle, credential,
and agentic-kit seams whose config changed, so a definitions edit reaches
the NEXT stage this worker serves without a pod restart. An attempt
already running keeps the kit it was handed. A reload that does not parse
is logged and rejected; the last-known-good tree stays in force.

Exit codes: 0 = clean drain, 1 = startup/connection error, 2 = usage error,
3 = drain timeout expired with in-flight work abandoned.
~~~

**Examples**

~~~console
$ goobers worker
$ goobers worker --task-queue goobers-engine --drain-timeout 60s
~~~

## `goobers workflow`

inspect workflows

~~~text
Usage: goobers workflow show [flags] <name> [path]

Show the named workflow as a text DAG or Graphviz DOT (default path ".").

Workflow

A declarative, versioned step-machine describing triggers, stages, gates, retries, and run conditions.
~~~

## `goobers workflow show`

show a workflow as a text DAG

~~~text
Usage: goobers workflow show [--dot] <name> [path]

Load the named workflow from the instance config and show its stages,
kinds, and transition targets as a text DAG or Graphviz DOT
(default path ".").
~~~

**Examples**

~~~console
$ goobers workflow show default-implement
$ goobers workflow show default-implement --dot
~~~

## `goobers workspace`

explicitly recover pinned repository workspaces

~~~text
Usage: goobers workspace reset <repo> [path]

Explicitly recover a configured pinned workspace. Reset refuses a live
lease, terminates lingering build processes, deletes the workspace, and
performs a full checkout at the same stable path. It never runs
automatically. Default instance path ".".
~~~

**Examples**

~~~console
$ goobers workspace reset my-repo
~~~

## `goobers workspace reset`

tear down and re-materialize a pinned repository workspace

~~~text
Usage: goobers workspace reset <repo> [path]

Tear down and re-materialize the pinned workspace for <repo>. The repo may
be selected by name, owner/name, or (for ADO) owner/project/name. A live
run lease makes the command fail without changing the workspace.
Exit codes: 0 = reset complete, 1 = reset refused/failed, 2 = usage error.
~~~

**Examples**

~~~console
$ goobers workspace reset my-repo
$ goobers workspace reset acme/my-repo ./instance
~~~
