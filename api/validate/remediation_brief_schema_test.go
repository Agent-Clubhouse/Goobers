package validate

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func minimalRemediationBrief() apiv1.RemediationBrief {
	return apiv1.RemediationBrief{
		Schema:                 apiv1.RemediationBriefVersion,
		SelectedNumber:         "55",
		Head:                   "goobers/implementation/run-55",
		Base:                   "main",
		WorkspaceBranch:        "goobers/implementation/run-55",
		IsBehindBase:           true,
		HasSubstantiveFindings: "true",
		HasFailingCI:           "false",
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseSHA:  "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Comments: []apiv1.RemediationThreadComment{},
		},
	}
}

func TestRemediationBriefWithoutOptionalGatherersValidates(t *testing.T) {
	brief := minimalRemediationBrief()
	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatalf("marshal remediation brief: %v", err)
	}
	v := newV(t)
	if err := v.ValidateJSON(schemas.RemediationBrief, data); err != nil {
		t.Fatalf("brief without optional gatherers should validate: %v\n%s", err, data)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal remediation brief: %v", err)
	}
	for _, section := range []string{
		"gatherCIFailures",
		"gatherReviewThreads",
		"gatherSiblingContext",
		"gatherIssueContext",
	} {
		if _, ok := raw[section]; ok {
			t.Errorf("%s is present; absent gatherers must be represented by omitted sections", section)
		}
	}
}

func TestCompleteRemediationBriefValidates(t *testing.T) {
	brief := minimalRemediationBrief()
	brief.GatherPRContext.Verdict = &apiv1.Verdict{
		Decision:    apiv1.VerdictNeedsChanges,
		Rationale:   "one defect remains",
		Digest:      "sha256:review-inputs",
		SourceRunID: "review-run",
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Message:  "fix the race",
			Class:    apiv1.FindingSubstantive,
		}},
	}
	brief.GatherPRContext.Comments = []apiv1.RemediationThreadComment{{
		Author: "reviewer", Body: "Please fix the race.", CreatedAt: "2026-07-21T12:00:00Z",
	}}
	brief.GatherCIFailures = &apiv1.RemediationCIFailures{
		Checks: []apiv1.RemediationCIFailure{{
			Name: "unit", Conclusion: "failure", Summary: "race detected",
			Annotations: []apiv1.RemediationCIAnnotation{{Path: "worker.go", StartLine: 42, Message: "data race"}},
		}},
	}
	brief.GatherReviewThreads = &apiv1.RemediationReviewThreads{
		Reviews: []apiv1.RemediationNativeReview{{State: "changes_requested", Body: "Fix the race."}},
		InlineComments: []apiv1.RemediationInlineComment{{
			Body: "This write is unsynchronized.", Path: "worker.go", Line: 42,
			OriginalLine: 40, DiffHunk: "@@ -40,1 +42,1 @@", IsResolved: false, IsOutdated: false,
			StartLine: 40, OriginalStartLine: 38, StartSide: "RIGHT",
		}},
	}
	brief.GatherSiblingContext = &apiv1.RemediationSiblingContext{
		PullRequests: []apiv1.RemediationSibling{{
			Number: 56, Blocking: true, Reason: "file-overlap", OverlappingFiles: []string{"worker.go"},
		}},
	}
	brief.GatherIssueContext = &apiv1.RemediationIssueContext{
		Issues: []apiv1.RemediationIssue{{Number: "937", Title: "Remediation brief", Body: "Acceptance criteria"}},
	}

	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatalf("marshal complete remediation brief: %v", err)
	}
	if err := newV(t).ValidateJSON(schemas.RemediationBrief, data); err != nil {
		t.Fatalf("complete remediation brief should validate: %v\n%s", err, data)
	}
}

func TestRemediationBriefSchemaIsClosedAndVersioned(t *testing.T) {
	v := newV(t)
	for name, doc := range map[string]string{
		"wrong version": `{"schema":"goobers.dev/remediation-brief/v1","selectedNumber":"55","head":"h","base":"main","workspaceBranch":"h","isBehindBase":false,"hasSubstantiveFindings":"false","hasFailingCI":"false","gatherPrContext":{"headSha":"a","baseSha":"b","verdict":null,"comments":[]}}`,
		"unknown field": `{"schema":"goobers.dev/remediation-brief/v2","selectedNumber":"55","head":"h","base":"main","workspaceBranch":"h","isBehindBase":false,"hasSubstantiveFindings":"false","hasFailingCI":"false","gatherPrContext":{"headSha":"a","baseSha":"b","verdict":null,"comments":[]},"futureSection":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := v.ValidateJSON(schemas.RemediationBrief, []byte(doc)); err == nil {
				t.Fatal("expected remediation brief schema validation to fail")
			}
		})
	}
}

func TestRemediationBriefV1RemainsImmutableAndValid(t *testing.T) {
	doc := `{"schema":"goobers.dev/remediation-brief/v1","selectedNumber":"55","head":"h","base":"main","workspaceBranch":"h","isBehindBase":false,"hasSubstantiveFindings":"false","hasFailingCI":"false","gatherPrContext":{"headSha":"a","baseSha":"b","verdict":null,"comments":[]},"gatherReviewThreads":{"reviews":[],"inlineComments":[{"body":"old shape","path":"worker.go"}]}}`
	if err := newV(t).ValidateJSON(schemas.RemediationBriefV1, []byte(doc)); err != nil {
		t.Fatalf("immutable v1 brief should remain valid: %v", err)
	}
}
