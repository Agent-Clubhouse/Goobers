package providers

import (
	"testing"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

func TestIntegrityForLabelsUsesConfiguredTrustLabel(t *testing.T) {
	const trustLabel = "team-approved"
	if got := IntegrityForLabels([]string{"bug", trustLabel}, trustLabel); got != apiintegrity.Maintainer {
		t.Fatalf("configured trust-label integrity = %q, want maintainer", got)
	}
	if got := IntegrityForLabels([]string{"bug", LabelApproved}, trustLabel); got != apiintegrity.Unapproved {
		t.Fatalf("different approval-label integrity = %q, want unapproved", got)
	}
	if got := IntegrityForLabels([]string{LabelApproved}, ""); got != apiintegrity.Unapproved {
		t.Fatalf("unconfigured trust-label integrity = %q, want unapproved", got)
	}
}

func TestProviderReadDoesNotInferIntegrityFromLabels(t *testing.T) {
	item := mapGitHubIssue(githubIssue{
		Labels: []githubLabel{{Name: LabelApproved}},
	})
	if item.Integrity != apiintegrity.Unapproved {
		t.Fatalf("provider-read integrity = %q, want unapproved", item.Integrity)
	}
}
