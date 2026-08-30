package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

// pr-select's FAIRNESS LEASE (#1336): the first-eligible timestamp per
// candidate PR that the 15-minute aging boost and the one-hour starvation
// guard are computed from. It is what stops a single PR monopolising the one
// pr-select slot.
//
// Since Goobers#3988 it is reached through the scheduler-state seam
// (openStageStateStore / openHeldStageStateStore) rather than through
// os.ReadFile + withClaimLock: the same file, in the same place, under the
// same claims.lock locally, and over the daemon's C2 plane from a stage pod —
// which is what lets `pr-select` run in a pod at all.
//
// Losing the lease in a pod does not degrade gracefully, which is why the fix
// is plane admission rather than tolerating the miss. A pod is never stamped
// GOOBERS_INSTANCE_ROOT, so the file would resolve under "." and vanish with
// the container: every run would read a FRESH lease, every candidate's wait
// would reset to zero, the aging boost would never accumulate and the
// starvation guard would never fire — silently, because a fresh lease is
// indistinguishable from a legitimately new one.
const (
	prSelectFairnessFileName = stateclient.KeyPRSelectFairness
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
	store, err := openPRSelectFairnessStore(layoutFor(root))
	if err != nil {
		return completeness, err
	}
	value, err := store.Get(stateContext(), stateclient.KeyPRSelectFairness)
	if err != nil {
		return completeness, err
	}
	state, err := decodePRSelectFairness(value)
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

// observePRSelectEligibility advances the fairness lease for this run's
// snapshot and reports what selection may rank.
//
// The lease's read-modify-write runs INSIDE the claim ledger's locked section
// and must stay there: a candidate's wait and the claim that ends it are one
// atomic step, so a concurrent run cannot observe a PR as unclaimed-and-aging
// while another run is in the middle of claiming it. That is why the store is
// the HELD one — locally the claims lock is already ours (a second flock on
// claims.lock would wait on itself until the timeout), and on the plane
// Locked is a pass-through because the daemon takes the very same lock on the
// caller's behalf for each round trip.
func observePRSelectEligibility(
	root string,
	repo providers.RepositoryRef,
	observed []providers.PullRequestSummary,
	eligible []providers.PullRequestSummary,
	completeness prSelectSnapshotCompleteness,
	now time.Time,
) (prSelectEligibilityObservation, error) {
	l := layoutFor(root)
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		return prSelectEligibilityObservation{}, fmt.Errorf("open claim ledger: %w", err)
	}
	state, err := openHeldPRSelectFairnessStore(l)
	if err != nil {
		return prSelectEligibilityObservation{}, fmt.Errorf("open scheduler state: %w", err)
	}
	var observation prSelectEligibilityObservation
	err = ledger.Locked(claimContext(), claimLockOperationPRSelectFairnessObserve, func(tx claimsclient.Ledger) error {
		scope := prSelectFairnessScope(repo)
		gaggle := providerGaggle()
		currentRunID := os.Getenv(executor.RunIDEnvVar)
		claims, err := pullRequestClaimListing(tx, gaggle, repo.Provider)
		if err != nil {
			return err
		}
		held, err := tx.ForRunAll(claimContext(), currentRunID)
		if err != nil {
			return fmt.Errorf("read this run's claims: %w", err)
		}
		hasLiveClaim := currentRunHasLivePullRequestClaim(
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

		// fn is re-run against the winner's value when the plane refuses a
		// lost compare-and-swap, so every accumulator it fills is reset here
		// rather than appended to across attempts — an observation assembled
		// from two different views of the lease would report a wait no stored
		// entry ever had.
		return state.Update(stateContext(), stateclient.KeyPRSelectFairness, claimLockOperationPRSelectFairnessObserve,
			func(value stateclient.Value) ([]byte, bool, error) {
				stored, decodeErr := decodePRSelectFairness(value)
				if decodeErr != nil {
					return nil, false, decodeErr
				}
				observation = prSelectEligibilityObservation{CurrentRunHasLiveClaim: hasLiveClaim}
				existing := make(map[int]prSelectFairnessEntry)
				kept := make([]prSelectFairnessEntry, 0, len(stored.Candidates)+len(eligible))
				for _, entry := range stored.Candidates {
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
				stored.Candidates = kept
				data, encodeErr := encodePRSelectFairness(stored)
				return data, true, encodeErr
			})
	})
	if err != nil {
		return prSelectEligibilityObservation{}, err
	}
	return observation, nil
}

// pullRequestClaimListing is the one namespace read the PR claim-status
// filters consult: the gaggle's own repository-provider namespace (its
// legacy unscoped entries included, which the ledger holds exclusive against
// every scoped claimant), or the legacy namespace alone when the stage runs
// ungaggled. provider must be the claiming PR's own repository provider
// (#3649) — a hardcoded provider would silently narrow the listing to a
// different provider's namespace and hide another provider's live claims.
//
// For a non-GitHub provider this ALSO reads the gaggle's github namespace
// (#3649 follow-up): every build before this change wrote every
// gaggle-scoped PR claim — ADO and Gitea repositories included — under a
// hardcoded ProviderGitHub key. A claim leased by one of those builds still
// lives under that key until it naturally expires, so a purely
// provider-scoped read would make it invisible and let the first
// post-upgrade selection claim and process the same PR concurrently.
// Entries already covered by the primary read (the gaggle's own legacy
// unscoped claims) are excluded from this second read to avoid double
// counting them.
func pullRequestClaimListing(ledger claimsclient.Ledger, gaggle string, provider providers.ProviderKind) (claimsclient.Listing, error) {
	providerNamespace := ""
	if gaggle != "" {
		providerNamespace = string(provider)
	}
	claims, err := ledger.ListNamespace(claimContext(), gaggle, providerNamespace)
	if err != nil {
		return claimsclient.Listing{}, fmt.Errorf("read PR claims: %w", err)
	}
	if gaggle != "" && provider != providers.ProviderGitHub {
		legacy, err := ledger.ListNamespace(claimContext(), gaggle, string(providers.ProviderGitHub))
		if err != nil {
			return claimsclient.Listing{}, fmt.Errorf("read legacy github-scoped PR claims: %w", err)
		}
		for _, entry := range legacy.Entries {
			if entry.Gaggle == gaggle && entry.Provider == string(providers.ProviderGitHub) {
				claims.Entries = append(claims.Entries, entry)
			}
		}
		for _, entry := range legacy.History {
			if entry.Gaggle == gaggle && entry.Provider == string(providers.ProviderGitHub) {
				claims.History = append(claims.History, entry)
			}
		}
	}
	return claims, nil
}

// currentRunHasLivePullRequestClaim reports whether held — the current
// run's claims (Ledger.ForRunAll) — carries a live PR lease in gaggle's
// namespace. For a non-GitHub provider this also recognizes a lease held
// under the legacy hardcoded-github namespace (#3649 follow-up): ForRunAll
// already returns every claim the run holds regardless of provider scope, so
// a pre-migration ADO/Gitea claim is present here — it just needs the same
// legacy-namespace fallback pullRequestClaimStatus applies, or a run that
// leased a PR before this build's rollout would stop recognizing it as its
// own.
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
		if entry.Gaggle == "" {
			return true
		}
		if entry.Gaggle != gaggle {
			continue
		}
		if entry.Provider == string(provider) {
			return true
		}
		if provider != providers.ProviderGitHub && entry.Provider == string(providers.ProviderGitHub) {
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
		if !ok && provider != providers.ProviderGitHub {
			// Pre-#3649 builds wrote every gaggle-scoped PR claim under a
			// hardcoded ProviderGitHub key regardless of the repository's
			// real provider. A non-GitHub claim leased before the upgrade
			// still lives under that key until it naturally expires — fall
			// back to it here (pullRequestClaimListing already fetched it
			// into claims) so it stays visible instead of letting the first
			// post-upgrade selection claim and process the same PR
			// concurrently.
			entry, ok = claims.Lookup(pullRequestClaimLedgerKey(gaggle, providers.ProviderGitHub, number))
		}
	}
	if !ok || !entry.ExpiresAt.After(now) {
		return false, false
	}
	return true, currentRunID != "" && entry.RunID == currentRunID
}

// clearPRSelectEligibilityWait retires the selected PR's lease entry, so its
// wait restarts from zero the next time it becomes eligible rather than
// carrying an aging boost it has already been paid for.
//
// This one is NOT inside the claim transaction — it follows a successful
// selection — so it takes the lease's own lock through the ordinary stage
// seam. That lock is claims.lock, exactly the flock the pre-plane
// withClaimLock call took, so the mutual exclusion with a concurrent
// observation is unchanged.
func clearPRSelectEligibilityWait(
	root string,
	repo providers.RepositoryRef,
	selected providers.PullRequestSummary,
) error {
	store, err := openPRSelectFairnessStore(layoutFor(root))
	if err != nil {
		return err
	}
	scope := prSelectFairnessScope(repo)
	gaggle := providerGaggle()
	return store.Update(stateContext(), stateclient.KeyPRSelectFairness, claimLockOperationPRSelectFairnessClear,
		func(value stateclient.Value) ([]byte, bool, error) {
			// Recomputed from the value observed on THIS attempt, so a lost
			// compare-and-swap retires the entry from the winner's lease
			// rather than reinstating entries the winner had already dropped.
			state, decodeErr := decodePRSelectFairness(value)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
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
				return nil, false, nil
			}
			state.Candidates = kept
			data, encodeErr := encodePRSelectFairness(state)
			return data, true, encodeErr
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

// decodePRSelectFairness parses one scheduler-state value into the lease. An
// ABSENT key is the empty lease, exactly as the pre-plane loader treated a
// missing file — the normal first-run state.
//
// Every other malformation is an ERROR, not an empty lease, and deliberately
// so: the lease is a correctness input, and silently treating a corrupt one
// as "nobody has been waiting" is precisely the fresh-lease failure this key
// was admitted to the plane to prevent.
func decodePRSelectFairness(value stateclient.Value) (prSelectFairnessFile, error) {
	if !value.Exists() {
		return prSelectFairnessFile{}, nil
	}
	var state prSelectFairnessFile
	if err := json.Unmarshal(value.Data, &state); err != nil {
		return prSelectFairnessFile{}, fmt.Errorf("decode %s: %w", prSelectFairnessFileName, err)
	}
	seen := make(map[string]bool, len(state.Candidates))
	for _, entry := range state.Candidates {
		if entry.Repository == "" || entry.Number <= 0 || entry.EligibleSince.IsZero() || entry.LastObserved.IsZero() {
			return prSelectFairnessFile{}, fmt.Errorf("decode %s: invalid fairness entry for PR #%d", prSelectFairnessFileName, entry.Number)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", entry.Gaggle, entry.Repository, entry.Number)
		if seen[key] {
			return prSelectFairnessFile{}, fmt.Errorf("decode %s: duplicate fairness entry for PR #%d", prSelectFairnessFileName, entry.Number)
		}
		seen[key] = true
	}
	return state, nil
}

// encodePRSelectFairness renders the lease exactly as the pre-plane writer
// did — same candidate ordering, same indentation, same trailing newline — so
// a type-1/type-2 instance's file keeps the bytes it had and the two backends
// hash to the same ETag for the same lease.
func encodePRSelectFairness(state prSelectFairnessFile) ([]byte, error) {
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
		return nil, fmt.Errorf("marshal %s: %w", prSelectFairnessFileName, err)
	}
	return append(data, '\n'), nil
}

// openPRSelectFairnessStore builds the lease's store, creating the scheduler
// directory for a standalone/manual invocation against a root that was never
// scaffolded — the lease rides claims.lock, and a flock cannot be created in a
// directory that does not exist. A stage pod creates nothing: the plane owns
// the daemon's scheduler directory.
func openPRSelectFairnessStore(l instance.Layout) (stateclient.Store, error) {
	if !statePlaneSelected() {
		if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
			return nil, err
		}
	}
	return openStageStateStore(l)
}

// openHeldPRSelectFairnessStore is openPRSelectFairnessStore for a caller
// ALREADY inside claims.lock — the claim transaction observePRSelectEligibility
// runs in. Locally that is the no-lock file store, because taking claims.lock
// a second time from inside claims.lock waits on itself until the timeout; on
// the plane it is the same HTTP store, where the daemon takes the lock
// server-side for each round trip.
func openHeldPRSelectFairnessStore(l instance.Layout) (stateclient.Store, error) {
	if !statePlaneSelected() {
		if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
			return nil, err
		}
	}
	return openHeldStageStateStore(l)
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
