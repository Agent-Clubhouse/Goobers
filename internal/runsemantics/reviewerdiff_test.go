package runsemantics

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func preparedReviewEnvelope(t *testing.T) apiv1.InvocationEnvelope {
	t.Helper()
	ref, err := journal.ArtifactRef([]byte("+change\n"))
	if err != nil {
		t.Fatal(err)
	}
	artifact := &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: ReviewerDiffMediaType, Integrity: apiv1.IntegrityDerived}
	return apiv1.InvocationEnvelope{RunID: "run", TaskID: "run:review", PreparedReviewerDiff: &apiv1.PreparedReviewerDiff{Observed: true, Artifact: artifact}, ContextPointers: []apiv1.ContextPointer{{Name: ReviewerDiffPointerName("review"), Integrity: apiv1.IntegrityDerived, Artifact: artifact}}}
}

func TestPreparedReviewerDiffRequiresMatchingRecordedEvidence(t *testing.T) {
	cases := map[string]func(*apiv1.InvocationEnvelope){
		"unobserved":   func(e *apiv1.InvocationEnvelope) { e.PreparedReviewerDiff.Observed = false },
		"noncanonical": func(e *apiv1.InvocationEnvelope) { e.PreparedReviewerDiff.Artifact.Path = "artifacts/invented.patch" },
		"empty":        func(e *apiv1.InvocationEnvelope) { e.PreparedReviewerDiff.Artifact.Size = 0 },
		"media":        func(e *apiv1.InvocationEnvelope) { e.PreparedReviewerDiff.Artifact.MediaType = "text/plain" },
		"integrity":    func(e *apiv1.InvocationEnvelope) { e.PreparedReviewerDiff.Artifact.Integrity = "" },
		"missing":      func(e *apiv1.InvocationEnvelope) { e.ContextPointers = nil },
		"foreign run":  func(e *apiv1.InvocationEnvelope) { e.ContextPointers[0].RunID = "foreign" },
		"mismatch": func(e *apiv1.InvocationEnvelope) {
			ref := *e.ContextPointers[0].Artifact
			ref.Size++
			e.ContextPointers[0].Artifact = &ref
		},
		"duplicate": func(e *apiv1.InvocationEnvelope) { e.ContextPointers = append(e.ContextPointers, e.ContextPointers[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			env := preparedReviewEnvelope(t)
			mutate(&env)
			if _, err := PreparedReviewerDiffPointer(env); err == nil {
				t.Fatal("accepted invalid prepared evidence")
			}
		})
	}
	env := preparedReviewEnvelope(t)
	if p, err := PreparedReviewerDiffPointer(env); err != nil || p == nil {
		t.Fatalf("valid: %v %v", p, err)
	}
	for _, prepared := range []*apiv1.PreparedReviewerDiff{nil, {Observed: true}, {Observed: false}} {
		env.PreparedReviewerDiff, env.ContextPointers = prepared, nil
		if p, err := PreparedReviewerDiffPointer(env); err != nil || p != nil {
			t.Fatalf("legacy/empty: %v %v", p, err)
		}
	}
}

func TestPreparedReviewerVerdictDistinguishesEmptyFromUnobserved(t *testing.T) {
	env := preparedReviewEnvelope(t)
	if verdict, err := PreparedReviewerVerdict(env, true); err != nil || verdict != nil {
		t.Fatalf("recorded=%+v %v", verdict, err)
	}
	env.PreparedReviewerDiff.Artifact = nil
	env.ContextPointers = nil
	if verdict, err := PreparedReviewerVerdict(env, true); err != nil || verdict == nil || verdict.Decision != apiv1.VerdictFail {
		t.Fatalf("empty=%+v %v", verdict, err)
	}
	env.PreparedReviewerDiff.Observed = false
	if _, err := PreparedReviewerVerdict(env, true); err == nil {
		t.Fatal("unobserved implementation admitted")
	}
	if verdict, err := PreparedReviewerVerdict(env, false); err != nil || verdict != nil {
		t.Fatalf("research=%+v %v", verdict, err)
	}
}
