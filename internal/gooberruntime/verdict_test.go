package gooberruntime

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestValidateMergeReviewVerdictAcceptsClassedFindings(t *testing.T) {
	err := ValidateMergeReviewVerdict(apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityError, Message: "handler drops the error", Class: apiv1.FindingSubstantive},
		},
	})
	if err != nil {
		t.Fatalf("ValidateMergeReviewVerdict: %v", err)
	}
}

func TestValidateMergeReviewVerdictRejectsInvalidDecision(t *testing.T) {
	err := ValidateMergeReviewVerdict(apiv1.Verdict{Decision: "maybe"})
	if err == nil || !strings.Contains(err.Error(), "invalid verdict decision") {
		t.Fatalf("error = %v, want invalid verdict decision", err)
	}
}

func TestValidateMergeReviewVerdictRejectsBadFindings(t *testing.T) {
	cases := []struct {
		name    string
		finding apiv1.Finding
		want    string
	}{
		{
			name:    "invalid severity",
			finding: apiv1.Finding{Severity: "fatal", Message: "boom", Class: apiv1.FindingSubstantive},
			want:    `finding[0].severity "fatal" is invalid`,
		},
		{
			name:    "missing message",
			finding: apiv1.Finding{Severity: apiv1.SeverityWarning, Message: "  ", Class: apiv1.FindingSubstantive},
			want:    "finding[0].message is required",
		},
		{
			// #747's fail-closed rule: a cross-pr-blocked finding without a
			// named blocker is an unusable record (nothing an automated
			// unpark could ever act on) — the whole verdict is rejected.
			name:    "cross-pr-blocked without blocker",
			finding: apiv1.Finding{Severity: apiv1.SeverityInfo, Message: "should land after a sibling PR", Class: apiv1.FindingCrossPRBlocked},
			want:    "finding[0] is invalid",
		},
		{
			// Merge review requires a routing class on every finding: a
			// classless finding silently skips remediation routing.
			name:    "missing class",
			finding: apiv1.Finding{Severity: apiv1.SeverityError, Message: "handler drops the error"},
			want:    `finding[0].class "" is invalid`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMergeReviewVerdict(apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Findings: []apiv1.Finding{tc.finding},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
