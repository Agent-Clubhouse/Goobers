package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestVerdictLabel(t *testing.T) {
	substantive := []apiv1.Finding{{Severity: apiv1.SeverityWarning, Message: "nit", Class: apiv1.FindingSubstantive}}
	allBlocked := []apiv1.Finding{
		{Severity: apiv1.SeverityInfo, Message: "wait on #10", Class: apiv1.FindingCrossPRBlocked, BlockingPRs: []int{10}},
		{Severity: apiv1.SeverityInfo, Message: "wait on #11", Class: apiv1.FindingCrossPRBlocked, BlockingPRs: []int{11}},
	}
	mixed := append(append([]apiv1.Finding{}, allBlocked...), substantive...)

	tests := []struct {
		name     string
		decision apiv1.VerdictDecision
		findings []apiv1.Finding
		want     string
	}{
		{name: "pass", decision: apiv1.VerdictPass, want: "goobers:merge-ready"},
		{name: "pass with findings ignored", decision: apiv1.VerdictPass, findings: allBlocked, want: "goobers:merge-ready"},
		{name: "fail", decision: apiv1.VerdictFail, want: "goobers:merge-escalated"},
		{name: "unknown", decision: apiv1.VerdictDecision("unknown"), want: "goobers:needs-remediation"},
		{name: "zero value", decision: apiv1.VerdictDecision(""), want: "goobers:needs-remediation"},
		{name: "needs-changes, no findings", decision: apiv1.VerdictNeedsChanges, want: "goobers:needs-remediation"},
		{name: "needs-changes, substantive only", decision: apiv1.VerdictNeedsChanges, findings: substantive, want: "goobers:needs-remediation"},
		// #747: entirely cross-pr-blocked findings route to blocked-on-sibling.
		{name: "needs-changes, entirely cross-pr-blocked", decision: apiv1.VerdictNeedsChanges, findings: allBlocked, want: blockedOnSiblingLabel},
		// #747 AC: a real defect present alongside cross-pr-blocked findings still wins.
		{name: "needs-changes, mixed", decision: apiv1.VerdictNeedsChanges, findings: mixed, want: "goobers:needs-remediation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdictLabel(tt.decision, tt.findings); got != tt.want {
				t.Fatalf("verdictLabel(%q, %+v) = %q, want %q", tt.decision, tt.findings, got, tt.want)
			}
		})
	}
}

// TestAllCrossPRBlockedEmptyFindingsIsFalse pins the edge case verdictLabel's
// own doc comment calls out: a needs-changes verdict with NO findings at all
// falls through to needs-remediation like today, not blocked-on-sibling —
// "entirely cross-pr-blocked" requires at least one such finding to be true.
func TestAllCrossPRBlockedEmptyFindingsIsFalse(t *testing.T) {
	if allCrossPRBlocked(nil) {
		t.Fatal("allCrossPRBlocked(nil) = true, want false")
	}
	if allCrossPRBlocked([]apiv1.Finding{}) {
		t.Fatal("allCrossPRBlocked(empty) = true, want false")
	}
}

// TestUnionBlockingPRsDedupesAndSorts proves blockedOnSiblingState.Blockers
// is the deduplicated, sorted union across every cross-pr-blocked finding —
// not just the first one's BlockingPRs.
func TestUnionBlockingPRsDedupesAndSorts(t *testing.T) {
	findings := []apiv1.Finding{
		{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: []int{30, 10}},
		{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: []int{10, 20}},
		{Class: apiv1.FindingSubstantive}, // no BlockingPRs, contributes nothing
	}
	got := unionBlockingPRs(findings)
	want := []int{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unionBlockingPRs = %v, want %v", got, want)
	}
}

// TestBlockedOnSiblingCommentRoundTrips proves the same render/parse
// round-trip discipline #362's verdict-json payload already has, for the
// new blocked-on-sibling marker #748's self-heal will read.
func TestBlockedOnSiblingCommentRoundTrips(t *testing.T) {
	s := blockedOnSiblingState{
		Blockers:   []int{10, 11},
		Reason:     "waiting on sibling PRs to land first",
		HeadSHA:    "headsha123",
		BaseSHA:    "basesha456",
		RecordedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	payload, err := blockedOnSiblingComment(s)
	if err != nil {
		t.Fatalf("blockedOnSiblingComment: %v", err)
	}
	if !strings.Contains(payload, "<!-- blocked-on-sibling:") {
		t.Fatalf("payload = %q, want the blocked-on-sibling marker", payload)
	}
	got, ok := parseBlockedOnSiblingComment(payload)
	if !ok {
		t.Fatalf("parseBlockedOnSiblingComment did not find a payload in: %q", payload)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("parsed state = %+v, want %+v", got, s)
	}
}

// TestParseBlockedOnSiblingCommentNoPayloadIsNotFound mirrors
// TestParseVerdictCommentNoPayloadIsNotFound: an ordinary comment with no
// embedded marker is a clean "not found," not a parse error.
func TestParseBlockedOnSiblingCommentNoPayloadIsNotFound(t *testing.T) {
	if _, ok := parseBlockedOnSiblingComment("please rebase, thanks!"); ok {
		t.Fatal("parseBlockedOnSiblingComment on a plain comment: ok = true, want false")
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
	if len(got.Findings) != 1 || !reflect.DeepEqual(got.Findings[0], v.Findings[0]) {
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
