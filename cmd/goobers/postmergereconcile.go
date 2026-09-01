package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

const (
	postMergeReconcileLedgerFile = stateclient.KeyPostMergeReconcileLedger
	postMergeReconcileLockFile   = "post-merge-reconcile.lock"
	postMergeReconcileVersion    = 1
	postMergeReconcilePending    = "pending"
	postMergeReconcileCompleted  = "completed"

	defaultPostMergeReconcileBatch    = 10
	maxPostMergeReconcileBatch        = 100
	defaultPostMergeReconcileLookback = 7 * 24 * time.Hour
)

type postMergeReconcileLedger struct {
	Version        int                                `json:"version"`
	Entries        map[string]postMergeReconcileEntry `json:"entries"`
	OpenPRScanPage map[string]int                     `json:"openPRScanPage,omitempty"`
}

type postMergeReconcileEntry struct {
	Repository    providers.RepositoryRef   `json:"repository"`
	PullNumber    string                    `json:"pullNumber"`
	State         string                    `json:"state"`
	TimedOutAt    time.Time                 `json:"timedOutAt"`
	LastCheckedAt *time.Time                `json:"lastCheckedAt,omitempty"`
	CompletedAt   *time.Time                `json:"completedAt,omitempty"`
	Actions       postMergeReconcileActions `json:"actions"`
}

type postMergeReconcileActions struct {
	BranchCleanup      bool            `json:"branchCleanup,omitempty"`
	SiblingFanOut      bool            `json:"siblingFanOut,omitempty"`
	ResolvedUnpark     bool            `json:"resolvedUnpark,omitempty"`
	EscalationUnpark   bool            `json:"escalationUnpark,omitempty"`
	DemotionUnpark     bool            `json:"demotionUnpark,omitempty"`
	ClosedIssueNumbers map[string]bool `json:"closedIssueNumbers,omitempty"`
}

type postMergeReconcileReport struct {
	Scanned    int
	Reconciled int
	Pending    int
	Expired    int
}

type postMergeReconcileProviderError struct {
	err error
}

func (e *postMergeReconcileProviderError) Error() string { return e.err.Error() }
func (e *postMergeReconcileProviderError) Unwrap() error { return e.err }

const reconcilePostMergeHelp = "Usage: goobers reconcile-post-merge [--max N] [--lookback D] [path]\n\n" +
	"Inspect a bounded batch of merge-queue entries whose queue-watch stage\n" +
	"timed out. A pull request that has since merged receives branch cleanup,\n" +
	"issue close-out, and sibling fan-out through the normal post-merge path;\n" +
	"an open or unmerged pull request remains pending. Completed entries are\n" +
	"durably skipped on later runs. Task inputs maxPullRequests and lookback\n" +
	"set the same bounds (defaults: 10 and 168h; hard maximum: 100).\n" +
	"Exit codes: 0 = sweep completed, 1 = business/provider error, 2 = usage error.\n"

func runReconcilePostMerge(args []string, stdout, stderr io.Writer) int {
	limitDefault, err := strconv.Atoi(providerInput("maxPullRequests", strconv.Itoa(defaultPostMergeReconcileBatch)))
	if err != nil {
		pf(stderr, "error: invalid maxPullRequests input: %v\n", err)
		return 1
	}
	lookbackDefault, err := time.ParseDuration(providerInput("lookback", defaultPostMergeReconcileLookback.String()))
	if err != nil {
		pf(stderr, "error: invalid lookback input: %v\n", err)
		return 1
	}

	fs := newCLIFlagSet("reconcile-post-merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("max", limitDefault, "maximum pending pull requests inspected in one sweep (1-100)")
	lookback := fs.Duration("lookback", lookbackDefault, "maximum age of a queue timeout eligible for reconciliation")
	fs.Usage = helpUsage(stderr, "reconcile-post-merge")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	if *limit < 1 || *limit > maxPostMergeReconcileBatch {
		pf(stderr, "error: max must be between 1 and %d\n", maxPostMergeReconcileBatch)
		return 1
	}
	if *lookback <= 0 {
		pf(stderr, "error: lookback must be positive\n")
		return 1
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if repo.Provider == providers.ProviderADO {
		return runReconcilePostMergeADO(root, repo, *limit, *lookback, stdout, stderr)
	}
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issuesToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := providerToken(capability.GitHubBranchDelete); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider, err := mergeStageProviderWithRecorder(root, repo, prToken, sidecarMutationRecorder{kind: "pr"})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issuesProvider, err := remediationStageProviderWithRecorder(root, repo, issuesToken, false, sidecarMutationRecorder{kind: "issue"})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	unparkErrs := reconcileOpenPullRequestParks(ctx, provider, repo, root, providerInput("base", providerBaseBranch()), *limit, stdout, stderr)
	report, err := reconcilePostMerges(ctx, provider, issuesProvider, repo, root, *limit, *lookback, time.Now, stdout, stderr)
	if err != nil {
		var providerErr *postMergeReconcileProviderError
		if errors.As(err, &providerErr) {
			return failProviderStage(stderr, "reconcile timed-out merge queue entries", providerErr.err, "")
		}
		pf(stderr, "error: reconcile timed-out merge queue entries: %v\n", err)
		return 1
	}
	pf(stdout, "post-merge reconciliation: scanned %d, reconciled %d, still pending %d, expired %d\n",
		report.Scanned, report.Reconciled, report.Pending, report.Expired)
	if len(unparkErrs) > 0 {
		return failProviderStage(stderr, "reconcile open pull request parks", errors.Join(unparkErrs...), "")
	}
	return 0
}

func runReconcilePostMergeADO(root string, repo providers.RepositoryRef, limit int, lookback time.Duration, stdout, stderr io.Writer) int {
	provider, err := newMergeReviewProviderAs[*providers.ADOProvider](root, repo, false,
		withStageProviderCapability(capability.ADOPRWrite),
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()

	var report postMergeReconcileReport
	err = withPostMergeReconcileLock(root, func(session *postMergeReconcileSession) error {
		ledger, err := session.read()
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-lookback)
		keys := pendingPostMergeReconcileKeys(ledger, repo)
		if len(keys) > limit {
			keys = keys[:limit]
		}
		var reconcileErrs []error
		for _, key := range keys {
			entry := ledger.Entries[key]
			report.Scanned++
			if entry.TimedOutAt.Before(cutoff) {
				delete(ledger.Entries, key)
				report.Expired++
				continue
			}
			poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{
				Repository: entry.Repository, PullID: entry.PullNumber,
			})
			if err != nil {
				return fmt.Errorf("poll pull request #%s: %w", entry.PullNumber, err)
			}
			checkedAt := time.Now().UTC()
			entry.LastCheckedAt = &checkedAt
			if !poll.Merged {
				ledger.Entries[key] = entry
				report.Pending++
				continue
			}
			actionErrs := performPostMergeADO(ctx, provider, backlogRepoRefForStage(root, repo), poll, entry.PullNumber, stdout, stderr)
			if len(actionErrs) > 0 {
				report.Pending++
				ledger.Entries[key] = entry
				reconcileErrs = append(reconcileErrs, fmt.Errorf("pr #%s: %w", entry.PullNumber, errors.Join(actionErrs...)))
				continue
			}
			// ADO's post-merge path closes work items only. Do not checkpoint
			// GitHub PR branch/fan-out actions that this path did not perform.
			closed := closingIssueNumbers(poll.Body)
			entry.Actions.ClosedIssueNumbers = make(map[string]bool, len(closed))
			for _, id := range closed {
				entry.Actions.ClosedIssueNumbers[id] = true
			}
			entry.State = postMergeReconcileCompleted
			completedAt := time.Now().UTC()
			entry.CompletedAt = &completedAt
			ledger.Entries[key] = entry
			report.Reconciled++
		}
		if err := session.write(ledger); err != nil {
			return err
		}
		if len(reconcileErrs) > 0 {
			return &postMergeReconcileProviderError{err: errors.Join(reconcileErrs...)}
		}
		return nil
	})
	if err != nil {
		pf(stdout, "post-merge reconciliation: scanned %d, reconciled %d, still pending %d, expired %d\n",
			report.Scanned, report.Reconciled, report.Pending, report.Expired)
		return failProviderStage(stderr, "reconcile timed-out merge queue entries", err, "")
	}
	pf(stdout, "post-merge reconciliation: scanned %d, reconciled %d, still pending %d, expired %d\n",
		report.Scanned, report.Reconciled, report.Pending, report.Expired)
	return 0
}

func reconcileOpenPullRequestParks(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	root string,
	base string,
	limit int,
	stdout, stderr io.Writer,
) []error {
	if base == "" {
		return nil
	}
	var others []providers.PullRequestSummary
	err := withPostMergeReconcileLock(root, func(session *postMergeReconcileSession) error {
		ledger, err := session.read()
		if err != nil {
			return err
		}
		key := postMergeOpenPRScanKey(repo, base)
		page := ledger.OpenPRScanPage[key]
		if page < 1 {
			page = 1
		}
		list := func(page int) error {
			others, err = provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
				Repository: repo, Base: base, Limit: limit, Page: page, SkipCheckState: true,
			})
			return err
		}
		if err := list(page); err != nil {
			return err
		}
		if len(others) == 0 && page > 1 {
			page = 1
			if err := list(page); err != nil {
				return err
			}
		}
		ledger.OpenPRScanPage[key] = page + 1
		return session.write(ledger)
	})
	if err != nil {
		return []error{fmt.Errorf("list bounded open pull requests targeting %s for park reconciliation: %w", base, err)}
	}
	namespace := providerBranchNamespace()
	namespaced := filterPullRequestsByHeadPrefix(others, namespace)
	demotedCandidates := filterPullRequestsByHeadPrefix(others, "goobers/")
	resolved, resolvedErrs := unparkResolvedSiblingsFrom(
		ctx, provider, repo, 0, namespaced, stderr,
	)
	escalated, escalationErrs := unparkSelfHealedEscalationsFrom(
		ctx, provider, repo, 0, namespaced, stderr,
	)
	demoted, demotionErrs := unparkSelfHealedDemotionsFrom(
		ctx, provider, repo, 0, demotedCandidates, stderr,
	)
	pf(stdout, "open-pr reconciliation: unparked %d resolved sibling(s), un-escalated %d self-healed pr(s), un-demoted %d self-healed pr(s)\n",
		len(resolved), len(escalated), len(demoted))
	return append(append(resolvedErrs, escalationErrs...), demotionErrs...)
}

func filterPullRequestsByHeadPrefix(prs []providers.PullRequestSummary, prefix string) []providers.PullRequestSummary {
	filtered := make([]providers.PullRequestSummary, 0, len(prs))
	for _, pr := range prs {
		if strings.HasPrefix(pr.Head, prefix) {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}

func reconcilePostMerges(
	ctx context.Context,
	provider mergeProvider,
	issuesProvider remediationProvider,
	repo providers.RepositoryRef,
	root string,
	limit int,
	lookback time.Duration,
	now func() time.Time,
	stdout, stderr io.Writer,
) (postMergeReconcileReport, error) {
	var report postMergeReconcileReport
	if limit < 1 || limit > maxPostMergeReconcileBatch {
		return report, fmt.Errorf("post-merge reconcile limit must be between 1 and %d", maxPostMergeReconcileBatch)
	}
	if lookback <= 0 {
		return report, fmt.Errorf("post-merge reconcile lookback must be positive")
	}
	if now == nil {
		now = time.Now
	}

	err := withPostMergeReconcileLock(root, func(session *postMergeReconcileSession) error {
		ledger, err := session.read()
		if err != nil {
			return err
		}
		current := now().UTC()
		cutoff := current.Add(-lookback)
		changed := false
		for key, entry := range ledger.Entries {
			if entry.TimedOutAt.Before(cutoff) {
				delete(ledger.Entries, key)
				report.Expired++
				changed = true
			}
		}

		keys := pendingPostMergeReconcileKeys(ledger, repo)
		if len(keys) > limit {
			keys = keys[:limit]
		}
		var reconcileErrs []error
		for _, key := range keys {
			entry := ledger.Entries[key]
			report.Scanned++
			poll, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{
				Repository: entry.Repository,
				PullID:     entry.PullNumber,
			})
			if err != nil {
				return &postMergeReconcileProviderError{err: fmt.Errorf("poll pull request #%s: %w", entry.PullNumber, err)}
			}
			checkedAt := now().UTC()
			entry.LastCheckedAt = &checkedAt
			if !poll.Merged {
				ledger.Entries[key] = entry
				report.Pending++
				changed = true
				if err := session.write(ledger); err != nil {
					return err
				}
				continue
			}

			ledger.Entries[key] = entry
			changed = true
			if err := session.write(ledger); err != nil {
				return err
			}
			actionErrs, err := reconcilePostMergeActions(ctx, provider, issuesProvider, root, poll, key, &ledger, session, stdout, stderr)
			if err != nil {
				return err
			}
			entry = ledger.Entries[key]
			if len(actionErrs) > 0 {
				report.Pending++
				reconcileErrs = append(reconcileErrs, fmt.Errorf("pr #%s: %w", entry.PullNumber, errors.Join(actionErrs...)))
				continue
			}
			if !postMergeReconcileActionsCompleted(entry, poll.Body) {
				return fmt.Errorf("post-merge actions for pr #%s stopped without an error or completion", entry.PullNumber)
			}
			completedAt := now().UTC()
			entry.State = postMergeReconcileCompleted
			entry.CompletedAt = &completedAt
			ledger.Entries[key] = entry
			report.Reconciled++
			changed = true
			if err := session.write(ledger); err != nil {
				return err
			}
		}
		if changed && len(keys) == 0 {
			return session.write(ledger)
		}
		if len(reconcileErrs) > 0 {
			return &postMergeReconcileProviderError{err: errors.Join(reconcileErrs...)}
		}
		return nil
	})
	return report, err
}

func reconcilePostMergeActions(
	ctx context.Context,
	provider mergeProvider,
	issuesProvider remediationProvider,
	root string,
	poll providers.PullRequestPollResult,
	key string,
	ledger *postMergeReconcileLedger,
	session *postMergeReconcileSession,
	stdout, stderr io.Writer,
) ([]error, error) {
	entry := ledger.Entries[key]
	if entry.Actions.ClosedIssueNumbers == nil {
		entry.Actions.ClosedIssueNumbers = map[string]bool{}
	}
	var actionErrs []error
	persist := func() error {
		ledger.Entries[key] = entry
		return session.write(*ledger)
	}
	run := func(name string, done *bool, action func() []error) error {
		if *done {
			return nil
		}
		errs := action()
		if len(errs) > 0 {
			for _, err := range errs {
				wrapped := fmt.Errorf("%s: %w", name, err)
				actionErrs = append(actionErrs, wrapped)
				pf(stderr, "warning: late-merged pr #%s %v\n", entry.PullNumber, wrapped)
			}
			return nil
		}
		*done = true
		return persist()
	}

	if err := run("branch cleanup", &entry.Actions.BranchCleanup, func() []error {
		cleanup := cleanupMergedBranch(ctx, root, poll.HeadRepository, poll.HeadBranch, provider)
		if cleanup.Error != "" {
			return []error{errors.New(cleanup.Error)}
		}
		pf(stdout, "branch cleanup %s (%s)\n", cleanup.Status, cleanup.HeadBranch)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := run("sibling fan-out", &entry.Actions.SiblingFanOut, func() []error {
		_, _, errs := fanOutNeedsRemediation(ctx, provider, entry.Repository, root, poll.Number, poll.BaseBranch, stderr)
		return errs
	}); err != nil {
		return nil, err
	}
	if err := run("resolved-sibling unpark", &entry.Actions.ResolvedUnpark, func() []error {
		_, errs := unparkResolvedSiblings(ctx, provider, entry.Repository, poll.Number, poll.BaseBranch, stderr)
		return errs
	}); err != nil {
		return nil, err
	}
	if err := run("self-healed escalation unpark", &entry.Actions.EscalationUnpark, func() []error {
		_, errs := unparkSelfHealedEscalations(ctx, provider, entry.Repository, poll.Number, poll.BaseBranch, stderr)
		return errs
	}); err != nil {
		return nil, err
	}
	if err := run("self-healed demotion unpark", &entry.Actions.DemotionUnpark, func() []error {
		_, errs := unparkSelfHealedDemotions(ctx, provider, entry.Repository, poll.Number, poll.BaseBranch, stderr)
		return errs
	}); err != nil {
		return nil, err
	}
	for _, issueID := range closingIssueNumbers(poll.Body) {
		if entry.Actions.ClosedIssueNumbers[issueID] {
			continue
		}
		if err := closeReferencedIssue(ctx, issuesProvider, entry.Repository, issueID, entry.PullNumber); err != nil {
			wrapped := fmt.Errorf("close issue #%s: %w", issueID, err)
			actionErrs = append(actionErrs, wrapped)
			pf(stderr, "warning: late-merged pr #%s %v\n", entry.PullNumber, wrapped)
			continue
		}
		entry.Actions.ClosedIssueNumbers[issueID] = true
		if err := persist(); err != nil {
			return nil, err
		}
	}
	ledger.Entries[key] = entry
	return actionErrs, nil
}

func postMergeReconcileActionsCompleted(entry postMergeReconcileEntry, body string) bool {
	if !entry.Actions.BranchCleanup ||
		!entry.Actions.SiblingFanOut ||
		!entry.Actions.ResolvedUnpark ||
		!entry.Actions.EscalationUnpark ||
		!entry.Actions.DemotionUnpark {
		return false
	}
	for _, issueID := range closingIssueNumbers(body) {
		if !entry.Actions.ClosedIssueNumbers[issueID] {
			return false
		}
	}
	return true
}

func recordPostMergeTimeout(root string, repo providers.RepositoryRef, pullNumber string, at time.Time) error {
	if strings.TrimSpace(pullNumber) == "" {
		return fmt.Errorf("pull number is required")
	}
	return withPostMergeReconcileLock(root, func(session *postMergeReconcileSession) error {
		ledger, err := session.read()
		if err != nil {
			return err
		}
		key := postMergeReconcileKey(repo, pullNumber)
		if ledger.Entries[key].State == postMergeReconcileCompleted {
			return nil
		}
		if existing, ok := ledger.Entries[key]; ok {
			existing.State = postMergeReconcilePending
			existing.TimedOutAt = at.UTC()
			existing.LastCheckedAt = nil
			existing.CompletedAt = nil
			ledger.Entries[key] = existing
		} else {
			ledger.Entries[key] = postMergeReconcileEntry{
				Repository: repo,
				PullNumber: pullNumber,
				State:      postMergeReconcilePending,
				TimedOutAt: at.UTC(),
			}
		}
		return session.write(ledger)
	})
}

func postMergeReconciliationCompleted(ledger postMergeReconcileLedger, repo providers.RepositoryRef, pullNumber string) bool {
	return ledger.Entries[postMergeReconcileKey(repo, pullNumber)].State == postMergeReconcileCompleted
}

func completePostMergeReconciliation(ledger *postMergeReconcileLedger, repo providers.RepositoryRef, pullNumber string) bool {
	key := postMergeReconcileKey(repo, pullNumber)
	entry, ok := ledger.Entries[key]
	if !ok || entry.State == postMergeReconcileCompleted {
		return false
	}
	completedAt := time.Now().UTC()
	entry.State = postMergeReconcileCompleted
	entry.CompletedAt = &completedAt
	ledger.Entries[key] = entry
	return true
}

func pendingPostMergeReconcileKeys(ledger postMergeReconcileLedger, repo providers.RepositoryRef) []string {
	keys := make([]string, 0, len(ledger.Entries))
	for key, entry := range ledger.Entries {
		if entry.State == postMergeReconcilePending && sameRepository(entry.Repository, repo) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := ledger.Entries[keys[i]], ledger.Entries[keys[j]]
		leftChecked, rightChecked := time.Time{}, time.Time{}
		if left.LastCheckedAt != nil {
			leftChecked = *left.LastCheckedAt
		}
		if right.LastCheckedAt != nil {
			rightChecked = *right.LastCheckedAt
		}
		if !leftChecked.Equal(rightChecked) {
			return leftChecked.Before(rightChecked)
		}
		if !left.TimedOutAt.Equal(right.TimedOutAt) {
			return left.TimedOutAt.Before(right.TimedOutAt)
		}
		return keys[i] < keys[j]
	})
	return keys
}

func sameRepository(left, right providers.RepositoryRef) bool {
	return providers.SameRepository(left, right)
}

// postMergeReconcileKey keys a durable reconciliation record by the canonical
// provider-complete repository identity (#3649): owner/name alone repeat
// across providers, Azure DevOps projects, and self-hosted services, which let
// one repository's completed record suppress another's reconciliation.
func postMergeReconcileKey(repo providers.RepositoryRef, pullNumber string) string {
	return repo.CanonicalKey() + "#" + pullNumber
}

func postMergeOpenPRScanKey(repo providers.RepositoryRef, base string) string {
	return repo.CanonicalKey() + "#" + strings.ToLower(strings.TrimSpace(base))
}

// postMergeReconcileSession is one critical section over the reconcile ledger.
// It exists because this ledger is not a single read-modify-write: the scan
// reads once and then persists partial progress repeatedly as it polls the
// provider and takes post-merge actions, so a crash mid-scan does not repeat
// an action it already completed.
//
// Every write carries the ETag the session last observed. On the instance's
// own files that is a formality — the whole session runs inside
// post-merge-reconcile.lock, so the ETag always matches. On the
// scheduler-state plane (#3878) it is the real isolation: an interleaved
// writer is refused rather than clobbered, and the stage fails loudly instead
// of losing the other writer's reconcile progress.
type postMergeReconcileSession struct {
	ctx   context.Context
	store stateclient.Store
	etag  string
}

// read loads the ledger and records its ETag as the precondition for this
// session's next write.
func (s *postMergeReconcileSession) read() (postMergeReconcileLedger, error) {
	value, err := s.store.Get(s.ctx, stateclient.KeyPostMergeReconcileLedger)
	if err != nil {
		return emptyPostMergeReconcileLedger(), fmt.Errorf("read post-merge reconcile ledger: %w", err)
	}
	ledger, err := decodePostMergeReconcileLedger(value)
	if err != nil {
		return ledger, err
	}
	s.etag = value.ETag
	return ledger, nil
}

// write persists the ledger, conditional on nothing else having written since
// this session last read or wrote.
func (s *postMergeReconcileSession) write(ledger postMergeReconcileLedger) error {
	data, err := encodePostMergeReconcileLedger(ledger)
	if err != nil {
		return err
	}
	value, err := s.store.Put(s.ctx, stateclient.KeyPostMergeReconcileLedger, data, s.etag)
	if err != nil {
		return fmt.Errorf("write post-merge reconcile ledger: %w", err)
	}
	s.etag = value.ETag
	return nil
}

// withPostMergeReconcileLock runs fn as one critical section over the reconcile
// ledger. The name is unchanged because the guarantee is unchanged: locally it
// is still post-merge-reconcile.lock held across the whole of fn.
func withPostMergeReconcileLock(root string, fn func(*postMergeReconcileSession) error) error {
	return withPostMergeReconcileSession(stateContext(), root, fn)
}

func withPostMergeReconcileSession(ctx context.Context, root string, fn func(*postMergeReconcileSession) error) error {
	layout := layoutFor(root)
	// The plane owns the daemon's scheduler directory; a stage pod has no
	// business creating one of its own.
	if !statePlaneSelected() {
		if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
			return fmt.Errorf("create scheduler directory: %w", err)
		}
	}
	store, err := openStageStateStore(layout)
	if err != nil {
		return err
	}
	// The session reads and writes through the HELD store: Section already
	// holds the key's lock for the whole of fn on the file backend, and taking
	// it again per operation would wait on the section standing above it. On
	// the plane both stores are the same client — the daemon takes the lock
	// per request and the session's If-Match is what isolates it.
	held, err := openHeldStageStateStore(layout)
	if err != nil {
		return err
	}
	session := &postMergeReconcileSession{ctx: ctx, store: held}
	return store.Section(ctx, stateclient.KeyPostMergeReconcileLedger, stateLockOperationPostMergeUpdate, func() error {
		return fn(session)
	})
}

func emptyPostMergeReconcileLedger() postMergeReconcileLedger {
	return postMergeReconcileLedger{
		Version:        postMergeReconcileVersion,
		Entries:        map[string]postMergeReconcileEntry{},
		OpenPRScanPage: map[string]int{},
	}
}

func decodePostMergeReconcileLedger(value stateclient.Value) (postMergeReconcileLedger, error) {
	ledger := emptyPostMergeReconcileLedger()
	if !value.Exists() {
		return ledger, nil
	}
	if err := json.Unmarshal(value.Data, &ledger); err != nil {
		return emptyPostMergeReconcileLedger(), fmt.Errorf("decode post-merge reconcile ledger: %w", err)
	}
	if ledger.Version != postMergeReconcileVersion {
		return ledger, fmt.Errorf("unsupported post-merge reconcile ledger version %d", ledger.Version)
	}
	if ledger.Entries == nil {
		ledger.Entries = map[string]postMergeReconcileEntry{}
	}
	if ledger.OpenPRScanPage == nil {
		ledger.OpenPRScanPage = map[string]int{}
	}
	ledger.Entries = canonicalizePostMergeReconcileEntries(ledger.Entries)
	return ledger, nil
}

// canonicalizePostMergeReconcileEntries rekeys records written before the
// canonical provider-complete key (#3649). Each entry carries the repository
// it belongs to, so the canonical key is recomputable in place; without this,
// a ledger written by an earlier build would keep pending records under keys
// no lookup can ever reach again, re-reconciling them forever.
func canonicalizePostMergeReconcileEntries(
	entries map[string]postMergeReconcileEntry,
) map[string]postMergeReconcileEntry {
	canonical := make(map[string]postMergeReconcileEntry, len(entries))
	for key, entry := range entries {
		target := key
		if entry.PullNumber != "" {
			target = postMergeReconcileKey(entry.Repository, entry.PullNumber)
		}
		existing, clash := canonical[target]
		if clash && !postMergeEntryPreferred(entry, existing) {
			continue
		}
		canonical[target] = entry
	}
	return canonical
}

// postMergeEntryPreferred resolves two legacy records that collapse onto the
// same canonical key: a completed record wins so reconciliation is never
// repeated, then the most recent timeout, so the outcome does not depend on
// map iteration order.
func postMergeEntryPreferred(candidate, existing postMergeReconcileEntry) bool {
	if (candidate.State == postMergeReconcileCompleted) != (existing.State == postMergeReconcileCompleted) {
		return candidate.State == postMergeReconcileCompleted
	}
	return candidate.TimedOutAt.After(existing.TimedOutAt)
}

func encodePostMergeReconcileLedger(ledger postMergeReconcileLedger) ([]byte, error) {
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode post-merge reconcile ledger: %w", err)
	}
	return append(data, '\n'), nil
}
