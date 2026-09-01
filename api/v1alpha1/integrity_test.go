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

// --- #3928: system-generated correction pointers survive contextFrom --------

// implementationContextFrom is the flagship implementation lane's own filter
// (reference-workflows/gaggles/goobers/workflows/implementation.yaml). The
// defect these cases pin was invisible to every existing learning-episode test
// because those fixtures declare no contextFrom at all, so the shape is spelled
// out here rather than reduced to a one-element list.
var implementationContextFrom = []string{"query-backlog", "implement", "remediate-ci", "review"}

func selectedNames(pointers []ContextPointer, sources []string) []string {
	selected := SelectContextPointers(pointers, sources)
	names := make([]string, 0, len(selected))
	for _, pointer := range selected {
		names = append(names, pointer.Name)
	}
	return names
}

func namesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The positive case, in the lane's own shape: the episode a repassing review
// gate injected must reach the stage it re-enters even though that stage
// declares contextFrom. Ordering is asserted too — the result is threaded
// straight into an invocation envelope whose digest is conformance-normative.
func TestSelectContextPointersKeepsInjectedLearningEpisode(t *testing.T) {
	pointers := []ContextPointer{
		{Name: "query-backlog.artifact[0]", Integrity: IntegrityDerived},
		{Name: "implement.artifact[0]", Integrity: IntegrityDerived},
		{Name: "learning.episode[7]", Integrity: IntegrityDerived},
		{Name: "review.verdict", Integrity: IntegrityDerived},
	}
	want := []string{
		"query-backlog.artifact[0]", "implement.artifact[0]", "learning.episode[7]", "review.verdict",
	}
	if got := selectedNames(pointers, implementationContextFrom); !namesEqual(got, want) {
		t.Fatalf("selected = %v, want %v — the injected correction feedback must survive contextFrom", got, want)
	}
}

// The episode survives a contextFrom that names NOTHING it could belong to.
// There is no source name a workflow author could write to select it, so a
// filter it cannot be named in must not be able to drop it either.
func TestSelectContextPointersKeepsLearningEpisodeUnderUnrelatedSources(t *testing.T) {
	pointers := []ContextPointer{
		{Name: "learning.episode[12]"},
		{Name: "review.verdict"},
	}
	if got := selectedNames(pointers, []string{"some-other-stage"}); !namesEqual(got, []string{"learning.episode[12]"}) {
		t.Fatalf("selected = %v, want only the learning episode", got)
	}
}

// NEGATIVE ISOLATION. The exemption is for exactly one naming contract. A
// pointer with no contract — a bare name, an external reference, a
// near-miss on the episode contract — is still dropped.
func TestSelectContextPointersStillDropsUnclassifiedPointers(t *testing.T) {
	pointers := []ContextPointer{
		{Name: "comments"},
		{Name: "learning"},
		{Name: "learning.episode"},
		{Name: "learning.episodes[3]"},
		{Name: "enrich.learning.episode[3]"},
		{Name: "learning.episode[3].artifact"},
		{Name: "verdict"},
		{Name: "artifact[0]"},
		{Name: ""},
	}
	if got := selectedNames(pointers, implementationContextFrom); len(got) != 0 {
		t.Fatalf("selected = %v, want none — contextFrom must not open unclassified pointers", got)
	}
}

// SOURCE SCOPING is unchanged in both directions: a producer the stage did not
// name is still excluded, and one it did name is still included.
func TestSelectContextPointersStillScopesSourcedPointers(t *testing.T) {
	pointers := []ContextPointer{
		{Name: "enrich.artifact[0]"},
		{Name: "enrich.verdict"},
		{Name: "query-backlog.artifact[0]"},
		{Name: "query-backlog-extra.artifact[0]"},
		{Name: "review.verdict"},
		{Name: "review.diff"},
	}
	want := []string{"query-backlog.artifact[0]", "review.verdict"}
	if got := selectedNames(pointers, implementationContextFrom); !namesEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
}

// MALFORMED NAMES FAIL CLOSED. Each of these resembles the episode contract
// without honouring it, and the exemption must not widen to cover any of them:
// a name that is not exactly what LearningEpisodePointerName emits gets no
// special treatment and, having no source either, is dropped.
func TestSelectContextPointersDropsMalformedLearningEpisodePointers(t *testing.T) {
	malformed := []string{
		"learning.episode[]",
		"learning.episode[abc]",
		"learning.episode[3",
		"learning.episode3]",
		"learning.episode[ 3 ]",
		"learning.episode[3][4]",
		"learning.episode[-1]",
		"learning.episode[+1]",
		"learning.episode[3.0]",
		"learning.episode[0x3]",
		"Learning.Episode[3]",
		" learning.episode[3]",
	}
	for _, name := range malformed {
		t.Run(name, func(t *testing.T) {
			if class, source := ClassifyContextPointer(name); class == ContextPointerLearningEpisode {
				t.Fatalf("ClassifyContextPointer(%q) = %v (source %q), want not a learning episode",
					name, class, source)
			}
			got := selectedNames([]ContextPointer{{Name: name}}, implementationContextFrom)
			if len(got) != 0 {
				t.Fatalf("selected = %v for malformed %q, want none", got, name)
			}
		})
	}
}

// A well-formed episode name is classified for any journal sequence, including
// the boundaries of the uint64 the sequence actually is.
func TestClassifyContextPointerAcceptsWellFormedLearningEpisodes(t *testing.T) {
	for _, name := range []string{
		"learning.episode[0]",
		"learning.episode[7]",
		"learning.episode[00]",
		"learning.episode[18446744073709551615]",
	} {
		class, source := ClassifyContextPointer(name)
		if class != ContextPointerLearningEpisode {
			t.Fatalf("ClassifyContextPointer(%q) = %v, want learning-episode", name, class)
		}
		if source != "" {
			t.Fatalf("ClassifyContextPointer(%q) source = %q, want empty — an episode has no producing state",
				name, source)
		}
		if !class.SystemGenerated() || class.SourceScoped() {
			t.Fatalf("learning-episode class: systemGenerated=%t sourceScoped=%t, want true/false",
				class.SystemGenerated(), class.SourceScoped())
		}
	}
}

// The classifier must report the SAME source the old prefix/equality tests
// derived, or contextFrom silently widens or narrows for every existing lane.
func TestClassifyContextPointerSources(t *testing.T) {
	tests := []struct {
		name       string
		wantClass  ContextPointerClass
		wantSource string
	}{
		{"implement.artifact[0]", ContextPointerStageArtifact, "implement"},
		{"implement.artifact[11]", ContextPointerStageArtifact, "implement"},
		{"a.b.artifact[0]", ContextPointerStageArtifact, "a.b"},
		{".artifact[0]", ContextPointerStageArtifact, ""},
		{"review.verdict", ContextPointerGateVerdict, "review"},
		{"review.verdict.verdict", ContextPointerGateVerdict, "review.verdict"},
		{"review.diff", ContextPointerUnclassified, ""},
		{"review.verdicts", ContextPointerUnclassified, ""},
		{"comments", ContextPointerUnclassified, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			class, source := ClassifyContextPointer(test.name)
			if class != test.wantClass || source != test.wantSource {
				t.Fatalf("ClassifyContextPointer(%q) = (%v, %q), want (%v, %q)",
					test.name, class, source, test.wantClass, test.wantSource)
			}
			if test.wantClass.SystemGenerated() {
				t.Fatalf("%q classified system-generated; only injected episodes are", test.name)
			}
		})
	}
}

// An unclassified pointer is neither source-scoped nor system-generated. That
// is what makes the fail-closed default fail closed: were "not source-scoped"
// read as "exempt", every malformed name above would ride through.
func TestUnclassifiedContextPointerIsNeitherScopedNorExempt(t *testing.T) {
	if ContextPointerUnclassified.SourceScoped() || ContextPointerUnclassified.SystemGenerated() {
		t.Fatalf("unclassified: sourceScoped=%t systemGenerated=%t, want false/false",
			ContextPointerUnclassified.SourceScoped(), ContextPointerUnclassified.SystemGenerated())
	}
}

// An empty contextFrom still preserves everything, unchanged and in order —
// including pointers of no recognized class, which classification must not
// start filtering out of a stage that declared no filter.
func TestSelectContextPointersEmptySourcesPreservesEverything(t *testing.T) {
	pointers := []ContextPointer{
		{Name: "comments"},
		{Name: "learning.episode[9]"},
		{Name: "learning.episode[bogus]"},
		{Name: "review.verdict"},
	}
	want := []string{"comments", "learning.episode[9]", "learning.episode[bogus]", "review.verdict"}
	if got := selectedNames(pointers, nil); !namesEqual(got, want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
}

// INTEGRITY ORDERING. Selection runs before ValidateInputIntegrity on both
// runners, so a pointer it drops is also never graded. Now that the episode
// survives, it must be graded — and a derived episode is admissible at the
// maintainer minimum the implementation lane declares, which is why the fix
// does not change that lane's admission outcome.
func TestSelectedLearningEpisodeIsGradedAtTheImplementationMinimum(t *testing.T) {
	artifact := &ArtifactPointer{
		Path: "artifacts/learning/episode-review-7.json", Digest: Digest([]byte("episode")),
		Integrity: IntegrityDerived,
	}
	pointers := SelectContextPointers([]ContextPointer{
		{Name: "learning.episode[7]", Integrity: IntegrityDerived, Artifact: artifact},
	}, implementationContextFrom)
	if len(pointers) != 1 {
		t.Fatalf("selected = %+v, want the episode", pointers)
	}
	if err := ValidateInputIntegrity(nil, pointers, IntegrityMaintainer); err != nil {
		t.Fatalf("ValidateInputIntegrity: %v — a derived episode must be admissible at the maintainer "+
			"minimum the implementation lane declares", err)
	}
	// And it is genuinely subject to admission rather than exempt from it: an
	// unlabeled episode fails closed like any other pointer.
	err := ValidateInputIntegrity(nil, []ContextPointer{{Name: "learning.episode[7]"}}, IntegrityMaintainer)
	var admission *IntegrityAdmissionError
	if !errors.As(err, &admission) || admission.Input != "learning.episode[7]" {
		t.Fatalf("unlabeled episode admission error = %v, want an IntegrityAdmissionError naming it", err)
	}
}
