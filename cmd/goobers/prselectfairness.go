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

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/journal"
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
	CrownedLander     bool
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
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		return prSelectEligibilityObservation{}, fmt.Errorf("open claim ledger: %w", err)
	}
	var observation prSelectEligibilityObservation
	err = ledger.Locked(claimContext(), "pr-select.fairness-observe", func(tx claimsclient.Ledger) error {
		state, err := readPRSelectFairnessFile(path)
		if err != nil {
			return err
		}
		scope := prSelectFairnessScope(repo)
		gaggle := providerGaggle()
		currentRunID := os.Getenv("GOOBERS_RUN_ID")
		claims, err := pullRequestClaimListing(tx, gaggle, repo.Provider)
		if err != nil {
			return err
		}
		held, err := tx.ForRunAll(claimContext(), currentRunID)
		if err != nil {
			return fmt.Errorf("read this run's claims: %w", err)
		}
		observation.CurrentRunHasLiveClaim = currentRunHasLivePullRequestClaim(
			held, gaggle, repo.Provider, currentRunID, now,
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
				claims, gaggle, repo.Provider, pr.Number, currentRunID, now,
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

// pullRequestClaimListing is the one namespace read the PR claim-status
// filters consult: the gaggle's own repository-provider namespace (its
// legacy unscoped entries included, which the ledger holds exclusive against
// every scoped claimant), or the legacy namespace alone when the stage runs
// ungaggled. provider must be the claiming PR's own repository provider
// (#3649) — a hardcoded provider would silently narrow the listing to a
// different provider's namespace and hide another provider's live claims.
func pullRequestClaimListing(ledger claimsclient.Ledger, gaggle string, provider providers.ProviderKind) (claimsclient.Listing, error) {
	providerNamespace := ""
	if gaggle != "" {
		providerNamespace = string(provider)
	}
	claims, err := ledger.ListNamespace(claimContext(), gaggle, providerNamespace)
	if err != nil {
		return claimsclient.Listing{}, fmt.Errorf("read PR claims: %w", err)
	}
	return claims, nil
}

// currentRunHasLivePullRequestClaim reports whether held — the current
// run's claims (Ledger.ForRunAll) — carries a live PR lease in gaggle's
// namespace.
func currentRunHasLivePullRequestClaim(
	held []claimsclient.Entry,
	gaggle string,
	provider providers.ProviderKind,
	currentRunID string,
	now time.Time,
) bool {
	if currentRunID == "" {
		return false
	}
	for _, entry := range held {
		if !entry.ExpiresAt.After(now) || !strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
			continue
		}
		if entry.Gaggle == "" || (entry.Gaggle == gaggle && entry.Provider == string(provider)) {
			return true
		}
	}
	return false
}

func pullRequestClaimStatus(
	claims claimsclient.Listing,
	gaggle string,
	provider providers.ProviderKind,
	number int,
	currentRunID string,
	now time.Time,
) (claimed, ownedByCurrentRun bool) {
	var (
		entry claimsclient.Entry
		ok    bool
	)
	if gaggle == "" {
		entry, ok = claims.Lookup(claimsclient.Key{ExternalID: pullRequestClaimKey(number)})
	} else {
		if legacy, held := claims.Lookup(claimsclient.Key{ExternalID: pullRequestClaimKey(number)}); held && legacy.ExpiresAt.After(now) {
			return true, false
		}
		entry, ok = claims.Lookup(pullRequestClaimLedgerKey(gaggle, provider, number))
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
			CrownedLander:     blockedDependents[pr.Number] > 0,
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
		if left.CrownedLander != right.CrownedLander {
			return left.CrownedLander
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
