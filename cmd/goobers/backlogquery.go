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
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
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

func runBacklogQuery(args []string, stdout, stderr io.Writer) int {
	return runBacklogQueryWithClaimBarrier(args, stdout, stderr, nil)
}

const backlogQueryHelp = "Usage: goobers backlog-query [--debug] [--read-only | --claim | --reconcile | --release] [path]\n\n" +
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
	"--debug writes candidate eligibility, exclusion, and claim-loss details to\n" +
	"stderr. Diagnostics contain item IDs and selection metadata only; normal\n" +
	"output and claim behavior are unchanged.\n\n" +
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
	fs := newCLIFlagSet("backlog-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	readOnly := fs.Bool("read-only", false, "list backlog items without mutating provider or scheduler state")
	claim := fs.Bool("claim", false, "claim the first eligible item (mirrors the claim in the local ledger + provider)")
	reconcile := fs.Bool("reconcile", false, "repair drifted backlog metadata and report the correction count")
	release := fs.Bool("release", false, "remove provider claim markers and release this run's claim ledger leases early (issues #234/#1003)")
	debug := fs.Bool("debug", false, "explain candidate eligibility, exclusions, and lost claim attempts on stderr")
	fs.Usage = helpUsage(stderr, "backlog-query")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	mode, ok := selectBacklogQueryMode(*readOnly, *claim, *reconcile, *release)
	if !ok {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	env := backlogQueryEnv{
		root:   root,
		layout: layoutFor(root),
		stdout: stdout,
		stderr: stderr,
		debug:  *debug,
	}
	if mode == backlogQueryModeRelease {
		return runBacklogQueryRelease(env)
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	env.repo = repo
	if code := env.openProvider(mode == backlogQueryModeReadOnly); code != 0 {
		return code
	}
	env.backlogRepo = backlogRepoRefForStage(root, repo)
	return runBacklogQueryMode(mode, env, beforeClaimTransaction)
}

type backlogQueryMode uint8

const (
	backlogQueryModePlain backlogQueryMode = iota
	backlogQueryModeReadOnly
	backlogQueryModeClaim
	backlogQueryModeReconcile
	backlogQueryModeRelease
)

func selectBacklogQueryMode(readOnly, claim, reconcile, release bool) (backlogQueryMode, bool) {
	mode := backlogQueryModePlain
	for _, candidate := range []struct {
		mode    backlogQueryMode
		enabled bool
	}{
		{backlogQueryModeReadOnly, readOnly},
		{backlogQueryModeClaim, claim},
		{backlogQueryModeReconcile, reconcile},
		{backlogQueryModeRelease, release},
	} {
		if !candidate.enabled {
			continue
		}
		if mode != backlogQueryModePlain {
			return 0, false
		}
		mode = candidate.mode
	}
	return mode, true
}

type backlogQueryEnv struct {
	root            string
	layout          instance.Layout
	repo            providers.RepositoryRef
	backlogRepo     providers.RepositoryRef
	issueProvider   backlogIssueProvider
	ghIssueProvider *providers.GitHubProvider
	stdout          io.Writer
	stderr          io.Writer
	debug           bool
}

func (env backlogQueryEnv) debugf(format string, args ...interface{}) {
	if env.debug {
		pf(env.stderr, "debug: "+format+"\n", args...)
	}
}

func (env *backlogQueryEnv) openProvider(readOnly bool) int {
	// Provider-neutral backlog access (#772 ADO parity): the eligibility scan,
	// claim marking, and list output go through backlogIssueProvider, which both
	// the GitHub and ADO providers satisfy. GitHub-only extras (curation/reconcile
	// metadata, the open-PR eligibility backstop, contested-file dispatch) need
	// the concrete provider and stay gated on ghIssueProvider being non-nil — for
	// ADO they are simply skipped, exactly like a GitHub stage that never opted
	// into github:pr:write.
	opts := []stageProviderOption{withStageProviderMutations("issue")}
	if !readOnly {
		opts = append(opts, withStageProviderCache())
	}
	provider, err := newProviderForStage(env.root, env.repo, readOnly, opts...)
	if err != nil {
		pf(env.stderr, "error: %v\n", err)
		return 1
	}
	issueProvider, ok := provider.(backlogIssueProvider)
	if !ok {
		pf(env.stderr, "error: backlog-query does not support repository provider %q\n", env.repo.Provider)
		return 1
	}
	env.issueProvider = issueProvider
	env.ghIssueProvider, _ = provider.(*providers.GitHubProvider)
	return 0
}

func runBacklogQueryMode(mode backlogQueryMode, env backlogQueryEnv, beforeClaimTransaction func()) int {
	root, repo := env.root, env.repo
	ghIssueProvider, stderr := env.ghIssueProvider, env.stderr
	claim := mode == backlogQueryModeClaim
	reconcile := mode == backlogQueryModeReconcile
	readOnly := mode == backlogQueryModeReadOnly
	// ADO splits the code repository (where branches/PRs land) from the backlog
	// project (where PBIs live), so work-item provider calls — list, dependency/
	// blocked checks, and claim — must address the backlog project rather than
	// the routed code repo. On GitHub the two coincide and backlogRepo == repo.
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
	curationRun := claim && providerInput("curation", "false") == "true"
	reconcileBeforeClaim := curationRun && providerInput("reconcileMetadata", "true") != "false"
	var stalenessPolicy backlogStalenessPolicy
	if reconcileBeforeClaim || reconcile {
		stalenessPolicy, err = readBacklogStalenessPolicy()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
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
	if (claim || reconcile) && trustLabel == "" {
		pln(stderr, "error: trustLabel is required to claim or reconcile (SEC-047: backlog content is untrusted input on a public repo) — declare inputs.trustLabel")
		return 1
	}
	var runID, workflow string
	if claim {
		runID, workflow, err = providerRunContext()
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	if readOnly {
		return runReadOnlyBacklogQuery(ctx, env, backlogScanOptions{
			trustLabel:        trustLabel,
			labelFilter:       labelFilter,
			fieldFilter:       fieldFilter,
			fieldOrder:        fieldOrder,
			selectionPriority: selectionPriority,
			respectAssignee:   respectAssignee,
			assignedTo:        assignedTo,
		})
	}
	observedAt := time.Now().UTC()

	if reconcile {
		return runReconcileBacklogQuery(ctx, env, trustLabel, stalenessPolicy, observedAt)
	}
	if curationRun {
		if code := reconcileBacklogQueryMetadata(ctx, env, trustLabel, stalenessPolicy, observedAt, "claimed-items.json"); code != 0 {
			return code
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
		if claim {
			if err := reconcileClosedUnmergedInReview(ctx, ghIssueProvider, prProvider, repo); err != nil {
				return failProviderStage(stderr, "reconcile closed pull requests", err, "claimed-item.json")
			}
		}
	}

	scan, code := scanBacklogEligibility(ctx, env, backlogScanOptions{
		trustLabel:        trustLabel,
		requireLabels:     requireLabels,
		excludeLabels:     excludeLabels,
		labelExpression:   labelExpression,
		fieldExpression:   fieldExpression,
		labelFilter:       labelFilter,
		fieldFilter:       fieldFilter,
		fieldOrder:        fieldOrder,
		selectionPriority: selectionPriority,
		respectAssignee:   respectAssignee,
		assignedTo:        assignedTo,
		scanLimit:         scanLimit,
		openIssues:        openIssues,
	})
	if code != 0 {
		return code
	}
	eligible := scan.eligible
	lockPath, cursorPath := scan.lockPath, scan.cursorPath
	scanCursor, nextScanCursor := scan.cursor, scan.nextCursor
	observedRecords, remainingRecords := scan.observedRecords, scan.remainingRecords
	verifiedSkips, observedSkips := scan.verifiedSkips, scan.observedSkips
	forwardEligibleCount := len(eligible)

	resweep, code := runBacklogResweep(ctx, env, backlogResweepOptions{
		enabled:           resweepEnabled,
		policy:            resweepPolicy,
		eligible:          eligible,
		maxItems:          maxItems,
		trustLabel:        trustLabel,
		fieldFilter:       fieldFilter,
		fieldOrder:        fieldOrder,
		selectionPriority: selectionPriority,
		observedAt:        observedAt,
		openIssues:        openIssues,
		lockPath:          lockPath,
	})
	if code != 0 {
		return code
	}
	eligible = resweep.eligible
	readOnlyResweep := resweep.readOnly
	curationModeByID := resweep.modeByID
	persistResweepState := resweep.persist

	if !claim {
		reportBacklogEligibility(env, scan.eligible, nil, verifiedSkips)
		return runPlainBacklogQuery(env, scan)
	}
	reportBacklogEligibility(env, eligible, readOnlyResweep, verifiedSkips)
	return runClaimBacklogQuery(ctx, env, backlogClaimOptions{
		eligible:               eligible,
		readOnlyResweep:        readOnlyResweep,
		forwardEligibleCount:   forwardEligibleCount,
		prProvider:             prProvider,
		maxItems:               maxItems,
		persistResweepState:    persistResweepState,
		lockPath:               lockPath,
		cursorPath:             cursorPath,
		scanCursor:             scanCursor,
		nextScanCursor:         nextScanCursor,
		observedRecords:        observedRecords,
		remainingRecords:       remainingRecords,
		verifiedSkips:          verifiedSkips,
		observedSkips:          observedSkips,
		runID:                  runID,
		workflow:               workflow,
		labelFilter:            labelFilter,
		curationRun:            curationRun,
		stalenessPolicy:        stalenessPolicy,
		observedAt:             observedAt,
		curationModeByID:       curationModeByID,
		beforeClaimTransaction: beforeClaimTransaction,
	})
}

type backlogClaimOptions struct {
	eligible               []providers.WorkItem
	readOnlyResweep        []providers.WorkItem
	forwardEligibleCount   int
	prProvider             *providers.GitHubProvider
	maxItems               int
	persistResweepState    func() error
	lockPath               string
	cursorPath             string
	scanCursor             backlogScanCursor
	nextScanCursor         backlogScanCursor
	observedRecords        map[string]blockedRecord
	remainingRecords       map[string]blockedRecord
	verifiedSkips          map[string]blockedEligibilitySkip
	observedSkips          []blockedEligibilitySkip
	runID                  string
	workflow               string
	labelFilter            *labelpredicate.Predicate
	curationRun            bool
	stalenessPolicy        backlogStalenessPolicy
	observedAt             time.Time
	curationModeByID       map[string]string
	beforeClaimTransaction func()
}

func runClaimBacklogQuery(ctx context.Context, env backlogQueryEnv, opts backlogClaimOptions) int {
	l, backlogRepo := env.layout, env.backlogRepo
	stdout, stderr := env.stdout, env.stderr
	eligible, readOnlyResweep := opts.eligible, opts.readOnlyResweep
	lockPath, cursorPath := opts.lockPath, opts.cursorPath
	scanCursor, nextScanCursor := opts.scanCursor, opts.nextScanCursor
	observedRecords, remainingRecords := opts.observedRecords, opts.remainingRecords
	verifiedSkips, observedSkips := opts.verifiedSkips, opts.observedSkips
	maxItems, runID, workflow := opts.maxItems, opts.runID, opts.workflow
	labelFilter, curationRun := opts.labelFilter, opts.curationRun
	stalenessPolicy, observedAt := opts.stalenessPolicy, opts.observedAt
	curationModeByID := opts.curationModeByID
	persistResweepState := opts.persistResweepState
	eligible = reorderContestedBacklogItems(ctx, env, opts.prProvider, eligible, opts.forwardEligibleCount)

	if len(eligible) == 0 && len(readOnlyResweep) == 0 {
		err := withClaimLock(lockPath, claimLockOperationBacklogFilterBlocked, func() error {
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

	session := backlogClaimSession{
		env:              env,
		instanceLog:      instanceLog,
		eligible:         eligible,
		observedRecords:  observedRecords,
		remainingRecords: remainingRecords,
		verifiedSkips:    verifiedSkips,
		observedSkips:    observedSkips,
		lockPath:         lockPath,
		runID:            runID,
		workflow:         workflow,
		leaseDuration:    leaseDuration,
		maxItems:         maxItems,
		gaggle:           providerGaggle(),
	}
	claimResultCommitted := false
	if opts.beforeClaimTransaction != nil {
		opts.beforeClaimTransaction()
	}
	defer func() {
		if claimResultCommitted {
			return
		}
		for _, item := range session.newlyClaimed {
			if releaseErr := session.rollback(ctx, item); releaseErr != nil {
				pf(stderr, "error: roll back claim %s after backlog query failure: %v\n", item.ID, releaseErr)
			}
		}
	}()
	malformedReadyItems, code := session.collect(ctx, labelFilter)
	if code != 0 {
		return code
	}
	eligible, observedSkips, claimed := session.eligible, session.observedSkips, session.claimed
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

	if code := writeClaimedBacklogResult(ctx, env, claimed, readOnlyResweep, claimedBacklogResultOptions{
		maxItems:            maxItems,
		curationRun:         curationRun,
		stalenessPolicy:     stalenessPolicy,
		observedAt:          observedAt,
		curationModeByID:    curationModeByID,
		persistResweepState: persistResweepState,
	}); code != 0 {
		return code
	}
	claimResultCommitted = true
	return 0
}

type claimedBacklogResultOptions struct {
	maxItems            int
	curationRun         bool
	stalenessPolicy     backlogStalenessPolicy
	observedAt          time.Time
	curationModeByID    map[string]string
	persistResweepState func() error
}

func writeClaimedBacklogResult(
	ctx context.Context,
	env backlogQueryEnv,
	claimed, readOnly []providers.WorkItem,
	opts claimedBacklogResultOptions,
) int {
	var curationItems []curationClaimedItem
	var err error
	if opts.curationRun {
		curationItems, err = enrichClaimedItemsWithStaleness(
			ctx, env.ghIssueProvider, env.repo, claimed, opts.observedAt, opts.stalenessPolicy,
		)
		if err != nil {
			return failProviderStage(env.stderr, "compute claimed-item staleness", err, "claimed-items.json")
		}
		for index := range curationItems {
			curationItems[index].CurationMode = opts.curationModeByID[curationItems[index].ID]
		}
		readOnlyItems, enrichErr := enrichClaimedItemsWithStaleness(
			ctx, env.ghIssueProvider, env.repo, readOnly, opts.observedAt, opts.stalenessPolicy,
		)
		if enrichErr != nil {
			return failProviderStage(env.stderr, "compute read-only re-sweep staleness", enrichErr, "claimed-items.json")
		}
		for index := range readOnlyItems {
			readOnlyItems[index].CurationMode = "read-only"
			readOnlyItems[index].ReadOnly = true
		}
		curationItems = append(curationItems, readOnlyItems...)
	}
	data, err := marshalClaimedBacklogItems(claimed, curationItems, opts.curationRun, opts.maxItems)
	if err != nil {
		pf(env.stderr, "error: marshal claimed item(s): %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "claimed-item.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(env.stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	if err := opts.persistResweepState(); err != nil {
		pf(env.stderr, "error: %v\n", err)
		return 1
	}
	writeClaimedBacklogSummary(env.stdout, claimed, readOnly)
	return 0
}

func marshalClaimedBacklogItems(
	claimed []providers.WorkItem,
	curationItems []curationClaimedItem,
	curationRun bool,
	maxItems int,
) ([]byte, error) {
	switch {
	case curationRun && maxItems == 1:
		return json.Marshal(curationItems[0])
	case curationRun:
		return json.Marshal(curationItems)
	case maxItems == 1:
		return json.Marshal(claimed[0])
	default:
		return json.Marshal(claimed)
	}
}

func writeClaimedBacklogSummary(stdout io.Writer, claimed, readOnly []providers.WorkItem) {
	switch {
	case len(claimed) == 0:
		pf(stdout, "selected %d read-only in-flight item(s) for re-sweep\n", len(readOnly))
	case len(readOnly) == 0 && len(claimed) == 1:
		pf(stdout, "claimed %s: %s\n", claimed[0].ID, claimed[0].Title)
	case len(readOnly) == 0:
		pf(stdout, "claimed %d items\n", len(claimed))
	default:
		pf(stdout, "claimed %d item(s), selected %d read-only in-flight item(s)\n", len(claimed), len(readOnly))
	}
}

func reorderContestedBacklogItems(
	ctx context.Context,
	env backlogQueryEnv,
	prProvider *providers.GitHubProvider,
	eligible []providers.WorkItem,
	forwardCount int,
) []providers.WorkItem {
	if len(eligible) <= 1 || providerInput("deprioritizeContestedFiles", "true") != "true" || prProvider == nil {
		return eligible
	}
	minPRs := 2
	if value := providerInput("contestedFileMinPRs", ""); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 1 {
			minPRs = parsed
		} else {
			pf(env.stderr, "warning: invalid contestedFileMinPRs %q; using %d\n", value, minPRs)
		}
	}
	touches, err := openPRTouches(ctx, prProvider, env.repo, "")
	if err != nil {
		pf(env.stderr, "warning: contested-file dispatch awareness unavailable (%v); using FIFO order\n", err)
		return eligible
	}
	forward := eligible[:forwardCount]
	resweep := eligible[forwardCount:]
	reordered, deprioritized := partitionByContention(forward, touches, minPRs)
	if count := len(deprioritized); count > 0 && count < len(reordered) {
		pf(env.stderr, "contested-file dispatch: deprioritized %d contested issue(s) [%s] behind %d disjoint one(s)\n",
			count, strings.Join(deprioritized, ","), len(reordered)-count)
	}
	return append(reordered, resweep...)
}

type backlogClaimSession struct {
	env                 backlogQueryEnv
	instanceLog         *journal.InstanceLog
	eligible            []providers.WorkItem
	observedRecords     map[string]blockedRecord
	remainingRecords    map[string]blockedRecord
	verifiedSkips       map[string]blockedEligibilitySkip
	observedSkips       []blockedEligibilitySkip
	claimed             []providers.WorkItem
	newlyClaimed        []providers.WorkItem
	preexistingClaimIDs map[string]struct{}
	lockPath            string
	runID               string
	workflow            string
	gaggle              string
	leaseDuration       time.Duration
	maxItems            int
	nextClaimIndex      int
	claimSetPrepared    bool
}

func (session *backlogClaimSession) collect(ctx context.Context, labelFilter *labelpredicate.Predicate) (int, int) {
	malformedReadyItems := 0
	for !session.claimSetPrepared || (len(session.claimed) < session.maxItems && session.nextClaimIndex < len(session.eligible)) {
		firstNewClaim := len(session.claimed)
		if err := session.acquire(); err != nil {
			pf(session.env.stderr, "error: %v\n", err)
			return malformedReadyItems, 1
		}
		if labelFilter.ReferencesLabel(providers.LabelReady) {
			for index := firstNewClaim; index < len(session.claimed); {
				if !session.claimed[index].HasLabel(providers.LabelReady) {
					index++
					continue
				}
				transitions, err := session.env.issueProvider.ListWorkItemLabelTransitionsForItem(
					ctx, session.env.backlogRepo, session.claimed[index].ID, providers.LabelReady,
				)
				if err != nil {
					return malformedReadyItems, failProviderStage(session.env.stderr, "read ready-label transitions", err, "claimed-item.json")
				}
				if err := annotateReadyTimes(session.claimed[index:index+1], providers.LabelReady, transitions); err != nil {
					malformed := session.claimed[index]
					if releaseErr := session.releaseLedger(malformed); releaseErr != nil {
						pf(session.env.stderr, "error: release malformed eligible item %s: %v\n", malformed.ID, releaseErr)
						return malformedReadyItems, 1
					}
					session.forgetNewClaim(malformed.ID)
					pf(session.env.stderr, "warning: skipping malformed eligible item %s: measure ready age: %v\n", malformed.ID, err)
					session.claimed = append(session.claimed[:index], session.claimed[index+1:]...)
					malformedReadyItems++
					continue
				}
				index++
			}
		}
		if err := session.confirmProviderClaims(ctx, firstNewClaim); err != nil {
			pf(session.env.stderr, "error: confirm provider claim: %v\n", err)
			return malformedReadyItems, 1
		}
	}
	return malformedReadyItems, 0
}

func (session *backlogClaimSession) acquire() error {
	return withClaimLock(session.lockPath, claimLockOperationBacklogClaim, session.acquireLocked)
}

func (session *backlogClaimSession) acquireLocked() error {
	if !session.claimSetPrepared {
		var err error
		session.eligible, session.observedSkips, err = reconcileBlockedEligibilityLocked(
			blockedRecordsPath(session.env.layout),
			session.env.backlogRepo,
			session.eligible,
			session.observedRecords,
			session.remainingRecords,
			session.verifiedSkips,
		)
		if err != nil {
			return err
		}
		if err := session.journalBlockedSkips(); err != nil {
			return err
		}
		session.claimSetPrepared = true
	}
	ledger, err := openBacklogClaimLedger(
		filepath.Join(session.env.layout.SchedulerDir(), claimLedgerFileName),
		localscheduler.WithInstanceLog(session.instanceLog),
	)
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	session.rememberPreexistingClaims(ledger)
	for session.nextClaimIndex < len(session.eligible) && len(session.claimed) < session.maxItems {
		item := session.eligible[session.nextClaimIndex]
		session.nextClaimIndex++
		ok, err := session.claimItem(ledger, item)
		if err != nil {
			return err
		}
		if ok {
			session.claimed = append(session.claimed, item)
			if _, preexisting := session.preexistingClaimIDs[item.ID]; !preexisting {
				session.newlyClaimed = append(session.newlyClaimed, item)
			}
		} else {
			session.env.debugf("claim lost %s: ledger claim held by another run", item.ID)
		}
	}
	return nil
}

func (session *backlogClaimSession) journalBlockedSkips() error {
	for _, skip := range session.observedSkips {
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
		if err := session.instanceLog.Append(journal.Event{
			Type:     journal.EventRunnerAnnotation,
			Workflow: session.workflow,
			RunID:    session.runID,
			Reason:   skip.reason(),
			Runner:   runner,
		}); err != nil {
			return fmt.Errorf("journal blocked eligibility skip for %s: %w", skip.ItemID, err)
		}
	}
	return nil
}

func (session *backlogClaimSession) rememberPreexistingClaims(ledger backlogClaimLedger) {
	if session.preexistingClaimIDs != nil {
		return
	}
	session.preexistingClaimIDs = make(map[string]struct{})
	for _, entry := range ledger.ForRunAll(session.runID) {
		if session.gaggle == "" {
			if entry.Gaggle == "" && entry.Provider == "" {
				session.preexistingClaimIDs[entry.ItemID] = struct{}{}
			}
			continue
		}
		if entry.Gaggle == session.gaggle && entry.Provider == string(session.env.repo.Provider) {
			session.preexistingClaimIDs[entry.ExternalID] = struct{}{}
		}
	}
}

func (session *backlogClaimSession) claimItem(ledger backlogClaimLedger, item providers.WorkItem) (bool, error) {
	var ok bool
	var err error
	if session.gaggle == "" {
		ok, _, err = ledger.Claim(item.ID, session.runID, session.workflow, session.leaseDuration)
	} else {
		ok, _, err = ledger.ClaimScoped(localscheduler.ClaimKey{
			Gaggle:     session.gaggle,
			Provider:   string(session.env.repo.Provider),
			ExternalID: item.ID,
		}, session.runID, session.workflow, session.leaseDuration)
	}
	if err != nil {
		return false, fmt.Errorf("claim %s in ledger: %w", item.ID, err)
	}
	return ok, nil
}

func (session *backlogClaimSession) confirmProviderClaims(ctx context.Context, start int) error {
	for index := start; index < len(session.claimed); {
		item := session.claimed[index]
		result, err := session.env.issueProvider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{
			Repository: session.env.backlogRepo,
			ID:         item.ID,
			RunID:      session.runID,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", item.ID, err)
		}
		if result.Claimed {
			index++
			continue
		}

		if err := session.releaseLedger(item); err != nil {
			return fmt.Errorf("release losing ledger claim %s: %w", item.ID, err)
		}
		session.forgetNewClaim(item.ID)
		session.claimed = append(session.claimed[:index], session.claimed[index+1:]...)
		pf(session.env.stderr, "warning: claim race lost for item %s to run %s; released local claim and stopped this run from processing it\n", item.ID, result.ClaimedBy)
	}
	return nil
}

func (session *backlogClaimSession) forgetNewClaim(itemID string) {
	for index := range session.newlyClaimed {
		if session.newlyClaimed[index].ID != itemID {
			continue
		}
		session.newlyClaimed = append(session.newlyClaimed[:index], session.newlyClaimed[index+1:]...)
		return
	}
}

func (session *backlogClaimSession) rollback(ctx context.Context, item providers.WorkItem) error {
	_, providerErr := session.env.issueProvider.ReleaseWorkItemClaim(ctx, providers.ClaimWorkItemRequest{
		Repository: session.env.backlogRepo,
		ID:         item.ID,
		RunID:      session.runID,
	})
	ledgerErr := session.releaseLedger(item)
	return errors.Join(providerErr, ledgerErr)
}

func (session *backlogClaimSession) releaseLedger(item providers.WorkItem) error {
	return withClaimLock(session.lockPath, claimLockOperationBacklogRelease, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(session.env.layout.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(session.instanceLog),
		)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		if session.gaggle == "" {
			return ledger.Release(item.ID, session.runID)
		}
		return ledger.ReleaseScoped(localscheduler.ClaimKey{
			Gaggle:     session.gaggle,
			Provider:   string(session.env.repo.Provider),
			ExternalID: item.ID,
		}, session.runID)
	})
}

type backlogResweepOptions struct {
	enabled           bool
	policy            backlogResweepPolicy
	eligible          []providers.WorkItem
	maxItems          int
	trustLabel        string
	fieldFilter       *fieldpredicate.Predicate
	fieldOrder        fieldpredicate.Order
	selectionPriority []string
	observedAt        time.Time
	openIssues        map[string]bool
	lockPath          string
}

type backlogResweepResult struct {
	eligible  []providers.WorkItem
	readOnly  []providers.WorkItem
	modeByID  map[string]string
	state     backlogResweepState
	lockPath  string
	statePath string
	observed  uint64
	dirty     bool
}

func (result backlogResweepResult) persist() error {
	if !result.dirty {
		return nil
	}
	return advanceBacklogResweepState(
		result.lockPath,
		result.statePath,
		result.observed,
		result.state,
	)
}

func runBacklogResweep(ctx context.Context, env backlogQueryEnv, opts backlogResweepOptions) (backlogResweepResult, int) {
	result := backlogResweepResult{eligible: opts.eligible, lockPath: opts.lockPath}
	if !opts.enabled || len(result.eligible) >= opts.maxItems {
		return result, 0
	}
	result.statePath = backlogResweepStatePath(
		env.layout.SchedulerDir(), env.repo, providerGaggle(), opts.trustLabel, opts.policy.readyLabel,
	)
	var err error
	result.state, err = readBacklogResweepState(opts.lockPath, result.statePath)
	if err != nil {
		pf(env.stderr, "error: read backlog re-sweep state: %v\n", err)
		return result, 1
	}
	result.observed = result.state.Generation
	if !backlogResweepDue(result.state, opts.observedAt, opts.policy.interval) {
		return result, 0
	}
	result.modeByID = make(map[string]string)
	selected, code := appendBlockedResweepCandidates(ctx, env, opts, &result)
	if code != 0 {
		return result, code
	}
	ready, nextCursor, code := appendReadyResweepCandidates(ctx, env, opts, &result)
	if code != 0 {
		return result, code
	}
	selected = append(selected, ready...)
	result.state = recordBacklogResweep(result.state, selected, opts.observedAt, opts.policy.interval)
	result.state.Cursor = nextCursor.Cursor
	result.dirty = true
	return result, 0
}

func appendBlockedResweepCandidates(
	ctx context.Context,
	env backlogQueryEnv,
	opts backlogResweepOptions,
	result *backlogResweepResult,
) ([]providers.WorkItem, int) {
	items, nextCursor, err := listBacklogScanWindow(
		ctx,
		env.issueProvider,
		env.repo,
		compactLabels(opts.trustLabel, blockedOnSiblingLabel),
		"",
		opts.fieldFilter,
		backlogScanCeiling,
		backlogScanCursor{Cursor: result.state.BlockedCursor},
		false,
	)
	if err != nil {
		return nil, failProviderStage(env.stderr, "list blocked items for dependency recheck", err, "claimed-items.json")
	}
	filtered := items[:0]
	for _, item := range items {
		env.debugf("candidate %s reached eligibility evaluation (blocked re-sweep)", item.ID)
		if !item.HasLabel(opts.trustLabel) {
			env.debugf("excluded %s: missing trust label %q", item.ID, opts.trustLabel)
			continue
		}
		if !item.HasLabel(blockedOnSiblingLabel) {
			env.debugf("excluded %s: missing required label %q", item.ID, blockedOnSiblingLabel)
			continue
		}
		if item.HasLabel(providers.LabelNeedsHuman) {
			env.debugf("excluded %s: has excluded label %q", item.ID, providers.LabelNeedsHuman)
			continue
		}
		if item.State != "" && !strings.EqualFold(item.State, "open") {
			env.debugf("excluded %s: state is not open", item.ID)
			continue
		}
		item.Integrity = providers.IntegrityForLabels(item.Labels, opts.trustLabel)
		filtered = append(filtered, item)
	}
	items = filtered
	if err := sortBacklogResweepCandidates(items, opts.selectionPriority, opts.fieldOrder, result.state.LastSweptAt); err != nil {
		pf(env.stderr, "error: order blocked dependency rechecks: %v\n", err)
		return nil, 1
	}
	if len(items) > opts.policy.maxItems {
		for _, item := range items[opts.policy.maxItems:] {
			env.debugf("excluded %s: blocked re-sweep selection capacity exhausted", item.ID)
		}
		items = items[:opts.policy.maxItems]
	}
	budget := opts.maxItems - len(result.eligible)
	for _, item := range items {
		blockers, err := env.ghIssueProvider.ListWorkItemBlockers(ctx, env.repo, item.ID)
		if err != nil {
			pf(env.stderr, "warning: dependency recheck item %s: %v\n", item.ID, err)
			env.debugf("excluded %s: native issue dependency check unavailable: %v", item.ID, err)
			continue
		}
		if len(blockers) == 0 {
			pf(env.stderr, "warning: dependency recheck item %s has no named native blocker; leaving it parked\n", item.ID)
			env.debugf("excluded %s: dependency recheck has no named native blocker", item.ID)
			continue
		}
		if !blockersActionable(blockers) {
			env.debugf("excluded %s: %s", item.ID, openBlockersExclusionReason(blockers))
			continue
		}
		if budget == 0 {
			env.debugf("excluded %s: blocked re-sweep selection capacity exhausted", item.ID)
			continue
		}
		result.eligible = append(result.eligible, item)
		result.modeByID[item.ID] = "dependency-recheck"
		budget--
	}
	result.state.BlockedCursor = nextCursor.Cursor
	return items, 0
}

func blockersActionable(blockers []providers.WorkItem) bool {
	for _, blocker := range blockers {
		if blocker.State != "" && strings.EqualFold(blocker.State, "open") && !blocker.HasLabel(providers.LabelNeedsHuman) {
			return false
		}
	}
	return true
}

func openBlockersExclusionReason(blockers []providers.WorkItem) string {
	var ids []string
	for _, blocker := range blockers {
		if blocker.State != "" && strings.EqualFold(blocker.State, "open") && !blocker.HasLabel(providers.LabelNeedsHuman) {
			ids = append(ids, blocker.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "dependency recheck still has an actionable open blocker"
	}
	return fmt.Sprintf("dependency recheck still has actionable open blocker(s): %s", strings.Join(ids, ","))
}

func appendReadyResweepCandidates(
	ctx context.Context,
	env backlogQueryEnv,
	opts backlogResweepOptions,
	result *backlogResweepResult,
) ([]providers.WorkItem, backlogScanCursor, int) {
	items, nextCursor, err := listBacklogScanWindow(
		ctx,
		env.issueProvider,
		env.repo,
		compactLabels(opts.trustLabel, opts.policy.readyLabel),
		"",
		opts.fieldFilter,
		backlogScanCeiling,
		backlogScanCursor{Cursor: result.state.Cursor},
		false,
	)
	if err != nil {
		return nil, nextCursor, failProviderStage(env.stderr, "list ready items for re-sweep", err, "claimed-items.json")
	}
	filtered := items[:0]
	for _, item := range items {
		env.debugf("candidate %s reached eligibility evaluation (ready re-sweep)", item.ID)
		if !item.HasLabel(opts.trustLabel) {
			env.debugf("excluded %s: missing trust label %q", item.ID, opts.trustLabel)
			continue
		}
		if !item.HasLabel(opts.policy.readyLabel) {
			env.debugf("excluded %s: missing required label %q", item.ID, opts.policy.readyLabel)
			continue
		}
		if item.HasLabel(providers.LabelNeedsHuman) {
			env.debugf("excluded %s: has excluded label %q", item.ID, providers.LabelNeedsHuman)
			continue
		}
		if item.State != "" && !strings.EqualFold(item.State, "open") {
			env.debugf("excluded %s: state is not open", item.ID)
			continue
		}
		matched, matchErr := opts.fieldFilter.Matches(item.Fields)
		if matchErr != nil {
			pf(env.stderr, "error: evaluate fieldPredicate for re-sweep item %s: %v\n", item.ID, matchErr)
			return nil, nextCursor, 1
		}
		if !matched {
			env.debugf("excluded %s: field predicate not matched", item.ID)
			continue
		}
		item.Integrity = providers.IntegrityForLabels(item.Labels, opts.trustLabel)
		filtered = append(filtered, item)
	}
	items = filtered
	if err := sortBacklogResweepCandidates(items, opts.selectionPriority, opts.fieldOrder, result.state.LastSweptAt); err != nil {
		pf(env.stderr, "error: order backlog re-sweep: %v\n", err)
		return nil, nextCursor, 1
	}
	budget := min(opts.policy.maxItems, opts.maxItems-len(result.eligible))
	if len(items) > budget {
		for _, item := range items[budget:] {
			env.debugf("excluded %s: ready re-sweep selection capacity exhausted", item.ID)
		}
		items = items[:budget]
	}
	for _, item := range items {
		if item.HasLabel(inReviewStatusLabel) || item.HasLabel(providers.LabelClaimed) || opts.openIssues[item.ID] {
			result.readOnly = append(result.readOnly, item)
			result.modeByID[item.ID] = "read-only"
		} else {
			result.eligible = append(result.eligible, item)
			result.modeByID[item.ID] = "resweep"
		}
	}
	return items, nextCursor, 0
}

func reportBacklogEligibility(
	env backlogQueryEnv,
	eligible, readOnly []providers.WorkItem,
	skipped map[string]blockedEligibilitySkip,
) {
	if !env.debug {
		return
	}
	eligibleCount := 0
	for _, item := range eligible {
		if _, blocked := skipped[item.ID]; blocked {
			continue
		}
		env.debugf("eligible %s", item.ID)
		eligibleCount++
	}
	for _, item := range readOnly {
		env.debugf("eligible %s (read-only re-sweep)", item.ID)
		eligibleCount++
	}
	if eligibleCount == 0 {
		env.debugf("eligible set empty")
	}
}

func runPlainBacklogQuery(env backlogQueryEnv, scan backlogEligibilityScan) int {
	err := withClaimLock(scan.lockPath, claimLockOperationBacklogFilterBlocked, func() error {
		var err error
		scan.eligible, _, err = reconcileBlockedEligibilityLocked(
			blockedRecordsPath(env.layout),
			env.backlogRepo,
			scan.eligible,
			scan.observedRecords,
			scan.remainingRecords,
			scan.verifiedSkips,
		)
		return err
	})
	if err != nil {
		pf(env.stderr, "error: %v\n", err)
		return 1
	}
	if len(scan.eligible) == 0 {
		if err := advanceBacklogScanCursor(scan.lockPath, scan.cursorPath, scan.cursor, scan.nextCursor); err != nil {
			pf(env.stderr, "error: advance backlog scan cursor: %v\n", err)
			return 1
		}
		// #233 parity for list/scan pumps: a stage that declares a resultFile is
		// a scheduled pump feeding a downstream stage, not an interactive
		// listing. An empty scan must then report ResultNoWork so the runner
		// short-circuits to a clean PhaseCompleted before any downstream agentic
		// stage runs — otherwise a scan-then-act workflow invokes its
		// model-backed stage every tick only to rediscover there is nothing to
		// act on, burning tokens on every empty poll. Interactive `backlog-query`
		// (no declared resultFile) keeps its human-readable "no eligible items".
		if providerInput("resultFile", "") != "" {
			return writeNoWorkResult(env.stdout, env.stderr, "no eligible items")
		}
		pln(env.stdout, "no eligible items")
		return 0
	}
	for _, item := range scan.eligible {
		pf(env.stdout, "%s\t%s\n", item.ID, item.Title)
	}
	return 0
}

func runReconcileBacklogQuery(
	ctx context.Context,
	env backlogQueryEnv,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	observedAt time.Time,
) int {
	reconciled, code := performBacklogQueryReconciliation(
		ctx, env, trustLabel, stalenessPolicy, observedAt, "backlog-reconciliation.json",
	)
	if code != 0 {
		return code
	}
	return writeBacklogReconciliationResult(reconciled, env.stdout, env.stderr)
}

func reconcileBacklogQueryMetadata(
	ctx context.Context,
	env backlogQueryEnv,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	observedAt time.Time,
	resultFile string,
) int {
	_, code := performBacklogQueryReconciliation(ctx, env, trustLabel, stalenessPolicy, observedAt, resultFile)
	return code
}

func performBacklogQueryReconciliation(
	ctx context.Context,
	env backlogQueryEnv,
	trustLabel string,
	stalenessPolicy backlogStalenessPolicy,
	observedAt time.Time,
	resultFile string,
) (int, int) {
	if env.ghIssueProvider == nil {
		err := fmt.Errorf("backlog curation/reconcile is not supported on Azure DevOps yet (BL-033); run it against a GitHub backlog")
		return 0, failProviderStage(env.stderr, "reconcile backlog metadata", err, resultFile)
	}
	reconciled, err := reconcileBacklogMetadata(
		ctx,
		env.layout,
		env.ghIssueProvider,
		env.repo,
		trustLabel,
		stalenessPolicy,
		func() time.Time { return observedAt },
	)
	if err != nil {
		return 0, failProviderStage(env.stderr, "reconcile backlog metadata", err, resultFile)
	}
	return reconciled, 0
}

type backlogScanOptions struct {
	trustLabel        string
	requireLabels     []string
	excludeLabels     []string
	labelExpression   string
	fieldExpression   string
	labelFilter       *labelpredicate.Predicate
	fieldFilter       *fieldpredicate.Predicate
	fieldOrder        fieldpredicate.Order
	selectionPriority []string
	respectAssignee   bool
	assignedTo        string
	scanLimit         int
	openIssues        map[string]bool
}

type backlogEligibilityScan struct {
	eligible         []providers.WorkItem
	lockPath         string
	cursorPath       string
	cursor           backlogScanCursor
	nextCursor       backlogScanCursor
	observedRecords  map[string]blockedRecord
	remainingRecords map[string]blockedRecord
	verifiedSkips    map[string]blockedEligibilitySkip
	observedSkips    []blockedEligibilitySkip
}

func scanBacklogEligibility(ctx context.Context, env backlogQueryEnv, opts backlogScanOptions) (backlogEligibilityScan, int) {
	var result backlogEligibilityScan
	labels := compactLabels(opts.trustLabel)
	labels = append(labels, opts.labelFilter.RequiredLabels()...)
	queryAssignee := ""
	if opts.respectAssignee && opts.assignedTo != "" {
		queryAssignee = opts.assignedTo
	}
	result.lockPath = filepath.Join(env.layout.SchedulerDir(), claimLockFileName)
	result.cursorPath = backlogScanCursorPath(
		env.layout.SchedulerDir(), env.backlogRepo, opts.trustLabel, opts.labelExpression, opts.fieldExpression,
		opts.requireLabels, opts.excludeLabels, queryAssignee,
	)
	exhaustiveScan := opts.fieldOrder.Configured()
	var err error
	if !exhaustiveScan {
		result.cursor, err = readBacklogScanCursor(result.lockPath, result.cursorPath)
		if err != nil {
			pf(env.stderr, "error: read backlog scan cursor: %v\n", err)
			return result, 1
		}
	}
	items, nextCursor, err := listBacklogScanWindow(
		ctx, env.issueProvider, env.backlogRepo, labels, queryAssignee, opts.fieldFilter, opts.scanLimit, result.cursor, exhaustiveScan,
	)
	if err != nil {
		return result, failProviderStage(env.stderr, "list work items", err, "claimed-item.json")
	}
	result.nextCursor = nextCursor
	for _, item := range items {
		env.debugf("candidate %s reached eligibility evaluation", item.ID)
		if opts.trustLabel != "" && !item.HasLabel(opts.trustLabel) {
			env.debugf("excluded %s: missing trust label %q", item.ID, opts.trustLabel)
			continue
		}
		if opts.respectAssignee && item.Assignee != opts.assignedTo {
			env.debugf("excluded %s: assignment does not match configured assignee", item.ID)
			continue
		}
		matched, matchErr := opts.labelFilter.Matches(item.Labels)
		if matchErr != nil {
			pf(env.stderr, "error: evaluate labelPredicate for item %s: %v\n", item.ID, matchErr)
			return result, 1
		}
		if !matched {
			env.debugf("excluded %s: %s", item.ID, labelExclusionReason(item, opts))
			continue
		}
		matched, matchErr = opts.fieldFilter.Matches(item.Fields)
		if matchErr != nil {
			pf(env.stderr, "error: evaluate fieldPredicate for item %s: %v\n", item.ID, matchErr)
			return result, 1
		}
		if !matched {
			env.debugf("excluded %s: field predicate not matched", item.ID)
			continue
		}
		if item.State != "" && !strings.EqualFold(item.State, "open") {
			env.debugf("excluded %s: state is not open", item.ID)
			continue
		}
		item.Integrity = providers.IntegrityForLabels(item.Labels, opts.trustLabel)
		result.eligible = append(result.eligible, item)
	}
	result.eligible, err = filterDecompositionEligibility(ctx, env.issueProvider, env.backlogRepo, result.eligible)
	if err != nil {
		return result, failProviderStage(env.stderr, "verify decomposition publication barrier", err, "claimed-item.json")
	}
	if opts.openIssues != nil {
		backstopped := result.eligible[:0]
		for _, item := range result.eligible {
			if !opts.openIssues[item.ID] {
				backstopped = append(backstopped, item)
			} else {
				env.debugf("excluded %s: review state has a linked open pull request", item.ID)
			}
		}
		result.eligible = backstopped
	}
	var warnings []string
	var dependencyExcluded func(providers.WorkItem, string)
	if env.debug {
		dependencyExcluded = func(item providers.WorkItem, reason string) {
			env.debugf("excluded %s: %s", item.ID, reason)
		}
	}
	result.eligible, warnings = filterDeclaredDependencyEligibilityDebug(
		ctx,
		env.issueProvider,
		env.backlogRepo,
		result.eligible,
		dependencyExcluded,
	)
	for _, warning := range warnings {
		pf(env.stderr, "warning: native issue dependencies: %s\n", warning)
	}
	result.observedRecords, err = snapshotBlockedRecordsForRepository(env.layout, env.backlogRepo)
	if err != nil {
		pf(env.stderr, "error: %v\n", err)
		return result, 1
	}
	result.remainingRecords = make(map[string]blockedRecord, len(result.observedRecords))
	for itemID, record := range result.observedRecords {
		result.remainingRecords[itemID] = record
	}
	_, result.observedSkips, _, warnings = filterBlockedEligibility(
		ctx,
		env.issueProvider,
		env.backlogRepo,
		append([]providers.WorkItem(nil), result.eligible...),
		result.remainingRecords,
	)
	for _, warning := range warnings {
		pf(env.stderr, "warning: blocked records: %s\n", warning)
	}
	if needsHumanAssignee, cfgErr := needsHumanAssigneeFor(env.layout); cfgErr != nil {
		pf(env.stderr, "warning: blocked cycle reconciliation: %v\n", cfgErr)
	} else {
		for _, warning := range reconcileBlockedCycleLabels(ctx, env.issueProvider, result.remainingRecords, needsHumanAssignee) {
			pf(env.stderr, "warning: blocked cycle reconciliation: %s\n", warning)
		}
	}
	result.verifiedSkips = make(map[string]blockedEligibilitySkip, len(result.observedSkips))
	for _, skip := range result.observedSkips {
		result.verifiedSkips[skip.ItemID] = skip
		env.debugf("excluded %s: %s", skip.ItemID, skip.reason())
	}
	if err := sortEligibleByFields(result.eligible, opts.selectionPriority, opts.fieldOrder); err != nil {
		pf(env.stderr, "error: apply fieldOrder: %v\n", err)
		return result, 1
	}
	return result, 0
}

func labelExclusionReason(item providers.WorkItem, opts backlogScanOptions) string {
	for _, label := range opts.requireLabels {
		if !item.HasLabel(label) {
			return fmt.Sprintf("missing required label %q", label)
		}
	}
	for _, label := range opts.excludeLabels {
		if item.HasLabel(label) {
			return fmt.Sprintf("has excluded label %q", label)
		}
	}
	return "label predicate not matched"
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
	env backlogQueryEnv,
	opts backlogScanOptions,
) int {
	labels := compactLabels(opts.trustLabel)
	labels = append(labels, opts.labelFilter.RequiredLabels()...)
	queryAssignee := ""
	if opts.respectAssignee && opts.assignedTo != "" {
		queryAssignee = opts.assignedTo
	}
	items, _, err := listBacklogScanWindow(
		ctx,
		env.issueProvider,
		env.backlogRepo,
		labels,
		queryAssignee,
		opts.fieldFilter,
		backlogScanCeiling,
		backlogScanCursor{},
		false,
	)
	if err != nil {
		return failProviderStage(env.stderr, "list work items", err, "claimed-item.json")
	}

	eligible := items[:0]
	for _, item := range items {
		env.debugf("candidate %s reached eligibility evaluation", item.ID)
		if opts.trustLabel != "" && !item.HasLabel(opts.trustLabel) {
			env.debugf("excluded %s: missing trust label %q", item.ID, opts.trustLabel)
			continue
		}
		if opts.respectAssignee && item.Assignee != opts.assignedTo {
			env.debugf("excluded %s: assignment does not match configured assignee", item.ID)
			continue
		}
		matched, matchErr := opts.labelFilter.Matches(item.Labels)
		if matchErr != nil {
			pf(env.stderr, "error: evaluate labelPredicate for item %s: %v\n", item.ID, matchErr)
			return 1
		}
		if !matched {
			env.debugf("excluded %s: %s", item.ID, labelExclusionReason(item, opts))
			continue
		}
		matched, matchErr = opts.fieldFilter.Matches(item.Fields)
		if matchErr != nil {
			pf(env.stderr, "error: evaluate fieldPredicate for item %s: %v\n", item.ID, matchErr)
			return 1
		}
		if !matched {
			env.debugf("excluded %s: field predicate not matched", item.ID)
			continue
		}
		if item.State != "" && !strings.EqualFold(item.State, "open") {
			env.debugf("excluded %s: state is not open", item.ID)
			continue
		}
		eligible = append(eligible, item)
	}
	eligible, err = filterDecompositionEligibility(ctx, env.issueProvider, env.backlogRepo, eligible)
	if err != nil {
		return failProviderStage(env.stderr, "verify decomposition publication barrier", err, "claimed-item.json")
	}
	if err := sortEligibleByFields(eligible, opts.selectionPriority, opts.fieldOrder); err != nil {
		pf(env.stderr, "error: apply fieldOrder: %v\n", err)
		return 1
	}

	if len(eligible) == 0 {
		env.debugf("eligible set empty")
		pln(env.stdout, "no eligible items")
		return 0
	}
	for _, item := range eligible {
		env.debugf("eligible %s", item.ID)
		pf(env.stdout, "%s\t%s\n", item.ID, item.Title)
	}
	return 0
}

func filterDecompositionEligibility(
	ctx context.Context,
	provider providers.BacklogProvider,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
) ([]providers.WorkItem, error) {
	filtered := items[:0]
	for _, item := range items {
		parentID, digest, _, marked, conflict := decomposition.ChildBatchIdentity(item.Body)
		if !marked {
			filtered = append(filtered, item)
			continue
		}
		if conflict {
			continue
		}
		if _, err := provider.GetWorkItem(ctx, repo, parentID); err != nil {
			return nil, fmt.Errorf("get decomposition parent %s for child %s: %w", parentID, item.ID, err)
		}
		comments, err := provider.ListComments(ctx, repo, parentID)
		if err != nil {
			return nil, fmt.Errorf("list decomposition parent %s comments for child %s: %w", parentID, item.ID, err)
		}
		published, recordConflict := decomposition.PublishedRecordIncludes(comments, parentID, digest, item.ID)
		if published && !recordConflict {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
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
// filterDeclaredDependencyEligibilityDebug can wrap it in a providers.Dispatcher
// (CONF-5, #2078): the native-dependency check goes through
// backlog.blockers instead of calling HasOpenWorkItemBlocker directly, so a
// provider that doesn't declare the capability fails closed with
// ErrUnsupported instead of risking a silent fail-open answer (#2059).
type backlogIssueProvider interface {
	providers.Provider
	ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error)
	ListWorkItemLabelTransitionsForItem(context.Context, providers.RepositoryRef, string, string) ([]providers.WorkItemLabelTransition, error)
}

func filterDeclaredDependencyEligibilityDebug(
	ctx context.Context,
	provider backlogIssueProvider,
	repo providers.RepositoryRef,
	eligible []providers.WorkItem,
	excluded func(providers.WorkItem, string),
) ([]providers.WorkItem, []string) {
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
			if excluded != nil {
				excluded(item, fmt.Sprintf("native issue dependency check unavailable: %v", err))
			}
			continue
		}
		if !blocked {
			filtered = append(filtered, item)
			continue
		}
		if excluded != nil {
			excluded(item, nativeDependencyExclusionReason(ctx, provider, repo, item))
		}
	}
	return filtered, warnings
}

type workItemBlockerLister interface {
	ListWorkItemBlockers(context.Context, providers.RepositoryRef, string) ([]providers.WorkItem, error)
}

func nativeDependencyExclusionReason(
	ctx context.Context,
	provider backlogIssueProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
) string {
	lister, ok := provider.(workItemBlockerLister)
	if !ok {
		return "native issue dependencies include an open blocker"
	}
	blockers, err := lister.ListWorkItemBlockers(ctx, repo, item.ID)
	if err != nil {
		return "native issue dependencies include an open blocker"
	}
	ids := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		if blocker.State == "" || strings.EqualFold(blocker.State, "open") {
			ids = append(ids, blocker.ID)
		}
	}
	if len(ids) == 0 {
		return "native issue dependencies include an open blocker"
	}
	sort.Strings(ids)
	return fmt.Sprintf("native issue dependencies include open blocker(s): %s", strings.Join(ids, ","))
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
func runBacklogQueryRelease(env backlogQueryEnv) int {
	root, l := env.root, env.layout
	stdout, stderr := env.stdout, env.stderr
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

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
		stageProvider, err := newProviderForStage(root, repo, false, withStageProviderMutations("issue"))
		if err != nil {
			return err
		}
		issueProvider, ok := stageProvider.(backlogIssueProvider)
		if !ok {
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
