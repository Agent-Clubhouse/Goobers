package remediationselect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

// NamespaceLister is the one ledger read the PR claim filters need.
// claimsclient.Ledger satisfies it; the cloud claims plane supplies its own.
type NamespaceLister interface {
	ListNamespace(ctx context.Context, gaggle, provider string) (claimsclient.Listing, error)
}

// PullRequestClaimListingContext is the one namespace read the PR claim-status
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
//
// The caller supplies the context and the narrow ledger read, so callers that
// are not stage processes (the cloud scheduler) can use it.
func PullRequestClaimListingContext(ctx context.Context, ledger NamespaceLister, gaggle string, provider providers.ProviderKind) (claimsclient.Listing, error) {
	providerNamespace := ""
	if gaggle != "" {
		providerNamespace = string(provider)
	}
	claims, err := ledger.ListNamespace(ctx, gaggle, providerNamespace)
	if err != nil {
		return claimsclient.Listing{}, fmt.Errorf("read PR claims: %w", err)
	}
	if gaggle != "" && provider != providers.ProviderGitHub {
		legacy, err := ledger.ListNamespace(ctx, gaggle, string(providers.ProviderGitHub))
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

// PullRequestClaimStatus reports whether a PR's own lease is live, and whether the current run holds it.
func PullRequestClaimStatus(
	claims claimsclient.Listing,
	gaggle string,
	provider providers.ProviderKind,
	number int,
	currentRunID string,
	now time.Time,
) (claimed, ownedByCurrentRun bool) {
	entry, ok, ownershipComparable := ResolvePullRequestClaim(claims, gaggle, provider, number, now)
	if !ok || !entry.ExpiresAt.After(now) {
		return false, false
	}
	return true, ownershipComparable && currentRunID != "" && entry.RunID == currentRunID
}

// PullRequestClaimStatusFor is pullRequestClaimStatus for a concrete PR: on top
// of the PR's own pr/<n> lease it treats the PR as claimed while the run that
// opened it (named by the run-scoped head branch <namespace><workflow>/<runID>,
// providers.BranchNameIn) still holds a live claim (#7062). The opening run
// keeps pushing to that branch (ci-poll, remediate-ci, local-ci) without ever
// taking a pr/<n> lease, so without this a PR-consuming lane would rebase the
// same branch concurrently and one of the two pushes would be lost. The origin
// run itself is not blocked from its own PR.
func PullRequestClaimStatusFor(
	claims claimsclient.Listing,
	gaggle string,
	provider providers.ProviderKind,
	pr providers.PullRequestSummary,
	currentRunID string,
	now time.Time,
) (claimed, ownedByCurrentRun bool) {
	claimed, ownedByCurrentRun = PullRequestClaimStatus(claims, gaggle, provider, pr.Number, currentRunID, now)
	if claimed {
		return claimed, ownedByCurrentRun
	}
	originRunID := RunBranchOriginRunID(pr.Head)
	if originRunID == "" || originRunID == currentRunID {
		return false, false
	}
	for _, entry := range claims.Entries {
		if entry.RunID == originRunID && entry.ReleasedAt == nil && entry.ExpiresAt.After(now) {
			return true, false
		}
	}
	return false, false
}

// RunBranchOriginRunID extracts the run ID from a run-scoped head branch
// (providers.BranchNameIn: <namespace><workflow>/<runID>). The namespace is
// gaggle-configurable, so only the shape is checked: at least
// <namespace>/<workflow>/<runID> with a 32-hex run ID. Anything else returns "".
func RunBranchOriginRunID(head string) string {
	parts := strings.Split(head, "/")
	if len(parts) < 3 {
		return ""
	}
	id := parts[len(parts)-1]
	if len(id) != 32 {
		return ""
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return id
}

// ResolvePullRequestClaim is shared by selection and its operator report.
// A live unscoped lease blocks a scoped claimant even when the run IDs match:
// the selector cannot treat ownership across those namespaces as equivalent.
func ResolvePullRequestClaim(
	claims claimsclient.Listing,
	gaggle string,
	provider providers.ProviderKind,
	number int,
	now time.Time,
) (entry claimsclient.Entry, found, ownershipComparable bool) {
	var (
		ok bool
	)
	if gaggle == "" {
		entry, ok = claims.Lookup(claimsclient.Key{ExternalID: PullRequestClaimKey(number)})
	} else {
		if legacy, held := claims.Lookup(claimsclient.Key{ExternalID: PullRequestClaimKey(number)}); held && legacy.ExpiresAt.After(now) {
			return legacy, true, false
		}
		entry, ok = claims.Lookup(PullRequestClaimLedgerKey(gaggle, provider, number))
		if !ok && provider != providers.ProviderGitHub {
			// Pre-#3649 builds wrote every gaggle-scoped PR claim under a
			// hardcoded ProviderGitHub key regardless of the repository's
			// real provider. A non-GitHub claim leased before the upgrade
			// still lives under that key until it naturally expires — fall
			// back to it here (pullRequestClaimListing already fetched it
			// into claims) so it stays visible instead of letting the first
			// post-upgrade selection claim and process the same PR
			// concurrently.
			entry, ok = claims.Lookup(PullRequestClaimLedgerKey(gaggle, providers.ProviderGitHub, number))
		}
	}
	return entry, ok, true
}
