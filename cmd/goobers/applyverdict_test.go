package main

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestVerdictLabel(t *testing.T) {
	tests := []struct {
		name     string
		decision apiv1.VerdictDecision
		want     string
	}{
		{name: "pass", decision: apiv1.VerdictPass, want: "goobers:merge-ready"},
		{name: "fail", decision: apiv1.VerdictFail, want: "goobers:merge-escalated"},
		{name: "unknown", decision: apiv1.VerdictDecision("unknown"), want: "goobers:needs-remediation"},
		{name: "zero value", decision: apiv1.VerdictDecision(""), want: "goobers:needs-remediation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdictLabel(tt.decision); got != tt.want {
				t.Fatalf("verdictLabel(%q) = %q, want %q", tt.decision, got, tt.want)
			}
		})
	}
}

// TestRenderVerdictCommentEmbedsRecoverableVerdict is #362's prerequisite:
// pr-remediation's gather-pr-context runs in a different workflow's run than
// merge-review's apply-verdict, so it must recover the structured Verdict
// from the PR comment itself. Proves the round-trip: render, then parse back
// out, must reproduce the original Verdict exactly.
func TestRenderVerdictCommentEmbedsRecoverableVerdict(t *testing.T) {
	v := apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Summary:   "Rebase and address one nit.",
		Rationale: "The base moved and there's a naming nit.",
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityWarning, Message: "nit", Class: apiv1.FindingSubstantive, Location: "foo.go:10"},
		},
		HeadSHA: "headsha123",
		BaseSHA: "basesha456",
	}

	comment := renderVerdictComment(v)

	if !strings.Contains(comment, v.Summary) {
		t.Fatalf("comment = %q, want prose summary preserved", comment)
	}
	if !strings.Contains(comment, "<!-- verdict-json:") {
		t.Fatalf("comment = %q, want an embedded verdict-json payload", comment)
	}

	got, ok := parseVerdictComment(comment)
	if !ok {
		t.Fatalf("parseVerdictComment did not find a payload in: %q", comment)
	}
	if got.Decision != v.Decision || got.Summary != v.Summary || got.Rationale != v.Rationale ||
		got.HeadSHA != v.HeadSHA || got.BaseSHA != v.BaseSHA {
		t.Fatalf("parsed verdict = %+v, want %+v", got, v)
	}
	if len(got.Findings) != 1 || got.Findings[0] != v.Findings[0] {
		t.Fatalf("parsed findings = %+v, want %+v", got.Findings, v.Findings)
	}
}

// TestParseVerdictCommentNoPayloadIsNotFound proves an ordinary human/other-
// agent comment (no embedded payload — the common case on a PR thread) is a
// clean "not found," not a parse error.
func TestParseVerdictCommentNoPayloadIsNotFound(t *testing.T) {
	if _, ok := parseVerdictComment("please rebase, thanks!"); ok {
		t.Fatal("parseVerdictComment on a plain comment: ok = true, want false")
	}
}
