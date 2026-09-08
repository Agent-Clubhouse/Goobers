package readservice

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func populateOperatorReview(review *OperatorReview, verdict apiv1.Verdict) {
	review.Rationale = verdict.Rationale
	review.ReasonCode = verdict.ReasonCode
	review.Findings = verdict.Findings
	review.LegacyFailAmbiguous = review.Verdict == string(apiv1.VerdictFail) && verdict.ReasonCode == ""
	if review.Rationale == "" {
		review.Rationale = strings.TrimSpace(verdict.Summary)
	}
}
