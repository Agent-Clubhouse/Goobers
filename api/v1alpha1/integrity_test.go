package v1alpha1

import (
	"errors"
	"testing"
)

func TestValidateInputIntegrityRejectsUnapprovedContext(t *testing.T) {
	err := ValidateInputIntegrity(
		&BacklogItem{ID: "42", Integrity: IntegrityMaintainer},
		[]ContextPointer{{
			Name: "comments", Integrity: IntegrityUnapproved,
			External: &ExternalRef{Kind: "issue-comments", URI: "https://example.test/issues/42#comments"},
		}},
		IntegrityMaintainer,
	)
	var admission *IntegrityAdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("error = %v, want IntegrityAdmissionError", err)
	}
	if admission.Input != "comments" || admission.Actual != IntegrityUnapproved ||
		admission.Minimum != IntegrityMaintainer {
		t.Fatalf("admission = %+v", admission)
	}
}

func TestValidateInputIntegrityAcceptsDerivedAtMaintainerTier(t *testing.T) {
	artifact := &ArtifactPointer{
		Path: "artifacts/review.json", Digest: Digest([]byte("review")), Integrity: IntegrityDerived,
	}
	err := ValidateInputIntegrity(nil, []ContextPointer{{
		Name: "review", Integrity: IntegrityDerived, Artifact: artifact,
	}}, IntegrityMaintainer)
	if err != nil {
		t.Fatalf("ValidateInputIntegrity: %v", err)
	}
}

func TestSelectContextPointersRoutesNamedProducers(t *testing.T) {
	pointers := []ContextPointer{
		{Name: "query-backlog.artifact[0]"},
		{Name: "query-backlog-extra.artifact[0]"},
		{Name: "gather-implement-context.artifact[0]"},
		{Name: "review.verdict"},
	}
	got := SelectContextPointers(pointers, []string{"query-backlog", "review"})
	if len(got) != 2 || got[0].Name != pointers[0].Name || got[1].Name != pointers[3].Name {
		t.Fatalf("selected pointers = %+v, want query-backlog and review", got)
	}
	if got := SelectContextPointers(pointers, nil); len(got) != len(pointers) {
		t.Fatalf("default selected pointers = %d, want %d", len(got), len(pointers))
	}
}
