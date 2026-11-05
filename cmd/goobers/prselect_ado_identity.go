package main

import (
	"strings"

	"github.com/goobers/goobers/providers"
)

// Match ADO UPN filters using ADO's case-insensitive identity semantics while
// preserving the summary's spelling and the shared title/assignee predicates.
func adoSelectionMatchesSummary(filters providers.ListPullRequestsRequest, pr providers.PullRequestSummary) (bool, error) {
	if strings.EqualFold(filters.Author, pr.Author) {
		filters.Author = pr.Author
	}
	for _, reviewer := range pr.RequestedReviewers {
		if strings.EqualFold(filters.RequestedReviewer, reviewer) {
			filters.RequestedReviewer = reviewer
			break
		}
	}
	return filters.MatchesSummary(pr)
}
