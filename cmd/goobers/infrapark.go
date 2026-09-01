package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/goobers/goobers/providers"
)

// remediationParkCommentPrefix and humanParkCommentPrefix are the two prefixes
// issue-close-out writes when it parks an item (issuecloseout.go, #2028). They
// are matched here rather than re-derived because the comment is the only
// record of WHY an item was parked that survives the run's retention.
const (
	remediationParkCommentPrefix = "Implementation parked for remediation: "
	humanParkCommentPrefix       = "Implementation parked for human review: "
)

// infrastructureParkReason matches the two reasons issueCloseOutReason
// produces for a gate whose terminal outcome was `infra` — the branch
// implementation.yaml's local-gate takes when failure-class has determined the
// failure belongs to the substrate and not to the item's diff.
//
// This is a string our own code writes, in one place, from a journal event's
// gate name and verdict — not operator prose — which is what makes matching it
// a sound test rather than a guess. Both spellings are covered: the plain
// terminal outcome, and the repass-exhausted form, since exhausting the repass
// budget on repeated infrastructure failures is still an infrastructure
// failure.
// The trailing boundary is load-bearing: without it `infrastructure` — a word
// that turns up in operator prose constantly — reads as the outcome `infra`.
var infrastructureParkReason = regexp.MustCompile(
	`^gate \S+ (?:returned terminal outcome|escalated after outcome) infra(?:\s|$)`)

// infrastructureParkResolvedReason explains a cleared park in the reconcile
// report.
const infrastructureParkResolvedReason = "removed `goobers:needs-remediation` because the park recorded an " +
	"infrastructure outcome, which is a statement about the substrate at one moment and not about this item"

// staleInfrastructureRemediationPark reports whether an ISSUE's
// needs-remediation park was charged to it for an INFRASTRUCTURE failure
// (#4154).
//
// The park itself is right for a mechanical failure: no policy question is
// pending, so waking a human would be wrong. What is not right is that
// local-gate's `infra` branch reaches the same terminal state as a genuine
// implementation failure. failure-class exists precisely to separate "the
// implementer got this wrong" from "the substrate was broken while this item
// happened to be in flight", and then both outcomes park the item forever —
// nothing anywhere removes this label from an issue. One bad week on the
// substrate therefore cost the cloud partition twenty items permanently, and
// the symptom (backlog-curation reporting no work) is indistinguishable from a
// healthy drained backlog.
//
// POLARITY, deliberately the same as staleBlockedOnSiblingMarker's: removing a
// label is an ACTION, so it happens only on positive proof. No park comment,
// an unparseable one, or a merit park means no action at all. A later
// needs-human park supersedes an earlier infrastructure one and also means no
// action, which is why this reads the LAST park comment rather than searching
// for the first infrastructure one.
//
// Like that function, it deliberately does NOT re-apply goobers:ready.
// Clearing the park states that a condition no longer holds; deciding the item
// deserves another attempt is a separate judgement (operator ruling
// 2026-08-22). Clearing the label is enough to return the item to
// query-backlog's candidate set, and curation then makes the readiness call on
// its own terms — which is where that call belongs.
func staleInfrastructureRemediationPark(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
) (bool, error) {
	if !item.HasLabel(needsRemediationLabel) {
		return false, nil
	}
	// A pending human decision outranks anything this function could conclude:
	// the item is parked for a reason that is not the substrate's.
	if item.HasLabel(providers.LabelNeedsHuman) {
		return false, nil
	}
	comments, err := provider.ListComments(ctx, repo, item.ID)
	if err != nil {
		return false, err
	}
	return latestParkIsInfrastructure(comments), nil
}

// latestParkIsInfrastructure reports whether the most recent park comment in
// comments (ListComments' order is oldest first) is a remediation park whose
// recorded reason is an infrastructure gate outcome.
func latestParkIsInfrastructure(comments []providers.Comment) bool {
	for i := len(comments) - 1; i >= 0; i-- {
		body := strings.TrimSpace(comments[i].Body)
		if strings.HasPrefix(body, humanParkCommentPrefix) {
			return false
		}
		reason, ok := strings.CutPrefix(body, remediationParkCommentPrefix)
		if !ok {
			continue
		}
		// issue-close-out may append evidence below the reason; only the
		// reason line is the machine-written part.
		line := strings.TrimSpace(strings.SplitN(reason, "\n", 2)[0])
		return infrastructureParkReason.MatchString(line)
	}
	return false
}
