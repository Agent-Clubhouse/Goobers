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

// adoGoobersLabelNamespaces are the label prefixes Goobers reserves. Every
// label Goobers itself writes under them is lower case, so a read tag in
// these namespaces is folded to lower case.
var adoGoobersLabelNamespaces = []string{"goobers:", "goobers/"}

// canonicalADOLabel returns the spelling Goobers compares label against: the
// exact spelling of a wanted label it matches case-insensitively, else the
// lower-case form of a label in a Goobers namespace, else label unchanged.
func canonicalADOLabel(label string, wanted []string) string {
	for _, want := range wanted {
		if strings.EqualFold(label, want) {
			return want
		}
	}
	lower := strings.ToLower(label)
	for _, prefix := range adoGoobersLabelNamespaces {
		if strings.HasPrefix(lower, prefix) {
			return lower
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
// exactly: its native label filter and its label predicate's labels.
func adoRequestedLabels(req ListWorkItemsRequest) []string {
	return append(append([]string(nil), req.Labels...), req.LabelPredicate.Labels()...)
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
