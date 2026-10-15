package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/goobers/goobers/providers"
)

// ciNoVerdictNoticeMarker opens the hidden marker that identifies the sticky
// notice record-merge-refusal posts when CI finished without a verdict
// (#6817), and the head it was posted for.
const ciNoVerdictNoticeMarker = "<!-- goobers:ci-no-verdict head="

var ciNoVerdictNoticeHead = regexp.MustCompile(`<!-- goobers:ci-no-verdict head=([0-9A-Za-z._-]*) -->`)

// ciNoVerdictOnlyRefusal reports whether a merge-pr refusal's only reason is
// CI that finished without a verdict. merge-pr joins reasons with "; " and
// the ci-no-verdict reason holds none, so any other reason disqualifies it.
func ciNoVerdictOnlyRefusal(reason string) bool {
	return strings.HasPrefix(reason, ciNoVerdictReason+":") && !strings.Contains(reason, "; ")
}

// recordCINoVerdictNotice handles a refusal caused only by CI that finished
// without a verdict (#6817). It records no demotion. It posts one sticky
// notice per head asking for a CI rerun, since nothing reruns such checks by
// itself and no other lane says so on the PR. A notice already posted for
// this head is left alone; one for an older head is updated in place.
func recordCINoVerdictNotice(ctx context.Context, provider remediationProvider, repo providers.RepositoryRef, prNumber int, headSHA, reason string, comments []providers.Comment, stdout, stderr io.Writer) int {
	existingID, noticedHead := latestCINoVerdictNotice(comments)
	if existingID != "" && noticedHead == headSHA {
		pf(stdout, "PR #%d: CI finished without a verdict at head %s, already noted — deferring without recording demotion\n", prNumber, headSHA)
		return 0
	}
	if err := postOrUpdateStickyComment(ctx, provider, repo, prNumber, existingID, renderCINoVerdictNotice(headSHA, reason)); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("record CI no-verdict notice on PR #%d", prNumber), err, "")
	}
	pf(stdout, "PR #%d: CI finished without a verdict at head %s — noted, deferring without recording demotion\n", prNumber, headSHA)
	return 0
}

// latestCINoVerdictNotice finds the newest no-verdict notice and the head it
// names.
func latestCINoVerdictNotice(comments []providers.Comment) (id, head string) {
	for i := len(comments) - 1; i >= 0; i-- {
		if match := ciNoVerdictNoticeHead.FindStringSubmatch(comments[i].Body); match != nil {
			return comments[i].ID, match[1]
		}
	}
	return "", ""
}

func renderCINoVerdictNotice(headSHA, reason string) string {
	detail := strings.TrimPrefix(reason, ciNoVerdictReason+": ")
	return fmt.Sprintf(
		"⚠️ **CI needs a rerun** (#6817). CI on head `%s` finished without a verdict: %s. "+
			"The checks were cancelled, never started or went stale, so there is nothing for an agent to fix, "+
			"and merge-review cannot land this pull request until CI passes. Rerun the checks, or push a commit. "+
			"This is not counted toward merge demotion.\n\n%s%s -->",
		headSHA, detail, ciNoVerdictNoticeMarker, headSHA)
}
