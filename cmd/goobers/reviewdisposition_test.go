package main

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestOrderingDeferralPreservesReviewEvidence(t *testing.T) {
	v := apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: " Wait for #10.\n\nComplete original rationale.\n", Findings: []apiv1.Finding{blockedFinding(10)}}
	got := orderingDeferralVerdict(v)
	if got.Decision != apiv1.VerdictDefer || got.ReasonCode != apiv1.VerdictReasonOrdering || got.Elected || got.Rationale != v.Rationale || !reflect.DeepEqual(got.Findings, v.Findings) {
		t.Fatalf("ordering normalization lost the disposition or evidence: %+v", got)
	}
	v.Findings = append(v.Findings, apiv1.Finding{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "actual defect"})
	if got := orderingDeferralVerdict(v); !reflect.DeepEqual(got, v) {
		t.Fatalf("ordering normalization hid a repairable defect: %+v", got)
	}
}

func TestLegacyFailAmbiguityIsVisibleWithoutRewritingVerdict(t *testing.T) {
	v := apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "Original reviewer statement."}
	comment := renderVerdictComment(v)
	if !strings.Contains(comment, legacyFailAmbiguous) || publishedVerdictReason(v) != legacyFailAmbiguous {
		t.Fatalf("legacy fail silently treated as typed rejection: %s", comment)
	}
	parsed, ok := parseVerdictComment(comment)
	if !ok || !reflect.DeepEqual(parsed, v) {
		t.Fatalf("legacy evidence rewritten: %+v", parsed)
	}
	v.ReasonCode = apiv1.VerdictReasonImplementationRejected
	if strings.Contains(renderVerdictComment(v), legacyFailAmbiguous) || publishedVerdictReason(v) != string(v.ReasonCode) {
		t.Fatal("explicit rejection reported as ambiguous")
	}
}

func TestTerminalVerdictRequirementRejectsFixableAndOrderingFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    apiv1.Verdict
		want string
	}{
		{name: "ordering fail is rejected", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonOrdering, Rationale: "wait for sibling"}, want: "terminal fail verdict"},
		{name: "cross-pr-blocked fail is rejected", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonImplementationRejected, Rationale: "This PR is blocked by #10.", Findings: []apiv1.Finding{{Severity: apiv1.SeverityInfo, Class: apiv1.FindingCrossPRBlocked, Message: "wait on #10", BlockingPRs: []int{10}}}}, want: "terminal fail verdict"},
		{name: "unsalvageable requires rationale", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonUnsalvageableDesign}, want: "unsalvageable-design fail verdict requires rationale"},
		{name: "typed failure remains valid", v: apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonImplementationRejected, Rationale: "Approach violates the required contract."}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := terminalVerdictRequirement(tc.v)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("terminalVerdictRequirement() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("terminalVerdictRequirement() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateVerdictForPublishRejectsContradictoryTerminalStatus(t *testing.T) {
	v := apiv1.Verdict{Decision: apiv1.VerdictFail, ReasonCode: apiv1.VerdictReasonOrdering, Rationale: "Wait for sibling #10."}
	if err := validateVerdictForPublish(v); err == nil {
		t.Fatal("validateVerdictForPublish accepted a terminal fail with ordering semantics")
	}
}
