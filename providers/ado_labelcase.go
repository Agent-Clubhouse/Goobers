package providers

import (
	"fmt"
	"strings"
)

// Azure DevOps work-item tags and pull-request labels share one project-wide
// namespace that matches case-insensitively: the casing ADO returns is the
// casing of whoever wrote the tag first, not the casing a later writer sent.
// Goobers compares labels exactly everywhere else (the label predicate,
// WorkItem.HasLabel, cmd/goobers' selectors), so the ADO provider folds what
// it reads onto a canonical spelling instead of pushing case-insensitivity
// into provider-neutral code. GitHub and Gitea are untouched.

// goobersOwnedLabels lists every label Goobers itself writes and compares
// exactly: the marker labels in model.go, the status labels, and the labels
// cmd/goobers owns (TestGoobersOwnedLabelsCoverCommandLabels pins that each
// of cmd/goobers' label constants is listed here). The ADO provider reads a
// tag matching one of them ignoring case back in this spelling.
var goobersOwnedLabels = []string{
	LabelApproved, LabelClaimed, LabelReady, LabelCritical, LabelNeedsHuman,
	LabelNominated, LabelAutoClose, LabelStale, LabelTracking,
	statusLabel(WorkItemStatusOpen), statusLabel(WorkItemStatusClaimed),
	statusLabel(WorkItemStatusInProgress), statusLabel(WorkItemStatusInReview),
	statusLabel(WorkItemStatusDone), statusLabel(WorkItemStatusClosed),
	statusLabelPrefix + "decomposing",
	"goobers:needs-remediation", "goobers:run-aborted", "goobers:merge-ready",
	"goobers:merge-escalated", "goobers:no-merge-review", "goobers:scope-gate",
	"goobers:scope-gate-ack", "goobers:blocked-on-sibling", "goobers:scope-drift",
	"goobers:merge-demoted",
}

// canonicalADOLabel returns the spelling Goobers compares label against: the
// exact spelling of a wanted label it matches case-insensitively, else that of
// a Goobers-owned label it matches, else label unchanged. Tags matching
// neither keep ADO's casing, so a configured mixed-case label such as
// goobers:Hold still compares exactly as it did before.
func canonicalADOLabel(label string, wanted []string) string {
	for _, want := range wanted {
		if strings.EqualFold(label, want) {
			return want
		}
	}
	for _, owned := range goobersOwnedLabels {
		if strings.EqualFold(label, owned) {
			return owned
		}
	}
	return label
}

// canonicalADOLabels folds each label with canonicalADOLabel and drops the
// duplicates folding can create, keeping first-seen order.
func canonicalADOLabels(labels, wanted []string) []string {
	if len(labels) == 0 {
		return labels
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, canonicalADOLabel(label, wanted))
	}
	return uniqueStrings(out)
}

// adoRequestedLabels lists every label a ListWorkItems request compares
// exactly: its native label filter, its label predicate's labels and the
// caller's CompareLabels.
func adoRequestedLabels(req ListWorkItemsRequest) []string {
	requested := append(append([]string(nil), req.Labels...), req.LabelPredicate.Labels()...)
	return append(requested, req.CompareLabels...)
}

// adoHasLabel reports whether labels holds label, ignoring case.
func adoHasLabel(labels []string, label string) bool {
	for _, have := range labels {
		if strings.EqualFold(have, label) {
			return true
		}
	}
	return false
}

// applyADOTagSet is applyLabelSet for ADO's case-insensitive tag namespace:
// a remove drops every tag equal to it ignoring case, and an add already
// present in any casing keeps the existing tag rather than appending a
// duplicate ADO would merge anyway.
func applyADOTagSet(current, add, remove []string) []string {
	next := make([]string, 0, len(current)+len(add))
	for _, tag := range current {
		if !adoHasLabel(remove, tag) {
			next = append(next, tag)
		}
	}
	for _, tag := range add {
		if !adoHasLabel(next, tag) {
			next = append(next, tag)
		}
	}
	return uniqueStrings(next)
}

// adoDropStatusTags removes every Goobers status tag in any casing, so
// replaceStatusLabel's exact-prefix drop cannot leave a differently cased
// stale status tag beside the new one.
func adoDropStatusTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !strings.HasPrefix(strings.ToLower(tag), statusLabelPrefix) {
			out = append(out, tag)
		}
	}
	return out
}

// PullRequestLabelAddError reports a multi-label AddPullRequestLabels call
// that applied some labels and failed others. The labels that applied are
// kept, not rolled back; the error names only the labels that failed.
type PullRequestLabelAddError struct {
	// Applied lists the labels this call added.
	Applied []string
	// Failed lists the labels this call could not add.
	Failed []string
	errs   []error
}

func (e *PullRequestLabelAddError) Error() string {
	parts := make([]string, 0, len(e.Failed))
	for i, name := range e.Failed {
		parts = append(parts, fmt.Sprintf("%q: %v", name, e.errs[i]))
	}
	return fmt.Sprintf("add pull request labels: %d of %d failed: %s",
		len(e.Failed), len(e.Failed)+len(e.Applied), strings.Join(parts, "; "))
}

// Unwrap exposes each failed label's cause to errors.Is and errors.As.
func (e *PullRequestLabelAddError) Unwrap() []error {
	return e.errs
}
