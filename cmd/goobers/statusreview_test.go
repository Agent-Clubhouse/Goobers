package main

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/readservice"
)

func TestStatusReviewNamesDispositionWithoutRewritingRationale(t *testing.T) {
	for _, tc := range []struct {
		decision string
		reason   apiv1.VerdictReasonCode
		want     string
	}{
		{"defer", apiv1.VerdictReasonOrdering, "ordering"},
		{"defer", apiv1.VerdictReasonNoLander, "no-lander"},
		{"escalate", apiv1.VerdictReasonRepassBudget, "repass-budget-exhausted"},
		{"fail", apiv1.VerdictReasonPolicyRejected, "policy-rejected"},
		{"fail", "", legacyFailAmbiguous},
	} {
		const rationale = "  Original rationale.\n\nSecond paragraph.  "
		var out strings.Builder
		renderStatusReview(&out, &readservice.OperatorReview{Verdict: tc.decision, ReasonCode: tc.reason, Rationale: rationale})
		if !strings.Contains(out.String(), rationale) || !strings.Contains(out.String(), "review reason: "+tc.want) {
			t.Fatalf("lost rationale or reason %s: %q", tc.want, out.String())
		}
	}
}
