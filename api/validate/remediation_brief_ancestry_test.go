package validate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestRemediationBriefV3AncestryIsClosed: the optional ancestry section
// (#6125) validates when well formed and rejects an unknown omission reason
// or an undeclared property, like every other closed brief section.
func TestRemediationBriefV3AncestryIsClosed(t *testing.T) {
	build := func(reason string) []byte {
		brief := minimalRemediationBrief()
		brief.GatherIssueContext = &apiv1.RemediationIssueContext{
			Issues: []apiv1.RemediationIssue{},
			Ancestry: &apiv1.RemediationAncestry{
				Status: "incomplete", Provider: "ado", MaxDepth: 3, MaxItems: 10, CrossProject: "deny",
				Items: []apiv1.RemediationAncestor{},
				Omissions: []apiv1.RemediationAncestryOmission{{
					Child: "ado:project:1", Parent: "2", Depth: 1, Reason: reason,
				}},
			},
		}
		data, err := json.Marshal(brief)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	v := newV(t)
	if err := v.ValidateJSON(schemas.RemediationBrief, build("cycle")); err != nil {
		t.Fatalf("well-formed ancestry should validate: %v", err)
	}
	if err := v.ValidateJSON(schemas.RemediationBrief, build("because")); err == nil {
		t.Fatal("unknown omission reason validated, want rejection")
	}
	extra := strings.Replace(string(build("cycle")), `"status":"incomplete"`, `"status":"incomplete","raw":{}`, 1)
	if err := v.ValidateJSON(schemas.RemediationBrief, []byte(extra)); err == nil {
		t.Fatal("undeclared ancestry property validated, want rejection")
	}
}
