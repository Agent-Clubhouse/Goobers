package worktree

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupRetryOptions bounds one prompt retry pass. After is an opaque cursor
// returned by the preceding pass; Limit counts attempted pending cleanups, not
// successful removals.
type CleanupRetryOptions struct {
	After string
	Limit int
	// PassBudget bounds the WHOLE pass — discovery and attempts together —
	// rather than any single operation within it (#5264). Individual cleanup
	// subprocesses are already bounded (#92dc5e4b7a), but that does not bound
	// a pass across a large retained population, which is what could delay
	// startup and preparation indefinitely. Zero means unbounded, preserving
	// the previous behavior for any caller that does not opt in.
	PassBudget time.Duration
	// DiscoveryLimit caps how many durable records one pass READS while looking
	// for candidates. Zero derives a default from Limit.
	DiscoveryLimit int
	// Clock is a test seam for PassBudget. Nil means time.Now.
	Clock func() time.Time
}

// CleanupRetryReport describes one bounded pass over the durable
// cleanup-pending queue.
type CleanupRetryReport struct {
	Next      string
	Attempted int
	Removed   []ReapResult
	Warnings  []ReapWarning
	// Examined counts durable records this pass read while discovering
	// candidates — the progress report #5264 asks for, so an operator can see a
	// pass doing work even when it removed nothing.
	Examined int
	// Elapsed is the whole pass's measured duration.
	Elapsed time.Duration
	// Deferred reports that the pass stopped before exhausting its attempt
	// limit, and DeferralReason says why. A deferred pass has NOT completed the
	// queue: reporting that honestly is the point, because presenting a
	// truncated pass as a finished cleanup is what would let a large retained
	// population look healthy while never draining.
	Deferred       bool
	DeferralReason CleanupDeferralReason
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
	budget := newPassBudget(opts.Clock, opts.PassBudget)
	discovery, err := m.cleanupRetryCandidates(ctx, opts, budget)
	report := CleanupRetryReport{
		Warnings: discovery.warnings,
		Examined: discovery.examined,
		Elapsed:  budget.elapsed(),
	}
	if err != nil || len(discovery.candidates) == 0 {
		m.noteCleanupDeferral(&report, discovery.reason, budget)
		return report, err
	}

	attempts := min(opts.Limit, len(discovery.candidates))
	reason := discovery.reason
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			report.Elapsed = budget.elapsed()
			m.noteCleanupDeferral(&report, reason, budget)
			return report, err
		}
		// The whole-pass budget is checked BEFORE starting each attempt, never
		// mid-attempt: a cleanup that has begun holds the repository lock and
		// has durable records mid-transition, so abandoning it partway is how a
		// record pair would be left disagreeing. Bounding the pass means
		// declining to start more work, not interrupting work in flight.
		if budget.exhausted() {
			// Keep the EARLIEST reason. If discovery already deferred, that is
			// the root cause worth reporting — the pass never saw all the
			// pending work — and relabeling it here would hide that behind the
			// downstream symptom.
			if reason == CleanupDeferralNone {
				reason = CleanupDeferralBudgetAttempts
			}
			break
		}
		candidate := discovery.candidates[i]
		// Advance before attempting. A permanently blocked candidate therefore
		// cannot starve later pending work across passes.
		report.Next = candidate.token
		report.Attempted++
		removed, warning := m.retryCleanupPendingOne(ctx, candidate)
		if warning != nil {
			report.Warnings = append(report.Warnings, ReapWarning{
				Path: candidate.markerPath, Err: warning, Class: classifyCleanupWarning(warning),
			})
			continue
		}
		if removed != nil {
			report.Removed = append(report.Removed, *removed)
		}
	}
	report.Elapsed = budget.elapsed()
	m.noteCleanupDeferral(&report, reason, budget)
	return report, nil
}

// noteCleanupDeferral records why a pass stopped short, as a report field and as
// a warning so a caller that only aggregates warnings still surfaces it.
func (m *Manager) noteCleanupDeferral(report *CleanupRetryReport, reason CleanupDeferralReason, budget *passBudget) {
	if reason == CleanupDeferralNone {
		return
	}
	report.Deferred = true
	report.DeferralReason = reason
	report.Warnings = append(report.Warnings, ReapWarning{
		Path: m.Root,
		Err:  errors.New(deferralMessage(reason, report.Examined, report.Attempted, budget.elapsed())),
	})
}

// cleanupRetryCandidates discovers pending candidates under a bound.
//
// Before #5264 this read EVERY durable record under Root on every pass and only
// then applied opts.Limit to the attempts. With a large retained population
// (#5214: 1,590 terminal worktrees) that made the scan itself the cost, on a
// path that startup and stale-preparation both wait behind — so the pass could
// not be bounded by bounding attempts alone.
//
// The scan is now ordered and cursor-resuming, which is what makes truncating
// it safe. Records are enumerated in token order (os.ReadDir sorts, and the
// token is repoKey + "/" + markerFile, so the whole walk ascends), tokens at or
// before opts.After are skipped WITHOUT being read, and the scan stops once it
// has enough forward work to fill the pass. A truncated scan therefore still
// makes forward progress rather than re-reading the same prefix every pass.
//
// Fairness is preserved by wrapping: if the forward scan reaches the end of the
// tree without filling the pass, a second phase collects the records at or
// before the cursor. Without that, a cursor parked near the end of the
// population would leave the earlier records unattempted indefinitely.
//
// What is bounded is durable-record READS, which is the I/O this exists to cap.
// Directory entries are still enumerated to locate those records; bounding that
// too would need a separate index of pending records, which is not in scope
// here. Stating the distinction is deliberate — a bound that is described as
// total but is not would be the more expensive kind of wrong.
func (m *Manager) cleanupRetryCandidates(ctx context.Context, opts CleanupRetryOptions, budget *passBudget) (cleanupDiscovery, error) {
	var discovery cleanupDiscovery
	if err := ctx.Err(); err != nil {
		return discovery, err
	}
	limit := discoveryLimitFor(opts)
	repos, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return discovery, nil
		}
		return discovery, fmt.Errorf("worktree: list root %s for cleanup retry: %w", m.Root, err)
	}

	// Phase 1 takes the records strictly after the cursor; phase 2 wraps to the
	// ones at or before it, and only runs if phase 1 completed without
	// truncating. Running phase 2 after a truncated phase 1 would re-read the
	// prefix the cursor exists to skip.
	forward, err := m.scanCleanupMarkers(ctx, &discovery, repos, opts.After, true, limit, budget)
	if err != nil {
		return discovery, err
	}
	if forward && len(discovery.candidates) < limit && opts.After != "" {
		if _, err := m.scanCleanupMarkers(ctx, &discovery, repos, opts.After, false, limit, budget); err != nil {
			return discovery, err
		}
	}
	return discovery, nil
}

// scanCleanupMarkers walks the marker tree once, collecting pending candidates
// on one side of the cursor. It reports whether it finished the walk (true) as
// opposed to stopping on a bound, so the caller can decide whether wrapping is
// safe.
func (m *Manager) scanCleanupMarkers(
	ctx context.Context,
	discovery *cleanupDiscovery,
	repos []os.DirEntry,
	after string,
	forward bool,
	limit int,
	budget *passBudget,
) (bool, error) {
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
			return false, fmt.Errorf("worktree: list markers for cleanup retry %s: %w", key, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			token := key + "/" + entry.Name()
			// Side selection happens before the read: skipping is the cheap
			// operation, and doing it here is what keeps a resumed pass from
			// paying the cost of the prefix it already processed.
			if forward != (token > after) {
				continue
			}
			if len(discovery.candidates) >= limit {
				discovery.reason = CleanupDeferralDiscoveryLimit
				return false, nil
			}
			if budget.exhausted() {
				discovery.reason = CleanupDeferralBudgetDiscovery
				return false, nil
			}
			markerPath := filepath.Join(markersDir, entry.Name())
			discovery.examined++
			mk, err := readMarker(markerPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				discovery.addWarning(limit, markerPath, fmt.Errorf("%w: %w", errCleanupRecordInvalid, err))
				continue
			}
			if mk.Status != statusCleanupPending {
				continue
			}
			discovery.candidates = append(discovery.candidates, cleanupRetryCandidate{
				token: token, key: key, markerPath: markerPath,
			})
		}
	}
	return true, nil
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
	if ownership.Status == statusCleanupRetained && sameWorkspaceIdentity(primary, ownership) &&
		ownership.CleanupDisposition != "" {
		primary.Status = statusCleanupRetained
		primary.RetainedAt = ownership.RetainedAt
		primary.CleanupDisposition = ownership.CleanupDisposition
		if err := writeMarker(candidate.markerPath, primary); err != nil {
			return nil, fmt.Errorf("worktree: repair cleanup retention marker: %w", err)
		}
		return nil, fmt.Errorf("%w: %s", ErrCleanupRetained, ownership.CleanupDisposition)
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
		m.Directory != worktreeDirectoryName(m.RunID) ||
		m.CreatedAt.IsZero() || m.PID <= 0 || len(m.RepositoryDigest) != 64 ||
		len(key) != 16 || m.RepositoryDigest[:16] != key {
		return false
	}
	digest, err := hex.DecodeString(m.RepositoryDigest)
	return err == nil && len(digest) == 32 && hex.EncodeToString(digest) == m.RepositoryDigest
}
