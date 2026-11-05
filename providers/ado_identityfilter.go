package providers

import "strings"

// ADO UPNs are case-insensitive. Keep the original spelling in summaries and
// leave the provider-neutral matcher unchanged for other forges.
func adoMatchesPullRequestIdentities(req ListPullRequestsRequest, author string, reviewers []string) bool {
	if req.Author != "" && !strings.EqualFold(req.Author, author) {
		return false
	}
	if req.RequestedReviewer == "" {
		return true
	}
	for _, reviewer := range reviewers {
		if strings.EqualFold(req.RequestedReviewer, reviewer) {
			return true
		}
	}
	return false
}
