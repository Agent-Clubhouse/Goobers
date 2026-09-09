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
