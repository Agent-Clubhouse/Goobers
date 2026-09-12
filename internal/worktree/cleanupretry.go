package worktree

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CleanupRetryOptions bounds one prompt retry pass. After is an opaque cursor
// returned by the preceding pass; Limit counts attempted pending cleanups, not
// successful removals.
type CleanupRetryOptions struct {
	After string
	Limit int
}

// CleanupRetryReport describes one bounded pass over the durable
// cleanup-pending queue.
type CleanupRetryReport struct {
	Next      string
	Attempted int
	Removed   []ReapResult
	Warnings  []ReapWarning
}

type cleanupRetryCandidate struct {
	token      string
	key        string
	markerPath string
}

// RetryCleanupPending promptly retries worktrees that Remove durably marked
// cleanup-pending. The two marker copies are the queue: no scheduler-local
// state is needed, so a daemon restart cannot lose an outstanding retry.
//
// Discovery reads only primary marker files. Before any destructive action,
// the repository lock is acquired and both records are re-read and required to
// agree on a modern, explicit ownership identity and pending status. Cleanup
// then uses the same guarded tail as Reap.
func (m *Manager) RetryCleanupPending(ctx context.Context, opts CleanupRetryOptions) (CleanupRetryReport, error) {
	if opts.Limit <= 0 {
		return CleanupRetryReport{}, fmt.Errorf("worktree: cleanup retry limit must be positive")
	}
	candidates, warnings, err := m.cleanupRetryCandidates(ctx, opts.Limit)
	report := CleanupRetryReport{Warnings: warnings}
	if err != nil || len(candidates) == 0 {
		return report, err
	}

	start := sort.Search(len(candidates), func(i int) bool {
		return candidates[i].token > opts.After
	})
	if start == len(candidates) {
		start = 0
	}
	attempts := opts.Limit
	if attempts > len(candidates) {
		attempts = len(candidates)
	}
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		candidate := candidates[(start+i)%len(candidates)]
		// Advance before attempting. A permanently blocked candidate therefore
		// cannot starve later pending work across passes.
		report.Next = candidate.token
		report.Attempted++
		removed, warning := m.retryCleanupPendingOne(ctx, candidate)
		if warning != nil {
			report.Warnings = append(report.Warnings, ReapWarning{Path: candidate.markerPath, Err: warning})
			continue
		}
		if removed != nil {
			report.Removed = append(report.Removed, *removed)
		}
	}
	return report, nil
}

func (m *Manager) cleanupRetryCandidates(ctx context.Context, warningLimit int) ([]cleanupRetryCandidate, []ReapWarning, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	repos, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("worktree: list root %s for cleanup retry: %w", m.Root, err)
	}
	var candidates []cleanupRetryCandidate
	var warnings []ReapWarning
	suppressedWarnings := 0
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		key := repo.Name()
		markersDir := m.markersDirForKey(key)
		entries, err := os.ReadDir(markersDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return candidates, warnings, fmt.Errorf("worktree: list markers for cleanup retry %s: %w", key, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return candidates, warnings, err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			markerPath := filepath.Join(markersDir, entry.Name())
			mk, err := readMarker(markerPath)
			if err != nil {
				if len(warnings) < warningLimit {
					warnings = append(warnings, ReapWarning{Path: markerPath, Err: err})
				} else {
					suppressedWarnings++
				}
				continue
			}
			if mk.Status != statusCleanupPending {
				continue
			}
			candidates = append(candidates, cleanupRetryCandidate{
				token: key + "/" + entry.Name(), key: key, markerPath: markerPath,
			})
		}
	}
	if suppressedWarnings > 0 {
		warnings = append(warnings, ReapWarning{
			Path: m.Root,
			Err:  fmt.Errorf("worktree: %d additional unreadable cleanup retry markers suppressed", suppressedWarnings),
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].token < candidates[j].token })
	return candidates, warnings, nil
}

func (m *Manager) retryCleanupPendingOne(ctx context.Context, candidate cleanupRetryCandidate) (*ReapResult, error) {
	lock := m.lockFor(candidate.key)
	lock.Lock()
	defer lock.Unlock()

	primary, err := readMarker(candidate.markerPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worktree: re-read pending marker: %w", err)
	}
	if primary.Status != statusCleanupPending {
		return nil, nil
	}
	if !validCleanupRetryIdentity(primary, candidate.key) ||
		filepath.Base(candidate.markerPath) != primary.RunID+".json" {
		return nil, fmt.Errorf("worktree: pending marker has invalid durable identity")
	}
	directory, err := primary.directoryName()
	if err != nil {
		return nil, err
	}
	ownershipPath := m.ownershipPath(candidate.key, directory)
	ownership, err := readMarker(ownershipPath)
	if err != nil {
		return nil, fmt.Errorf("worktree: read pending ownership record: %w", err)
	}
	if ownership.Status != statusCleanupPending || !sameWorkspaceIdentity(primary, ownership) {
		return nil, fmt.Errorf("worktree: pending ownership records disagree")
	}

	path := filepath.Join(m.runsDirForKey(candidate.key), directory)
	worktreeBytes, measured, measurementErr := m.measureWorktree(path)
	defer m.observeUsage(ctx, UsageOperationHousekeeping, primary.OwnerRunID, primary.RunID, worktreeBytes, measured, measurementErr)
	if err := m.reapOneLocked(ctx, candidate.key, path, candidate.markerPath, &primary); err != nil {
		return nil, fmt.Errorf("worktree: retry pending run %s: %w", primary.RunID, err)
	}
	return &ReapResult{RunID: primary.RunID, Path: path, Reason: ReapReasonCleanupPending}, nil
}

// validCleanupRetryIdentity rejects legacy or synthetic marker shapes before
// they can authorize prompt destruction. Retry only trusts the full modern
// identity Create writes: safe run identities, the canonical SHA-256 repo
// digest and derived key, a hashed directory, and nonzero creation/process
// provenance. Optional fields (writer identity and PID start time) remain
// optional because their Manager/OS sources are explicitly best-effort.
func validCleanupRetryIdentity(m marker, key string) bool {
	if !validRunID(m.RunID) || !validRunID(m.OwnerRunID) || m.Directory == "" ||
		m.Directory != worktreeDirectoryName(m.RunID) || m.BaseRef == "" ||
		m.CreatedAt.IsZero() || m.PID <= 0 || len(m.RepositoryDigest) != 64 ||
		len(key) != 16 || m.RepositoryDigest[:16] != key {
		return false
	}
	digest, err := hex.DecodeString(m.RepositoryDigest)
	return err == nil && len(digest) == 32 && hex.EncodeToString(digest) == m.RepositoryDigest
}
