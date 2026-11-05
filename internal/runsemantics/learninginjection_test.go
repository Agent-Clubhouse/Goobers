package runsemantics

import (
	"encoding/json"
	"strings"
	"testing"

	// Coverage for the SHARED half of learning-episode injection (#3913, plan item
	// E10) and for the one route that stays runner-only.
	//
	// internal/engine's gate retry arm calls BuildLearningEpisode,
	// LearningEpisodeAnnotation, LearningSourceEvent and the two name builders.
	// That makes them a contract between two runners rather than internal helpers
	// of this one: an edit here changes the engine's behaviour too, and — because
	// the episode is content-addressed — changes the ID every cross-run learning
	// consumer correlates on. These tests state the properties that sharing is
	// supposed to buy, which the parity harness cannot: parity compares two walks
	// of the SAME helpers, so a helper that drifted would drift identically on both
	// sides and every row would stay green.
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func learningEpisodeTestInput() LearningEpisodeInput {
	return LearningEpisodeInput{
		RunID:          "run-1",
		Workflow:       "web",
		WorkflowDigest: "sha256:wf",
		Gate:           "review",
		Stage:          "implement",
		SourceSeq:      4,
		SourceAttempt:  1,
		SourceResult: apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "3 tests failed",
			Error:   &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "# tests failed"},
		},
	}
}

// The names both runners derive must encode the SOURCE event's sequence, and
// the artifact name and pointer name must agree about it.
//
// This is what makes an injected pointer traceable: "learning.episode[4]" in a
// stage's envelope and "learning/episode-review-4.json" in the journal are the
// same claim about the same failure. A builder that drifted — a different
// separator, a gate-only name, an attempt number instead of a sequence — would
// break every consumer that joins the two, and would do it identically on both
// runners, which is exactly the class of bug parity cannot catch.
func TestLearningEpisodeNamesEncodeTheSourceSequence(t *testing.T) {
	if got, want := LearningEpisodeArtifactName("review", 4), "learning/episode-review-4.json"; got != want {
		t.Fatalf("artifact name = %q, want %q", got, want)
	}
	if got, want := LearningEpisodePointerName(4), "learning.episode[4]"; got != want {
		t.Fatalf("pointer name = %q, want %q", got, want)
	}
	// Both prefixes are what consumers (and the parity harness) match on to
	// find an injection without knowing which event it corrects.
	if !strings.HasPrefix(LearningEpisodeArtifactName("review", 4), "learning/episode-") {
		t.Fatal(`artifact name does not carry the "learning/episode-" prefix consumers match on`)
	}
	if !strings.HasPrefix(LearningEpisodePointerName(4), "learning.episode[") {
		t.Fatal(`pointer name does not carry the "learning.episode[" prefix consumers match on`)
	}
}

// The same input must produce byte-identical episode JSON, and therefore the
// same id and signature, every time.
//
// The engine records this artifact from inside a replayed workflow, so this is
// not a style preference: an unstable encoding there is a nondeterminism panic
// that wedges a live run. Building the episode twice and comparing the bytes
// catches map-ordered encoding, time reads, or any other impurity at the point
// it is introduced rather than on a production replay.
func TestBuildLearningEpisodeIsDeterministic(t *testing.T) {
	first := BuildLearningEpisode(learningEpisodeTestInput())
	second := BuildLearningEpisode(learningEpisodeTestInput())
	// Both runners marshal the episode with encoding/json and address the
	// bytes, so comparing the encodings is comparing what actually gets
	// digested and named.
	firstData, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("encode episode: %v", err)
	}
	secondData, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("encode episode (second): %v", err)
	}
	if string(firstData) != string(secondData) {
		t.Fatalf("episode encoding is not stable:\n  first:  %s\n  second: %s", firstData, secondData)
	}
	if first.ID != second.ID || first.Signature != second.Signature {
		t.Fatalf("episode identity is not stable: (%s, %s) vs (%s, %s)",
			first.ID, first.Signature, second.ID, second.Signature)
	}
	if first.ID == "" || first.Signature == "" {
		t.Fatalf("episode identity is empty: id=%q signature=%q", first.ID, first.Signature)
	}
	// And the id is genuinely content-addressed over the episode rather than
	// over its identity fields alone: a different correction produces a
	// different id, so consumers keyed on it do not collapse two failures.
	other := learningEpisodeTestInput()
	other.SourceResult.Error = &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "a different failure"}
	changed := BuildLearningEpisode(other)
	if changed.ID == first.ID {
		t.Fatalf("two different failures produced the same episode id %q", first.ID)
	}
}

// A deterministic failure — no reviewer, no verdict — must still produce a
// usable episode.
//
// This is the property that makes the injection lane-agnostic, and therefore
// the reason #3913 is the GENERIC retry arm rather than part of #3882's
// verdict-gated work. If the non-verdict fallback ever stopped synthesizing a
// finding, the port would quietly become reviewer-only and every deterministic
// repass would lose its correction feedback without any test failing.
func TestBuildLearningEpisodeFallsBackWithoutAVerdict(t *testing.T) {
	episode := BuildLearningEpisode(learningEpisodeTestInput())
	if len(episode.Findings) != 1 {
		t.Fatalf("episode carries %d findings, want 1 synthesized from the failure", len(episode.Findings))
	}
	if got := episode.Findings[0].Message; got != "# tests failed" {
		t.Fatalf("synthesized finding message = %q, want the failure's error message", got)
	}
	if episode.Findings[0].Location != "implement" {
		t.Fatalf("synthesized finding location = %q, want the failing stage", episode.Findings[0].Location)
	}
	if episode.Classification != apiv1.LearningValidation {
		t.Fatalf("classification = %q, want %q", episode.Classification, apiv1.LearningValidation)
	}
	if episode.CorrectionFeedback != "# tests failed" {
		t.Fatalf("correction feedback = %q, want the error message (the runner's precedence: rationale, "+
			"then error message, then summary)", episode.CorrectionFeedback)
	}
	if episode.NextAttempt != episode.SourceAttempt+1 {
		t.Fatalf("nextAttempt = %d, want sourceAttempt+1 (%d)", episode.NextAttempt, episode.SourceAttempt+1)
	}

	// The last-resort message: a failure carrying nothing at all still yields a
	// finding rather than an empty one.
	bare := learningEpisodeTestInput()
	bare.SourceResult = apiv1.ResultEnvelope{Status: apiv1.ResultFailure}
	empty := BuildLearningEpisode(bare)
	if len(empty.Findings) != 1 || empty.Findings[0].Message != "validation failed" {
		t.Fatalf("bare failure produced findings %+v, want one \"validation failed\" finding",
			empty.Findings)
	}
}

// With a reviewer verdict, the verdict's findings replace the synthesized one,
// the rationale becomes the correction feedback, and the verdict POINTER leads
// the evidence list.
//
// The evidence order is the subtle part and the reason the engine passes its
// verdict pointer into the shared builder rather than appending it afterwards:
// evidence is inside the content-addressed episode, so a runner that ordered it
// differently would produce a different id for the same failure — silently.
func TestBuildLearningEpisodeUsesTheVerdictArm(t *testing.T) {
	in := learningEpisodeTestInput()
	in.Verdict = &apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: "the reviewer explained why",
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError, Message: "missing test coverage", Location: "implement",
			LearningClassification: apiv1.LearningValidation,
		}},
	}
	in.VerdictPointer = &apiv1.ContextPointer{
		Name:      "review.verdict",
		Integrity: apiv1.IntegrityDerived,
		Artifact:  &apiv1.ArtifactPointer{Path: "artifacts/sha256/aa/bb", Digest: "sha256:aabb"},
	}
	episode := BuildLearningEpisode(in)
	if len(episode.Findings) != 1 || episode.Findings[0].Message != "missing test coverage" {
		t.Fatalf("episode findings = %+v, want the verdict's findings", episode.Findings)
	}
	if episode.Findings[0].ID == "" {
		t.Fatal("verdict findings must be normalized (identity assigned) before they enter the episode")
	}
	if episode.CorrectionFeedback != "the reviewer explained why" {
		t.Fatalf("correction feedback = %q, want the verdict rationale", episode.CorrectionFeedback)
	}
	if len(episode.Evidence) == 0 {
		t.Fatal("episode carries no evidence")
	}
	if got := episode.Evidence[0].Digest; got != "sha256:aabb" {
		t.Fatalf("evidence[0] digest = %q, want the verdict artifact's — the verdict pointer leads the "+
			"evidence list, and evidence is inside the content-addressed episode", got)
	}
}
