package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestAliasBearingDeepCopiesPreserveWireShapeAndIsolation(t *testing.T) {
	t.Run("artifact pointer", func(t *testing.T) {
		original := &ContextPointer{
			Name: "review",
			Artifact: &ArtifactPointer{
				Path:      "artifacts/review.json",
				Digest:    Digest([]byte("review")),
				Integrity: IntegrityDerived,
			},
		}
		copied := original.DeepCopy()

		assertSameJSON(t, original, copied)
		if copied.Artifact == original.Artifact {
			t.Fatal("DeepCopy aliases the artifact pointer")
		}
		copied.Artifact.Integrity = IntegrityTrusted
		if original.Artifact.Integrity != IntegrityDerived {
			t.Fatalf("original artifact integrity = %q, want %q", original.Artifact.Integrity, IntegrityDerived)
		}
	})

	t.Run("continuation inputs", func(t *testing.T) {
		original := &ContinuationRequest{
			Inputs: []ContinuationInput{{
				Name:      "issue",
				Content:   "8271",
				Source:    "operator",
				Integrity: IntegrityMaintainer,
			}},
		}
		copied := original.DeepCopy()

		assertSameJSON(t, original, copied)
		if &copied.Inputs[0] == &original.Inputs[0] {
			t.Fatal("DeepCopy aliases continuation inputs")
		}
		copied.Inputs[0].Integrity = IntegrityUnapproved
		if original.Inputs[0].Integrity != IntegrityMaintainer {
			t.Fatalf("original input integrity = %q, want %q", original.Inputs[0].Integrity, IntegrityMaintainer)
		}
	})

	t.Run("verdict evidence", func(t *testing.T) {
		original := &Verdict{
			Decision: VerdictPass,
			Evidence: []ArtifactPointer{{
				Path:      "artifacts/evidence.json",
				Digest:    Digest([]byte("evidence")),
				Integrity: IntegrityDerived,
			}},
		}
		copied := original.DeepCopy()

		assertSameJSON(t, original, copied)
		if &copied.Evidence[0] == &original.Evidence[0] {
			t.Fatal("DeepCopy aliases verdict evidence")
		}
		copied.Evidence[0].Integrity = IntegrityTrusted
		if original.Evidence[0].Integrity != IntegrityDerived {
			t.Fatalf("original evidence integrity = %q, want %q", original.Evidence[0].Integrity, IntegrityDerived)
		}
	})
}

func assertSameJSON(t *testing.T, original, copied any) {
	t.Helper()

	originalJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	copiedJSON, err := json.Marshal(copied)
	if err != nil {
		t.Fatalf("marshal copy: %v", err)
	}
	if string(copiedJSON) != string(originalJSON) {
		t.Fatalf("copy JSON = %s, want %s", copiedJSON, originalJSON)
	}
}
