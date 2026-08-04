package main

import "testing"

func TestFormatAndParseIssueSpecPinRoundTrips(t *testing.T) {
	line := formatIssueSpecPin("123", "2026-08-01T12:00:00Z", "Title", "Body")
	if line == "" {
		t.Fatal("formatIssueSpecPin returned empty string for valid inputs")
	}
	body := "## Summary\n\nSome PR body.\n\n---\nFixes #123\n" + line
	pin, ok := parseIssueSpecPin(body)
	if !ok {
		t.Fatalf("parseIssueSpecPin did not find a pin in body:\n%s", body)
	}
	if pin.IssueID != "123" || pin.UpdatedAt != "2026-08-01T12:00:00Z" ||
		pin.SpecDigest != issueSpecDigest("Title", "Body") {
		t.Fatalf("pin = %+v, want issue ID, timestamp, and title/body digest", pin)
	}
}

func TestFormatIssueSpecPinEmptyWhenNothingToPin(t *testing.T) {
	tests := []struct {
		name           string
		issueID        string
		issueUpdatedAt string
	}{
		{"no issue", "", "2026-08-01T12:00:00Z"},
		{"no updatedAt", "123", ""},
		{"neither", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatIssueSpecPin(tc.issueID, tc.issueUpdatedAt, "", ""); got != "" {
				t.Fatalf("formatIssueSpecPin(%q, %q) = %q, want empty", tc.issueID, tc.issueUpdatedAt, got)
			}
		})
	}
}

func TestParseIssueSpecPinAbsentOrMalformed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no marker at all", "## Summary\n\nJust a normal PR body.\n\n---\nFixes #123"},
		{"malformed json", "<!-- issue-spec-pin: {not json} -->"},
		{"missing issueId", `<!-- issue-spec-pin: {"updatedAt":"2026-08-01T12:00:00Z"} -->`},
		{"missing updatedAt", `<!-- issue-spec-pin: {"issueId":"123"} -->`},
		{"empty body", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseIssueSpecPin(tc.body); ok {
				t.Fatalf("parseIssueSpecPin(%q) unexpectedly found a pin", tc.body)
			}
		})
	}
}
