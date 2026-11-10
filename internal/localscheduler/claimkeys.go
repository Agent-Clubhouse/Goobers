package localscheduler

import "strings"

// Ledger-only claim namespaces. The claim ledger leases provider work items
// under their bare external ID ("536"), and several other things under
// synthetic IDs in a namespace of their own. A synthetic lease has no
// provider claim marker and its ID is not a work-item id, so it must never be
// resolved through a work-item API (#6798). Every minting site uses these
// prefixes, so IsWorkItemClaimKey is the one place that knows them all.
const (
	// PullRequestItemIDPrefix namespaces pr-remediation / merge-review PR
	// claims: "pr/536".
	PullRequestItemIDPrefix = "pr/"
	// AdvisoryPRItemIDPrefix namespaces advisory PR review claims:
	// "advisory-pr/<scope>/<review type>/<n>".
	AdvisoryPRItemIDPrefix = "advisory-pr/"
	// DecompositionTargetItemIDPrefix namespaces publish-batch's target lease,
	// kept apart from the parent item's own claim: "decomposition-target:<id>".
	DecompositionTargetItemIDPrefix = "decomposition-target:"
	// MergeLockItemIDPrefix namespaces merge-pr's per-repository merge window:
	// "merge-lock/<owner>/<repo>".
	MergeLockItemIDPrefix = "merge-lock/"
	// PublicationLockItemIDPrefix reserves fixed-duration publication custody.
	// These leases outlive run cleanup and are never renewed or reclaimed by
	// generic lifecycle paths; only verified release or expiry ends custody.
	PublicationLockItemIDPrefix = "publication-lock/"
	// PublicationLockReleaseCode marks an explicitly verified custody release.
	PublicationLockReleaseCode = "verdict_publication_closed"
)

var ledgerOnlyItemIDPrefixes = [...]string{
	PullRequestItemIDPrefix,
	AdvisoryPRItemIDPrefix,
	DecompositionTargetItemIDPrefix,
	MergeLockItemIDPrefix,
	PublicationLockItemIDPrefix,
}

// IsWorkItemClaimKey reports whether a claim's external ID names a provider
// work item, as opposed to a ledger-only synthetic lease.
func IsWorkItemClaimKey(id string) bool {
	if id == "" {
		return false
	}
	for _, prefix := range ledgerOnlyItemIDPrefixes {
		if strings.HasPrefix(id, prefix) {
			return false
		}
	}
	return true
}

// IsPublicationClaimKey identifies fixed-duration custody, including the
// earlier nested encoding. The legacy shape has provider/owner/repo after
// its prefix; an ordinary merge lock owned by an org named publication does not.
func IsPublicationClaimKey(id string) bool {
	if strings.HasPrefix(id, PublicationLockItemIDPrefix) {
		return true
	}
	legacy, ok := strings.CutPrefix(id, MergeLockItemIDPrefix+"publication/")
	return ok && strings.Count(legacy, "/") >= 2
}

// IsPublicationClaim classifies both scoped and older item-only records.
func (e ClaimEntry) IsPublicationClaim() bool {
	if e.ExternalID != "" {
		return IsPublicationClaimKey(e.ExternalID)
	}
	return IsPublicationClaimKey(e.ItemID)
}
