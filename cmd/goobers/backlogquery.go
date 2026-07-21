package main

import (
	"context"
	"encoding/json"
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
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// DefaultClaimLease bounds how long a claimed item stays held before
// localscheduler.ClaimLedger.RecoverExpired (wired into `goobers up`, #131)
// releases it back to the pool. Raised from the original 2h to 6h (issue
// #235, edge 2): a real implementation run is implement -> reviewer gate ->
// make ci -> open-pr -> ci-poll, and ci-poll alone can legitimately run
// close to its own DefaultPollTimeout (30m) *per attempt*, retried — the old
// 2h default was reachable by a real run, not just a theoretical bound,
// which meant RecoverExpired's known liveness-unaware hazard (see its own
// doc comment) could fire on a still-live run in the shipped config, not
// only on a genuinely abandoned one. 6h is comfortably above that realistic
// ceiling while still bounding a genuinely abandoned claim (a crashed run
// whose lease never gets explicitly released) to a reasonable stuck time.
// Overridable via the leaseDuration Task.Input (a time.ParseDuration
// string) — must be positive; see the leaseDuration parsing below and
// localscheduler.ClaimLedger.Claim's own fail-closed check.
const DefaultClaimLease = 6 * time.Hour

// backlogScanCeiling is the floor on how many candidates a backlog query
// fetches from the provider, independent of maxItems (#532) — "how many to
// scan" and "how many to claim this run" are different questions. High enough
// that the full eligible set is normally covered outright (the live backlog
// runs ~40 eligible items; 250 is ~6x that), low enough to bound provider
// pagination (3 pages at GitHub's per_page=100 max). Truncation past this
// ceiling is starvation-safe because the fetch is OldestFirst — see the
// ListWorkItems call below.
const backlogScanCeiling = 250

const blockedEligibilitySkipAnnotation = "backlog.blocked-item-skipped"

const inReviewStatusLabel = "goobers/status:in-review"

func runBacklogQuery(args []string, stdout, stderr io.Writer) int {
	return runBacklogQueryWithClaimBarrier(args, stdout, stderr, nil)
}

const backlogQueryHelp = "Usage: goobers backlog-query [--claim | --release] [path]\n\n" +
	"Query the provider for eligible backlog items — labeled with both\n" +
	"trustLabel (SEC-047: required on public repos, since backlog content is\n" +
	"untrusted input otherwise) and requireLabels. With --claim, claims\n" +
	"exactly one via the local claim ledger (source of truth) mirrored to a\n" +
	"provider-visible marker, and writes it to the declared result file.\n" +
	"trustLabel is required with --claim (SEC-047 fails closed, not open) —\n" +
	"a plain list (no --claim) does not require it.\n\n" +
	"With --release, releases every claim this run holds in the local ledger\n" +
	"(issue #234: a workflow that only reads/labels an item, never opening a\n" +
	"PR or closing the issue — e.g. backlog-curation — must release its own\n" +
	"claim explicitly, since issue-close-out's release is reached only by the\n" +
	"implementation workflow). Idempotent: releasing claims this run does not\n" +
	"hold (already released, e.g. re-run after a crash) is a no-op success, not\n" +
	"an error. --claim and --release are mutually exclusive.\n\n" +
	"With --claim, contested-file dispatch awareness (#1085) deprioritizes\n" +
	"claiming an issue whose referenced files are already contested by\n" +
	"contestedFileMinPRs+ (default 2) open PRs, so new work isn't fed into an\n" +
	"overlap cluster faster than merge-review can drain it. It only reorders\n" +
	"candidates (never drops one — an all-contested cycle still claims FIFO)\n" +
	"and falls back to FIFO on any provider error. Disable with input\n" +
	"deprioritizeContestedFiles=false.\n\n" +
	"Exit codes: 0 = eligible item found (and claimed, if --claim) / released\n" +
	"(--release), 1 = business error (no eligible/claimable item, missing\n" +
	"trustLabel with --claim, config/credential/provider error), 2 =\n" +
	"usage/IO error.\n"

// The barrier lets the blocked-record race regression pause immediately before
// the lock-protected reconciliation and claim transaction.
func runBacklogQueryWithClaimBarrier(args []string, stdout, stderr io.Writer, beforeClaimTransaction func()) int {
	fs := flag.NewFlagSet("backlog-query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	claim := fs.Bool("claim", false, "claim the first eligible item (mirrors the claim in the local ledger + provider)")
	release := fs.Bool("release", false, "release this run's claim ledger leases early (issue #234) — no provider access, pure ledger operation")
	fs.Usage = helpUsage(stderr, "backlog-query")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *claim && *release {
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
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issueProvider := newGitHubProvider(token, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "issue"}))

	trustLabel := providerInput("trustLabel", "")
	requireLabels := splitLabelList(providerInput("requireLabels", ""))
	excludeLabels := splitLabelList(providerInput("excludeLabels", ""))

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
	if *claim && trustLabel == "" {
		pln(stderr, "error: trustLabel is required to claim (SEC-047: backlog content is untrusted input on a public repo) — declare inputs.trustLabel")
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()

	var (
		prProvider *providers.GitHubProvider
		openIssues map[string]bool
	)
	if prToken, tokenErr := providerToken(capability.GitHubPRWrite); tokenErr == nil {
		prProvider = newGitHubProvider(prToken)
		openIssues, err = openPRIssueNumbers(ctx, prProvider, repo)
		if err != nil {
			return failProviderStage(stderr, "list open pull requests", err, "claimed-item.json")
		}
		if *claim {
			if err := reconcileClosedUnmergedInReview(ctx, issueProvider, prProvider, repo); err != nil {
				return failProviderStage(stderr, "reconcile closed pull requests", err, "claimed-item.json")
			}
		}
	}

	labels := make([]string, 0, 1+len(requireLabels))
	if trustLabel != "" {
		labels = append(labels, trustLabel)
	}
	labels = append(labels, requireLabels...)
	items, err := issueProvider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repo,
		Labels:      labels,
		State:       "open",
		Limit:       scanLimit,
		OldestFirst: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list work items", err, "claimed-item.json")
	}

	// Re-verify eligibility in code (SEC-047: backlog content is untrusted
	// input on a public repo) rather than trusting the provider query's
	// labels filter alone — a defense-in-depth check, not a redundant one.
	var eligible []providers.WorkItem
	for _, item := range items {
		if trustLabel != "" && !item.HasLabel(trustLabel) {
			continue
		}
		hasRequiredLabels := true
		for _, label := range requireLabels {
			if !item.HasLabel(label) {
				hasRequiredLabels = false
				break
			}
		}
		if !hasRequiredLabels {
			continue
		}
		if hasAnyLabel(item.Labels, excludeLabels) {
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

	// Dependency-aware skip (#552): snapshot blocked.json under its local
	// lock, then resolve every provider-backed issue state after releasing it.
	// A stalled provider must never prevent terminal claim finalization.
	observedRecords, err := snapshotBlockedRecordsForRepository(l, repo)
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
		repo,
		append([]providers.WorkItem(nil), eligible...),
		remainingRecords,
	)
	for _, warning := range blockedWarnings {
		// Warn, never fail the whole query: only the affected record stays
		// parked when its provider state cannot be resolved.
		pf(stderr, "warning: blocked records: %s\n", warning)
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
	// GitHub's — can't silently flip claim order again. A fuller
	// configurable priority mechanism (native-field or label-list ranking,
	// configurable tie-break) is tracked separately; this is the
	// unconditional baseline every claim order now starts from.
	sortEligibleFIFO(eligible)

	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	if !*claim {
		err = withClaimLock(lockPath, claimLockOperationBacklogFilterBlocked, func() error {
			var rerr error
			eligible, _, rerr = reconcileBlockedEligibilityLocked(
				blockedRecordsPath(l),
				repo,
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
			if touches, terr := openPRTouches(ctx, prProvider, repo); terr != nil {
				pf(stderr, "warning: contested-file dispatch awareness unavailable (%v); using FIFO order\n", terr)
			} else {
				reordered, deprioritized := partitionByContention(eligible, touches, minPRs)
				if n := len(deprioritized); n > 0 && n < len(reordered) {
					pf(stderr, "contested-file dispatch: deprioritized %d contested issue(s) [%s] behind %d disjoint one(s)\n",
						n, strings.Join(deprioritized, ","), len(reordered)-n)
				}
				eligible = reordered
			}
		}
	}

	if len(eligible) == 0 {
		err = withClaimLock(lockPath, claimLockOperationBacklogFilterBlocked, func() error {
			_, _, rerr := reconcileBlockedEligibilityLocked(
				blockedRecordsPath(l),
				repo,
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
		return writeNoWorkResult(stdout, stderr, "no eligible item to claim")
	}

	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
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
	var claimed []providers.WorkItem
	gaggle := providerGaggle()
	if beforeClaimTransaction != nil {
		beforeClaimTransaction()
	}
	err = withClaimLock(lockPath, claimLockOperationBacklogClaim, func() error {
		var lerr error
		eligible, observedSkips, lerr = reconcileBlockedEligibilityLocked(
			blockedRecordsPath(l),
			repo,
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

		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName), localscheduler.WithInstanceLog(instanceLog))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		for i := range eligible {
			if len(claimed) >= maxItems {
				break
			}
			item := eligible[i]
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
			}
		}
		return nil
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "no eligible item to claim")
	}
	// Every eligible item is already claimed by another run — a routine no-work
	// tick (#233), not an error: exit 0 with the structured noWork result the
	// runner short-circuits on, rather than the old return 1. Batch-aware len
	// check (#236) replaces #274's pointer-nil check.
	if len(claimed) == 0 {
		return writeNoWorkResult(stdout, stderr, "every eligible item is already claimed by another run")
	}

	// Provider-visible marker per claimed item: best-effort mirror of the
	// ledger's (already authoritative, per localscheduler.ClaimLedger's doc)
	// decision, for human visibility on the provider. A failure here does not
	// undo the ledger claim — the ledger, not this marker, is the source of truth.
	for i := range claimed {
		if _, err := issueProvider.ClaimWorkItem(ctx, providers.ClaimWorkItemRequest{Repository: repo, ID: claimed[i].ID, RunID: runID}); err != nil {
			pf(stderr, "warning: provider claim marker for %s failed (ledger claim still holds): %v\n", claimed[i].ID, err)
		}
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
	if maxItems == 1 {
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

	if len(claimed) == 1 {
		pf(stdout, "claimed %s: %s\n", claimed[0].ID, claimed[0].Title)
	} else {
		pf(stdout, "claimed %d items\n", len(claimed))
	}
	return 0
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

// sortEligibleFIFO orders items ascending by numeric ID in place — both
// GitHubProvider and ADOProvider mint WorkItem.ID as strconv.Itoa of the
// issue/work-item number, which is monotonically increasing with creation
// order, so this is exactly "oldest filed first" without needing a
// dedicated CreatedAt field or per-provider sort-parameter wiring (#350).
// A non-numeric ID (a future provider whose IDs don't work this way) falls
// back to a stable lexical compare rather than leaving that item's relative
// position to whatever order the provider happened to return it in.
func sortEligibleFIFO(items []providers.WorkItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ni, iOK := parseWorkItemID(items[i].ID)
		nj, jOK := parseWorkItemID(items[j].ID)
		if iOK && jOK {
			return ni < nj
		}
		return items[i].ID < items[j].ID
	})
}

func parseWorkItemID(id string) (int64, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	return n, err == nil
}

// runBacklogQueryRelease implements `backlog-query --release` (issue #234):
// an explicit, deterministic-path release of every claim this run holds, for a
// workflow whose consuming stage neither opens a PR nor closes the issue
// (backlog-curation's curate: it triages/labels the item and stops) — the
// only existing release path, issue-close-out, is reached solely by the
// implementation workflow after it opens a PR, so a curation run's claim was
// otherwise held for the full lease (up to 6h now, was 2h — #235) even
// though curation's own work on the item finished immediately.
//
// No provider/credential access at all — this is a pure ledger operation,
// unlike --claim (which needs a token to list/mirror against the provider).
// That keeps a curation workflow's release stage free of any capability
// declaration, and keeps ledger mutations on the deterministic path per the
// issue's own fix-shape guidance.
func runBacklogQueryRelease(root string, stdout, stderr io.Writer) int {
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	l := layoutFor(root)
	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	var released []string
	err = withClaimLock(lockPath, claimLockOperationBacklogRelease, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		// ForRunAll + Release, not a blind Release-by-guess: idempotency (issue
		// #234's acceptance criterion) requires distinguishing "this run
		// holds nothing to release" (a no-op success — already released, or
		// a crash-resume of this same stage) from an actual release, without
		// needing the caller to already know the item id. Release itself is
		// already a no-op on an unheld item (its own doc comment), so this
		// is belt-and-suspenders for the "nothing to report" stdout case
		// below, not required for correctness.
		for _, entry := range ledger.ForRunAll(runID) {
			if rerr := ledger.ReleaseEntry(entry, runID); rerr != nil {
				return fmt.Errorf("release %s in ledger: %w", entry.ItemID, rerr)
			}
			released = append(released, entry.ItemID)
		}
		return nil
	})
	if err != nil {
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
