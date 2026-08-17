package decomposition

import (
	"strings"
	"testing"
)

func validParent() ParentRef {
	return ParentRef{Provider: "github", Repository: "acme/widgets", ID: "419", ObservedRevision: "rev-1"}
}

func validSelection(t *testing.T, live LiveParentSnapshot) Selection {
	t.Helper()
	digest, err := IssueSnapshotDigest(live.ID, live.Title, live.Body, live.Labels, live.State)
	if err != nil {
		t.Fatal(err)
	}
	return Selection{
		Mode:                SelectionModeEscalation,
		SourceRunID:         "escalated-1",
		SourceWorkflow:      "implementation",
		SourceStage:         "implement",
		ErrorCode:           "ISSUE_OVER_SCOPE",
		Parent:              validParent(),
		IssueSnapshotDigest: digest,
	}
}

func validLiveParent() LiveParentSnapshot {
	return LiveParentSnapshot{ID: "419", Title: "Big issue", Body: "This issue is too large for one PR.", Labels: []string{"area:workflows"}, State: "open"}
}

func validPlan(selection Selection) Plan {
	return Plan{
		SchemaVersion: PlanSchemaV1,
		Selection: PlanSelection{
			Mode:                selection.Mode,
			SourceRunID:         selection.SourceRunID,
			IssueSnapshotDigest: selection.IssueSnapshotDigest,
		},
		Parent:  selection.Parent,
		Summary: "Split the large issue into a selector and a validator.",
		Children: []ChildPlan{
			{
				Key:                "selector",
				Title:              "Add decomposition disposition selection",
				Body:               "Implement select-source to find and claim an unconsumed L6 disposition.",
				AcceptanceCriteria: "Fixtures cover every excluded escalation class.",
				ValidationBoundary: "unit tests over the selector logic",
				Labels:             []string{"area:workflows", "type:feature"},
			},
			{
				Key:                "validator",
				Title:              "Add decomposition-plan schema and validator",
				Body:               "Implement validate-plan to check the plan produced by design-slices.",
				AcceptanceCriteria: "Invalid plans produce zero provider mutations.",
				ValidationBoundary: "unit tests over the validator logic",
				Labels:             []string{"area:workflows", "type:feature"},
				DependsOn:          []string{"selector"},
			},
		},
	}
}

func TestValidatePlanAccepts(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)

	result := ValidatePlan(plan, selection, live)
	if !result.Valid {
		t.Fatalf("expected valid plan, got errors=%v conflict=%v", result.Errors, result.Conflict)
	}
}

func TestValidatePlanRejectsUnsupportedSchemaVersion(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.SchemaVersion = "v99"

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for unsupported schema version")
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "schemaVersion") {
		t.Fatalf("errors = %v, want exactly one schemaVersion error", result.Errors)
	}
}

func TestValidatePlanRejectsMalformedSchemaVersion(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.SchemaVersion = ""

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for empty schema version")
	}
}

func TestValidatePlanRequiresAtLeastTwoChildrenUnlessReplacement(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Children = plan.Children[:1]

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for a single child with no replacement reason")
	}

	plan.SingleReplacementReason = "the issue is actually small enough for one PR once scoped correctly"
	result = ValidatePlan(plan, selection, live)
	if !result.Valid {
		t.Fatalf("expected a single-child replacement plan to be valid, got errors=%v", result.Errors)
	}
}

func TestValidatePlanRejectsDuplicateKeys(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Children[1].Key = plan.Children[0].Key

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for duplicate child keys")
	}
	if !containsSubstring(result.Errors, "duplicate child key") {
		t.Fatalf("errors = %v, want a duplicate child key finding", result.Errors)
	}
}

func TestValidatePlanRejectsDuplicateContent(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Children[1].Title = plan.Children[0].Title
	plan.Children[1].Body = plan.Children[0].Body

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for two children with identical title and body")
	}
	if !containsSubstring(result.Errors, "duplicates child") {
		t.Fatalf("errors = %v, want a duplicate-content finding", result.Errors)
	}
}

func TestValidatePlanRejectsDependencyCycle(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Children[0].DependsOn = []string{plan.Children[1].Key}
	// plan.Children[1] already depends on Children[0] ("selector") -> cycle.

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for a dependency cycle")
	}
	if !containsSubstring(result.Errors, "dependency cycle") {
		t.Fatalf("errors = %v, want a dependency cycle finding", result.Errors)
	}
}

func TestValidatePlanAllowsExternalIssueDependency(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Children[0].DependsOn = []string{"2019"} // an existing issue ID, not a sibling key

	result := ValidatePlan(plan, selection, live)
	if !result.Valid {
		t.Fatalf("expected external issue-ID dependency to be allowed, got errors=%v", result.Errors)
	}
}

func TestValidatePlanRejectsPublisherOwnedLabels(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Children[0].Labels = append(plan.Children[0].Labels, "goobers:ready")

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for a publisher-owned label request")
	}
	if !containsSubstring(result.Errors, "publisher-owned") {
		t.Fatalf("errors = %v, want a publisher-owned label finding", result.Errors)
	}
}

func TestValidatePlanRejectsBindingMismatch(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.Selection.SourceRunID = "some-other-run"

	result := ValidatePlan(plan, selection, live)
	if result.Valid {
		t.Fatal("expected invalid result for a selection binding mismatch")
	}
	if !containsSubstring(result.Errors, "sourceRunId") {
		t.Fatalf("errors = %v, want a sourceRunId binding finding", result.Errors)
	}
}

func TestValidatePlanFlagsLiveParentConflictDistinctlyFromErrors(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)

	changedLive := live
	changedLive.Title = "A retitled issue since selection"

	result := ValidatePlan(plan, selection, changedLive)
	if result.Valid {
		t.Fatal("expected an invalid result when the live parent changed")
	}
	if result.Conflict == nil {
		t.Fatal("expected a Conflict, not just an ordinary structural error, for a changed live parent")
	}
	if len(result.Errors) != 0 {
		t.Fatalf("errors = %v, want no ordinary structural errors alongside the conflict", result.Errors)
	}
}

func TestValidatePlanFlagsUnresolvedProductDecision(t *testing.T) {
	live := validLiveParent()
	selection := validSelection(t, live)
	plan := validPlan(selection)
	plan.UnresolvedDecision = "Should the replacement preserve the legacy API?"

	result := ValidatePlan(plan, selection, live)
	if result.Valid || !result.UnresolvedDecision {
		t.Fatalf("result = %+v, want a distinct unresolved-decision signal", result)
	}
	if len(result.Errors) != 0 || result.Conflict != nil {
		t.Fatalf("result = %+v, want no structural errors or parent conflict", result)
	}
}

func containsSubstring(errs []string, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}
