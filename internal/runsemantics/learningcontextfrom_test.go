package runsemantics

import (
	"testing"
	// #3928: a stage that declares contextFrom must still receive the learning
	// episode a repassing gate injected for it.
	//
	// Every pre-existing learning-episode fixture — the shared-helper suite in
	// learninginjection_test.go, the parity rows, the engine replay tests — walks a
	// stage with NO contextFrom, which is the one shape where the defect is
	// invisible: with an empty source list SelectContextPointers is the identity.
	// The flagship implementation lane declares one
	// (reference-workflows/gaggles/goobers/workflows/implementation.yaml), so on
	// the lane the injection was built for, the correction was dropped before
	// dispatch and before admission.
	//
	// This file walks a real Start() on that shape and asserts the pointer lands in
	// the repass's actual invocation envelope, alongside the negative claims that
	// stop the fix from being "select everything": a producer this stage did not
	// name is still excluded, and its artifacts do not arrive just because an
	// episode did.
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// The producer and the classifier must agree, or the exemption is decorative:
// LearningEpisodePointerName is the ONLY writer of this pointer name (both
// runners call it, and resume.go rebuilds the same shape), so whatever it emits
// has to be exactly what api/v1alpha1 recognizes as system-generated.
func TestLearningEpisodePointerNameIsClassifiedSystemGenerated(t *testing.T) {
	for _, seq := range []uint64{0, 1, 4, 42, 1 << 32, ^uint64(0)} {
		name := LearningEpisodePointerName(seq)
		class, source := apiv1.ClassifyContextPointer(name)
		if class != apiv1.ContextPointerLearningEpisode {
			t.Fatalf("ClassifyContextPointer(%q) = %v, want learning-episode — the producer and the "+
				"contextFrom classifier have drifted, so the injected pointer is filtered out again", name, class)
		}
		if !class.SystemGenerated() || source != "" {
			t.Fatalf("%q: systemGenerated=%t source=%q, want true and empty",
				name, class.SystemGenerated(), source)
		}
		if got := apiv1.SelectContextPointers(
			[]apiv1.ContextPointer{{Name: name}}, []string{"implement", "review"}); len(got) != 1 {
			t.Fatalf("SelectContextPointers dropped %q under a contextFrom filter", name)
		}
	}
}
