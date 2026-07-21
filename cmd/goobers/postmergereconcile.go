package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	postMergeReconcileLedgerFile = "post-merge-reconcile.json"
	postMergeReconcileLockFile   = "post-merge-reconcile.lock"
	postMergeReconcileVersion    = 1
	postMergeReconcilePending    = "pending"
	postMergeReconcileCompleted  = "completed"

	defaultPostMergeReconcileBatch    = 10
	maxPostMergeReconcileBatch        = 100
	defaultPostMergeReconcileLookback = 7 * 24 * time.Hour
)

type postMergeReconcileLedger struct {
	Version int                                `json:"version"`
	Entries map[string]postMergeReconcileEntry `json:"entries"`
}

type postMergeReconcileEntry struct {
	Repository    providers.RepositoryRef `json:"repository"`
	PullNumber    string                  `json:"pullNumber"`
	State         string                  `json:"state"`
	TimedOutAt    time.Time               `json:"timedOutAt"`
	LastCheckedAt *time.Time              `json:"lastCheckedAt,omitempty"`
	CompletedAt   *time.Time              `json:"completedAt,omitempty"`
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

	fs := flag.NewFlagSet("reconcile-post-merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("max", limitDefault, "maximum pending pull requests inspected in one sweep (1-100)")
	lookback := fs.Duration("lookback", lookbackDefault, "maximum age of a queue timeout eligible for reconciliation")
	fs.Usage = helpUsage(stderr, "reconcile-post-merge")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
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

	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := providerToken(capability.GitHubBranchDelete); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(prToken, providers.WithMutationRecorder(sidecarMutationRecorder{kind: "pr"}))

	ctx, cancel := providerCommandContext()
	defer cancel()
	report, err := reconcilePostMerges(ctx, provider, repo, root, *limit, *lookback, time.Now, stdout, stderr)
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
	return 0
}

func reconcilePostMerges(
	ctx context.Context,
	provider *providers.GitHubProvider,
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

	err := withPostMergeReconcileLock(root, func(ledgerPath string) error {
		ledger, err := readPostMergeReconcileLedger(ledgerPath)
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
				if err := writePostMergeReconcileLedger(ledgerPath, ledger); err != nil {
					return err
				}
				continue
			}

			cleanup := cleanupMergedBranch(ctx, poll.HeadRepository, poll.HeadBranch, provider)
			if cleanup.Error != "" {
				pf(stderr, "warning: late-merged pr #%s branch cleanup failed: %s\n", entry.PullNumber, cleanup.Error)
			} else {
				pf(stdout, "branch cleanup %s (%s)\n", cleanup.Status, cleanup.HeadBranch)
			}
			performPostMerge(ctx, provider, entry.Repository, root, entry.PullNumber, poll, stdout, stderr)
			completedAt := now().UTC()
			entry.State = postMergeReconcileCompleted
			entry.CompletedAt = &completedAt
			ledger.Entries[key] = entry
			report.Reconciled++
			changed = true
			if err := writePostMergeReconcileLedger(ledgerPath, ledger); err != nil {
				return err
			}
		}
		if changed && len(keys) == 0 {
			return writePostMergeReconcileLedger(ledgerPath, ledger)
		}
		return nil
	})
	return report, err
}

func recordPostMergeTimeout(root string, repo providers.RepositoryRef, pullNumber string, at time.Time) error {
	if strings.TrimSpace(pullNumber) == "" {
		return fmt.Errorf("pull number is required")
	}
	return withPostMergeReconcileLock(root, func(ledgerPath string) error {
		ledger, err := readPostMergeReconcileLedger(ledgerPath)
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
		return writePostMergeReconcileLedger(ledgerPath, ledger)
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
	return strings.EqualFold(left.Owner, right.Owner) && strings.EqualFold(left.Name, right.Name)
}

func postMergeReconcileKey(repo providers.RepositoryRef, pullNumber string) string {
	return strings.ToLower(repo.Owner) + "/" + strings.ToLower(repo.Name) + "#" + pullNumber
}

func withPostMergeReconcileLock(root string, fn func(string) error) error {
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return fmt.Errorf("create scheduler directory: %w", err)
	}
	lockPath := filepath.Join(schedulerDir, postMergeReconcileLockFile)
	ledgerPath := filepath.Join(schedulerDir, postMergeReconcileLedgerFile)
	return withFileLock(lockPath, func() error { return fn(ledgerPath) })
}

func readPostMergeReconcileLedger(path string) (postMergeReconcileLedger, error) {
	ledger := postMergeReconcileLedger{
		Version: postMergeReconcileVersion,
		Entries: map[string]postMergeReconcileEntry{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return ledger, fmt.Errorf("read post-merge reconcile ledger: %w", err)
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		return ledger, fmt.Errorf("decode post-merge reconcile ledger: %w", err)
	}
	if ledger.Version != postMergeReconcileVersion {
		return ledger, fmt.Errorf("unsupported post-merge reconcile ledger version %d", ledger.Version)
	}
	if ledger.Entries == nil {
		ledger.Entries = map[string]postMergeReconcileEntry{}
	}
	return ledger, nil
}

func writePostMergeReconcileLedger(path string, ledger postMergeReconcileLedger) error {
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("encode post-merge reconcile ledger: %w", err)
	}
	data = append(data, '\n')
	if err := journal.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write post-merge reconcile ledger: %w", err)
	}
	return nil
}
