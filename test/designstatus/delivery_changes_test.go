package main

import (
	"strings"
	"testing"
)

func TestClosingDesignWorkRequiresEvidenceAndScopeUpdate(t *testing.T) {
	old := document{Path: "docs/design/x.md", Status: "approved", Tracking: []string{"#42"}, Remaining: []string{"#42"}, ScopeDelta: "Implementation remains", Verified: "old revision"}
	updated := old
	updated.DeliveredBy, updated.Remaining = []string{"#42"}, nil
	updated.ScopeDelta, updated.Verified = "No delta; all designed scope shipped", "new revision"
	if problems := checkDeliveryChanges([]document{old}, []document{updated}, []string{"#42"}); len(problems) != 0 {
		t.Fatalf("honest update rejected: %v", problems)
	}
	for _, tc := range []struct {
		name string
		head []document
		want string
	}{
		{"unchanged", []document{old}, "Delivered-by"},
		{"deleted", nil, "retaining"},
		{"tracking removed", []document{{Path: old.Path}}, "Delivered-by"},
		{"stale verification", []document{{Path: old.Path, DeliveredBy: []string{"#42"}, ScopeDelta: "none", Verified: old.Verified}}, "refreshed Verified"},
		{"missing delta", []document{{Path: old.Path, DeliveredBy: []string{"#42"}, Verified: "new"}}, "Scope-delta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkDeliveryChanges([]document{old}, tc.head, []string{"#42"})
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("problems=%v want=%s", problems, tc.want)
			}
		})
	}
	if problems := checkDeliveryChanges([]document{old}, []document{old}, []string{"#99"}); len(problems) != 0 {
		t.Fatalf("unrelated work blocked: %v", problems)
	}
}
