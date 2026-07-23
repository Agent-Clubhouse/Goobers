package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

const (
	prSelectFairnessFileName = "pr-select-fairness.json"
	prSelectAgingInterval    = 15 * time.Minute
	prSelectStarvationLimit  = time.Hour
)

type prSelectFairnessFile struct {
	Candidates []prSelectFairnessEntry `json:"candidates"`
}

type prSelectFairnessEntry struct {
	Gaggle        string    `json:"gaggle,omitempty"`
	Repository    string    `json:"repository"`
	Number        int       `json:"number"`
	HeadSHA       string    `json:"headSha"`
	EligibleSince time.Time `json:"eligibleSince"`
	LastObserved  time.Time `json:"lastObserved"`
}

type prSelectPriority struct {
	EligibleSince     time.Time
	Wait              time.Duration
	AgingBoost        int64
	EffectivePriority int64
	StarvationGuarded bool
}

type prSelectFairnessMetrics struct {
	MaxWait time.Duration
	Starved []int
}

type prSelectSnapshotCompleteness bool

const (
	prSelectPartialSnapshot  prSelectSnapshotCompleteness = false
	prSelectCompleteSnapshot prSelectSnapshotCompleteness = true
)

type prSelectEligibilityObservation struct {
	UnclaimedEligible       []providers.PullRequestSummary
	CurrentRunClaimEligible []providers.PullRequestSummary
	EligibleSince           map[int]time.Time
	CurrentRunHasLiveClaim  bool
}

func prSelectSnapshotCompletenessFromTriggerRef(triggerRef string) prSelectSnapshotCompleteness {
	if _, targeted := webhookhttp.PullNumberFromTriggerRef(triggerRef); targeted {
		return prSelectPartialSnapshot
	}
	return prSelectCompleteSnapshot
}

func prSelectSnapshotCompletenessForRun(
	root string,
	repo providers.RepositoryRef,
	triggerRef string,
	now time.Time,
) (prSelectSnapshotCompleteness, error) {
	completeness := prSelectSnapshotCompletenessFromTriggerRef(triggerRef)
	if completeness == prSelectCompleteSnapshot {
		return completeness, nil
	}
	state, err := readPRSelectFairnessFile(
		filepath.Join(layoutFor(root).SchedulerDir(), prSelectFairnessFileName),
	)
	if err != nil {
		return completeness, err
	}
	scope := prSelectFairnessScope(repo)
	gaggle := providerGaggle()
	guardCutoff := now.Add(-prSelectStarvationLimit)
	for _, entry := range state.Candidates {
		if entry.Gaggle == gaggle &&
			entry.Repository == scope &&
			!entry.EligibleSince.After(guardCutoff) {
			return prSelectCompleteSnapshot, nil
		}
	}
	return completeness, nil
}

func observePRSelectEligibility(
	root string,
	repo providers.RepositoryRef,
	observed []providers.PullRequestSummary,
	eligible []providers.PullRequestSummary,
	completeness prSelectSnapshotCompleteness,
	now time.Time,
) (prSelectEligibilityObservation, error) {
	l := layoutFor(root)
	path := filepath.Join(l.SchedulerDir(), prSelectFairnessFileName)
	var observation prSelectEligibilityObservation
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), "pr-select.fairness-observe", func() error {
		state, err := readPRSelectFairnessFile(path)
		if err != nil {
			return err
		}
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		scope := prSelectFairnessScope(repo)
		gaggle := providerGaggle()
		currentRunID := os.Getenv("GOOBERS_RUN_ID")
		observation.CurrentRunHasLiveClaim = currentRunHasLivePullRequestClaim(
			ledger, gaggle, currentRunID, now,
		)
		observedNumbers := make(map[int]bool, len(observed))
		for _, pr := range observed {
			observedNumbers[pr.Number] = true
		}
		for _, pr := range eligible {
			if !observedNumbers[pr.Number] {
				return fmt.Errorf("eligible PR #%d is missing from the observed snapshot", pr.Number)
			}
		}
		existing := make(map[int]prSelectFairnessEntry)
		kept := make([]prSelectFairnessEntry, 0, len(state.Candidates)+len(eligible))
		for _, entry := range state.Candidates {
			sameScope := entry.Gaggle == gaggle && entry.Repository == scope
			if sameScope && (completeness == prSelectCompleteSnapshot || observedNumbers[entry.Number]) {
				existing[entry.Number] = entry
				continue
			}
			kept = append(kept, entry)
		}

		observation.EligibleSince = make(map[int]time.Time, len(eligible))
		for _, pr := range eligible {
			claimed, ownedByCurrentRun := pullRequestClaimStatus(
				ledger, gaggle, pr.Number, currentRunID, now,
			)
			if claimed {
				if ownedByCurrentRun {
					observation.CurrentRunClaimEligible = append(observation.CurrentRunClaimEligible, pr)
				}
				continue
			}
			since := now
			entry, ok := existing[pr.Number]
			if ok && entry.HeadSHA == pr.HeadSHA && !entry.EligibleSince.After(now) {
				since = entry.EligibleSince
			}
			observation.UnclaimedEligible = append(observation.UnclaimedEligible, pr)
			observation.EligibleSince[pr.Number] = since
			kept = append(kept, prSelectFairnessEntry{
				Gaggle:        gaggle,
				Repository:    scope,
				Number:        pr.Number,
				HeadSHA:       pr.HeadSHA,
				EligibleSince: since,
				LastObserved:  now,
			})
		}
		state.Candidates = kept
		return writePRSelectFairnessFile(path, state)
	})
	return observation, err
}

func currentRunHasLivePullRequestClaim(
	ledger *localscheduler.ClaimLedger,
	gaggle string,
	currentRunID string,
	now time.Time,
) bool {
	if currentRunID == "" {
		return false
	}
	for _, entry := range ledger.ForRunAll(currentRunID) {
		if !entry.ExpiresAt.After(now) || !strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
			continue
		}
		if entry.Gaggle == "" || (entry.Gaggle == gaggle && entry.Provider == string(providers.ProviderGitHub)) {
			return true
		}
	}
	return false
}

func pullRequestClaimStatus(
	ledger *localscheduler.ClaimLedger,
	gaggle string,
	number int,
	currentRunID string,
	now time.Time,
) (claimed, ownedByCurrentRun bool) {
	var (
		entry localscheduler.ClaimEntry
		ok    bool
	)
	if gaggle == "" {
		entry, ok = ledger.Lookup(pullRequestClaimKey(number))
	} else {
		if legacy, held := ledger.Lookup(pullRequestClaimKey(number)); held && legacy.ExpiresAt.After(now) {
			return true, false
		}
		entry, ok = ledger.LookupScoped(localscheduler.ClaimKey{
			Gaggle:     gaggle,
			Provider:   string(providers.ProviderGitHub),
			ExternalID: pullRequestClaimKey(number),
		})
	}
	if !ok || !entry.ExpiresAt.After(now) {
		return false, false
	}
	return true, currentRunID != "" && entry.RunID == currentRunID
}

func clearPRSelectEligibilityWait(
	root string,
	repo providers.RepositoryRef,
	selected providers.PullRequestSummary,
) error {
	l := layoutFor(root)
	path := filepath.Join(l.SchedulerDir(), prSelectFairnessFileName)
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), "pr-select.fairness-clear", func() error {
		state, err := readPRSelectFairnessFile(path)
		if err != nil {
			return err
		}
		scope := prSelectFairnessScope(repo)
		gaggle := providerGaggle()
		kept := state.Candidates[:0]
		removed := false
		for _, entry := range state.Candidates {
			if entry.Gaggle == gaggle && entry.Repository == scope && entry.Number == selected.Number {
				removed = true
				continue
			}
			kept = append(kept, entry)
		}
		if !removed {
			return nil
		}
		state.Candidates = kept
		return writePRSelectFairnessFile(path, state)
	})
}

func rankEligiblePullRequests(
	eligible []providers.PullRequestSummary,
	blockedDependents map[int]int,
	eligibleSince map[int]time.Time,
	now time.Time,
) ([]providers.PullRequestSummary, map[int]prSelectPriority, prSelectFairnessMetrics) {
	ranked := append([]providers.PullRequestSummary(nil), eligible...)
	priorities := make(map[int]prSelectPriority, len(ranked))
	var metrics prSelectFairnessMetrics
	for _, pr := range ranked {
		since := eligibleSince[pr.Number]
		if since.IsZero() || since.After(now) {
			since = now
		}
		wait := now.Sub(since)
		agingBoost := int64(wait / prSelectAgingInterval)
		priority := prSelectPriority{
			EligibleSince:     since,
			Wait:              wait,
			AgingBoost:        agingBoost,
			EffectivePriority: int64(blockedDependents[pr.Number]) + agingBoost,
			StarvationGuarded: wait >= prSelectStarvationLimit,
		}
		priorities[pr.Number] = priority
		if wait > metrics.MaxWait {
			metrics.MaxWait = wait
		}
		if wait > prSelectStarvationLimit {
			metrics.Starved = append(metrics.Starved, pr.Number)
		}
	}
	sort.Ints(metrics.Starved)
	sort.Slice(ranked, func(i, j int) bool {
		left, right := priorities[ranked[i].Number], priorities[ranked[j].Number]
		if left.StarvationGuarded != right.StarvationGuarded {
			return left.StarvationGuarded
		}
		if left.StarvationGuarded && !left.EligibleSince.Equal(right.EligibleSince) {
			return left.EligibleSince.Before(right.EligibleSince)
		}
		if left.EffectivePriority != right.EffectivePriority {
			return left.EffectivePriority > right.EffectivePriority
		}
		return ranked[i].Number < ranked[j].Number
	})
	return ranked, priorities, metrics
}

func readPRSelectFairnessFile(path string) (prSelectFairnessFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return prSelectFairnessFile{}, nil
	}
	if err != nil {
		return prSelectFairnessFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var state prSelectFairnessFile
	if err := json.Unmarshal(data, &state); err != nil {
		return prSelectFairnessFile{}, fmt.Errorf("decode %s: %w", path, err)
	}
	seen := make(map[string]bool, len(state.Candidates))
	for _, entry := range state.Candidates {
		if entry.Repository == "" || entry.Number <= 0 || entry.EligibleSince.IsZero() || entry.LastObserved.IsZero() {
			return prSelectFairnessFile{}, fmt.Errorf("decode %s: invalid fairness entry for PR #%d", path, entry.Number)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", entry.Gaggle, entry.Repository, entry.Number)
		if seen[key] {
			return prSelectFairnessFile{}, fmt.Errorf("decode %s: duplicate fairness entry for PR #%d", path, entry.Number)
		}
		seen[key] = true
	}
	return state, nil
}

func writePRSelectFairnessFile(path string, state prSelectFairnessFile) error {
	sort.Slice(state.Candidates, func(i, j int) bool {
		left, right := state.Candidates[i], state.Candidates[j]
		if left.Gaggle != right.Gaggle {
			return left.Gaggle < right.Gaggle
		}
		if left.Repository != right.Repository {
			return left.Repository < right.Repository
		}
		return left.Number < right.Number
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create PR fairness state directory: %w", err)
	}
	if err := journal.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("persist %s: %w", path, err)
	}
	return nil
}

func prSelectFairnessScope(repo providers.RepositoryRef) string {
	return repo.Owner + "/" + repo.Name
}

func joinPRNumbers(numbers []int) string {
	values := make([]string, len(numbers))
	for i, number := range numbers {
		values[i] = strconv.Itoa(number)
	}
	return strings.Join(values, ",")
}

func noneIfEmpty(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
