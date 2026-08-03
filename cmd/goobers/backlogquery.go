package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// DefaultClaimLease bounds how long a claimed item stays held before
// localscheduler.ClaimLedger.RecoverExpired (wired into `goobers up`, #131)
// releases it back to the pool.
//
// Shrunk from 6h to 30m by issue #2014, now that a live run's lease is
// periodically renewed (renewLiveClaims, cmd/goobers' claimTicker) rather
// than needing to outlast the run's entire realistic duration in one shot —
// the 6h figure existed only because nothing renewed the lease (see #2014's
// change to RecoverExpired's own doc for that history). 30m is sized off
// claimRecoverInterval (5m): 6x that gives a wide margin for a missed tick or
// slow lock acquisition under shared-host load before a still-renewing run's
// claim could ever reach its own expiry, while still bounding a genuinely
// abandoned claim (a crashed run, or a daemon restart slower than 30m) to a
// reasonable stuck time — down from the old 6h ceiling, not eliminated: a
// restart that takes longer than this can still reap a claim its own
// resumeInterruptedRunsWithRunners would have renewed seconds later, exactly
// as raising DefaultClaimLease to 6h never fully eliminated the analogous gap
// for a slow run either.
//
// Overridable via the leaseDuration Task.Input (a time.ParseDuration
// string) — must be positive; see the leaseDuration parsing below and
// localscheduler.ClaimLedger.Claim's own fail-closed check.
const DefaultClaimLease = 30 * time.Minute

// backlogScanCeiling is the floor on how many candidates a backlog query
// fetches from the provider, independent of maxItems (#532) — "how many to
// scan" and "how many to claim this run" are different questions. High enough
// that the full eligible set is normally covered outright (the live backlog
// runs ~40 eligible items; 250 is ~6x that), low enough to bound provider
// pagination (3 pages at GitHub's per_page=100 max). Truncation past this
// ceiling is starvation-safe because empty windows advance a durable cursor
// and wrap after reaching the end of the oldest-first result set.
const backlogScanCeiling = 250
const backlogScanPageSize = 100

type backlogScanCursor struct {
	Cursor string `json:"cursor,omitempty"`
}

const blockedEligibilitySkipAnnotation = "backlog.blocked-item-skipped"

// blockedOnlyCompletionAnnotation marks a cycle that claimed nothing solely
// because every remaining candidate was skipped as blocked (#1907): without
// this, such a cycle's run.finished(status=completed) is byte-identical to a
// cycle that found a genuinely empty backlog, or one that did real work —
// the exact ambiguity that let a 3.5h claim-selection stall go undetected
// until someone cross-referenced claim.acquired counts against run.finished
// status by hand. One annotation per run (not per skipped item, unlike
// blockedEligibilitySkipAnnotation above) gives a watcher/telemetry query a
// single, unambiguous signal to filter or alert on.
const blockedOnlyCompletionAnnotation = "backlog.completed-with-blocked-only"

const inReviewStatusLabel = "goobers/status:in-review"

type backlogClaimLedger interface {
	Claim(itemID, runID, workflow string, leaseDuration time.Duration) (bool, string, error)
	ClaimScoped(key localscheduler.ClaimKey, runID, workflow string, leaseDuration time.Duration) (bool, string, error)
	ForRunAll(runID string) []localscheduler.ClaimEntry
}

var openBacklogClaimLedger = func(path string, opts ...localscheduler.LedgerOption) (backlogClaimLedger, error) {
	return localscheduler.OpenClaimLedger(path, opts...)
}

func backlogQueryToken(readOnly bool) (string, error) {
	if readOnly {
		return providerToken(capability.GitHubIssuesRead)
	}
	return providerToken(capability.GitHubIssuesWrite)
}

func runBacklogQuery(args []string, stdout, stderr io.Writer) int {
	return runBacklogQueryWithClaimBarrier(args, stdout, stderr, nil)
}

const backlogQueryHelp = "Usage: goobers backlog-query [--read-only | --claim | --reconcile | --release] [path]\n\n" +
	"Query the provider for eligible backlog items — labeled with trustLabel\n" +
	"(SEC-047: required on public repos, since backlog content is untrusted\n" +
	"input otherwise), requireLabels, excludeLabels, and the optional\n" +
	"labelPredicate CEL expression. CEL supports string membership in `labels`\n" +
	"combined with &&, ||, and !. fieldPredicate adds typed comparisons against\n" +
	"provider-native scalar fields using fields[\"name\"]; unavailable fields fail\n" +
	"explicitly. With --claim, claims\n" +
	"exactly one via the local claim ledger (source of truth) mirrored to a\n" +
	"provider-visible marker, and writes it to the declared result file.\n" +
	"trustLabel is required with --claim (SEC-047 fails closed, not open) —\n" +
	"a plain list (no --claim) does not require it. --read-only also bypasses\n" +
	"claim locks, blocked-record reconciliation, scan cursors, and read caches,\n" +
	"and uses only the github:issues:read capability.\n\n" +
	"With --release, removes the provider-visible claim marker and then releases\n" +
	"every claim this run holds in the local ledger (issues #234/#1003). A\n" +
	"workflow that only reads/labels an item, never opening a PR or closing the\n" +
	"issue — e.g. backlog-curation — must release its own claim explicitly,\n" +
	"since issue-close-out's release is reached only by implementation. Claims\n" +
	"require github:issues:write so the label mirror stays symmetric with the\n" +
	"ledger. Idempotent: releasing claims this run does not hold (already\n" +
	"released, e.g. re-run after a crash) is a no-op success, not an error.\n" +
	"With --reconcile, repairs drifted backlog labels against the claim ledger\n" +
	"and live issue/child state, then writes the actual correction count to the\n" +
	"declared result file. --claim, --reconcile, and --release are mutually\n" +
	"exclusive.\n\n" +
	"With --claim, contested-file dispatch awareness (#1085) deprioritizes\n" +
	"claiming an issue whose referenced files are already contested by\n" +
	"contestedFileMinPRs+ (default 2) open PRs, so new work isn't fed into an\n" +
	"overlap cluster faster than merge-review can drain it. It only reorders\n" +
	"candidates (never drops one — an all-contested cycle still claims FIFO)\n" +
	"and falls back to FIFO on any provider error. Disable with input\n" +
	"deprioritizeContestedFiles=false.\n\n" +
	"selectionPriority (#1335) is an opt-in, ordered comma-separated label\n" +
	"list (highest priority first, e.g. \"security,bug\") applied before FIFO:\n" +
	"eligible items carrying an earlier-listed label claim ahead of items\n" +
	"carrying only a later one or none at all; FIFO still breaks ties within\n" +
	"a priority tier. An item carrying more than one listed label ranks by\n" +
	"whichever appears earliest in selectionPriority. Unset (the default)\n" +
	"preserves plain FIFO exactly. fieldOrder is an optional comma-separated\n" +
	"field[:asc|desc] list applied within each label-priority tier before FIFO.\n\n" +
	"backlog-curation may opt into a bounded ready-item re-sweep with\n" +
	"resweepMaxItems. Forward candidates always consume maxItems first; a\n" +
	"re-sweep uses only leftover capacity, no more often than resweepInterval\n" +
	"(default 24h), and rotates within selectionPriority tiers. Ready items\n" +
	"already in implementation/review are emitted as read-only context and are\n" +
	"never claimed.\n\n" +
	"respectAssignee (#1820) is an opt-in claim-scoping flag, default off (the\n" +
	"assignee field is ignored entirely, exactly as today). Once true,\n" +
	"assignedTo selects the mode: a real identity (either a literal login, or\n" +
	"left undeclared so the runner's configured self identity fills it in at\n" +
	"run time) makes only items assigned to that identity eligible — assigned\n" +
	"to anyone else, or unassigned, is excluded. assignedTo declared with no\n" +
	"value is the null/unassigned-only mode: only items with no assignee at\n" +
	"all are eligible.\n\n" +
	"Exit codes: 0 = eligible item found (and claimed, if --claim) / released\n" +
	"(--release), 1 = business error (no eligible/claimable item, missing\n" +
	"trustLabel with --claim, config/credential/provider error), 2 =\n" +
	"usage/IO error.\n"

// The barrier lets the blocked-record race regression pause immediately before
// the lock-protected reconciliation and claim transaction.
func runBacklogQueryWithClaimBarrier(args []string, stdout, stderr io.Writer, beforeClaimTransaction func()) int {
	fs := flag.NewFlagSet("backlog-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	readOnly := fs.Bool("read-only", false, "list backlog items without mutating provider or scheduler state")
	claim := fs.Bool("claim", false, "claim the first eligible item (mirrors the claim in the local ledger + provider)")
	reconcile := fs.Bool("reconcile", false, "repair drifted backlog metadata and report the correction count")
	release := fs.Bool("release", false, "remove provider claim markers and release this run's claim ledger leases early (issues #234/#1003)")
	fs.Usage = helpUsage(stderr, "backlog-query")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if boolCount(*readOnly, *claim, *reconcile, *release) > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	l := layoutFor(root)

	if *release {
		return runBacklogQueryRelease(root, stdout, stderr)
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	// Provider-neutral backlog access (#772 ADO parity): the eligibility scan,
	// claim marking, and list output go through backlogIssueProvider, which both
	// the GitHub and ADO providers satisfy. GitHub-only extras (curation/reconcile
	// metadata, the open-PR eligibility backstop, contested-file dispatch) need
	// the concrete provider and stay gated on ghIssueProvider being non-nil — for
	// ADO they are simply skipped, exactly like a GitHub stage that never opted
	// into github:pr:write.
	var (
		issueProvider   backlogIssueProvider
		ghIssueProvider *providers.GitHubProvider
	)
	// Explicit per-kind dispatch (github | ado | gitea | default-error). The
	// former `if ADO {...} else {...}` GitHub-default silently routed a
	// gitea-provider repo to api.github.com; each backend is now named so a
	// gitea backlog is served by the gitea provider, not GitHub by omission.
	switch repo.Provider {
	case providers.ProviderADO:
		adoProvider, aerr := newADOProviderForStage(root, repo)
		if aerr != nil {
			pf(stderr, "error: %v\n", aerr)
			return 1
		}
		issueProvider = adoProvider
	case providers.ProviderGitea:
		token, terr := backlogQueryToken(*readOnly)
		if terr != nil {
			pf(stderr, "error: %v\n", terr)
			return 1
		}
		var opts []func(*providers.GiteaProvider)
		if !*readOnly {
			opts = append(opts, providers.WithGiteaMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
		}
		giteaProvider, gerr := newGiteaProviderForStage(root, repo, token, opts...)
		if gerr != nil {
			pf(stderr, "error: %v\n", gerr)
			return 1
		}
		issueProvider = giteaProvider
	case providers.ProviderGitHub:
		token, terr := backlogQueryToken(*readOnly)
		if terr != nil {
			pf(stderr, "error: %v\n", terr)
			return 1
		}
		if *readOnly {
			ghIssueProvider = newGitHubProvider(token)
		} else {
			ghIssueProvider = newCachedGitHubProvider(root, token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
		}
		issueProvider = ghIssueProvider
	default:
		pf(stderr, "error: backlog-query does not support repository provider %q\n", repo.Provider)
		return 1
	}

	// ADO splits the code repository (where branches/PRs land) from the backlog
	// project (where PBIs live), so work-item provider calls — list, dependency/
	// blocked checks, and claim — must address the backlog project rather than
	// the routed code repo. On GitHub the two coincide and backlogRepo == repo.
	backlogRepo := backlogRepoRefForStage(root, repo)

	trustLabel := providerInput("trustLabel", "")
	requireLabels := splitLabelList(providerInput("requireLabels", ""))
	excludeLabels := splitLabelList(providerInput("excludeLabels", ""))
	labelExpression := providerInput("labelPredicate", "")
	labelFilter, err := labelpredicate.Compile(labelExpression, requireLabels, excludeLabels)
	if err != nil {
		pf(stderr, "error: invalid labelPredicate: %v\n", err)
		return 1
	}
	fieldExpression := providerInput("fieldPredicate", "")
	fieldFilter, err := fieldpredicate.Compile(fieldExpression)
	if err != nil {
		pf(stderr, "error: invalid fieldPredicate: %v\n", err)
		return 1
	}
	fieldOrderExpression := providerInput("fieldOrder", "")
	fieldOrder, err := fieldpredicate.ParseOrder(fieldOrderExpression)
	if err != nil {
		pf(stderr, "error: invalid fieldOrder: %v\n", err)
		return 1
	}
	// selectionPriority is #1335's opt-in priority contract: an ordered label
	// list, highest priority first. Empty (the default) preserves #350's
	// plain FIFO claim order unchanged — priority is strictly additive.
	selectionPriority := splitLabelList(providerInput("selectionPriority", ""))
	// respectAssignee/assignedTo (#1820, COORD-2): opt-in claim-scoping
	// filter, mirroring how trustLabel/requireLabels/excludeLabels already
	// work. Unset (the default) is byte-identical to today — assignedTo is
	// read but never consulted below unless respectAssignee is explicitly
	// "true". Once opted in, assignedTo's resolved value selects the mode:
	// a real identity — a literal login, or the runner's configured self
	// identity when the task leaves assignedTo undeclared
	// (defaultBacklogQueryAssignedTo, internal/runner/run.go) — requires an
	// exact match; empty (declared with no value, or undeclared with no
	// self identity configured) is the null/unassigned-only mode. Both
	// modes are the same equality check (item.Assignee == assignedTo): "must
	// equal <login>" and "must be empty" are the same comparison, so there
	// is no separate null-mode branch to get wrong.
	respectAssignee := providerInput("respectAssignee", "") == "true"
	assignedTo := providerInput("assignedTo", "")
	curationRun := *claim && os.Getenv("GOOBERS_WORKFLOW") == "backlog-curation"
	reconcileBeforeClaim := curationRun && providerInput("reconcileMetadata", "true") != "false"
	var stalenessPolicy backlogStalenessPolicy
	if reconcileBeforeClaim || *reconcile {
		stalenessPolicy, err = readBacklogStalenessPolicy()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}

	// maxItems caps how many eligible items one --claim run claims (#236): it was
	// a dead input everywhere (the query hardcoded a limit and --claim took
	// exactly one), so a documented input was silently ignored — the #130 class
	// of gap. Default 1 (the single-item implementation shape).
	maxItems := 1
	if s := providerInput("maxItems", ""); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n < 1 {
			pf(stderr, "error: invalid maxItems %q (want a positive integer)\n", s)
			return 1
		}
		maxItems = n
	}
	resweepPolicy, resweepEnabled, err := readBacklogResweepPolicy(maxItems)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if resweepEnabled && !curationRun {
		pln(stderr, "error: re-sweep inputs are only valid for backlog-curation --claim runs")
		return 1
	}
	// How many candidates to SCAN is deliberately decoupled from how many to
	// CLAIM (#532): the old scan window was max(maxItems, 20), so once
	// maxItems reached 20 (curation's batch size) the two were the same
	// variable — a fetch window of exactly the claim count, filled newest-first
	// by GitHub's default sort, permanently starved everything older
	// (issues #434–#463 for 3+ hours; #441 unclaimed for 4+ hours, live).
	// The wide ceiling gives client-side filtering (trust re-verify,
	// excludeLabels, the open-PR backstop below) real headroom: even if every
	// item in a claim-count-sized prefix were filtered, eligible items beyond
	// it are still in view. OldestFirst on the fetch (below) covers the
	// truncation case past even this ceiling: dropping the NEWEST items is
	// safe because FIFO claiming drains the queue from the front, so they
	// become reachable as older items complete — no item is ever permanently
	// invisible.
	scanLimit := maxItems
	if scanLimit < backlogScanCeiling {
		scanLimit = backlogScanCeiling
	}

	// SEC-047 fails CLOSED, not open: an empty trustLabel must refuse to
	// claim, not silently skip the trust check and claim anything eligible
	// by requireLabels alone — backlog content on a public repo is untrusted
	// input, and claiming is the mutating, consequential action (it starts
	// implementation work). A read-only list (no --claim) is informational,
	// so it's not gated the same way.
	if (*claim || *reconcile) && trustLabel == "" {
		pln(stderr, "error: trustLabel is required to claim or reconcile (SEC-047: backlog content is untrusted input on a public repo) — declare inputs.trustLabel")
		return 1
	}
	var runID, workflow string
	if *claim {
		runID, workflow, err = providerRunContext()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	if *readOnly {
		return runReadOnlyBacklogQuery(
			ctx,
			issueProvider,
			backlogRepo,
			trustLabel,
			labelFilter,
			fieldFilter,
			fieldOrder,
			selectionPriority,
			respectAssignee,
			assignedTo,
			stdout,
			stderr,
		)
	}
	observedAt := time.Now().UTC()

	if curationRun || *reconcile {
		if ghIssueProvider == nil {
			// Curation/reconcile metadata is the GitHub V0 backlog workload
			// (staleness reconciliation, native-dependency edits); it has no
			// ADO parity yet (BL-033). Fail closed with an actionable message
			// rather than silently skipping a reconcile the caller asked for.
			resultFile := "claimed-items.json"
			if *reconcile {
				resultFile = "backlog-reconciliation.json"
			}

			return failProviderStage(stderr, "reconcile backlog metadata", fmt.Errorf("backlog curation/reconcile is not supported on Azure DevOps yet (BL-033); run it against a GitHub backlog"), resultFile)
		}
		reconciled, reconcileErr := reconcileBacklogMetadata(
			ctx,
			l,
			ghIssueProvider,
			repo,
			trustLabel,
			stalenessPolicy,
			func() time.Time { return observedAt },
		)
		if reconcileErr != nil {
			resultFile := "claimed-items.json"
			if *reconcile {
				resultFile = "backlog-reconciliation.json"
			}
			return failProviderStage(stderr, "reconcile backlog metadata", reconcileErr, resultFile)
		}
		if *reconcile {
			return writeBacklogReconciliationResult(reconciled, stdout, stderr)
		}
	}

	var (
		prProvider *providers.GitHubProvider
		openIssues map[string]bool
	)
	// The open-PR eligibility backstop and closed-unmerged requeue read pull
	// requests through the GitHub PR API, so they need both a github:pr:write
	// token and the concrete GitHub provider. An ADO stage (ghIssueProvider nil)
	// gets exactly the pre-backstop label-only behavior — no hard failure.
	if prToken, tokenErr := providerToken(capability.GitHubPRWrite); tokenErr == nil && ghIssueProvider != nil {
		prProvider = newCachedGitHubProvider(root, prToken)
		openIssues, err = openPRIssueNumbers(ctx, prProvider, repo)
		if err != nil {
			return failProviderStage(stderr, "list open pull requests", err, "claimed-item.json")
		}
		if *claim {
			if err := reconcileClosedUnmergedInReview(ctx, ghIssueProvider, prProvider, repo); err != nil {
				return failProviderStage(stderr, "reconcile closed pull requests", err, "claimed-item.json")
			}
		}
	}

	labels := make([]string, 0, 1+len(requireLabels))
	if trustLabel != "" {
		labels = append(labels, trustLabel)
	}
	labels = append(labels, labelFilter.RequiredLabels()...)
	// queryAssignee narrows the provider query server-side only for a real
	// identity (fixed or runtime-derived) — providers' Assignee request
	// field means "must equal this login", so it has no way to express
	// "must be unassigned"; the null/unassigned-only mode (assignedTo=="")
	// relies entirely on the client-side re-verify below instead.
	queryAssignee := ""
	if respectAssignee && assignedTo != "" {
		queryAssignee = assignedTo
	}
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	cursorPath := backlogScanCursorPath(
		l.SchedulerDir(), backlogRepo, trustLabel, labelExpression, fieldExpression,
		requireLabels, excludeLabels, queryAssignee,
	)
	exhaustiveScan := fieldOrder.Configured()
	scanCursor := backlogScanCursor{}
	if !exhaustiveScan {
		scanCursor, err = readBacklogScanCursor(lockPath, cursorPath)
		if err != nil {
			pf(stderr, "error: read backlog scan cursor: %v\n", err)
			return 1
		}
	}
	items, nextScanCursor, err := listBacklogScanWindow(
		ctx, issueProvider, backlogRepo, labels, queryAssignee, fieldFilter, scanLimit, scanCursor, exhaustiveScan,
	)
	if err != nil {
		return failProviderStage(stderr, "list work items", err, "claimed-item.json")
	}

	// Re-verify eligibility in code (SEC-047: backlog content is untrusted
	// input on a public repo) rather than trusting the provider query's
	// labels filter alone — a defense-in-depth check, not a redundant one.
	// The assignee check follows the same discipline and is the sole
	// enforcement for the null/unassigned-only mode (queryAssignee above
	// narrows nothing when assignedTo is empty).
	var eligible []providers.WorkItem
	for _, item := range items {
		if trustLabel != "" && !item.HasLabel(trustLabel) {
			continue
		}
		if respectAssignee && item.Assignee != assignedTo {
			continue
		}
		matched, matchErr := labelFilter.Matches(item.Labels)
		if matchErr != nil {
			pf(stderr, "error: evaluate labelPredicate for item %s: %v\n", item.ID, matchErr)
			return 1
		}
		if !matched {
			continue
		}
		matched, matchErr = fieldFilter.Matches(item.Fields)
		if matchErr != nil {
			pf(stderr, "error: evaluate fieldPredicate for item %s: %v\n", item.ID, matchErr)
			return 1
		}
		if !matched {
			continue
		}
		// Defense-in-depth state re-verify (#947): the provider query above
		// already filters State:"open", but a closed issue that still carries
		// goobers:ready/goobers:approved (label bookkeeping that did not run on
		// close — the exact incoherent state #947 documents) must never be
		// claimable regardless of its labels. Re-check state in code, the same
		// SEC-047 "don't trust the provider filter alone" discipline the label
		// re-verify above applies. Empty State (a provider that doesn't report
		// it) is left to the query's own filter, not treated as ineligible.
		if item.State != "" && !strings.EqualFold(item.State, "open") {
			continue
		}
		item.Integrity = providers.IntegrityForLabels(item.Labels, trustLabel)
		eligible = append(eligible, item)
	}

	// Open-PR eligibility backstop (#414 design point 2): excludeLabels alone
	// depends on a label write at PR-open time (implementation.yaml's
	// goobers/status:in-review) that can be missed or, after close-out,
	// removed without the issue ever actually closing (issue-close-out's
	// status=in-review keeps the issue open until the merge event). Without
	// this, a completed rung's issue can look eligible again and get
	// re-claimed into a duplicate PR. Best-effort on capability: only runs
	// when the stage actually declares github:pr:write (implementation.yaml
	// and backlog-curation.yaml both do); a stage that hasn't opted in gets
	// exactly the pre-#414 label-only behavior, not a hard failure — this is
	// a backstop on top of the label check above, not a replacement for it.
	if openIssues != nil {
		backstopped := eligible[:0]
		for _, item := range eligible {
			if openIssues[item.ID] {
				continue
			}
			backstopped = append(backstopped, item)
		}
		eligible = backstopped
	}

	eligible, dependencyWarnings := filterDeclaredDependencyEligibility(ctx, issueProvider, backlogRepo, eligible)
	for _, warning := range dependencyWarnings {
		pf(stderr, "warning: native issue dependencies: %s\n", warning)
	}

	// Dependency-aware skip (#552): snapshot blocked.json under its local
	// lock, then resolve every provider-backed issue state after releasing it.
	// A stalled provider must never prevent terminal claim finalization.
	observedRecords, err := snapshotBlockedRecordsForRepository(l, backlogRepo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	remainingRecords := make(map[string]blockedRecord, len(observedRecords))
	for itemID, record := range observedRecords {
		remainingRecords[itemID] = record
	}
	_, observedSkips, _, blockedWarnings := filterBlockedEligibility(
		ctx,
		issueProvider,
		backlogRepo,
		append([]providers.WorkItem(nil), eligible...),
		remainingRecords,
	)
	for _, warning := range blockedWarnings {
		// Warn, never fail the whole query: only the affected record stays
		// parked when its provider state cannot be resolved.
		pf(stderr, "warning: blocked records: %s\n", warning)
	}
	// Re-affirm cycle escalation on every tick (#1405), using the freshest,
	// post-self-heal record set: a fully skip-parked cycle member is never
	// reclaimed and so never re-runs its own blocked-handler to notice a
	// sibling whose labels drifted without a blocked.json change. Best-effort
	// and warn-only, like filterBlockedEligibility above — a config load or
	// provider hiccup here must not block the eligibility scan itself.
	if needsHumanAssignee, cfgErr := needsHumanAssigneeFor(l); cfgErr != nil {
		pf(stderr, "warning: blocked cycle reconciliation: %v\n", cfgErr)
	} else {
		for _, warning := range reconcileBlockedCycleLabels(ctx, issueProvider, remainingRecords, needsHumanAssignee) {
			pf(stderr, "warning: blocked cycle reconciliation: %s\n", warning)
		}
	}
	verifiedSkips := make(map[string]blockedEligibilitySkip, len(observedSkips))
	for _, skip := range observedSkips {
		verifiedSkips[skip.ItemID] = skip
	}

	// Claim order was an accident of whichever sort order the provider's
	// List endpoint happens to default to (#350) — GitHub's is undocumented
	// desc-by-created (newest-first), the exact opposite of the README's
	// assumed "natural claim order is ~ascending issue #". Sorting
	// client-side, provider-independent, pins a deterministic FIFO default
	// (oldest-filed-first, the starvation-safe choice) so a future provider
	// API change — or a provider whose own default happens to differ from
	// GitHub's — can't silently flip claim order again. selectionPriority
	// (#1335) is the opt-in priority tier layered on top of that baseline:
	// FIFO still breaks ties within a tier, and an unconfigured
	// selectionPriority ranks every item identically, so behavior is
	// byte-identical to plain FIFO until an operator opts in.
	if err := sortEligibleByFields(eligible, selectionPriority, fieldOrder); err != nil {
		pf(stderr, "error: apply fieldOrder: %v\n", err)
		return 1
	}
	forwardEligibleCount := len(eligible)

	var (
		readOnlyResweep      []providers.WorkItem
		curationModeByID     map[string]string
		resweepState         backlogResweepState
		resweepStatePath     string
		resweepStateObserved uint64
		resweepStateDirty    bool
		selectedResweepItems []providers.WorkItem
	)
	if resweepEnabled && len(eligible) < maxItems {
		resweepStatePath = backlogResweepStatePath(
			l.SchedulerDir(), repo, providerGaggle(), trustLabel, resweepPolicy.readyLabel,
		)
		resweepState, err = readBacklogResweepState(lockPath, resweepStatePath)
		if err != nil {
			pf(stderr, "error: read backlog re-sweep state: %v\n", err)
			return 1
		}
		resweepStateObserved = resweepState.Generation
		if backlogResweepDue(resweepState, observedAt, resweepPolicy.interval) {
			curationModeByID = make(map[string]string)
			blockedCandidates, nextBlockedCursor, listErr := listBacklogScanWindow(
				ctx,
				issueProvider,
				repo,
				compactLabels(trustLabel, blockedOnSiblingLabel),
				"",
				fieldFilter,
				backlogScanCeiling,
				backlogScanCursor{Cursor: resweepState.BlockedCursor},
				false,
			)
			if listErr != nil {
				return failProviderStage(stderr, "list blocked items for dependency recheck", listErr, "claimed-items.json")
			}
			filteredBlocked := blockedCandidates[:0]
			for _, item := range blockedCandidates {
				if !item.HasLabel(trustLabel) ||
					!item.HasLabel(blockedOnSiblingLabel) ||
					item.HasLabel(providers.LabelNeedsHuman) ||
					(item.State != "" && !strings.EqualFold(item.State, "open")) {
					continue
				}
				item.Integrity = providers.IntegrityForLabels(item.Labels, trustLabel)
				filteredBlocked = append(filteredBlocked, item)
			}
			blockedCandidates = filteredBlocked
			if err := sortBacklogResweepCandidates(
				blockedCandidates,
				selectionPriority,
				fieldOrder,
				resweepState.LastSweptAt,
			); err != nil {
				pf(stderr, "error: order blocked dependency rechecks: %v\n", err)
				return 1
			}
			if len(blockedCandidates) > resweepPolicy.maxItems {
				blockedCandidates = blockedCandidates[:resweepPolicy.maxItems]
			}
			selectedResweepItems = append(selectedResweepItems, blockedCandidates...)
			for _, item := range blockedCandidates {
				blockers, blockerErr := ghIssueProvider.ListWorkItemBlockers(ctx, repo, item.ID)
				if blockerErr != nil {
					pf(stderr, "warning: dependency recheck item %s: %v\n", item.ID, blockerErr)
					continue
				}
				if len(blockers) == 0 {
					pf(stderr, "warning: dependency recheck item %s has no named native blocker; leaving it parked\n", item.ID)
					continue
				}
				actionable := true
				for _, blocker := range blockers {
					if blocker.State != "" && strings.EqualFold(blocker.State, "open") {
						if blocker.HasLabel(providers.LabelNeedsHuman) {
							continue
						}
						actionable = false
						break
					}
				}
				if actionable {
					eligible = append(eligible, item)
					curationModeByID[item.ID] = "dependency-recheck"
				}
			}
			resweepState.BlockedCursor = nextBlockedCursor.Cursor

			resweepCandidates, nextResweepCursor, listErr := listBacklogScanWindow(
				ctx,
				issueProvider,
				repo,
				compactLabels(trustLabel, resweepPolicy.readyLabel),
				// Re-sweep is read-only visibility across already in-flight
				// items regardless of assignee, not new claim eligibility —
				// respectAssignee does not apply here.
				"",
				fieldFilter,
				backlogScanCeiling,
				backlogScanCursor{Cursor: resweepState.Cursor},
				false,
			)
			if listErr != nil {
				return failProviderStage(stderr, "list ready items for re-sweep", listErr, "claimed-items.json")
			}
			filtered := resweepCandidates[:0]
			for _, item := range resweepCandidates {
				if !item.HasLabel(trustLabel) ||
					!item.HasLabel(resweepPolicy.readyLabel) ||
					item.HasLabel(providers.LabelNeedsHuman) ||
					(item.State != "" && !strings.EqualFold(item.State, "open")) {
					continue
				}
				matched, matchErr := fieldFilter.Matches(item.Fields)
				if matchErr != nil {
					pf(stderr, "error: evaluate fieldPredicate for re-sweep item %s: %v\n", item.ID, matchErr)
					return 1
				}
				if matched {
					item.Integrity = providers.IntegrityForLabels(item.Labels, trustLabel)
					filtered = append(filtered, item)
				}
			}
			resweepCandidates = filtered
			if err := sortBacklogResweepCandidates(
				resweepCandidates,
				selectionPriority,
				fieldOrder,
				resweepState.LastSweptAt,
			); err != nil {
				pf(stderr, "error: order backlog re-sweep: %v\n", err)
				return 1
			}
			resweepBudget := min(resweepPolicy.maxItems, maxItems-len(eligible))
			if len(resweepCandidates) > resweepBudget {
				resweepCandidates = resweepCandidates[:resweepBudget]
			}
			selectedResweepItems = append(selectedResweepItems, resweepCandidates...)
			for _, item := range resweepCandidates {
				if item.HasLabel(inReviewStatusLabel) ||
					item.HasLabel(providers.LabelClaimed) ||
					openIssues[item.ID] {
					readOnlyResweep = append(readOnlyResweep, item)
					curationModeByID[item.ID] = "read-only"
					continue
				}
				eligible = append(eligible, item)
				curationModeByID[item.ID] = "resweep"
			}
			resweepState = recordBacklogResweep(
				resweepState,
				selectedResweepItems,
				observedAt,
				resweepPolicy.interval,
			)
			resweepState.Cursor = nextResweepCursor.Cursor
			resweepStateDirty = true
		}
	}

	persistResweepState := func() error {
		if !resweepStateDirty {
			return nil
		}
		return advanceBacklogResweepState(
			lockPath,
			resweepStatePath,
			resweepStateObserved,
			resweepState,
		)
	}

	if !*claim {
		err = withClaimLock(lockPath, claimLockOperationBacklogFilterBlocked, func() error {
			var rerr error
			eligible, _, rerr = reconcileBlockedEligibilityLocked(
				blockedRecordsPath(l),
				backlogRepo,
				eligible,
				observedRecords,
				remainingRecords,
				verifiedSkips,
			)
			return rerr
		})
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}

		if len(eligible) == 0 {
			if err := advanceBacklogScanCursor(lockPath, cursorPath, scanCursor, nextScanCursor); err != nil {
				pf(stderr, "error: advance backlog scan cursor: %v\n", err)
				return 1
			}
			pln(stdout, "no eligible items")
			return 0
		}
		for _, item := range eligible {
			pf(stdout, "%s\t%s\n", item.ID, item.Title)
		}
		return 0
	}

	// Contested-file dispatch awareness (#1085): with more than one candidate
	// in hand, deprioritize claiming an issue whose referenced files are
	// already contested by contestedFileMinPRs+ open PRs, so `implementation`
	// stops feeding new work into an overlap cluster faster than merge-review
	// can drain it. Soft + best-effort by construction (contestedfiles.go):
	// it only REORDERS the FIFO'd candidates (never drops one, so a cycle where
	// every candidate is contested still claims FIFO — no starvation), and any
	// provider error falls back to plain FIFO rather than stalling dispatch.
	// Gated on github:pr:write, exactly like the open-PR backstop above, since
	// it lists open PRs and their files.
	if len(eligible) > 1 && providerInput("deprioritizeContestedFiles", "true") == "true" {
		if prProvider != nil {
			minPRs := 2
			if s := providerInput("contestedFileMinPRs", ""); s != "" {
				if n, perr := strconv.Atoi(s); perr == nil && n >= 1 {
					minPRs = n
				} else {
					pf(stderr, "warning: invalid contestedFileMinPRs %q; using %d\n", s, minPRs)
				}
			}
			if touches, terr := openPRTouches(ctx, prProvider, repo, ""); terr != nil {
				pf(stderr, "warning: contested-file dispatch awareness unavailable (%v); using FIFO order\n", terr)
			} else {
				forwardEligible := eligible[:forwardEligibleCount]
				resweepEligible := eligible[forwardEligibleCount:]
				reordered, deprioritized := partitionByContention(forwardEligible, touches, minPRs)
				if n := len(deprioritized); n > 0 && n < len(reordered) {
					pf(stderr, "contested-file dispatch: deprioritized %d contested issue(s) [%s] behind %d disjoint one(s)\n",
						n, strings.Join(deprioritized, ","), len(reordered)-n)
				}
				eligible = append(reordered, resweepEligible...)
			}
		}
	}

	if len(eligible) == 0 && len(readOnlyResweep) == 0 {
		err = withClaimLock(lockPath, claimLockOperationBacklogFilterBlocked, func() error {
			_, _, rerr := reconcileBlockedEligibilityLocked(
				blockedRecordsPath(l),
				backlogRepo,
				eligible,
				observedRecords,
				remainingRecords,
				verifiedSkips,
			)
			return rerr
		})
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		if err := advanceBacklogScanCursor(lockPath, cursorPath, scanCursor, nextScanCursor); err != nil {
			pf(stderr, "error: advance backlog scan cursor: %v\n", err)
			return 1
		}
		if err := persistResweepState(); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		return writeNoWorkResult(stdout, stderr, "no eligible item to claim")
	}
	leaseDuration := DefaultClaimLease
	if s := providerInput("leaseDuration", ""); s != "" {
		d, perr := time.ParseDuration(s)
		if perr != nil {
			pf(stderr, "error: invalid leaseDuration %q: %v\n", s, perr)
			return 1
		}
		// Fail closed here too, not just in ClaimLedger.Claim (issue #235,
		// edge 1): a non-positive duration is a workflow-authoring mistake,
		// not a business condition — catching it before ever reaching the
		// ledger gives a caller-facing, actionable error instead of a claim
		// silently having no exclusivity.
		if d <= 0 {
			pf(stderr, "error: invalid leaseDuration %q: must be positive\n", s)
			return 1
		}
		leaseDuration = d
	}

	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		pf(stderr, "error: open instance log: %v\n", err)
		return 1
	}
	defer func() { _ = instanceLog.Close() }()

	// Claim up to maxItems eligible items under this run (#236): curation runs a
	// batch (maxItems 20), implementation a single item (maxItems 1). All claims
	// share this run's id; each item gets its own ledger entry.
	var claimed, newlyClaimed []providers.WorkItem
	claimResultCommitted := false

	gaggle := providerGaggle()
	if beforeClaimTransaction != nil {
		beforeClaimTransaction()
	}
	nextClaimIndex := 0
	claimSetPrepared := false
	var preexistingClaimIDs map[string]struct{}
	acquireClaims := func() error {
		return withClaimLock(lockPath, claimLockOperationBacklogClaim, func() error {
			if !claimSetPrepared {
				var lerr error
				eligible, observedSkips, lerr = reconcileBlockedEligibilityLocked(
					blockedRecordsPath(l),
					backlogRepo,
					eligible,
					observedRecords,
					remainingRecords,
					verifiedSkips,
				)
				if lerr != nil {
					return lerr
				}
				for _, skip := range observedSkips {
					runner := map[string]any{
						"annotation":   blockedEligibilitySkipAnnotation,
						"itemId":       skip.ItemID,
						"openBlockers": skip.OpenBlockers,
					}
					if skip.ItemStateUnresolved {
						runner["itemStateUnresolved"] = true
					}
					if len(skip.UnresolvedBlockers) != 0 {
						runner["unresolvedBlockers"] = skip.UnresolvedBlockers
					}
					if skip.VerificationPending {
						runner["verificationPending"] = true
					}
					if jerr := instanceLog.Append(journal.Event{
						Type:     journal.EventRunnerAnnotation,
						Workflow: workflow,
						RunID:    runID,
						Reason:   skip.reason(),
						Runner:   runner,
					}); jerr != nil {
						return fmt.Errorf("journal blocked eligibility skip for %s: %w", skip.ItemID, jerr)
					}
				}
				claimSetPrepared = true
			}

			ledger, lerr := openBacklogClaimLedger(
				filepath.Join(l.SchedulerDir(), claimLedgerFileName),
				localscheduler.WithInstanceLog(instanceLog),
			)
			if lerr != nil {
				return fmt.Errorf("open claim ledger: %w", lerr)
			}
			if preexistingClaimIDs == nil {
				preexistingClaimIDs = make(map[string]struct{})
				for _, entry := range ledger.ForRunAll(runID) {
					if gaggle == "" {
						if entry.Gaggle == "" && entry.Provider == "" {
							preexistingClaimIDs[entry.ItemID] = struct{}{}
						}
						continue
					}
					if entry.Gaggle == gaggle && entry.Provider == string(repo.Provider) {
						preexistingClaimIDs[entry.ExternalID] = struct{}{}
					}
				}
			}
			for nextClaimIndex < len(eligible) && len(claimed) < maxItems {
				item := eligible[nextClaimIndex]
				nextClaimIndex++
				var ok bool
				var cerr error
				if gaggle == "" {
					ok, _, cerr = ledger.Claim(item.ID, runID, workflow, leaseDuration)
				} else {
					ok, _, cerr = ledger.ClaimScoped(localscheduler.ClaimKey{
						Gaggle:     gaggle,
						Provider:   string(repo.Provider),
						ExternalID: item.ID,
					}, runID, workflow, leaseDuration)
				}
				if cerr != nil {
					return fmt.Errorf("claim %s in ledger: %w", item.ID, cerr)
				}
				if ok {
					claimed = append(claimed, item)
					if _, preexisting := preexistingClaimIDs[item.ID]; !preexisting {
						newlyClaimed = append(newlyClaimed, item)
					}
				}
			}
			return nil
		})
	}
	releaseClaim := func(item providers.WorkItem) error {
		return withClaimLock(lockPath, claimLockOperationBacklogRelease, func() error {
			ledger, lerr := localscheduler.OpenClaimLedger(
				filepath.Join(l.SchedulerDir(), claimLedgerFileName),
				localscheduler.WithInstanceLog(instanceLog),
			)
			if lerr != nil {
				return fmt.Errorf("open claim ledger: %w", lerr)
			}
			if gaggle == "" {
				return ledger.Release(item.ID, runID)
			}
			return ledger.ReleaseScoped(localscheduler.ClaimKey{
				Gaggle:     gaggle,
				Provider:   string(repo.Provider),
				ExternalID: item.ID,
			}, runID)
		})
	}
	defer func() {
		if claimResultCommitted {
			return
		}
		for _, item := range newlyClaimed {
			if releaseErr := releaseClaim(item); releaseErr != nil {
				pf(stderr, "error: roll back claim %s after backlog query failure: %v\n", item.ID, releaseErr)
			}
		}
	}()

	malformedReadyItems := 0
	for !claimSetPrepared || (len(claimed) < maxItems && nextClaimIndex < len(eligible)) {
		firstNewClaim := len(claimed)
		if err := acquireClaims(); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		if !labelFilter.ReferencesLabel(providers.LabelReady) {
			continue
		}
		for i := firstNewClaim; i < len(claimed); {
			if !claimed[i].HasLabel(providers.LabelReady) {
				i++
				continue
			}
			transitions, transitionErr := issueProvider.ListWorkItemLabelTransitionsForItem(
				ctx, backlogRepo, claimed[i].ID, providers.LabelReady,
			)
			if transitionErr != nil {
				return failProviderStage(stderr, "read ready-label transitions", transitionErr, "claimed-item.json")
			}
			if transitionErr := annotateReadyTimes(claimed[i:i+1], providers.LabelReady, transitions); transitionErr != nil {
				malformed := claimed[i]
				if releaseErr := releaseClaim(malformed); releaseErr != nil {
					pf(stderr, "error: release malformed eligible item %s: %v\n", malformed.ID, releaseErr)
					return 1
				}
				pf(stderr, "warning: skipping malformed eligible item %s: measure ready age: %v\n", malformed.ID, transitionErr)
				claimed = append(claimed[:i], claimed[i+1:]...)
				malformedReadyItems++
				continue
			}
			i++
		}
	}
	if len(eligible) == 0 && len(readOnlyResweep) == 0 {
		if err := advanceBacklogScanCursor(lockPath, cursorPath, scanCursor, nextScanCursor); err != nil {
			pf(stderr, "error: advance backlog scan cursor: %v\n", err)
			return 1
		}
		if err := persistResweepState(); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		reason := "no eligible item to claim"
		if len(observedSkips) > 0 {
			// This cycle's only candidate(s) were all blocked — distinct from a
			// genuinely empty backlog (#1907). See blockedOnlyCompletionAnnotation.
			reason = fmt.Sprintf("no eligible item to claim (%d blocked candidate(s) skipped this cycle)", len(observedSkips))
			if jerr := instanceLog.Append(journal.Event{
				Type:     journal.EventRunnerAnnotation,
				Workflow: workflow,
				RunID:    runID,
				Reason:   reason,
				Runner: map[string]any{
					"annotation":     blockedOnlyCompletionAnnotation,
					"skippedBlocked": len(observedSkips),
				},
			}); jerr != nil {
				pf(stderr, "warning: journal blocked-only completion summary: %v\n", jerr)
			}
		}
		return writeNoWorkResult(stdout, stderr, reason)
	}
	// Every eligible item is already claimed by another run — a routine no-work
	// tick (#233), not an error: exit 0 with the structured noWork result the
	// runner short-circuits on, rather than the old return 1. Batch-aware len
	// check (#236) replaces #274's pointer-nil check.
	if len(claimed) == 0 && len(readOnlyResweep) == 0 {
		if err := advanceBacklogScanCursor(lockPath, cursorPath, scanCursor, nextScanCursor); err != nil {
			pf(stderr, "error: advance backlog scan cursor: %v\n", err)
			return 1
		}
		if malformedReadyItems > 0 {
			if err := persistResweepState(); err != nil {
				pf(stderr, "error: %v\n", err)
				return 1
			}
			return writeNoWorkResult(stdout, stderr, "no well-formed eligible item could be claimed")
		}
		if err := persistResweepState(); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		return writeNoWorkResult(stdout, stderr, "every eligible item is already claimed by another run")
	}

	var curationItems []curationClaimedItem
	if curationRun {
		// curationRun is GitHub-only (rejected above for ADO), so ghIssueProvider
		// is non-nil here.
		curationItems, err = enrichClaimedItemsWithStaleness(ctx, ghIssueProvider, repo, claimed, observedAt, stalenessPolicy)
		if err != nil {
			return failProviderStage(stderr, "compute claimed-item staleness", err, "claimed-items.json")
		}
		for i := range curationItems {
			curationItems[i].CurationMode = curationModeByID[curationItems[i].ID]
		}
		readOnlyItems, enrichErr := enrichClaimedItemsWithStaleness(
			ctx,
			ghIssueProvider,
			repo,
			readOnlyResweep,
			observedAt,
			stalenessPolicy,
		)
		if enrichErr != nil {
			return failProviderStage(stderr, "compute read-only re-sweep staleness", enrichErr, "claimed-items.json")
		}
		for i := range readOnlyItems {
			readOnlyItems[i].CurationMode = "read-only"
			readOnlyItems[i].ReadOnly = true
		}
		curationItems = append(curationItems, readOnlyItems...)
	}

	// Result-file shape follows the workflow's cardinality: a single-item run
	// (maxItems 1, implementation) writes the claimed WorkItem as an object so
	// its scalar fields (id/title) merge into the stage's journaled Outputs
	// (open-pr's #241 issue linkage reads them); a batch run (maxItems >1,
	// curation) writes the array the curator persona expects. The stage lifts
	// this file into an artifact only when the workflow declares the resultFile
	// input — curation's #236 fix adds it so the batch actually reaches curate.
	resultFile := providerInput("resultFile", "claimed-item.json")
	var data []byte
	if curationRun && maxItems == 1 {
		data, err = json.Marshal(curationItems[0])
	} else if curationRun {
		data, err = json.Marshal(curationItems)
	} else if maxItems == 1 {
		data, err = json.Marshal(claimed[0])
	} else {
		data, err = json.Marshal(claimed)
	}
	if err != nil {
		pf(stderr, "error: marshal claimed item(s): %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	if err := persistResweepState(); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	// Provider-visible marker per claimed item: best-effort mirror of the
	// ledger's (already authoritative, per localscheduler.ClaimLedger's doc)
	// decision, for human visibility on the provider. A failure here does not
	// undo the ledger claim — the ledger, not this marker, is the source of truth.
	for i := range claimed {
		if _, err := issueProvider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: backlogRepo, ID: claimed[i].ID, RunID: runID}); err != nil {
			pf(stderr, "warning: provider claim marker for %s failed (ledger claim still holds): %v\n", claimed[i].ID, err)
		}
	}

	if len(claimed) == 0 {
		pf(stdout, "selected %d read-only in-flight item(s) for re-sweep\n", len(readOnlyResweep))
	} else if len(readOnlyResweep) == 0 && len(claimed) == 1 {
		pf(stdout, "claimed %s: %s\n", claimed[0].ID, claimed[0].Title)
	} else if len(readOnlyResweep) == 0 {
		pf(stdout, "claimed %d items\n", len(claimed))
	} else {
		pf(stdout, "claimed %d item(s), selected %d read-only in-flight item(s)\n", len(claimed), len(readOnlyResweep))
	}
	claimResultCommitted = true
	return 0
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func writeBacklogReconciliationResult(reconciled int, stdout, stderr io.Writer) int {
	data, err := json.Marshal(map[string]int{"reconciled": reconciled})
	if err != nil {
		pf(stderr, "error: marshal backlog reconciliation: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "backlog-reconciliation.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "reconciled %d backlog item(s)\n", reconciled)
	return 0
}

func runReadOnlyBacklogQuery(
	ctx context.Context,
	provider providers.BacklogProvider,
	repo providers.RepositoryRef,
	trustLabel string,
	labelFilter *labelpredicate.Predicate,
	fieldFilter *fieldpredicate.Predicate,
	fieldOrder fieldpredicate.Order,
	selectionPriority []string,
	respectAssignee bool,
	assignedTo string,
	stdout, stderr io.Writer,
) int {
	labels := compactLabels(trustLabel)
	labels = append(labels, labelFilter.RequiredLabels()...)
	queryAssignee := ""
	if respectAssignee && assignedTo != "" {
		queryAssignee = assignedTo
	}
	items, _, err := listBacklogScanWindow(
		ctx,
		provider,
		repo,
		labels,
		queryAssignee,
		fieldFilter,
		backlogScanCeiling,
		backlogScanCursor{},
		false,
	)
	if err != nil {
		return failProviderStage(stderr, "list work items", err, "claimed-item.json")
	}

	eligible := items[:0]
	for _, item := range items {
		if trustLabel != "" && !item.HasLabel(trustLabel) {
			continue
		}
		if respectAssignee && item.Assignee != assignedTo {
			continue
		}
		matched, matchErr := labelFilter.Matches(item.Labels)
		if matchErr != nil {
			pf(stderr, "error: evaluate labelPredicate for item %s: %v\n", item.ID, matchErr)
			return 1
		}
		if !matched {
			continue
		}
		matched, matchErr = fieldFilter.Matches(item.Fields)
		if matchErr != nil {
			pf(stderr, "error: evaluate fieldPredicate for item %s: %v\n", item.ID, matchErr)
			return 1
		}
		if !matched || (item.State != "" && !strings.EqualFold(item.State, "open")) {
			continue
		}
		eligible = append(eligible, item)
	}
	if err := sortEligibleByFields(eligible, selectionPriority, fieldOrder); err != nil {
		pf(stderr, "error: apply fieldOrder: %v\n", err)
		return 1
	}
	if len(eligible) == 0 {
		pln(stdout, "no eligible items")
		return 0
	}
	for _, item := range eligible {
		pf(stdout, "%s\t%s\n", item.ID, item.Title)
	}
	return 0
}

// backlogIssueProvider is the provider surface backlog-query's provider-neutral
// path needs: the eligibility scan (ListWorkItems/GetWorkItem), the best-effort
// claim/release markers, native-dependency and ready-age checks. Both
// *providers.GitHubProvider and *providers.ADOProvider satisfy it, so the claim
// loop runs against either backend once the provider is resolved from the routed
// repo. GitHub-only extras (curation, the open-PR backstop) keep the concrete
// *providers.GitHubProvider and are skipped for ADO.
//
// It embeds the full providers.Provider (not just BacklogProvider) so
// filterDeclaredDependencyEligibility can wrap it in a providers.Dispatcher
// (CONF-5, #2078): the native-dependency check goes through
// backlog.blockers instead of calling HasOpenWorkItemBlocker directly, so a
// provider that doesn't declare the capability fails closed with
// ErrUnsupported instead of risking a silent fail-open answer (#2059).
type backlogIssueProvider interface {
	providers.Provider
	ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error)
	ListWorkItemLabelTransitionsForItem(context.Context, providers.RepositoryRef, string, string) ([]providers.WorkItemLabelTransition, error)
}

func filterDeclaredDependencyEligibility(ctx context.Context, provider backlogIssueProvider, repo providers.RepositoryRef, eligible []providers.WorkItem) ([]providers.WorkItem, []string) {
	dispatcher := providers.NewDispatcher(provider)
	filtered := eligible[:0]
	var warnings []string
	for _, item := range eligible {
		if item.BlockedByCount == 0 {
			filtered = append(filtered, item)
			continue
		}
		blocked, err := dispatcher.HasOpenWorkItemBlocker(ctx, repo, item.ID)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("check item %s: %v", item.ID, err))
			continue
		}
		if !blocked {
			filtered = append(filtered, item)
		}
	}
	return filtered, warnings
}

// openPRIssueNumbers returns the set of issue numbers already referenced by
// an open goober-authored PR's closing keywords (Fixes/Closes/Resolves #N —
// the same convention `goobers open-pr` writes and `goobers post-merge`
// already parses at merge time via closingIssueNumbers, postmerge.go) — the
// open-PR eligibility backstop (#414 design point 2). One ListPullRequests
// call, not one per candidate: GitHub's list-pulls response already carries
// each PR's body (PullRequestSummary.Body), so no second round-trip per PR
// is needed either.
func openPRIssueNumbers(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef) (map[string]bool, error) {
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: repo, HeadPrefix: providerBranchNamespace()})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(prs))
	for _, pr := range prs {
		// referencedIssueNumbers, not closingIssueNumbers (#980): a PR that
		// only says "Implements #N" — a structured body whose "Fixes #N"
		// footer was overridden or absent — still speaks for that issue and
		// must exclude it from re-selection, not just one with a closing
		// keyword.
		for _, id := range referencedIssueNumbers(pr.Body) {
			out[id] = true
		}
	}
	return out, nil
}

// reconcileClosedUnmergedInReview restores backlog eligibility for issues whose
// linked implementation PRs all closed without merging. The bot-authored issue
// breadcrumb is durable association evidence even if mutable PR metadata changes.
func reconcileClosedUnmergedInReview(
	ctx context.Context,
	issueProvider *providers.GitHubProvider,
	prProvider *providers.GitHubProvider,
	repo providers.RepositoryRef,
) error {
	items, err := issueProvider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repo,
		Labels:     []string{inReviewStatusLabel},
		State:      "open",
	})
	if err != nil {
		return fmt.Errorf("list in-review work items: %w", err)
	}
	if len(items) == 0 {
		return nil
	}
	associationAuthor, err := issueProvider.AuthenticatedLogin(ctx)
	if err != nil {
		return fmt.Errorf("resolve issue association author: %w", err)
	}

	for _, item := range items {
		if !item.HasLabel(inReviewStatusLabel) ||
			(item.State != "" && !strings.EqualFold(item.State, "open")) {
			continue
		}

		comments, err := issueProvider.ListComments(ctx, repo, item.ID)
		if err != nil {
			return fmt.Errorf("list issue #%s comments: %w", item.ID, err)
		}
		pullIDs := linkedImplementationPullIDs(repo, associationAuthor, comments)
		if len(pullIDs) == 0 {
			continue
		}

		closedUnmerged := false
		protected := false
		for _, pullID := range pullIDs {
			pr, err := prProvider.GetPullRequest(ctx, repo, pullID)
			if err != nil {
				return fmt.Errorf("read linked pull request #%s for issue #%s: %w", pullID, item.ID, err)
			}
			if pr.Merged {
				protected = true
			} else if strings.EqualFold(pr.State, "closed") {
				closedUnmerged = true
			} else {
				protected = true
			}
		}
		if !closedUnmerged || protected {
			continue
		}
		if _, err := issueProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repo,
			ID:           item.ID,
			RemoveLabels: []string{inReviewStatusLabel},
		}); err != nil {
			return fmt.Errorf("requeue issue #%s: %w", item.ID, err)
		}
	}
	return nil
}

func linkedImplementationPullIDs(repo providers.RepositoryRef, author string, comments []providers.Comment) []string {
	seen := make(map[string]bool)
	var out []string
	for _, comment := range comments {
		if !strings.EqualFold(comment.Author, author) {
			continue
		}
		body := strings.TrimSpace(comment.Body)
		if !strings.HasPrefix(body, implementationInReviewCommentPrefix) ||
			!strings.HasSuffix(body, implementationInReviewCommentSuffix) {
			continue
		}
		rawURL := strings.TrimSuffix(
			strings.TrimPrefix(body, implementationInReviewCommentPrefix),
			implementationInReviewCommentSuffix,
		)
		u, err := url.ParseRequestURI(rawURL)
		if err != nil || u.Scheme == "" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			continue
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 4 {
			continue
		}
		parts = parts[len(parts)-4:]
		if !strings.EqualFold(parts[0], repo.Owner) ||
			!strings.EqualFold(parts[1], repo.Name) ||
			parts[2] != "pull" {
			continue
		}
		n, err := strconv.ParseUint(parts[3], 10, 64)
		if err != nil || n == 0 {
			continue
		}
		pullID := strconv.FormatUint(n, 10)
		if !seen[pullID] {
			seen[pullID] = true
			out = append(out, pullID)
		}
	}
	return out
}

// sortEligibleByFields applies opt-in label priority, then native-field
// ordering, then the starvation-safe numeric-ID FIFO baseline.
func sortEligibleByFields(items []providers.WorkItem, priorityLabels []string, fieldOrder fieldpredicate.Order) error {
	fieldSets := make([]fieldpredicate.Fields, len(items))
	for i := range items {
		fieldSets[i] = items[i].Fields
	}
	if err := fieldOrder.Validate(fieldSets); err != nil {
		return err
	}
	var compareErr error
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := itemPriorityRank(items[i], priorityLabels), itemPriorityRank(items[j], priorityLabels)
		if ri != rj {
			return ri < rj
		}
		comparison, err := fieldOrder.Compare(items[i].Fields, items[j].Fields)
		if err != nil {
			compareErr = err
			return false
		}
		if comparison != 0 {
			return comparison < 0
		}
		ni, iOK := parseWorkItemID(items[i].ID)
		nj, jOK := parseWorkItemID(items[j].ID)
		if iOK && jOK {
			return ni < nj
		}
		return items[i].ID < items[j].ID
	})
	return compareErr
}

// itemPriorityRank reports item's priority tier: the index of the first
// (highest-priority) entry in priorityLabels that item carries, or
// len(priorityLabels) — the lowest tier — if item carries none, including
// when priorityLabels itself is empty.
func itemPriorityRank(item providers.WorkItem, priorityLabels []string) int {
	for i, label := range priorityLabels {
		if item.HasLabel(label) {
			return i
		}
	}
	return len(priorityLabels)
}

func parseWorkItemID(id string) (int64, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	return n, err == nil
}

func backlogScanCursorPath(
	schedulerDir string,
	repo providers.RepositoryRef,
	trustLabel, labelExpression, fieldExpression string,
	requireLabels, excludeLabels []string,
	queryAssignee string,
) string {
	key, _ := json.Marshal(struct {
		Repository      providers.RepositoryRef `json:"repository"`
		TrustLabel      string                  `json:"trustLabel,omitempty"`
		Expression      string                  `json:"expression,omitempty"`
		FieldExpression string                  `json:"fieldExpression,omitempty"`
		RequireLabels   []string                `json:"requireLabels,omitempty"`
		ExcludeLabels   []string                `json:"excludeLabels,omitempty"`
		// Assignee (#1820): only ever non-empty when respectAssignee narrows
		// the provider query server-side (a real-identity mode); omitempty
		// keeps the cache key — and therefore the cursor file path — byte-
		// identical to before this field existed for any gaggle that hasn't
		// opted in, so an in-flight scan's cursor isn't invalidated by this
		// change. Distinct assignee values get distinct cursors so a
		// narrowed scan's pagination progress never cross-contaminates a
		// differently-scoped one over the same labels/predicates.
		Assignee string `json:"assignee,omitempty"`
	}{
		Repository:      repo,
		TrustLabel:      trustLabel,
		Expression:      labelExpression,
		FieldExpression: fieldExpression,
		RequireLabels:   requireLabels,
		ExcludeLabels:   excludeLabels,
		Assignee:        queryAssignee,
	})
	sum := sha256.Sum256(key)
	return filepath.Join(schedulerDir, fmt.Sprintf("backlog-scan-%x.json", sum))
}

func readBacklogScanCursor(lockPath, cursorPath string) (backlogScanCursor, error) {
	cursor := backlogScanCursor{}
	err := withClaimLock(lockPath, claimLockOperationBacklogScanCursor, func() error {
		var err error
		cursor, err = loadBacklogScanCursor(cursorPath)
		return err
	})
	return cursor, err
}

func loadBacklogScanCursor(path string) (backlogScanCursor, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return backlogScanCursor{}, nil
	}
	if err != nil {
		return backlogScanCursor{}, err
	}
	var cursor backlogScanCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return backlogScanCursor{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return cursor, nil
}

func advanceBacklogScanCursor(
	lockPath, cursorPath string,
	observed, next backlogScanCursor,
) error {
	return withClaimLock(lockPath, claimLockOperationBacklogScanCursor, func() error {
		current, err := loadBacklogScanCursor(cursorPath)
		if err != nil {
			return err
		}
		if current != observed {
			return nil
		}
		data, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("marshal backlog scan cursor: %w", err)
		}
		if err := journal.WriteFileAtomic(cursorPath, data, 0o644); err != nil {
			return fmt.Errorf("write backlog scan cursor: %w", err)
		}
		return nil
	})
}

func listBacklogScanWindow(
	ctx context.Context,
	provider providers.BacklogProvider,
	repo providers.RepositoryRef,
	labels []string,
	assignee string,
	fieldFilter *fieldpredicate.Predicate,
	limit int,
	cursor backlogScanCursor,
	exhaustive bool,
) ([]providers.WorkItem, backlogScanCursor, error) {
	if limit <= 0 && !exhaustive {
		return nil, cursor, nil
	}
	items := make([]providers.WorkItem, 0, limit)
	maxPages := (limit + backlogScanPageSize - 1) / backlogScanPageSize
	for page := 0; exhaustive || page < maxPages; page++ {
		pageLimit := backlogScanPageSize
		if !exhaustive {
			pageLimit = min(pageLimit, limit)
		}
		pageInfo := &providers.ListWorkItemsPageInfo{}
		pageItems, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository:     repo,
			Labels:         labels,
			Assignee:       assignee,
			FieldPredicate: fieldFilter,
			State:          "open",
			Limit:          pageLimit,
			Cursor:         cursor.Cursor,
			PageInfo:       pageInfo,
			OldestFirst:    true,
		})
		if err != nil {
			return nil, cursor, err
		}
		// CandidateCount may legitimately exceed pageLimit (#2067): a
		// provider now scans more raw candidates than pageLimit in one
		// call when a filter it can't fully push server-side (ADO's state
		// normalization, its Labels/hasAllLabels substring-match recheck)
		// could reject the candidate a naive pageLimit-sized fetch would
		// have truncated on — the exact under-matching bug #2067 fixed.
		// Only a negative count is still a real provider bug.
		if pageInfo.CandidateCount < 0 {
			return nil, cursor, fmt.Errorf(
				"provider returned invalid work-item candidate count %d",
				pageInfo.CandidateCount,
			)
		}
		items = append(items, pageItems...)
		if !pageInfo.HasNext {
			return items, backlogScanCursor{}, nil
		}
		if pageInfo.CandidateCount == 0 || pageInfo.NextCursor == "" {
			return nil, cursor, fmt.Errorf("provider returned a non-advancing work-item cursor")
		}
		cursor.Cursor = pageInfo.NextCursor
		if !exhaustive {
			// limit is a raw-candidate scan budget, not a match target
			// (scanLimit is floored to backlogScanCeiling by the caller
			// specifically so a rejecting filter still gets a full window
			// examined). Decrementing by the actual CandidateCount stays
			// correct now that it can exceed pageLimit (#2067): the same
			// total-candidate budget is still consumed, just via fewer,
			// larger provider calls instead of many pageLimit-sized ones —
			// not an under-scan, just fewer round trips to reach it.
			limit -= pageInfo.CandidateCount
			if limit <= 0 {
				break
			}
		}
	}
	return items, cursor, nil
}

// runBacklogQueryRelease implements `backlog-query --release` (issues
// #234/#1003): an explicit, deterministic-path release of every claim this run
// holds for a workflow whose consuming stage neither opens a PR nor closes the
// issue. Curation still needs the early ledger release added by #234, but it
// must also remove the provider-visible marker that --claim added; otherwise
// the authoritative ledger becomes free while goobers:claimed leaks forever.
func runBacklogQueryRelease(root string, stdout, stderr io.Writer) int {
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	l := layoutFor(root)
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	var released []string
	var providerErr error
	err = withClaimLock(lockPath, claimLockOperationBacklogRelease, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		entries := ledger.ForRunAll(runID)
		if len(entries) == 0 {
			return nil
		}

		repo, rerr := providerRepo(root)
		if rerr != nil {
			return rerr
		}
		var issueProvider backlogIssueProvider
		switch repo.Provider {
		case providers.ProviderADO:
			adoProvider, aerr := newADOProviderForStage(root, repo)
			if aerr != nil {
				return aerr
			}
			issueProvider = adoProvider
		case providers.ProviderGitea:
			token, terr := providerToken(capability.GitHubIssuesWrite)
			if terr != nil {
				return terr
			}
			giteaProvider, gerr := newGiteaProviderForStage(root, repo, token, providers.WithGiteaMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
			if gerr != nil {
				return gerr
			}
			issueProvider = giteaProvider
		case providers.ProviderGitHub:
			token, terr := providerToken(capability.GitHubIssuesWrite)
			if terr != nil {
				return terr
			}
			issueProvider = newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))
		default:
			return fmt.Errorf("backlog-query release does not support repository provider %q", repo.Provider)
		}
		// Work-item claim markers live in the backlog project on ADO, not the
		// routed code repo (see backlogRepoRefForStage).
		backlogRepo := backlogRepoRefForStage(root, repo)
		ctx, cancel := providerCommandContext()
		defer cancel()

		for _, entry := range entries {
			// End the provider claim epoch while the claim lock still prevents a
			// new local owner from claiming this item. Keep the ledger entry if
			// provider cleanup fails so a retry retains the item ID.
			if _, rerr := issueProvider.ReleaseWorkItemClaim(ctx, providers.ClaimWorkItemRequest{
				Repository:       backlogRepo,
				ID:               entry.ItemID,
				RunID:            runID,
				LedgerAuthorized: true,
			}); rerr != nil {
				providerErr = fmt.Errorf("release provider claim marker for %s: %w", entry.ItemID, rerr)
				return providerErr
			}
			if rerr := ledger.ReleaseEntry(entry, runID); rerr != nil {
				return fmt.Errorf("release %s in ledger: %w", entry.ItemID, rerr)
			}
			released = append(released, entry.ItemID)
		}
		return nil
	})
	if err != nil {
		if providerErr != nil {
			return failProviderStage(stderr, "release backlog claims", providerErr, "")
		}
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if len(released) == 0 {
		pln(stdout, "nothing to release: run holds no claim")
		return 0
	}
	pf(stdout, "released %s\n", strings.Join(released, ", "))
	return 0
}

// writeNoWorkResult is `--claim`'s clean, structured "nothing to do this
// tick" outcome (issue #233): an empty eligible set, or every eligible item
// already claimed by another run, is a routine steady state — the same
// backlog-curation.yaml doc comment's own "re-running is a no-op" contract
// — not an error. Exit 0, with the declared result file (the same
// resultFile convention the successful-claim path uses) carrying
// executor.OutputNoWork=true so internal/executor/shell.go reports
// apiv1.ResultNoWork instead of ResultSuccess, and the runner short-circuits
// to a clean PhaseCompleted without ever invoking a downstream agentic
// stage with no subject (internal/runner's taskOutcome). A genuine
// provider/credential/list error is NOT routed through here — those return
// 1 from their own call sites above, unchanged, so a real outage still
// fails the run loudly (the acceptance criteria's negative control).
func writeNoWorkResult(stdout, stderr io.Writer, reason string) int {
	resultFile := providerInput("resultFile", "claimed-item.json")
	data, err := json.Marshal(map[string]interface{}{"claimed": false, executor.OutputNoWork: true})
	if err != nil {
		pf(stderr, "error: marshal no-work result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "no work: %s\n", reason)
	return 0
}
