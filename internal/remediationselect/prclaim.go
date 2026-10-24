package remediationselect

import (
	"strconv"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/providers"
)

// PullRequestClaimPrefix prefixes the external id of a PR claim.
const PullRequestClaimPrefix = claimsclient.PullRequestItemPrefix

// PullRequestClaimKey is the external id of the unscoped claim on PR number.
func PullRequestClaimKey(number int) string {
	return PullRequestClaimPrefix + strconv.Itoa(number)
}

// PullRequestClaimLedgerKey addresses PR number's lease: scoped to the
// gaggle's repository-provider namespace, or legacy (unscoped) when the
// stage runs ungaggled — the split the ledger's Claim/ClaimScoped pair
// expressed. provider must be the claiming PR's own repository provider
// (#3649): a hardcoded provider let two repositories on different providers
// collide on the same PR number within one gaggle.
func PullRequestClaimLedgerKey(gaggle string, provider providers.ProviderKind, number int) claimsclient.Key {
	if gaggle == "" {
		return claimsclient.Key{ExternalID: PullRequestClaimKey(number)}
	}
	return claimsclient.Key{
		Gaggle:     gaggle,
		Provider:   string(provider),
		ExternalID: PullRequestClaimKey(number),
	}
}
