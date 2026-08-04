package main

import (
	"encoding/json"
	"regexp"
)

// issueSpecPin is the implementation-time snapshot of the linked issue's
// UpdatedAt, embedded into the PR body by formatStructuredPRBody (#2340).
// merge-review's check-issue-staleness stage reads it back and compares
// against a fresh GetWorkItem to detect an issue edited after implementation
// began but before merge-review looked at the PR — the general case #2238's
// goobers:run-aborted label doesn't cover, since that only invalidates a PR
// whose OWN implementation run was cancelled.
//
// Deliberately pins UpdatedAt rather than a content digest (unlike
// internal/decomposition's IssueSnapshotDigest, DEC-1's own analog): the
// issue's labels aren't recoverable from the run's journaled scalar stage
// outputs without new plumbing (see openprbody.go), while UpdatedAt already
// is. The issue's acceptance criteria explicitly allow either.
type issueSpecPin struct {
	IssueID   string `json:"issueId"`
	UpdatedAt string `json:"updatedAt"`
}

var issueSpecPinPattern = regexp.MustCompile(`(?s)<!-- issue-spec-pin: (.*?) -->`)

// formatIssueSpecPin renders the marker line, or "" when there is nothing to
// pin (no linked issue, or the provider never populated UpdatedAt for it).
func formatIssueSpecPin(issueID, updatedAt string) string {
	if issueID == "" || updatedAt == "" {
		return ""
	}
	data, err := json.Marshal(issueSpecPin{IssueID: issueID, UpdatedAt: updatedAt})
	if err != nil {
		return ""
	}
	return "<!-- issue-spec-pin: " + string(data) + " -->"
}

// parseIssueSpecPin recovers the pin from a PR body. Absence (an older PR
// predating this feature, or one whose implementation run never resolved an
// UpdatedAt) is reported via ok=false, not an error — check-issue-staleness
// fails open (not stale) when there is nothing to compare against.
func parseIssueSpecPin(body string) (issueSpecPin, bool) {
	m := issueSpecPinPattern.FindStringSubmatch(body)
	if m == nil {
		return issueSpecPin{}, false
	}
	var pin issueSpecPin
	if err := json.Unmarshal([]byte(m[1]), &pin); err != nil || pin.IssueID == "" || pin.UpdatedAt == "" {
		return issueSpecPin{}, false
	}
	return pin, true
}

// replaceIssueSpecPin rewrites body's existing issue-spec-pin marker to
// pin the issue at updatedAt, or returns body unchanged if it carries no
// marker to replace (nothing for check-issue-staleness to have compared
// against in the first place).
//
// check-issue-staleness calls this after it detects and reports a stale
// PR: without it, the marker stays frozen at the original implementation-time
// snapshot forever — no other stage ever rewrites it (open-pr, the only
// other writer, belongs to implementation.yaml and never runs again once a
// PR exists) — so every future run of check-issue-staleness re-compares
// against that same stale snapshot and re-fires on the identical edit
// indefinitely, even though it already flagged that edit and routed the PR
// to remediation. Advancing the pin to the edit just observed means the
// next check only fires on a genuinely new edit.
func replaceIssueSpecPin(body, issueID, updatedAt string) string {
	if !issueSpecPinPattern.MatchString(body) {
		return body
	}
	replacement := formatIssueSpecPin(issueID, updatedAt)
	if replacement == "" {
		return body
	}
	return issueSpecPinPattern.ReplaceAllLiteralString(body, replacement)
}
